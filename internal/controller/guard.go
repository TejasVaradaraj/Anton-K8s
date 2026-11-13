package controller

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1 "github.com/TejasVaradaraj/anton-platform/api/v1alpha1"
	"github.com/TejasVaradaraj/anton-platform/internal/common"
	"github.com/TejasVaradaraj/anton-platform/internal/kv"
	"github.com/TejasVaradaraj/anton-platform/internal/metrics"
	"github.com/TejasVaradaraj/anton-platform/internal/planner"
	"github.com/TejasVaradaraj/anton-platform/internal/prefill"
)

// Guard watches inference replicas for disruption. It does two jobs:
//   - victim selection: scores each replica's cache and writes it as
//     pod-deletion-cost, so a scale-down removes the coldest replica;
//   - rescue: when a replica is about to die (spot notice, cordon/drain, or
//     deletion with a preStop window) it plans and runs KV offloads before
//     the deadline.
type Guard struct {
	client.Client
	PreStop  time.Duration // how long a deleted replica stays alive in its preStop hook
	HalfLife time.Duration

	mu      sync.Mutex
	handled map[types.UID]bool
	ticks   int
}

// Start makes Guard a controller-runtime Runnable.
func (g *Guard) Start(ctx context.Context) error {
	g.handled = map[types.UID]bool{}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			g.tick(ctx)
		}
	}
}

func (g *Guard) tick(ctx context.Context) {
	g.ticks++
	if common.Mode(ctx, g) != common.ModeAnton {
		return
	}
	var pods corev1.PodList
	if err := g.List(ctx, &pods, client.InNamespace(common.Namespace), client.MatchingLabels{"app": common.ReplicaApp}); err != nil {
		return
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.PodIP == "" || p.Status.Phase != corev1.PodRunning {
			continue
		}
		if deadline, risk := g.atRisk(ctx, p); risk {
			g.mu.Lock()
			first := !g.handled[p.UID]
			g.handled[p.UID] = true
			g.mu.Unlock()
			if first {
				go g.rescue(ctx, p.DeepCopy(), deadline)
			}
			continue
		}
		if g.ticks%5 == 0 {
			g.rank(ctx, p)
		}
	}
}

// atRisk returns the moment the replica's KV cache will be gone.
func (g *Guard) atRisk(ctx context.Context, p *corev1.Pod) (time.Time, bool) {
	if p.DeletionTimestamp != nil {
		grace := 30 * time.Second
		if p.DeletionGracePeriodSeconds != nil {
			grace = time.Duration(*p.DeletionGracePeriodSeconds) * time.Second
		}
		// DeletionTimestamp is "requested at + grace"; the process itself dies
		// when the preStop hook returns, which is earlier.
		return p.DeletionTimestamp.Add(-grace).Add(min(g.PreStop, grace)), true
	}
	var n corev1.Node
	if err := g.Get(ctx, types.NamespacedName{Name: p.Spec.NodeName}, &n); err != nil {
		return time.Time{}, false
	}
	if t, ok := common.SpotDeadline(&n); ok {
		return t, true
	}
	if n.Spec.Unschedulable { // cordoned: a drain is coming
		return time.Now().Add(g.PreStop), true
	}
	return time.Time{}, false
}

func candidates(info *kv.ReplicaInfo, prio map[string]int32) []planner.Candidate {
	cs := make([]planner.Candidate, 0, len(info.Sessions))
	for _, s := range info.Sessions {
		cs = append(cs, planner.Candidate{
			ID: s.SessionID, Tokens: s.Tokens, Bytes: s.Bytes, Priority: prio[s.SessionID],
			LastAccess: time.UnixMilli(s.LastAccessUnixMs), Hits: s.Hits,
		})
	}
	return cs
}

func (g *Guard) params(info *kv.ReplicaInfo) planner.Params { return replicaParams(info, g.HalfLife) }

// replicaParams builds the cost model from the rates a replica reports.
func replicaParams(info *kv.ReplicaInfo, halfLife time.Duration) planner.Params {
	curve, _ := prefill.Parse(info.PrefillCurve) // nil (linear rate) if absent or invalid
	return planner.Params{
		Curve:              curve,
		PrefillSecPerToken: info.PrefillMsPerToken / 1000,
		OffloadBps:         info.OffloadBytesPerSec,
		RestoreBps:         info.RestoreBytesPerSec,
		HalfLife:           halfLife,
	}
}

// hibernateMove decides whether hibernating a session should move its KV
// cache to the tier. It is the same offload-or-recompute rule the rescue
// planner uses: on hardware where recomputing is cheaper (a fast GPU and a
// slow tier) the cache is left on the replica, where vLLM can still reuse it
// until memory is needed.
func hibernateMove(info *kv.ReplicaInfo, session string) (bool, string) {
	for _, sc := range info.Sessions {
		if sc.SessionID != session {
			continue
		}
		p := replicaParams(info, 0)
		c := planner.Candidate{ID: session, Tokens: sc.Tokens, Bytes: sc.Bytes}
		if planner.Worth(c, p) {
			return true, ""
		}
		return false, fmt.Sprintf("recomputing %d tokens (%.2f s) is cheaper than moving them (%.2f s out + %.2f s back); cache left on the replica",
			sc.Tokens, p.PrefillSec(sc.Tokens), p.OffloadSec(sc.Bytes), p.RestoreSec(sc.Bytes))
	}
	return false, "no KV cache for this session on its replica"
}

