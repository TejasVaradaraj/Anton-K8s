// The Anton session router. It sends each session to the replica that holds
// its KV cache, breaks that affinity only when waiting would cost more than
// recomputing, moves sessions off replicas that are about to die, and wakes
// hibernated sessions (restoring their KV cache before the first token).
package main

import (
	"net"

	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	extproc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1 "github.com/TejasVaradaraj/anton-platform/api/v1alpha1"
	"github.com/TejasVaradaraj/anton-platform/internal/common"
	"github.com/TejasVaradaraj/anton-platform/internal/kv"
	"github.com/TejasVaradaraj/anton-platform/internal/metrics"
	"github.com/TejasVaradaraj/anton-platform/internal/openai"
	"github.com/TejasVaradaraj/anton-platform/internal/prefill"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

type router struct {
	tenancy     bool // sessions live in team namespaces; requests carry the team as cache_salt
	c           client.Client
	msPerToken  float64
	curve       *prefill.Curve
	concurrency int

	mu        sync.Mutex
	assign    map[string]string    // session -> replica pod name
	seenRV    map[string]string    // session -> resourceVersion we already restored for
	lastTouch map[string]time.Time // session -> last lastActivity patch
	inflight  map[string]int       // replica -> requests in flight
	ewmaMs    map[string]float64   // replica -> smoothed service time
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	msPerToken := flag.Float64("prefill-ms-per-token", 0.4, "prefill cost used by the affinity-vs-load rule")
	eppAddr := flag.String("epp-addr", ":9002", "gRPC address for the Gateway API Inference Extension endpoint picker (empty disables it)")
	curveFlag := flag.String("prefill-curve", "", "measured prefill seconds by context, tokens:seconds,... (overrides --prefill-ms-per-token)")
	concurrency := flag.Int("replica-concurrency", 4, "requests a replica serves at once")
	tenancy := flag.Bool("tenancy", false, "sessions live in team namespaces (anton-team-<team>); requests carry the team as vLLM's cache_salt")
	flag.Parse()
	ctrl.SetLogger(zap.New())

	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = v1.AddToScheme(scheme)
	cfg := ctrl.GetConfigOrDie()
	// Namespaced RBAC: watch Anton's namespace and, with tenancy on, the team
	// namespaces whose RoleBinding for the router already exists.
	namespaces := map[string]cache.Config{common.Namespace: {}}
	var direct client.Client
	routerBound := func(ns string) bool {
		var rb rbacv1.RoleBinding
		return direct.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "anton-tenant-router-anton-router"}, &rb) == nil
	}
	if *tenancy {
		var err error
		if direct, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
			ctrl.Log.Error(err, "client")
			os.Exit(1)
		}
		tenants, err := common.TenantNamespaces(context.Background(), direct)
		if err != nil {
			ctrl.Log.Error(err, "listing team namespaces")
			os.Exit(1)
		}
		for _, ns := range tenants {
			if routerBound(ns) {
				namespaces[ns] = cache.Config{}
			}
		}
		ctrl.Log.Info("tenancy on", "namespaces", len(namespaces))
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"},
		Cache: cache.Options{DefaultNamespaces: namespaces},
	})
	if err != nil {
		ctrl.Log.Error(err, "manager")
		os.Exit(1)
	}
	rt := &router{
		tenancy: *tenancy,
		c:       mgr.GetClient(), msPerToken: *msPerToken, curve: prefill.MustParse(*curveFlag), concurrency: *concurrency,
		assign: map[string]string{}, seenRV: map[string]string{}, lastTouch: map[string]time.Time{},
		inflight: map[string]int{}, ewmaMs: map[string]float64{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", rt.handle)
	mux.HandleFunc("POST /s/{session}/v1/chat/completions", rt.handle)
	mux.HandleFunc("GET /v1/models", rt.models)
	mux.HandleFunc("GET /s/{session}/v1/models", rt.models)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("GET /metrics", promhttp.HandlerFor(ctrlmetrics.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /debug/assignments", func(w http.ResponseWriter, _ *http.Request) {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"assign": rt.assign, "inflight": rt.inflight})
	})
	// Added as a Runnable so it starts only after the informer cache has synced.
	_ = mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		srv := &http.Server{Addr: *addr, Handler: mux}
		go func() {
			<-ctx.Done()
			// Let streams that are in flight finish (a restart for a new team
			// namespace must not cut an agent's reply short).
			grace, stop := context.WithTimeout(context.Background(), 60*time.Second)
			defer stop()
			if srv.Shutdown(grace) != nil {
				srv.Close()
			}
		}()
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}))
	if *eppAddr != "" {
		// The Gateway API Inference Extension endpoint picker, sharing the
		// proxy's placement state (also started after the cache syncs).
		_ = mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			lis, err := net.Listen("tcp", *eppAddr)
			if err != nil {
				return err
			}
			gs := grpc.NewServer()
			extproc.RegisterExternalProcessorServer(gs, &eppServer{pick: rt.eppPick})
			go func() { <-ctx.Done(); gs.GracefulStop() }()
			return gs.Serve(lis)
		}))
	}
	ctx, cancel := context.WithCancel(ctrl.SetupSignalHandler())
	defer cancel()
	if *tenancy {
		// A team created after start-up is outside the cache. When its
		// namespace is provisioned, stop; the Deployment restarts the router,
		// which then watches it. In-flight responses get the server's
		// shutdown grace period below.
		_ = mgr.Add(manager.RunnableFunc(func(c context.Context) error {
			t := time.NewTicker(10 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-c.Done():
					return nil
				case <-t.C:
				}
				tenants, err := common.TenantNamespaces(c, direct)
				if err != nil {
					continue
				}
				for _, ns := range tenants {
					if _, ok := namespaces[ns]; !ok && routerBound(ns) {
						ctrl.Log.Info("new team namespace provisioned: restarting to watch it", "namespace", ns)
						cancel()
						return nil
					}
				}
			}
		}))
	}
	if err := mgr.Start(ctx); err != nil {
		ctrl.Log.Error(err, "run")
		os.Exit(1)
	}
}

