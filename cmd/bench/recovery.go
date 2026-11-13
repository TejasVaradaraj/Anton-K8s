package main

// The recovery scenario: real agent conversations through node loss.
//
// Each session's agent (the builtin agent loop, or OpenCode) holds a
// conversation in its workspace. The scenario talks to the agents directly,
// takes out two nodes that host sandboxes (a drain through the Eviction API
// and a spot-style hard kill), waits for the sessions to come back, and
// checks every disrupted session: is the conversation still there (same
// workspace, same number of turns), does the next turn work, and was the KV
// cache still warm for it?

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/TejasVaradaraj/anton-platform/api/v1alpha1"
	"github.com/TejasVaradaraj/anton-platform/internal/common"
)

type recoveryResult struct {
	Mode               string   `json:"mode"`
	Profile            string   `json:"profile,omitempty"`
	Sessions           int      `json:"sessions"`
	Disrupted          int      `json:"disrupted"`
	ConversationKept   int      `json:"conversation_kept"`
	NextTurnOK         int      `json:"next_turn_ok"`
	KVWarm             int      `json:"kv_warm"`
	ResumeP50Ms        float64  `json:"resume_ttft_p50_ms"`
	ResumeP95Ms        float64  `json:"resume_ttft_p95_ms"`
	RecoverySecondsP50 float64  `json:"recovery_seconds_p50"`
	Errors             []string `json:"errors,omitempty"`
}