// rank writes the replica's cache value as its pod-deletion-cost.
func (g *Guard) rank(ctx context.Context, p *corev1.Pod) {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	info, err := kv.Sessions(c, common.ReplicaURL(p.Status.PodIP))
	if err != nil {
		return
	}
	score := planner.ReplicaScore(candidates(info, nil), g.params(info), time.Now())
	metrics.ReplicaCacheValue.WithLabelValues(p.Name).Set(score)
	cost := strconv.Itoa(int(math.Min(score*1000, math.MaxInt32)))
	if p.Annotations[common.DeletionCost] == cost {
		return
	}
	orig := p.DeepCopy()
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	p.Annotations[common.DeletionCost] = cost
	_ = g.Patch(ctx, p, client.MergeFrom(orig))
}

func (g *Guard) rescue(ctx context.Context, p *corev1.Pod, deadline time.Time) {
	l := log.FromContext(ctx).WithValues("replica", p.Name, "node", p.Spec.NodeName)
	url := common.ReplicaURL(p.Status.PodIP)
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	info, err := kv.Sessions(c, url)
	cancel()
	if err != nil {
		l.Info("rescue: replica unreachable, its sessions will recompute", "err", err.Error())
		info = &kv.ReplicaInfo{}
	}

	var list v1.AgentSessionList
	_ = g.List(ctx, &list, client.InNamespace(common.Namespace))
	prio := map[string]int32{}
	release := map[string]string{} // session -> reason it was not rescued
	elsewhere := map[string]bool{} // sessions that now live on another replica
	for _, s := range list.Items {
		prio[s.Name] = s.Spec.Priority
		if s.Status.Replica == p.Name {
			release[s.Name] = "no KV cache on the lost replica"
		} else if s.Status.Replica != "" {
			elsewhere[s.Name] = true
		}
	}
	// A replica can still hold an old copy of a session the router has since
	// moved. That copy is stale: do not rescue it and do not touch the session.
	kept := info.Sessions[:0]
	for _, sc := range info.Sessions {
		if !elsewhere[sc.SessionID] {
			kept = append(kept, sc)
		}
	}
	info.Sessions = kept

	budget := time.Until(deadline) - time.Second // keep one second of safety margin
	plan := planner.PlanGreedy(candidates(info, prio), g.params(info), budget, time.Now())
	for _, sk := range plan.Skipped {
		release[sk.ID] = sk.Reason
	}
	for _, ch := range plan.Chosen {
		delete(release, ch.ID)
	}
	metrics.RescuePlans.Inc()
	metrics.RescueBudgetSeconds.Observe(budget.Seconds())
	metrics.RescueSessions.WithLabelValues("skipped").Add(float64(len(plan.Skipped)))
	l.Info("rescue plan", "budget", budget.Round(time.Millisecond).String(), "chosen", len(plan.Chosen),
		"skipped", len(plan.Skipped), "bytes", plan.MovedBytes, "expectedSavedSec", plan.SavedSec)

	// Sessions we are not saving are released immediately so the router can
	// move them without waiting.
	for name, reason := range release {
		_ = common.PatchStatus(ctx, g.Client, name, func(s *v1.AgentSession) {
			s.Status.Replica, s.Status.CacheLocation = "", v1.CacheNone
			s.Status.Message = "replica lost, not rescued: " + reason
		})
	}
	for _, ch := range plan.Chosen {
		_ = common.PatchStatus(ctx, g.Client, ch.ID, func(s *v1.AgentSession) {
			if s.Status.Phase == v1.PhaseActive {
				s.Status.Phase = v1.PhaseRescuing
			}
		})
	}

	dctx, dcancel := context.WithDeadline(ctx, deadline)
	defer dcancel()
	saved := 0
	for _, ch := range plan.Chosen {
		start := time.Now()
		err := kv.Offload(dctx, url, ch.ID, false)
		if err == nil {
			saved++
			metrics.RescueSessions.WithLabelValues("saved").Inc()
			metrics.RescueBytes.Add(float64(ch.Bytes))
		} else {
			metrics.RescueSessions.WithLabelValues("failed").Inc()
		}
		_ = common.PatchStatus(ctx, g.Client, ch.ID, func(s *v1.AgentSession) {
			if s.Status.Phase == v1.PhaseRescuing {
				s.Status.Phase = v1.PhaseActive
			}
			s.Status.Replica = ""
			if err == nil {
				s.Status.CacheLocation = v1.CacheTier
				s.Status.CachedTokens = ch.Tokens
				s.Status.Rescues++
				s.Status.Message = "KV cache rescued in " + time.Since(start).Round(time.Millisecond).String()
			} else {
				// Failure rule: fail to recompute, never hold the session hostage.
				s.Status.CacheLocation = v1.CacheNone
				s.Status.Message = "rescue failed, will recompute: " + err.Error()
			}
		})
	}
	l.Info("rescue done", "saved", saved, "planned", len(plan.Chosen), "released", len(release))
}
