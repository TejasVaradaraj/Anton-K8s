// vllmadapter runs beside a real vLLM + LMCache server in an inference
// replica pod and gives it the same small contract the mock replica has, so
// the operator, guard and router work unchanged on real GPUs:
//
//	POST /v1/chat/completions   proxied to vLLM (streams pass through); the
//	                            adapter records the session's prompt tokens
//	GET  /v1/models             proxied
//	GET  /kv/sessions           sessions this replica has served, with sizes,
//	                            measured rates and the prefill curve
//	POST /kv/offload            make the session's cache recoverable elsewhere
//	POST /kv/restore            load the session's cache before its next request
//	GET  /metrics, /healthz     vLLM's own
//
// How the cache moves. LMCache runs write-through: every KV chunk vLLM
// computes is stored in the local CPU tier and, asynchronously, in a shared
// LMCache server off the node. So the blocks already outlive this pod. What
// does not is the knowledge of which tokens make up a session: LMCache keys
// blocks by token content, not by session. Offload therefore saves the
// session's token IDs to Anton's tier, and restore on another replica sends
// those tokens as a one-token completion, which makes vLLM pull the blocks
// from LMCache into GPU memory before the agent's real request arrives.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TejasVaradaraj/anton-platform/internal/kv"
	"github.com/TejasVaradaraj/anton-platform/internal/openai"
)

type session struct {
	tokens []int
	last   time.Time
	hits   int
}

type adapter struct {
	vllm, tier, model, name string
	bytesPerToken           int64
	offloadBps, restoreBps  float64
	msPerToken              float64
	curve                   string
	concurrency             int

	mu       sync.Mutex
	sessions map[string]*session
	inflight atomic.Int64
	client   *http.Client
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	listen := flag.String("listen", ":8000", "address the router and operator call")
	flag.Parse()
	a := &adapter{
		vllm:  strings.TrimRight(env("VLLM_URL", "http://127.0.0.1:8001"), "/"),
		tier:  strings.TrimRight(env("TIER_URL", "http://anton-tier:9000"), "/"),
		model: env("MODEL", ""), name: env("POD_NAME", "replica"),
		curve: env("PREFILL_CURVE", ""), sessions: map[string]*session{},
		client: &http.Client{Timeout: 10 * time.Minute},
	}
	fmt.Sscan(env("BYTES_PER_TOKEN", "57344"), &a.bytesPerToken)
	fmt.Sscan(env("OFFLOAD_BYTES_PER_SEC", "1.28e9"), &a.offloadBps)
	fmt.Sscan(env("RESTORE_BYTES_PER_SEC", "6.3e8"), &a.restoreBps)
	fmt.Sscan(env("PREFILL_MS_PER_TOKEN", "0.3"), &a.msPerToken)
	fmt.Sscan(env("CONCURRENCY", "4"), &a.concurrency)
	log.Printf("vllm adapter %s: vLLM at %s, tier at %s", a.name, a.vllm, a.tier)
	log.Fatal(http.ListenAndServe(*listen, a.routes()))
}

func (a *adapter) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", a.chat)
	mux.HandleFunc("GET /v1/models", a.proxyGet("/v1/models"))
	mux.HandleFunc("GET /metrics", a.proxyGet("/metrics"))
	mux.HandleFunc("GET /healthz", a.proxyGet("/health"))
	mux.HandleFunc("GET /kv/sessions", a.sessionsHandler)
	mux.HandleFunc("POST /kv/offload", a.offload)
	mux.HandleFunc("POST /kv/restore", a.restore)
	return mux
}

func (a *adapter) proxyGet(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp, err := a.client.Get(a.vllm + path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}
}

// chat forwards to vLLM untouched and, on success, records the session's
// prompt as token IDs (vLLM's /tokenize applies the same chat template).
func (a *adapter) chat(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var in openai.Request
	_ = json.Unmarshal(raw, &in)
	sid := openai.Session(r, &in)

	a.inflight.Add(1)
	defer a.inflight.Add(-1)
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, a.vllm+"/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16<<10)
	var tail bytes.Buffer // the last bytes, to read usage from non-streamed replies
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
			if !in.Stream {
				tail.Write(buf[:n])
			}
		}
		if rerr != nil {
			break
		}
	}
	if resp.StatusCode != http.StatusOK || sid == "" || !in.IsChat() {
		return
	}
	hit := false
	var out struct {
		Usage openai.Usage `json:"usage"`
	}
	if json.Unmarshal(tail.Bytes(), &out) == nil && out.Usage.PromptTokensDetails != nil {
		hit = out.Usage.PromptTokensDetails.CachedTokens > 0
	}
	go a.record(sid, in.Messages, hit)
}