type agentTurn struct {
	Reply            string  `json:"reply"`
	Turns            int     `json:"turns"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	RecomputedTokens int64   `json:"recomputed_tokens"`
	Replica          string  `json:"replica"`
	TTFTMs           float64 `json:"ttft_ms"`
}

func sandboxPod(ctx context.Context, session string) (*corev1.Pod, error) {
	var p corev1.Pod
	err := c.Get(ctx, types.NamespacedName{Namespace: common.Namespace, Name: session + "-sandbox"}, &p)
	return &p, err
}

func agentChat(ctx context.Context, session, text string) (agentTurn, error) {
	p, err := sandboxPod(ctx, session)
	if err != nil {
		return agentTurn{}, err
	}
	if !common.PodReady(p) {
		return agentTurn{}, fmt.Errorf("sandbox not ready")
	}
	body, _ := json.Marshal(map[string]string{"content": text})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, common.AgentURL(p.Status.PodIP)+"/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return agentTurn{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var b bytes.Buffer
		b.ReadFrom(resp.Body)
		return agentTurn{}, fmt.Errorf("agent: %d %s", resp.StatusCode, strings.TrimSpace(b.String()))
	}
	var t agentTurn
	return t, json.NewDecoder(resp.Body).Decode(&t)
}

type sandboxSnapshot struct {
	node, workspace string
	turns           int
}

func runRecovery(ctx context.Context, mode string) recoveryResult {
	must(reset(ctx, mode))
	res := recoveryResult{Mode: mode, Profile: *profile, Sessions: *nSessions}
	names := make([]string, *nSessions)
	for i := range names {
		names[i] = fmt.Sprintf("%s-r%02d", mode, i)
		must(c.Create(ctx, &v1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Name: names[i], Namespace: common.Namespace},
			Spec:       v1.AgentSessionSpec{Team: "bench", Model: "agent"},
		}))
	}
	must(waitFor(ctx, "recovery sessions to become Active", 5*time.Minute, func() bool { return allActive(ctx, names) }))

	// Build up real conversations.
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			for t := 0; t < *recTurns; t++ {
				if _, err := agentChat(ctx, name, fmt.Sprintf("step %d: keep working on the task", t+1)); err != nil {
					mu.Lock()
					res.Errors = append(res.Errors, fmt.Sprintf("%s turn %d: %v", name, t+1, err))
					mu.Unlock()
					return
				}
			}
		}(name)
	}
	wg.Wait()

	before := snapshotSandboxes(ctx, names)

	// Only nodes that host an inference replica are disrupted: never the
	// node running the control components and the cache tier.
	count := map[string]int{}
	for _, b := range before {
		count[b.node]++
	}
	replicaNodes := map[string]bool{}
	for _, p := range replicaPods(ctx) {
		replicaNodes[p.Spec.NodeName] = true
	}
	var nodes []string
	for n := range replicaNodes {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool {
		return count[nodes[i]] > count[nodes[j]] || (count[nodes[i]] == count[nodes[j]] && nodes[i] < nodes[j])
	})

	var resume, recoverSecs []float64
	check := func(kind, node string, snap map[string]sandboxSnapshot) {
		disrupted := sessionsOn(snap, node)
		res.Disrupted += len(disrupted)
		start := time.Now()
		log.Printf("RECOVERY %s %s (%d sandboxes)", kind, node, len(disrupted))
		_ = setNode(ctx, node, func(n *corev1.Node) { n.Spec.Unschedulable = true })
		if kind == "drain" {
			evictPodsOn(ctx, node)
		} else {
			deletePodsOn(ctx, node, client.GracePeriodSeconds(0))
		}
		r, s := checkSessions(ctx, disrupted, snap, map[string]bool{node: true}, start, &res)
		resume, recoverSecs = append(resume, r...), append(recoverSecs, s...)
	}
	if *recSequential {
		// Two disruptions one after another, so a 2-GPU cluster always has a
		// surviving node to land on: drain A, bring it back, then kill B.
		if len(nodes) > 0 {
			check("drain", nodes[0], before)
			_ = setNode(ctx, nodes[0], func(n *corev1.Node) { n.Spec.Unschedulable = false })
			log.Printf("RECOVERY %s back", nodes[0])
			_ = waitFor(ctx, "replicas ready again", 10*time.Minute, func() bool { return readyReplicas(ctx) >= *replicas })
		}
		if len(nodes) > 1 {
			// Sessions moved during the drain: look again before the kill.
			now := snapshotSandboxes(ctx, names)
			check("hard kill", nodes[1], now)
		}
	} else {
		var drained, killed []string
		snap := before
		if len(nodes) > 0 {
			drained = sessionsOn(snap, nodes[0])
		}
		if len(nodes) > 1 {
			killed = sessionsOn(snap, nodes[1])
		}
		start := time.Now()
		gone := map[string]bool{}
		if len(nodes) > 0 {
			log.Printf("RECOVERY drain %s (%d sandboxes)", nodes[0], len(drained))
			_ = setNode(ctx, nodes[0], func(n *corev1.Node) { n.Spec.Unschedulable = true })
			evictPodsOn(ctx, nodes[0])
			gone[nodes[0]] = true
		}
		if len(nodes) > 1 {
			log.Printf("RECOVERY hard kill %s (%d sandboxes)", nodes[1], len(killed))
			_ = setNode(ctx, nodes[1], func(n *corev1.Node) { n.Spec.Unschedulable = true })
			deletePodsOn(ctx, nodes[1], client.GracePeriodSeconds(0))
			gone[nodes[1]] = true
		}
		all := append(append([]string{}, drained...), killed...)
		sort.Strings(all)
		res.Disrupted = len(all)
		resume, recoverSecs = checkSessions(ctx, all, snap, gone, start, &res)
	}
	res.ResumeP50Ms, res.ResumeP95Ms = pct(resume, 50), pct(resume, 95)
	res.RecoverySecondsP50 = pct(recoverSecs, 50)
	return res
}

func snapshotSandboxes(ctx context.Context, names []string) map[string]sandboxSnapshot {
	out := map[string]sandboxSnapshot{}
	for _, name := range names {
		p, err := sandboxPod(ctx, name)
		st, err2 := agentState(ctx, name)
		if err != nil || err2 != nil {
			continue
		}
		ws, _ := st["workspace"].(string)
		turns, _ := st["turns"].(float64)
		out[name] = sandboxSnapshot{node: p.Spec.NodeName, workspace: ws, turns: int(turns)}
	}
	return out
}

func sessionsOn(snap map[string]sandboxSnapshot, node string) []string {
	var out []string
	for name, b := range snap {
		if b.node == node {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func readyReplicas(ctx context.Context) int {
	n := 0
	pods := replicaPods(ctx)
	for i := range pods {
		if common.PodReady(&pods[i]) {
			n++
		}
	}
	return n
}

// checkSessions waits for each disrupted session to come back off the
// disrupted nodes, checks its conversation, and takes one more turn.
func checkSessions(ctx context.Context, disrupted []string, before map[string]sandboxSnapshot, gone map[string]bool,
	start time.Time, res *recoveryResult) (resume, recoverSecs []float64) {
	for _, name := range disrupted {
		var back time.Time
		_ = waitFor(ctx, name+" to come back", 6*time.Minute, func() bool {
			p, err := sandboxPod(ctx, name)
			if err == nil && common.PodReady(p) && !gone[p.Spec.NodeName] {
				back = time.Now()
				return true
			}
			return false
		})
		if back.IsZero() {
			res.Errors = append(res.Errors, name+": sandbox did not come back")
			continue
		}
		recoverSecs = append(recoverSecs, back.Sub(start).Seconds())
		if st, err := agentState(ctx, name); err == nil {
			ws, _ := st["workspace"].(string)
			turns, _ := st["turns"].(float64)
			if ws == before[name].workspace && int(turns) == before[name].turns {
				res.ConversationKept++
			} else {
				log.Printf("RECOVERY %s: conversation lost (workspace %q -> %q, turns %d -> %v)", name, before[name].workspace, ws, before[name].turns, turns)
			}
		}
		t, err := agentChat(ctx, name, "continue from where you were")
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s next turn: %v", name, err))
			continue
		}
		res.NextTurnOK++
		resume = append(resume, t.TTFTMs)
		if t.PromptTokens > 0 && t.CachedTokens*2 >= t.PromptTokens {
			res.KVWarm++
		}
		log.Printf("RECOVERY %s: turns %d, %d of %d prompt tokens cached, reply in %.0f ms on %s",
			name, t.Turns, t.CachedTokens, t.PromptTokens, t.TTFTMs, t.Replica)
	}
	return resume, recoverSecs
}

func allActive(ctx context.Context, names []string) bool {
	for _, n := range names {
		var s v1.AgentSession
		if c.Get(ctx, types.NamespacedName{Namespace: common.Namespace, Name: n}, &s) != nil || s.Status.Phase != v1.PhaseActive {
			return false
		}
	}
	return true
}

func printRecovery(rs []recoveryResult) {
	fmt.Println("\n| Recovery | " + strings.Join(func() []string {
		var m []string
		for _, r := range rs {
			m = append(m, r.Mode)
		}
		return m
	}(), " | ") + " |")
	fmt.Println("|---|" + strings.Repeat("---|", len(rs)))
	row := func(name string, f func(r recoveryResult) string) {
		var cells []string
		for _, r := range rs {
			cells = append(cells, f(r))
		}
		fmt.Println("| " + name + " | " + strings.Join(cells, " | ") + " |")
	}
	row("Sessions on disrupted nodes", func(r recoveryResult) string { return fmt.Sprintf("%d of %d", r.Disrupted, r.Sessions) })
	row("Conversation intact after recovery", func(r recoveryResult) string { return fmt.Sprintf("%d of %d", r.ConversationKept, r.Disrupted) })
	row("Next turn succeeded", func(r recoveryResult) string { return fmt.Sprintf("%d of %d", r.NextTurnOK, r.Disrupted) })
	row("Next turn found its KV cache warm", func(r recoveryResult) string { return fmt.Sprintf("%d of %d", r.KVWarm, r.Disrupted) })
	row("Next-turn latency p50 / p95 (ms)", func(r recoveryResult) string { return fmt.Sprintf("%.0f / %.0f", r.ResumeP50Ms, r.ResumeP95Ms) })
	row("Sandbox back after (s, p50)", func(r recoveryResult) string { return fmt.Sprintf("%.0f", r.RecoverySecondsP50) })
	row("Errors", func(r recoveryResult) string { return fmt.Sprintf("%d", len(r.Errors)) })
}
