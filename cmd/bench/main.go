// bench is the chaos benchmark. It runs the same seeded workload once per
// mode (plain Kubernetes, affinity routing only, full Anton), injects the same
// three disruptions each time (node drain, spot interruption, scale-down),
// then hibernates and wakes a few sessions. It runs inside the cluster as a Job.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/TejasVaradaraj/anton-platform/api/v1alpha1"
	"github.com/TejasVaradaraj/anton-platform/internal/common"
	"github.com/TejasVaradaraj/anton-platform/internal/kv"
)

var (
	routerURL     = flag.String("router", "http://anton-router:8080", "router base URL")
	tierURL       = flag.String("tier", "http://anton-tier:9000", "cache tier base URL")
	modes         = flag.String("modes", "plain,sandbox,routing,anton", "modes to run, in order")
	nSessions     = flag.Int("sessions", 24, "concurrent agent sessions")
	duration      = flag.Duration("duration", 3*time.Minute, "load duration per mode")
	replicas      = flag.Int("replicas", 4, "inference replicas at the start of each run")
	spotDeadline  = flag.Duration("spot-deadline", 10*time.Second, "notice given before a spot node vanishes")
	nHibernate    = flag.Int("hibernate", 4, "sessions to hibernate and wake after the load")
	seeds         = flag.String("seeds", "1", "comma-separated workload seeds; each seed runs every mode with the same workload")
	ctxMin        = flag.Int64("ctx-min", 8000, "smallest starting context, tokens")
	ctxMax        = flag.Int64("ctx-max", 40000, "largest starting context, tokens")
	growMin       = flag.Int64("grow-min", 300, "fewest tokens a turn appends")
	growMax       = flag.Int64("grow-max", 1500, "most tokens a turn appends")
	ctxCap        = flag.Int64("ctx-cap", 0, "context stops growing here, like an agent compacting at the model's window (0 = no cap)")
	chaos         = flag.String("chaos", "sim", "sim: the four scripted disruptions; external: none scripted, record replicas lost to real events (e.g. an AWS spot interruption); none")
	replicaWait   = flag.Duration("replica-timeout", 3*time.Minute, "how long to wait for fresh replicas (real vLLM loads a model: use ~20m)")
	scenario      = flag.String("scenario", "load", "load: the chaos benchmark; recovery: real agent conversations through node loss")
	recTurns      = flag.Int("recovery-turns", 3, "recovery: agent turns before the disruption")
	recSequential = flag.Bool("recovery-sequential", false, "recovery: drain one node, bring it back, then kill another (fits 2-node GPU clusters)")
	profile       = flag.String("profile", "", "label recorded with every result, e.g. qwen7b-l4")
	drainReturn   = flag.Duration("drain-return", 30*time.Second, "a drained node comes back (uncordoned) after this long")
)

type sample struct {
	Session    string
	Start, End time.Time
	TTFTMs     float64
	Ctx        int64
	Cached     int64
	Recomputed int64
	PrefillMs  float64
	Replica    string
	Err        string
}

type event struct {
	Kind      string
	At        time.Time
	Node      string
	Affected  map[string]bool   // replica pod names that were lost
	Sandboxes map[string]string // session -> workspace ID of sandboxes on the node, read just before
}

type eventResult struct {
	Kind          string  `json:"kind"`
	Affected      int     `json:"affected_sessions"`
	Lost          int     `json:"lost_kv_state"`
	Sandboxes     int     `json:"sandboxes_disrupted"`
	WorkspaceLost int     `json:"workspace_lost"`
	Unchecked     int     `json:"workspace_unchecked"`
	ResumeP50Ms   float64 `json:"resume_ttft_p50_ms"`
	ResumeP95Ms   float64 `json:"resume_ttft_p95_ms"`
	ResumeRecomp  int64   `json:"resume_recomputed_tokens"`
}

