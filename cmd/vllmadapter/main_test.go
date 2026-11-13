package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TejasVaradaraj/anton-platform/internal/kv"
)

// fakeVLLM implements the vLLM endpoints the adapter uses and records the
// prefetch it receives.
type fakeVLLM struct {
	mu       sync.Mutex
	prefetch []int
}

func (f *fakeVLLM) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "ok"}}},
			"usage":   map[string]any{"prompt_tokens": 5, "prompt_tokens_details": map[string]any{"cached_tokens": 3}},
		})
	})
	mux.HandleFunc("POST /tokenize", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"tokens": []int{11, 12, 13, 14, 15}, "count": 5})
	})
	mux.HandleFunc("POST /v1/completions", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Prompt    []int `json:"prompt"`
			MaxTokens int   `json:"max_tokens"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.prefetch = in.Prompt
		f.mu.Unlock()
		w.Write([]byte(`{"choices":[{"text":"x"}]}`))
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {})
	return httptest.NewServer(mux)
}

func fakeTier() (*httptest.Server, map[string]kv.Block) {
	blocks := map[string]kv.Block{}
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /blocks/{id}", func(w http.ResponseWriter, r *http.Request) {
		var b kv.Block
		json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		blocks[r.PathValue("id")] = b
		mu.Unlock()
	})
	mux.HandleFunc("GET /blocks/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		b, ok := blocks[r.PathValue("id")]
		mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(b)
	})
	return httptest.NewServer(mux), blocks
}

func newAdapter(vllm, tier string) *adapter {
	return &adapter{vllm: vllm, tier: tier, name: "r", bytesPerToken: 100, sessions: map[string]*session{},
		client: &http.Client{Timeout: 5 * time.Second}, concurrency: 4}
}

func TestChatRecordsSessionThenOffloadAndRestoreElsewhere(t *testing.T) {
	v := &fakeVLLM{}
	vs := v.server()
	defer vs.Close()
	tier, blocks := fakeTier()
	defer tier.Close()
	a := httptest.NewServer(newAdapter(vs.URL, tier.URL).routes())
	defer a.Close()

	resp, err := http.Post(a.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"q","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() // no session: nothing recorded
	req, _ := http.NewRequest(http.MethodPost, a.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"q","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Session-ID", "s1")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	var info kv.ReplicaInfo
	for i := 0; i < 50; i++ { // recording happens after the reply
		if kv.Do(t.Context(), http.MethodGet, a.URL+"/kv/sessions", nil, &info) == nil && len(info.Sessions) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(info.Sessions) != 1 || info.Sessions[0].Tokens != 5 || info.Sessions[0].Bytes != 500 || info.Sessions[0].Hits != 1 {
		t.Fatalf("sessions %+v", info.Sessions)
	}

	if err := kv.Offload(t.Context(), a.URL, "s1", true); err != nil {
		t.Fatal(err)
	}
	if b := blocks["s1"]; len(b.TokenIDs) != 5 || b.Bytes != 500 {
		t.Fatalf("tier block %+v", b)
	}

	// Another replica, which never saw s1, restores it from the tier.
	other := httptest.NewServer(newAdapter(vs.URL, tier.URL).routes())
	defer other.Close()
	if err := kv.Restore(t.Context(), other.URL, "s1"); err != nil {
		t.Fatal(err)
	}
	v.mu.Lock()
	got := v.prefetch
	v.mu.Unlock()
	if len(got) != 5 || got[0] != 11 {
		t.Fatalf("prefetch sent %v, want the session's token IDs", got)
	}
	if err := kv.Restore(t.Context(), other.URL, "unknown"); err == nil {
		t.Fatal("restoring an unknown session must fail")
	}
	if err := kv.Offload(t.Context(), a.URL, "s1", false); err == nil {
		t.Fatal("offload after delete must report no cache")
	}
}