func (a *adapter) record(sid string, msgs []openai.Message, hit bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ids, err := a.tokenize(ctx, msgs)
	if err != nil {
		log.Printf("tokenize %s: %v", sid, err)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[sid]
	if s == nil {
		s = &session{}
		a.sessions[sid] = s
	}
	s.tokens, s.last = ids, time.Now()
	if hit {
		s.hits++
	}
}

func (a *adapter) tokenize(ctx context.Context, msgs []openai.Message) ([]int, error) {
	body, _ := json.Marshal(map[string]any{"model": a.model, "messages": msgs, "add_generation_prompt": true})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, a.vllm+"/tokenize", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vLLM /tokenize: %d", resp.StatusCode)
	}
	var out struct {
		Tokens []int `json:"tokens"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Tokens, nil
}

func (a *adapter) sessionsHandler(w http.ResponseWriter, _ *http.Request) {
	info := kv.ReplicaInfo{
		Replica: a.name, InFlight: int(a.inflight.Load()), Concurrency: a.concurrency,
		PrefillMsPerToken: a.msPerToken, PrefillCurve: a.curve, BytesPerToken: a.bytesPerToken,
		OffloadBytesPerSec: a.offloadBps, RestoreBytesPerSec: a.restoreBps, Sessions: []kv.SessionCache{},
	}
	a.mu.Lock()
	for id, s := range a.sessions {
		n := int64(len(s.tokens))
		info.Sessions = append(info.Sessions, kv.SessionCache{SessionID: id, Tokens: n, Bytes: n * a.bytesPerToken,
			LastAccessUnixMs: s.last.UnixMilli(), Hits: s.hits})
	}
	a.mu.Unlock()
	sort.Slice(info.Sessions, func(i, j int) bool { return info.Sessions[i].SessionID < info.Sessions[j].SessionID })
	json.NewEncoder(w).Encode(info)
}

// offload makes the session's cache recoverable on any replica: LMCache has
// already written its blocks through to the shared server, so what is saved
// here is the token sequence that names them.
func (a *adapter) offload(w http.ResponseWriter, r *http.Request) {
	var in kv.OffloadRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.SessionID == "" {
		http.Error(w, "need session_id", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	s := a.sessions[in.SessionID]
	var ids []int
	if s != nil {
		ids = append(ids, s.tokens...)
	}
	if in.Delete {
		delete(a.sessions, in.SessionID) // vLLM frees GPU blocks itself under pressure
	}
	a.mu.Unlock()
	if len(ids) == 0 {
		http.Error(w, "no cache for this session", http.StatusNotFound)
		return
	}
	n := int64(len(ids))
	err := kv.Do(r.Context(), http.MethodPut, a.tier+"/blocks/"+in.SessionID,
		kv.Block{Tokens: n, Bytes: n * a.bytesPerToken, TokenIDs: ids}, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// restore prefetches the session's KV cache: a one-token completion over the
// session's tokens makes vLLM look the prefix up in LMCache and load it.
func (a *adapter) restore(w http.ResponseWriter, r *http.Request) {
	var in kv.OffloadRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.SessionID == "" {
		http.Error(w, "need session_id", http.StatusBadRequest)
		return
	}
	var b kv.Block
	if err := kv.Do(r.Context(), http.MethodGet, a.tier+"/blocks/"+in.SessionID, nil, &b); err != nil {
		if errors.Is(err, kv.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if len(b.TokenIDs) == 0 {
		http.Error(w, "tier has no token IDs for this session", http.StatusNotFound)
		return
	}
	body, _ := json.Marshal(map[string]any{"model": a.model, "prompt": b.TokenIDs, "max_tokens": 1, "temperature": 0})
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, a.vllm+"/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("vLLM prefetch: %d", resp.StatusCode), http.StatusBadGateway)
		return
	}
	a.mu.Lock()
	a.sessions[in.SessionID] = &session{tokens: b.TokenIDs, last: time.Now()}
	a.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