type result struct {
	Mode             string        `json:"mode"`
	Seed             int64         `json:"seed"`
	Profile          string        `json:"profile,omitempty"`
	Requests         int           `json:"requests"`
	Errors           int           `json:"errors"`
	TTFTP50Ms        float64       `json:"ttft_p50_ms"`
	TTFTP95Ms        float64       `json:"ttft_p95_ms"`
	RecomputedTokens int64         `json:"recomputed_tokens"`
	CachedTokens     int64         `json:"cached_tokens"`
	GPUSeconds       float64       `json:"prefill_gpu_seconds"`
	Events           []eventResult `json:"events"`
	ResumeP50Ms      float64       `json:"resume_ttft_p50_ms"`
	ResumeP95Ms      float64       `json:"resume_ttft_p95_ms"`
	Affected         int           `json:"affected_sessions"`
	Lost             int           `json:"lost_kv_state"`
	WakeP50Ms        float64       `json:"wake_ttft_p50_ms"`
	WakeKVKept       int           `json:"wake_kv_preserved"`
	WorkspaceKept    int           `json:"workspace_preserved"`
	Hibernated       int           `json:"hibernated"`
	Sandboxes        int           `json:"sandboxes_disrupted"`
	WorkspaceLost    int           `json:"workspace_lost"`
	Unchecked        int           `json:"workspace_unchecked"`
	SpotAffected     int           `json:"spot_affected"`
	SpotRescued      int           `json:"spot_rescued"`
}

var c client.Client

func main() {
	flag.Parse()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	var err error
	if c, err = client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme}); err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	if *scenario == "recovery" {
		var rs []recoveryResult
		for _, mode := range strings.Split(*modes, ",") {
			log.Printf("===== recovery, mode %s =====", mode)
			r := runRecovery(ctx, mode)
			b, _ := json.Marshal(r)
			fmt.Printf("ANTON_RECOVERY %s\n", b)
			rs = append(rs, r)
		}
		must(reset(ctx, common.ModeAnton))
		printRecovery(rs)
		return
	}
	var all []result
	for _, sd := range strings.Split(*seeds, ",") {
		seed, err := strconv.ParseInt(strings.TrimSpace(sd), 10, 64)
		if err != nil {
			log.Fatalf("bad seed %q", sd)
		}
		var results []result
		for _, mode := range strings.Split(*modes, ",") {
			log.Printf("===== mode %s, seed %d =====", mode, seed)
			r := runMode(ctx, mode, seed)
			b, _ := json.Marshal(r)
			fmt.Printf("ANTON_RESULT %s\n", b)
			results = append(results, r)
		}
		fmt.Printf("\nSeed %d\n", seed)
		printTable(results)
		all = append(all, results...)
	}
	must(reset(ctx, common.ModeAnton))
	if strings.Contains(*seeds, ",") {
		fmt.Printf("\nAll seeds combined (counts summed, latency percentiles averaged)\n")
		printTable(combineByMode(all))
	}
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

// ---------- cluster helpers ----------

func replicaPods(ctx context.Context) []corev1.Pod {
	var pods corev1.PodList
	_ = c.List(ctx, &pods, client.InNamespace(common.Namespace), client.MatchingLabels{"app": common.ReplicaApp})
	return pods.Items
}

func waitFor(ctx context.Context, what string, timeout time.Duration, cond func() bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", what)
}

func scaleReplicas(ctx context.Context, n int32) error {
	var d appsv1.Deployment
	if err := c.Get(ctx, types.NamespacedName{Namespace: common.Namespace, Name: common.ReplicaApp}, &d); err != nil {
		return err
	}
	orig := d.DeepCopy()
	d.Spec.Replicas = &n
	return c.Patch(ctx, &d, client.MergeFrom(orig))
}

func setNode(ctx context.Context, name string, f func(*corev1.Node)) error {
	var n corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &n); err != nil {
		return err
	}
	orig := n.DeepCopy()
	f(&n)
	return c.Patch(ctx, &n, client.MergeFrom(orig))
}

