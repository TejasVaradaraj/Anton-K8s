package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/TejasVaradaraj/anton-platform/internal/openai"
)

// A fake router that answers in the OpenAI format and records what it got.
func fakeRouter(t *testing.T, lastMsgs *atomic.Int64, lastPath *atomic.Value) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in openai.Request
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("router got a bad body: %v", err)
		}
		lastMsgs.Store(int64(len(in.Messages)))
		lastPath.Store(r.URL.Path)
		w.Header().Set("X-Anton-Replica", "r1")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "done"}}},
			"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 1, "prompt_tokens_details": map[string]any{"cached_tokens": 60}},
		})
	}))
}

func TestBuiltinAgentConversationSurvivesARestart(t *testing.T) {
	var msgs atomic.Int64
	var path atomic.Value
	srv := fakeRouter(t, &msgs, &path)
	defer srv.Close()
	dir := t.TempDir()

	a := newBuiltinAgent(dir, srv.URL, "sess-9")
	a.sysToks, a.toolToks = 50, 10
	first, err := a.chat("write the parser")
	if err != nil {
		t.Fatal(err)
	}
	if path.Load() != "/s/sess-9/v1/chat/completions" {
		t.Fatalf("agent must call its session's router path, called %v", path.Load())
	}
	if first.Turns != 1 || first.CachedTokens != 60 || first.RecomputedTokens != 40 || first.Replica != "r1" {
		t.Fatalf("first reply %+v", first)
	}
	if _, err := a.chat("now add tests"); err != nil {
		t.Fatal(err)
	}

	// The pod dies; a new agent process starts on another node with the same
	// (restored) workspace and must carry on with the whole conversation.
	b := newBuiltinAgent(dir, srv.URL, "sess-9")
	b.sysToks, b.toolToks = 50, 10
	if b.turns() != 2 {
		t.Fatalf("restarted agent sees %d turns, want 2", b.turns())
	}
	third, err := b.chat("and the docs")
	if err != nil {
		t.Fatal(err)
	}
	// Sent for the third turn: system + 2 x (user, assistant, tool output) + the new user message = 8.
	if third.Turns != 3 || msgs.Load() != 8 {
		t.Fatalf("turns %d, messages sent %d: the restarted agent did not resend the history", third.Turns, msgs.Load())
	}
}

func TestBuiltinAgentRejectsBadRouterResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no replica", http.StatusBadGateway)
	}))
	defer srv.Close()
	dir := t.TempDir()
	a := newBuiltinAgent(dir, srv.URL, "s")
	if _, err := a.chat("hi"); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("want a 502 error, got %v", err)
	}
	if a.turns() != 0 {
		t.Fatal("a failed turn must not be saved")
	}
}

// A fake OpenCode server: one turn made of two model calls (a tool call, then
// the answer). The reply must carry the usage of the turn's first call.
func TestOpenCodeAgentReportsFirstCallUsage(t *testing.T) {
	const list = `[
	 {"info":{"id":"u1","role":"user"}},
	 {"info":{"id":"a1","role":"assistant","parentID":"u1","tokens":{"input":900,"cache":{"read":100}}}},
	 {"info":{"id":"u2","role":"user"}},
	 {"info":{"id":"a2","role":"assistant","parentID":"u2","tokens":{"input":53,"cache":{"read":2413}}}},
	 {"info":{"id":"a3","role":"assistant","parentID":"u2","tokens":{"input":10,"cache":{"read":2600}}}}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			w.Write([]byte(`{"id":"ses1"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/session/ses1/message":
			w.Write([]byte(`{"info":{"id":"a3","role":"assistant"},"parts":[{"type":"text","text":"done"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/session/ses1/message":
			w.Write([]byte(list))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	a := &opencodeAgent{dir: dir, url: srv.URL, idFile: filepath.Join(dir, ".anton", "opencode-session")}
	out, err := a.chat("go on")
	if err != nil {
		t.Fatal(err)
	}
	if out.Reply != "done" || out.Turns != 2 || out.PromptTokens != 2466 || out.CachedTokens != 2413 || out.RecomputedTokens != 53 {
		t.Fatalf("got %+v, want reply done, 2 turns, 2466 prompt, 2413 cached, 53 recomputed", out)
	}
}
