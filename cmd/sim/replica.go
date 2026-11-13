package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TejasVaradaraj/anton-platform/internal/kv"
	"github.com/TejasVaradaraj/anton-platform/internal/openai"
	"github.com/TejasVaradaraj/anton-platform/internal/prefill"
)

type entry struct {
	tokens int64
	last   time.Time
	hits   int
}

// replica models the two things about vLLM that matter here: prefill time is
// proportional to the tokens NOT already in the KV cache, and that cache dies
// with the process. All timings come from env vars calibrated on a real GPU.
type replica struct {
	name        string
	tierURL     string
	msPerToken  float64
	curve       *prefill.Curve // measured prefill time by context; replaces msPerToken when set
	bytesPerTok int64
	offloadBps  float64
	restoreBps  float64
	decodeMs    float64
	capacity    int64
	concurrency int
	maxModelLen int64 // prompt + requested output must fit (vLLM --max-model-len)

	mu        sync.Mutex
	cache     map[string]*entry
	sem       chan struct{}
	offloadMu sync.Mutex // offloads share one outbound link, so they run one at a time
	inflight  atomic.Int64
	hitTok    atomic.Int64
	missTok   atomic.Int64
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func runReplica() {
	r := &replica{
		name:        envStr("POD_NAME", "replica"),
		tierURL:     envStr("TIER_URL", "http://anton-tier:9000"),
		msPerToken:  envFloat("PREFILL_MS_PER_TOKEN", 0.4),
		bytesPerTok: int64(envFloat("BYTES_PER_TOKEN", 100_000)),
		offloadBps:  envFloat("OFFLOAD_BYTES_PER_SEC", 1.5e9),
		restoreBps:  envFloat("RESTORE_BYTES_PER_SEC", 2e9),
		decodeMs:    envFloat("FIRST_TOKEN_MS", 30),
		capacity:    int64(envFloat("CAPACITY_TOKENS", 600_000)),
		concurrency: int(envFloat("CONCURRENCY", 4)),
		maxModelLen: int64(envFloat("MAX_MODEL_LEN", 32768)),
		cache:       map[string]*entry{},
	}
	if c, err := prefill.Parse(envStr("PREFILL_CURVE", "")); err != nil {
		log.Fatal(err)
	} else {
		r.curve = c
	}
	r.sem = make(chan struct{}, r.concurrency)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", r.complete)
	mux.HandleFunc("POST /kv/offload", r.offload)
	mux.HandleFunc("POST /kv/restore", r.restore)
	mux.HandleFunc("GET /kv/sessions", r.sessions)
	mux.HandleFunc("GET /v1/models", models)
	mux.HandleFunc("GET /metrics", r.metrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	log.Printf("replica %s: %.2f ms/token, %d bytes/token", r.name, r.msPerToken, r.bytesPerTok)
	// No graceful shutdown on purpose: SIGTERM kills the process and the KV
	// cache with it, exactly like a real inference pod.
	log.Fatal(http.ListenAndServe(":8000", mux))
}

// complete serves both request shapes: the benchmark's compact form
// (session_id + context_tokens) and an OpenAI chat-completions request, which
// is what agents (OpenCode) and real vLLM speak. Both run the same simulation.
func (r *replica) complete(w http.ResponseWriter, req *http.Request) {
	var in openai.Request
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	session, ctxTokens := in.SessionID, in.ContextTokens
	if in.IsChat() {
		session, ctxTokens = openai.Session(req, &in), openai.EstimateTokens(in.Messages)
	}
	// Like vLLM: the prompt plus the requested output must fit the model's
	// window. The mock used to accept anything, so a client asking for 32000
	// output tokens passed every rehearsal and failed on the real server.
	if maxOut := int64(max(in.MaxTokens, in.MaxCompletionTokens)); in.IsChat() && r.maxModelLen > 0 && maxOut+ctxTokens > r.maxModelLen {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"This model's maximum context length is %d tokens. However, you requested %d output tokens and your prompt contains at least %d input tokens.","type":"BadRequestError","code":400}}`,
			r.maxModelLen, maxOut, ctxTokens), http.StatusBadRequest)
		return
	}
	if session == "" || ctxTokens <= 0 {
		http.Error(w, "need a session (X-Session-ID, /s/{session}/ or session_id) and a prompt", http.StatusBadRequest)
		return
	}
	res, ok := r.simulate(req.Context(), session, ctxTokens)
	if !ok {
		return
	}
	if !in.IsChat() {
		json.NewEncoder(w).Encode(res)
		return
	}
	writeChat(w, &in, res, ctxTokens)
}

// simulate queues for a slot, charges prefill for the uncached part of the
// context, and records the session's cache. It returns false if the client
// went away.
func (r *replica) simulate(ctx context.Context, session string, ctxTokens int64) (kv.CompletionResponse, bool) {
	r.inflight.Add(1)
	defer r.inflight.Add(-1)
	queued := time.Now()
	select {
	case r.sem <- struct{}{}:
	case <-ctx.Done():
		return kv.CompletionResponse{}, false
	}
	defer func() { <-r.sem }()
	queueMs := float64(time.Since(queued).Microseconds()) / 1000

	r.mu.Lock()
	var cached int64
	if e := r.cache[session]; e != nil {
		cached = min(e.tokens, ctxTokens)
		e.hits++
	}
	r.mu.Unlock()
	recomputed := ctxTokens - cached
	prefillMs := float64(recomputed) * r.msPerToken
	if r.curve != nil {
		// New tokens attend over the whole context, so a suffix on top of a
		// long cached prefix costs more than the same tokens from scratch.
		prefillMs = r.curve.Cost(ctxTokens, cached) * 1000
	}
	if sleepCtx(ctx, time.Duration((prefillMs+r.decodeMs)*float64(time.Millisecond))) != nil {
		return kv.CompletionResponse{}, false
	}
	r.hitTok.Add(cached)
	r.missTok.Add(recomputed)

	r.mu.Lock()
	e := r.cache[session]
	if e == nil {
		e = &entry{}
		r.cache[session] = e
	}
	e.tokens, e.last = ctxTokens, time.Now()
	r.evictLocked(session)
	r.mu.Unlock()
	return kv.CompletionResponse{
		Replica: r.name, CachedTokens: cached, RecomputedTokens: recomputed, PrefillMs: prefillMs, QueueMs: queueMs,
	}, true
}

// evictLocked drops least-recently-used sessions when GPU memory is full.
func (r *replica) evictLocked(keep string) {
	var total int64
	ids := make([]string, 0, len(r.cache))
	for id, e := range r.cache {
		total += e.tokens
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return r.cache[ids[i]].last.Before(r.cache[ids[j]].last) })
	for _, id := range ids {
		if total <= r.capacity {
			return
		}
		if id != keep {
			total -= r.cache[id].tokens
			delete(r.cache, id)
		}
	}
}

func (r *replica) offload(w http.ResponseWriter, req *http.Request) {
	var in kv.OffloadRequest
	json.NewDecoder(req.Body).Decode(&in)
	r.mu.Lock()
	e := r.cache[in.SessionID]
	var tokens int64
	if e != nil {
		tokens = e.tokens
	}
	r.mu.Unlock()
	if e == nil {
		http.Error(w, "session not cached here", http.StatusNotFound)
		return
	}
	bytes := tokens * r.bytesPerTok
	r.offloadMu.Lock()
	err := sleepCtx(req.Context(), time.Duration(float64(bytes)/r.offloadBps*float64(time.Second)))
	r.offloadMu.Unlock()
	if err != nil {
		return // caller gave up (deadline); nothing was stored
	}
	if err := kv.Do(req.Context(), http.MethodPut, r.tierURL+"/blocks/"+in.SessionID, kv.Block{Tokens: tokens, Bytes: bytes}, nil); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if in.Delete {
		r.mu.Lock()
		delete(r.cache, in.SessionID)
		r.mu.Unlock()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (r *replica) restore(w http.ResponseWriter, req *http.Request) {
	var in kv.OffloadRequest
	json.NewDecoder(req.Body).Decode(&in)
	var b kv.Block
	if err := kv.Do(req.Context(), http.MethodGet, r.tierURL+"/blocks/"+in.SessionID, nil, &b); err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, kv.ErrNotFound) {
			code = http.StatusNotFound
		}
		http.Error(w, err.Error(), code)
		return
	}
	if sleepCtx(req.Context(), time.Duration(float64(b.Bytes)/r.restoreBps*float64(time.Second))) != nil {
		return
	}
	r.mu.Lock()
	if e := r.cache[in.SessionID]; e == nil || e.tokens < b.Tokens {
		r.cache[in.SessionID] = &entry{tokens: b.Tokens, last: time.Now()}
	}
	r.evictLocked(in.SessionID)
	r.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (r *replica) curveString() string {
	if r.curve == nil {
		return ""
	}
	return r.curve.String()
}

func (r *replica) info() kv.ReplicaInfo {
	info := kv.ReplicaInfo{
		Replica: r.name, InFlight: int(r.inflight.Load()), Concurrency: r.concurrency,
		PrefillMsPerToken: r.msPerToken, BytesPerToken: r.bytesPerTok, PrefillCurve: r.curveString(),
		OffloadBytesPerSec: r.offloadBps, RestoreBytesPerSec: r.restoreBps,
		Sessions: []kv.SessionCache{},
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, e := range r.cache {
		info.Sessions = append(info.Sessions, kv.SessionCache{
			SessionID: id, Tokens: e.tokens, Bytes: e.tokens * r.bytesPerTok, LastAccessUnixMs: e.last.UnixMilli(), Hits: e.hits,
		})
	}
	return info
}

func (r *replica) sessions(w http.ResponseWriter, _ *http.Request) {
	json.NewEncoder(w).Encode(r.info())
}

// metrics exposes vLLM-shaped gauges so dashboards built against the mock
// keep working against the real thing.
func (r *replica) metrics(w http.ResponseWriter, _ *http.Request) {
	info := r.info()
	var tokens int64
	for _, s := range info.Sessions {
		tokens += s.Tokens
	}
	fmt.Fprintf(w, "vllm:num_requests_running %d\n", info.InFlight)
	fmt.Fprintf(w, "vllm:gpu_cache_usage_perc %.4f\n", float64(tokens)/float64(r.capacity))
	fmt.Fprintf(w, "vllm:gpu_prefix_cache_hits_total %d\n", r.hitTok.Load())
	fmt.Fprintf(w, "vllm:gpu_prefix_cache_queries_total %d\n", r.hitTok.Load()+r.missTok.Load())
	fmt.Fprintf(w, "anton_kv_sessions %d\n", len(info.Sessions))
}