// reset returns the cluster to a clean, identical starting point.
func reset(ctx context.Context, mode string) error {
	var cm corev1.ConfigMap
	if err := c.Get(ctx, types.NamespacedName{Namespace: common.Namespace, Name: common.ModeConfigMap}, &cm); err != nil {
		return err
	}
	orig := cm.DeepCopy()
	cm.Data = map[string]string{"mode": mode}
	if err := c.Patch(ctx, &cm, client.MergeFrom(orig)); err != nil {
		return err
	}
	_ = c.DeleteAllOf(ctx, &v1.AgentSession{}, client.InNamespace(common.Namespace))
	if err := waitFor(ctx, "old sessions to go", 2*time.Minute, func() bool {
		var l v1.AgentSessionList
		return c.List(ctx, &l, client.InNamespace(common.Namespace)) == nil && len(l.Items) == 0
	}); err != nil {
		return err
	}
	var nodes corev1.NodeList
	_ = c.List(ctx, &nodes)
	for _, n := range nodes.Items {
		_ = setNode(ctx, n.Name, func(n *corev1.Node) {
			n.Spec.Unschedulable = false
			delete(n.Annotations, common.SpotAnnotation)
		})
	}
	if err := scaleReplicas(ctx, int32(*replicas)); err != nil {
		return err
	}
	_ = c.DeleteAllOf(ctx, &corev1.Pod{}, client.InNamespace(common.Namespace),
		client.MatchingLabels{"app": common.ReplicaApp}, client.GracePeriodSeconds(0))
	if err := waitFor(ctx, "fresh replicas", *replicaWait, func() bool {
		ready := 0
		pods := replicaPods(ctx)
		for i := range pods {
			if common.PodReady(&pods[i]) {
				ready++
			}
		}
		return ready == *replicas && len(pods) == *replicas
	}); err != nil {
		return err
	}
	_ = kv.Do(ctx, http.MethodDelete, *tierURL+"/blocks", nil, nil)
	time.Sleep(3 * time.Second) // let the operator and router caches settle
	return nil
}

// ---------- workload ----------

func doRequest(session string, ctxTok int64) sample {
	s := sample{Session: session, Ctx: ctxTok, Start: time.Now()}
	body, _ := json.Marshal(kv.CompletionRequest{SessionID: session, ContextTokens: ctxTok})
	req, _ := http.NewRequest(http.MethodPost, *routerURL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("X-Session-ID", session)
	hc := &http.Client{Timeout: 3 * time.Minute}
	resp, err := hc.Do(req)
	s.End = time.Now()
	s.TTFTMs = float64(s.End.Sub(s.Start).Microseconds()) / 1000
	if err != nil {
		s.Err = err.Error()
		return s
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.Err = "status " + strconv.Itoa(resp.StatusCode)
		return s
	}
	var out kv.CompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		s.Err = err.Error()
		return s
	}
	s.Cached, s.Recomputed, s.PrefillMs, s.Replica = out.CachedTokens, out.RecomputedTokens, out.PrefillMs, out.Replica
	return s
}

func runMode(ctx context.Context, mode string, seed int64) result {
	must(reset(ctx, mode))
	names := make([]string, *nSessions)
	for i := range names {
		names[i] = fmt.Sprintf("%s-s%02d", mode, i)
		prio := int32(0)
		if i%5 == 0 {
			prio = 10 // one session in five is high priority
		}
		must(c.Create(ctx, &v1.AgentSession{
			ObjectMeta: metav1.ObjectMeta{Name: names[i], Namespace: common.Namespace},
			Spec:       v1.AgentSessionSpec{Team: "bench", Model: "mock", Priority: prio},
		}))
	}
	must(waitFor(ctx, "sessions to become Active", 3*time.Minute, func() bool {
		var l v1.AgentSessionList
		_ = c.List(ctx, &l, client.InNamespace(common.Namespace))
		active := 0
		for _, s := range l.Items {
			if s.Status.Phase == v1.PhaseActive {
				active++
			}
		}
		return active == *nSessions
	}))

	var mu sync.Mutex
	var samples []sample
	lastCtx := map[string]int64{}
	end := time.Now().Add(*duration)
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed*1000 + int64(i)))
			ctxTok := *ctxMin + rng.Int63n(max(1, *ctxMax-*ctxMin))      // starting context
			time.Sleep(time.Duration(rng.Intn(3000)) * time.Millisecond) // stagger the cold starts
			for time.Now().Before(end) {
				s := doRequest(name, ctxTok)
				mu.Lock()
				samples = append(samples, s)
				lastCtx[name] = ctxTok
				mu.Unlock()
				time.Sleep(2*time.Second + time.Duration(rng.Intn(4000))*time.Millisecond) // agent "thinks"
				ctxTok += *growMin + rng.Int63n(max(1, *growMax-*growMin))                 // each turn appends context
				if *ctxCap > 0 && ctxTok > *ctxCap {
					ctxTok = *ctxCap
				}
			}
		}(i, name)
	}

	var events []event
	done := make(chan struct{})
	go func() {
		defer close(done)
		start := time.Now()
		used := map[string]bool{}
		at := func(frac float64) { time.Sleep(time.Until(start.Add(time.Duration(float64(*duration) * frac)))) }
		switch *chaos {
		case "sim":
			at(0.22)
			events = append(events, drain(ctx, used))
			at(0.42)
			events = append(events, spot(ctx, used))
			at(0.62)
			events = append(events, consolidate(ctx, used))
			at(0.80)
			events = append(events, scaleDown(ctx))
		case "external":
			events = watchExternal(ctx, start.Add(*duration))
		}
	}()
	wg.Wait()
	<-done

	res := analyze(mode, samples, events)
	res.Seed, res.Profile = seed, *profile
	workspaceCheck(ctx, events, &res)
	hibernateTest(ctx, names, lastCtx, &res)
	return res
}