// handle serves OpenAI chat-completions requests (what agents and vLLM speak)
// and the benchmark's compact form. The session comes from /s/{session}/...,
// the X-Session-ID header, or the legacy body field. Chat bodies are
// forwarded untouched; only an estimate of their length is read, to price a
// re-prefill in the affinity rule. Responses, including server-sent-event
// streams, are passed through as they arrive.
func (rt *router) handle(w http.ResponseWriter, req *http.Request) {
	raw, _ := io.ReadAll(req.Body)
	var in openai.Request
	_ = json.Unmarshal(raw, &in)
	sid := openai.Session(req, &in)
	if sid == "" {
		http.Error(w, "need a session: /s/{session}/v1/..., the X-Session-ID header, or session_id", http.StatusBadRequest)
		return
	}
	view := requestView(sid, &in)
	if !in.IsChat() {
		raw, _ = json.Marshal(view)
	}
	ctx := req.Context()
	if rt.tenancy && in.IsChat() {
		// Shared replicas, separate caches: vLLM keys its prefix cache by
		// token content, so without a salt one team could detect another's
		// prompt by timing. The salt is the team, whatever the client sent.
		if s, ok := rt.session(ctx, sid); ok && s.Spec.Team != "" {
			raw = openai.WithCacheSalt(raw, s.Spec.Team)
		}
	}
	mode := rt.mode(ctx)

	start := time.Now()
	rt.awaitActive(ctx, sid)
	waitMs := msSince(start)
	metrics.RouterWakeWaitSeconds.Observe(waitMs / 1000)

	tried := map[string]bool{}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		target, restoreMs, decision, err := rt.pick(ctx, mode, &view, tried)
		if err != nil {
			lastErr = err
			break
		}
		observeDecision(mode, decision)
		if decision == "restore" {
			metrics.RouterRestoreSeconds.Observe(restoreMs / 1000)
		}
		resp, done, err := rt.forward(ctx, target, raw, sid)
		if err != nil {
			// Nothing has reached the client yet, so another replica can take it.
			tried[target.Name], lastErr = true, err
			continue
		}
		for _, h := range []string{"Content-Type", "Cache-Control"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.Header().Set("X-Anton-Replica", target.Name)
		w.Header().Set("X-Anton-Wait-Ms", fmt.Sprintf("%.1f", waitMs))
		w.Header().Set("X-Anton-Restore-Ms", fmt.Sprintf("%.1f", restoreMs))
		w.WriteHeader(resp.StatusCode)
		streamCopy(w, resp.Body)
		resp.Body.Close()
		done()
		if resp.StatusCode == http.StatusOK {
			rt.touch(sid, view.ContextTokens)
		}
		return
	}
	metrics.RouterRequests.WithLabelValues(mode, "error").Inc()
	http.Error(w, fmt.Sprint("no replica could serve the request: ", lastErr), http.StatusBadGateway)
}