// ---------- chaos ----------

// watchExternal records replicas lost to events the benchmark did not cause
// (a real spot interruption, a Karpenter consolidation): the moment a replica
// pod starts terminating, the sessions it served count as disrupted, exactly
// like a scripted disruption.
func watchExternal(ctx context.Context, until time.Time) []event {
	seen := map[string]bool{}
	var out []event
	for time.Now().Before(until) {
		for _, p := range replicaPods(ctx) {
			if p.DeletionTimestamp == nil || seen[p.Name] {
				continue
			}
			seen[p.Name] = true
			e := event{Kind: "external", Node: p.Spec.NodeName, Affected: map[string]bool{p.Name: true},
				Sandboxes: sandboxesOn(ctx, p.Spec.NodeName), At: time.Now()}
			log.Printf("CHAOS external: replica %s on %s is terminating (%d sandboxes there)", p.Name, p.Spec.NodeName, len(e.Sandboxes))
			out = append(out, e)
		}
		time.Sleep(time.Second)
	}
	return out
}

// pickNode chooses an undisturbed node that hosts at least one ready replica.
func pickNode(ctx context.Context, used map[string]bool) (string, map[string]bool) {
	byNode := map[string]map[string]bool{}
	for _, p := range replicaPods(ctx) {
		if common.PodReady(&p) && !used[p.Spec.NodeName] {
			if byNode[p.Spec.NodeName] == nil {
				byNode[p.Spec.NodeName] = map[string]bool{}
			}
			byNode[p.Spec.NodeName][p.Name] = true
		}
	}
	var nodes []string
	for n := range byNode {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	if len(nodes) == 0 {
		return "", nil
	}
	used[nodes[0]] = true
	return nodes[0], byNode[nodes[0]]
}

// sandboxesOn records the workspace ID of every sandbox on the node, so the
// run can tell afterwards whether each workspace survived the disruption.
func sandboxesOn(ctx context.Context, node string) map[string]string {
	out := map[string]string{}
	var pods corev1.PodList
	_ = c.List(ctx, &pods, client.InNamespace(common.Namespace), client.MatchingLabels{"app": common.SandboxApp})
	for _, p := range pods.Items {
		if p.Spec.NodeName != node {
			continue
		}
		session := p.Labels[common.SessionLabel]
		if st, err := agentState(ctx, session); err == nil {
			if ws, ok := st["workspace"].(string); ok {
				out[session] = ws
			}
		}
	}
	return out
}

// evictPodsOn evicts Anton's pods from a node through the Eviction API, as
// kubectl drain and Karpenter do, so PodDisruptionBudgets are honoured.
func evictPodsOn(ctx context.Context, node string) {
	var pods corev1.PodList
	_ = c.List(ctx, &pods, client.InNamespace(common.Namespace))
	var wg sync.WaitGroup
	for i := range pods.Items {
		p := &pods.Items[i]
		app := p.Labels["app"]
		if p.Spec.NodeName != node || (app != common.ReplicaApp && app != common.SandboxApp) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ev := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace}}
			for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
				err := c.SubResource("eviction").Create(ctx, p, ev)
				if err == nil || apierrors.IsNotFound(err) {
					return
				}
				if !apierrors.IsTooManyRequests(err) { // 429 = a PDB says wait
					log.Printf("evict %s: %v", p.Name, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func deletePodsOn(ctx context.Context, node string, opts ...client.DeleteOption) {
	var pods corev1.PodList
	_ = c.List(ctx, &pods, client.InNamespace(common.Namespace))
	for i := range pods.Items {
		p := &pods.Items[i]
		app := p.Labels["app"]
		if p.Spec.NodeName == node && (app == common.ReplicaApp || app == common.SandboxApp) {
			_ = c.Delete(ctx, p, opts...)
		}
	}
}

// drain is node maintenance: cordon, evict through the Eviction API, and
// bring the node back after drain-return.
func drain(ctx context.Context, used map[string]bool) event {
	node, affected := pickNode(ctx, used)
	e := event{Kind: "drain", Node: node, Affected: affected, Sandboxes: sandboxesOn(ctx, node), At: time.Now()}
	log.Printf("CHAOS drain %s (replicas %v, sandboxes %d)", node, keys(affected), len(e.Sandboxes))
	_ = setNode(ctx, node, func(n *corev1.Node) { n.Spec.Unschedulable = true })
	evictPodsOn(ctx, node)
	go func() {
		time.Sleep(*drainReturn)
		_ = setNode(ctx, node, func(n *corev1.Node) { n.Spec.Unschedulable = false })
		log.Printf("CHAOS %s back from maintenance", node)
	}()
	return e
}

// consolidate is what Karpenter does to an underused node: pick the worker
// running the fewest Anton pods, cordon it, evict everything; it never returns.
func consolidate(ctx context.Context, used map[string]bool) event {
	count := map[string]int{}
	replicasOn := map[string]map[string]bool{}
	var pods corev1.PodList
	_ = c.List(ctx, &pods, client.InNamespace(common.Namespace))
	for _, p := range pods.Items {
		app := p.Labels["app"]
		if p.Spec.NodeName == "" || used[p.Spec.NodeName] || (app != common.ReplicaApp && app != common.SandboxApp) {
			continue
		}
		count[p.Spec.NodeName]++
		if app == common.ReplicaApp && common.PodReady(&p) {
			if replicasOn[p.Spec.NodeName] == nil {
				replicasOn[p.Spec.NodeName] = map[string]bool{}
			}
			replicasOn[p.Spec.NodeName][p.Name] = true
		}
	}
	node := ""
	for n := range replicasOn { // only nodes that host a replica make an interesting test
		if node == "" || count[n] < count[node] || (count[n] == count[node] && n < node) {
			node = n
		}
	}
	if node == "" {
		log.Printf("CHAOS consolidate: no eligible node")
		return event{Kind: "consolidate", At: time.Now(), Affected: map[string]bool{}}
	}
	used[node] = true
	e := event{Kind: "consolidate", Node: node, Affected: replicasOn[node], Sandboxes: sandboxesOn(ctx, node), At: time.Now()}
	log.Printf("CHAOS consolidate %s (%d pods, replicas %v)", node, count[node], keys(e.Affected))
	_ = setNode(ctx, node, func(n *corev1.Node) { n.Spec.Unschedulable = true })
	evictPodsOn(ctx, node)
	return e
}

// spot is an involuntary disruption: a short notice, then the node is gone.
func spot(ctx context.Context, used map[string]bool) event {
	node, affected := pickNode(ctx, used)
	e := event{Kind: "spot", Node: node, Affected: affected, Sandboxes: sandboxesOn(ctx, node), At: time.Now()}
	deadline := time.Now().Add(*spotDeadline)
	log.Printf("CHAOS spot notice on %s, gone at %s (replicas %v)", node, deadline.Format(time.TimeOnly), keys(affected))
	_ = setNode(ctx, node, func(n *corev1.Node) {
		if n.Annotations == nil {
			n.Annotations = map[string]string{}
		}
		n.Annotations[common.SpotAnnotation] = deadline.Format(time.RFC3339)
	})
	time.Sleep(time.Until(deadline))
	_ = setNode(ctx, node, func(n *corev1.Node) { n.Spec.Unschedulable = true })
	deletePodsOn(ctx, node, client.GracePeriodSeconds(0))
	return e
}

// scaleDown removes one replica; Kubernetes picks the victim, steered (in
// anton mode) by the pod-deletion-cost the guard wrote.
func scaleDown(ctx context.Context) event {
	e := event{Kind: "scale-down", At: time.Now(), Affected: map[string]bool{}}
	before := replicaPods(ctx)
	_ = scaleReplicas(ctx, int32(len(before)-1))
	_ = waitFor(ctx, "scale-down victim", 10*time.Second, func() bool {
		for _, p := range replicaPods(ctx) {
			if p.DeletionTimestamp != nil {
				e.Affected[p.Name] = true
			}
		}
		return len(e.Affected) > 0
	})
	log.Printf("CHAOS scale-down removed %v", keys(e.Affected))
	return e
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------- hibernate / wake ----------

func agentState(ctx context.Context, session string) (map[string]any, error) {
	var p corev1.Pod
	if err := c.Get(ctx, types.NamespacedName{Namespace: common.Namespace, Name: session + "-sandbox"}, &p); err != nil {
		return nil, err
	}
	var st map[string]any
	return st, kv.Do(ctx, http.MethodGet, common.AgentURL(p.Status.PodIP)+"/state", nil, &st)
}

func hibernateTest(ctx context.Context, names []string, lastCtx map[string]int64, res *result) {
	n := min(*nHibernate, len(names))
	var wake []float64
	for _, name := range names[:n] {
		before, err := agentState(ctx, name)
		if err != nil {
			log.Printf("hibernate %s: cannot read sandbox state: %v", name, err)
			continue
		}
		var s v1.AgentSession
		must(c.Get(ctx, types.NamespacedName{Namespace: common.Namespace, Name: name}, &s))
		orig := s.DeepCopy()
		s.Spec.DesiredState = v1.StateHibernated
		must(c.Patch(ctx, &s, client.MergeFrom(orig)))
		if err := waitFor(ctx, name+" to hibernate", 90*time.Second, func() bool {
			var cur v1.AgentSession
			return c.Get(ctx, types.NamespacedName{Namespace: common.Namespace, Name: name}, &cur) == nil && cur.Status.Phase == v1.PhaseHibernated
		}); err != nil {
			log.Print(err)
			continue
		}
		res.Hibernated++
		var hib v1.AgentSession
		_ = c.Get(ctx, types.NamespacedName{Namespace: common.Namespace, Name: name}, &hib)
		log.Printf("HIBERNATED %s cache=%s note=%q", name, hib.Status.CacheLocation, hib.Status.Message)
		time.Sleep(2 * time.Second)
		prev := lastCtx[name]
		smp := doRequest(name, prev+500) // the request itself wakes the session
		if smp.Err != "" {
			log.Printf("wake %s failed: %s", name, smp.Err)
			continue
		}
		wake = append(wake, smp.TTFTMs)
		log.Printf("WAKE %s ttft=%.0fms ctx=%d cached=%d on %s (replica recorded before hibernate: %q)",
			name, smp.TTFTMs, prev+500, smp.Cached, smp.Replica, s.Status.Replica)
		if smp.Cached >= prev/2 {
			res.WakeKVKept++
		}
		if after, err := agentState(ctx, name); err == nil && after["restored"] == true &&
			after["workspace"] == before["workspace"] && after["lines"].(float64) >= before["lines"].(float64) {
			res.WorkspaceKept++
		}
	}
	res.WakeP50Ms = pct(wake, 50)
}

// workspaceCheck compares each disrupted sandbox's workspace ID with the one
// read just before its node was disrupted.
func workspaceCheck(ctx context.Context, events []event, res *result) {
	for i, e := range events {
		for session, before := range e.Sandboxes {
			// A sandbox we cannot reach is a failed measurement, not a lost
			// workspace: retry briefly, then count it as unchecked.
			var after map[string]any
			var err error
			for try := 0; try < 3; try++ {
				if after, err = agentState(ctx, session); err == nil {
					break
				}
				time.Sleep(2 * time.Second)
			}
			if err != nil {
				log.Printf("workspace check %s: %v", session, err)
				res.Events[i].Unchecked++
				res.Unchecked++
				continue
			}
			res.Events[i].Sandboxes++
			res.Sandboxes++
			if after["workspace"] != before {
				res.Events[i].WorkspaceLost++
				res.WorkspaceLost++
			}
		}
	}
}

// combineByMode merges per-seed results: counts are summed (so "lost of
// affected" reads across all seeds) and latency percentiles are averaged.
func combineByMode(rs []result) []result {
	var order []string
	by := map[string][]result{}
	for _, r := range rs {
		if by[r.Mode] == nil {
			order = append(order, r.Mode)
		}
		by[r.Mode] = append(by[r.Mode], r)
	}
	var out []result
	for _, mode := range order {
		group := by[mode]
		n := float64(len(group))
		c := result{Mode: mode, Events: make([]eventResult, len(group[0].Events))}
		for i := range c.Events {
			c.Events[i].Kind = group[0].Events[i].Kind
		}
		for _, r := range group {
			c.Requests += r.Requests
			c.Errors += r.Errors
			c.RecomputedTokens += r.RecomputedTokens
			c.CachedTokens += r.CachedTokens
			c.GPUSeconds += r.GPUSeconds
			c.Affected += r.Affected
			c.Lost += r.Lost
			c.WakeKVKept += r.WakeKVKept
			c.WorkspaceKept += r.WorkspaceKept
			c.Hibernated += r.Hibernated
			c.Sandboxes += r.Sandboxes
			c.WorkspaceLost += r.WorkspaceLost
			c.Unchecked += r.Unchecked
			c.SpotAffected += r.SpotAffected
			c.SpotRescued += r.SpotRescued
			c.TTFTP50Ms += r.TTFTP50Ms / n
			c.TTFTP95Ms += r.TTFTP95Ms / n
			c.ResumeP50Ms += r.ResumeP50Ms / n
			c.ResumeP95Ms += r.ResumeP95Ms / n
			c.WakeP50Ms += r.WakeP50Ms / n
			for i := range c.Events {
				if i < len(r.Events) {
					c.Events[i].Affected += r.Events[i].Affected
					c.Events[i].Lost += r.Events[i].Lost
					c.Events[i].Sandboxes += r.Events[i].Sandboxes
					c.Events[i].WorkspaceLost += r.Events[i].WorkspaceLost
					c.Events[i].ResumeP50Ms += r.Events[i].ResumeP50Ms / n
				}
			}
		}
		out = append(out, c)
	}
	return out
}

// ---------- analysis ----------

func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[min(len(s)-1, int(float64(len(s))*p/100))]
}

func analyze(mode string, samples []sample, events []event) result {
	res := result{Mode: mode}
	var ttft []float64
	bySession := map[string][]sample{}
	for _, s := range samples {
		res.Requests++
		if s.Err != "" {
			res.Errors++
			continue
		}
		ttft = append(ttft, s.TTFTMs)
		res.RecomputedTokens += s.Recomputed
		res.CachedTokens += s.Cached
		res.GPUSeconds += s.PrefillMs / 1000
		bySession[s.Session] = append(bySession[s.Session], s)
	}
	res.TTFTP50Ms, res.TTFTP95Ms = pct(ttft, 50), pct(ttft, 95)

	var allResume []float64
	for _, e := range events {
		er := eventResult{Kind: e.Kind}
		var resume []float64
		for _, ss := range bySession {
			// The session is affected if its last request before the event was
			// served by a replica that the event destroyed.
			var last *sample
			for i := range ss {
				if !ss[i].End.After(e.At) {
					last = &ss[i]
				}
			}
			if last == nil || !e.Affected[last.Replica] {
				continue
			}
			// Its resume request is the first one served by a surviving replica.
			for i := range ss {
				if ss[i].Start.Before(e.At) || e.Affected[ss[i].Replica] {
					continue
				}
				er.Affected++
				resume = append(resume, ss[i].TTFTMs)
				er.ResumeRecomp += ss[i].Recomputed
				if ss[i].Cached < last.Ctx/2 {
					er.Lost++
				}
				break
			}
		}
		er.ResumeP50Ms, er.ResumeP95Ms = pct(resume, 50), pct(resume, 95)
		res.Events = append(res.Events, er)
		res.Affected += er.Affected
		res.Lost += er.Lost
		if e.Kind == "spot" {
			res.SpotAffected += er.Affected
			res.SpotRescued += er.Affected - er.Lost
		}
		allResume = append(allResume, resume...)
	}
	res.ResumeP50Ms, res.ResumeP95Ms = pct(allResume, 50), pct(allResume, 95)
	return res
}

func printTable(rs []result) {
	fmt.Println("\n| Metric | " + strings.Join(colNames(rs), " | ") + " |")
	fmt.Println("|---|" + strings.Repeat("---|", len(rs)))
	row := func(name string, f func(r result) string) {
		cells := make([]string, len(rs))
		for i, r := range rs {
			cells[i] = f(r)
		}
		fmt.Println("| " + name + " | " + strings.Join(cells, " | ") + " |")
	}
	row("Requests (errors)", func(r result) string { return fmt.Sprintf("%d (%d)", r.Requests, r.Errors) })
	row("TTFT p50 / p95 (ms)", func(r result) string { return fmt.Sprintf("%.0f / %.0f", r.TTFTP50Ms, r.TTFTP95Ms) })
	row("Recomputed prefill tokens", func(r result) string { return strconv.FormatInt(r.RecomputedTokens, 10) })
	row("Recomputed tokens per request", func(r result) string {
		return fmt.Sprintf("%.0f", float64(r.RecomputedTokens)/float64(max(1, r.Requests-r.Errors)))
	})
	row("Prefill GPU-seconds", func(r result) string { return fmt.Sprintf("%.1f", r.GPUSeconds) })
	row("Prefill GPU-seconds per request", func(r result) string {
		return fmt.Sprintf("%.2f", r.GPUSeconds/float64(max(1, r.Requests-r.Errors)))
	})
	row("Post-disruption resume TTFT p50 / p95 (ms)", func(r result) string { return fmt.Sprintf("%.0f / %.0f", r.ResumeP50Ms, r.ResumeP95Ms) })
	row("Disrupted sessions that lost KV state", func(r result) string { return fmt.Sprintf("%d of %d", r.Lost, r.Affected) })
	row("Spot: sessions whose KV cache survived the notice", func(r result) string { return fmt.Sprintf("%d of %d", r.SpotRescued, r.SpotAffected) })
	row("Disrupted sandboxes that lost their workspace", func(r result) string {
		s := fmt.Sprintf("%d of %d", r.WorkspaceLost, r.Sandboxes)
		if r.Unchecked > 0 {
			s += fmt.Sprintf(" (%d unchecked)", r.Unchecked)
		}
		return s
	})
	for i := range rs[0].Events {
		kind := rs[0].Events[i].Kind
		row("  "+kind+": lost / affected, resume p50 (ms)", func(r result) string {
			if i >= len(r.Events) {
				return "-"
			}
			e := r.Events[i]
			return fmt.Sprintf("%d / %d, %.0f", e.Lost, e.Affected, e.ResumeP50Ms)
		})
	}
	row("Wake-from-hibernate TTFT p50 (ms)", func(r result) string { return fmt.Sprintf("%.0f", r.WakeP50Ms) })
	row("Wakes with KV cache preserved", func(r result) string { return fmt.Sprintf("%d of %d", r.WakeKVKept, r.Hibernated) })
	row("Wakes with workspace preserved", func(r result) string { return fmt.Sprintf("%d of %d", r.WorkspaceKept, r.Hibernated) })
}

func colNames(rs []result) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Mode
	}
	return out
}