// requestView is what placement needs from a request: the session and how
// long its prompt is (estimated for chat requests, given for the benchmark's).
func requestView(session string, in *openai.Request) kv.CompletionRequest {
	v := kv.CompletionRequest{SessionID: session, ContextTokens: in.ContextTokens}
	if in.IsChat() {
		v.ContextTokens = openai.EstimateTokens(in.Messages)
	}
	return v
}

func (rt *router) mode(ctx context.Context) string { return common.Mode(ctx, rt.c) }

func observeDecision(mode, decision string) {
	metrics.RouterRequests.WithLabelValues(mode, decision).Inc()
}

const replicaPort = common.ReplicaPort

// streamCopy writes the body through as it arrives, flushing each read so
// token streams reach the agent without buffering.
func streamCopy(w http.ResponseWriter, body io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// models answers GET /v1/models from any ready replica (agents call it to
// discover the model name).
func (rt *router) models(w http.ResponseWriter, req *http.Request) {
	pods := rt.eligible(req.Context(), common.Mode(req.Context(), rt.c), map[string]bool{})
	if len(pods) == 0 {
		http.Error(w, "no ready replicas", http.StatusServiceUnavailable)
		return
	}
	resp, err := http.Get(common.ReplicaURL(pods[0].Status.PodIP) + "/v1/models")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func msSince(t time.Time) float64 { return float64(time.Since(t).Microseconds()) / 1000 }

func (rt *router) session(ctx context.Context, name string) (*v1.AgentSession, bool) {
	s, err := common.FindSession(ctx, rt.c, name)
	if err != nil {
		return &v1.AgentSession{}, false
	}
	return s, true
}

// awaitActive is the activator: a request to a hibernated session flips it
// back to Active and waits for the operator to bring the sandbox up. Requests
// that arrive mid-rescue or mid-migration wait for that to finish too.
func (rt *router) awaitActive(ctx context.Context, name string) {
	deadline := time.Now().Add(60 * time.Second)
	woke := false
	for time.Now().Before(deadline) {
		s, ok := rt.session(ctx, name)
		if !ok {
			return // not a managed session: route it anyway
		}
		if s.Spec.DesiredState == v1.StateHibernated {
			if !woke {
				orig := s.DeepCopy()
				s.Spec.DesiredState = v1.StateActive
				_ = rt.c.Patch(ctx, s, client.MergeFrom(orig))
				woke = true
			}
		} else if s.Status.Phase == v1.PhaseActive {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// eligible lists replicas that may take traffic. Only anton mode looks at
// node-level warnings; the baselines learn about a disruption when the pod dies.
func (rt *router) eligible(ctx context.Context, mode string, tried map[string]bool) []*corev1.Pod {
	var pods corev1.PodList
	_ = rt.c.List(ctx, &pods, client.InNamespace(common.Namespace), client.MatchingLabels{"app": common.ReplicaApp})
	var out []*corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if !common.PodReady(p) || tried[p.Name] {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// estWaitMs estimates queueing delay on a replica from the router's own counters.
func (rt *router) estWaitMs(name string) float64 {
	q := rt.inflight[name] - rt.concurrency + 1
	if q <= 0 {
		return 0
	}
	return float64(q) * rt.ewmaMs[name] / float64(rt.concurrency)
}

// survivors drops replicas on nodes that are going away; if every replica is
// on such a node, all are returned (serving late beats not serving).
func (rt *router) survivors(ctx context.Context, pods []*corev1.Pod) []*corev1.Pod {
	var out []*corev1.Pod
	for _, p := range pods {
		var n corev1.Node
		if err := rt.c.Get(ctx, types.NamespacedName{Name: p.Spec.NodeName}, &n); err == nil {
			if _, spot := common.SpotDeadline(&n); spot || n.Spec.Unschedulable {
				continue
			}
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return pods
	}
	return out
}

func contains(pods []*corev1.Pod, p *corev1.Pod) bool {
	for _, q := range pods {
		if q.Name == p.Name {
			return true
		}
	}
	return false
}

func (rt *router) leastLoaded(pods []*corev1.Pod) *corev1.Pod {
	owned := map[string]int{}
	for _, r := range rt.assign {
		owned[r]++
	}
	best := pods[0]
	for _, p := range pods[1:] {
		a, b := rt.inflight[p.Name], rt.inflight[best.Name]
		if a < b || (a == b && owned[p.Name] < owned[best.Name]) {
			best = p
		}
	}
	return best
}

// reprefillMs is what moving a session costs: recomputing its whole context.
func (rt *router) reprefillMs(tokens int64) float64 {
	if rt.curve != nil {
		return rt.curve.Seconds(tokens) * 1000
	}
	return float64(tokens) * rt.msPerToken
}

// pick chooses the replica for one request. The returned decision names the
// rule that fired: random (plain mode), new (no placement yet or its replica
// is gone), affinity, affinity_broken (queueing cost more than re-prefill) or
// restore (KV blocks are loaded from the tier first).
func (rt *router) pick(ctx context.Context, mode string, in *kv.CompletionRequest, tried map[string]bool) (*corev1.Pod, float64, string, error) {
	pods := rt.eligible(ctx, mode, tried)
	if len(pods) == 0 {
		return nil, 0, "", errors.New("no ready replicas")
	}
	if common.RandomRouting(mode) {
		return pods[rand.Intn(len(pods))], 0, "random", nil // what a plain Service does
	}
	byName := map[string]*corev1.Pod{}
	for _, p := range pods {
		byName[p.Name] = p
	}
	// In anton mode, replicas on a node that is going away (spot notice or
	// cordon) take no NEW sessions, but sessions already there stay while the
	// replica is alive: their cache is still warm, and if it is worth saving
	// the guard copies it out in the background. Moving them early only pays
	// a re-prefill sooner (measured: it doubled recompute on A100 rates).
	fresh := pods
	if mode == common.ModeAnton {
		fresh = rt.survivors(ctx, pods)
	}
	sid := in.SessionID
	rt.mu.Lock()
	cur := rt.assign[sid]
	rt.mu.Unlock()

	// Our replica is leaving. In anton mode, give the guard a moment to finish
	// offloading this session's KV cache so we can restore it elsewhere.
	if cur != "" && byName[cur] == nil && mode == common.ModeAnton && !tried[cur] {
		rt.awaitRelease(ctx, sid, cur)
	}

	s, managed := rt.session(ctx, sid)
	rt.mu.Lock()
	needRestore := mode == common.ModeAnton && managed && s.Status.CacheLocation == v1.CacheTier &&
		s.Status.Replica == "" && rt.seenRV[sid] != s.ResourceVersion
	target := byName[cur]
	decision := "affinity"
	if target == nil || needRestore && !contains(fresh, target) {
		// No placement yet, or a restore that must land on a replica that stays.
		target, decision = rt.leastLoaded(fresh), "new"
	} else if !needRestore && len(pods) > 1 {
		// Affinity vs load: leave the cache behind only if queueing here costs
		// more than queueing elsewhere plus recomputing the whole context.
		var others []*corev1.Pod
		for _, p := range fresh {
			if p.Name != cur {
				others = append(others, p)
			}
		}
		if len(others) > 0 {
			alt := rt.leastLoaded(others)
			if rt.estWaitMs(alt.Name)+rt.reprefillMs(in.ContextTokens) < rt.estWaitMs(cur) {
				target, decision = alt, "affinity_broken"
			}
		}
	}
	if needRestore {
		decision = "restore"
	}
	if needRestore {
		rt.seenRV[sid] = s.ResourceVersion
	}
	changed := target.Name != cur || needRestore
	rt.assign[sid] = target.Name
	rt.mu.Unlock()

	var restoreMs float64
	if needRestore { // warm wake: pre-load the KV blocks before the first token
		t0 := time.Now()
		if err := kv.Restore(ctx, common.ReplicaURL(target.Status.PodIP), sid); err != nil && !errors.Is(err, kv.ErrNotFound) {
			ctrl.Log.Info("restore failed, replica will recompute", "session", sid, "err", err.Error())
		}
		restoreMs = msSince(t0)
	}
	// Also repair the record if the cluster's view of this session's replica
	// has drifted from where it is actually being served.
	if managed && (changed || s.Status.Replica != target.Name) {
		name := target.Name
		go func() {
			_ = common.PatchStatus(context.Background(), rt.c, sid, func(s *v1.AgentSession) {
				s.Status.Replica, s.Status.CacheLocation = name, v1.CacheReplica
			})
		}()
	}
	return target, restoreMs, decision, nil
}

// awaitRelease waits until the guard has dealt with this session (it clears
// status.replica once the KV is in the tier or judged not worth saving).
// awaitRelease waits while the guard is rescuing this session's KV cache from
// the replica that is going away, so the request can restore it elsewhere
// instead of recomputing. It returns at once unless a rescue is actually in
// progress (phase Rescuing): when the guard decided recomputing is cheaper,
// waiting for the dying replica would only add latency.
func (rt *router) awaitRelease(ctx context.Context, sid, replica string) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		s, ok := rt.session(ctx, sid)
		if !ok || s.Status.Replica != replica || s.Status.Phase != v1.PhaseRescuing {
			return
		}
		var p corev1.Pod
		if rt.c.Get(ctx, types.NamespacedName{Namespace: common.Namespace, Name: replica}, &p) != nil {
			return // the replica is already gone; nothing more will be rescued
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// forward sends the request to a replica and returns its response unread, so
// the caller can stream it. A connection error or a 5xx is returned as an
// error (another replica may serve it); a 4xx is the client's problem and is
// passed through. done() must be called after the body has been consumed; it
// closes the in-flight bookkeeping the load estimate uses.
func (rt *router) forward(ctx context.Context, p *corev1.Pod, body []byte, session string) (*http.Response, func(), error) {
	rt.mu.Lock()
	rt.inflight[p.Name]++
	rt.mu.Unlock()
	start := time.Now()
	done := func() {
		rt.mu.Lock()
		rt.inflight[p.Name]--
		ms := msSince(start)
		if old := rt.ewmaMs[p.Name]; old == 0 {
			rt.ewmaMs[p.Name] = ms
		} else {
			rt.ewmaMs[p.Name] = 0.8*old + 0.2*ms
		}
		rt.mu.Unlock()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, common.ReplicaURL(p.Status.PodIP)+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		done()
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(openai.SessionHeader, session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		done()
		return nil, nil, err
	}
	if resp.StatusCode >= 500 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		done()
		return nil, nil, fmt.Errorf("replica %s: %d %s", p.Name, resp.StatusCode, bytes.TrimSpace(b))
	}
	return resp, done, nil
}

// touch records activity for idle detection, at most once every 3s per session.
func (rt *router) touch(sid string, tokens int64) {
	rt.mu.Lock()
	recent := time.Since(rt.lastTouch[sid]) < 3*time.Second
	if !recent {
		rt.lastTouch[sid] = time.Now()
	}
	rt.mu.Unlock()
	if recent {
		return
	}
	go func() {
		_ = common.PatchStatus(context.Background(), rt.c, sid, func(s *v1.AgentSession) {
			now := metav1.Now()
			s.Status.LastActivity = &now
			s.Status.CachedTokens = tokens
		})
	}()
}
