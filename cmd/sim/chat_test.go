package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testReplica() *replica {
	r := &replica{name: "r1", msPerToken: 0.001, decodeMs: 0, capacity: 1_000_000, concurrency: 2, cache: map[string]*entry{}}
	r.sem = make(chan struct{}, r.concurrency)
	return r
}

func chatBody(stream bool, turns int) string {
	msgs := []map[string]string{{"role": "system", "content": strings.Repeat("You are a careful coding agent. ", 40)}}
	for i := 0; i < turns; i++ {
		msgs = append(msgs, map[string]string{"role": "user", "content": strings.Repeat("fix the failing test ", 20)})
	}
	b, _ := json.Marshal(map[string]any{"model": "qwen", "messages": msgs, "stream": stream})
	return string(b)
}

func TestChatCompletionReportsCacheHitsLikeVLLM(t *testing.T) {
	r := testReplica()
	post := func(path, body string) map[string]any {
		w := httptest.NewRecorder()
		r.complete(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := post("/s/sess-1/v1/chat/completions", chatBody(false, 1))
	if first["object"] != "chat.completion" {
		t.Fatalf("not an OpenAI response: %v", first)
	}
	cached := func(o map[string]any) float64 {
		return o["usage"].(map[string]any)["prompt_tokens_details"].(map[string]any)["cached_tokens"].(float64)
	}
	if cached(first) != 0 {
		t.Fatal("first turn cannot be cached")
	}
	second := post("/s/sess-1/v1/chat/completions", chatBody(false, 2))
	prompt := second["usage"].(map[string]any)["prompt_tokens"].(float64)
	if c := cached(second); c == 0 || c >= prompt {
		t.Fatalf("second turn should reuse the first turn's prefix: cached %v of %v", c, prompt)
	}
	if cached(post("/s/other/v1/chat/completions", chatBody(false, 2))) != 0 {
		t.Fatal("another session must not hit this session's cache")
	}
}

func TestChatCompletionStreams(t *testing.T) {
	r := testReplica()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody(true, 1)))
	req.Header.Set("X-Session-ID", "sess-2")
	w := httptest.NewRecorder()
	r.complete(w, req)
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	var text strings.Builder
	sawUsage, done := false, false
	sc := bufio.NewScanner(w.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			done = true
			continue
		}
		var c struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &c); err != nil || c.Object != "chat.completion.chunk" {
			t.Fatalf("bad chunk %q", data)
		}
		text.WriteString(c.Choices[0].Delta.Content)
		sawUsage = sawUsage || c.Usage != nil
	}
	if !done || !sawUsage || !strings.Contains(text.String(), "prompt tokens") {
		t.Fatalf("done=%v usage=%v text=%q", done, sawUsage, text.String())
	}
}

func TestLegacyBenchmarkRequestsStillWork(t *testing.T) {
	r := testReplica()
	w := httptest.NewRecorder()
	r.complete(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"session_id":"b","context_tokens":1000}`)))
	var out struct {
		Replica          string `json:"replica"`
		RecomputedTokens int64  `json:"recomputed_tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Replica != "r1" || out.RecomputedTokens != 1000 {
		t.Fatalf("legacy response %s", w.Body.String())
	}
}

// The mock must refuse what vLLM refuses: a request whose prompt plus
// max_tokens does not fit the model window.
func TestMockRejectsOutputThatDoesNotFitTheWindow(t *testing.T) {
	r := &replica{name: "r", sem: make(chan struct{}, 1), maxModelLen: 32768}
	for _, body := range []string{
		`{"model":"m","max_tokens":32000,"messages":[{"role":"user","content":"` + strings.Repeat("word ", 2000) + `"}]}`,
		`{"model":"m","max_completion_tokens":40000,"messages":[{"role":"user","content":"hi"}]}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/s/s1/v1/chat/completions", strings.NewReader(body))
		w := httptest.NewRecorder()
		r.complete(w, req)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "maximum context length") {
			t.Fatalf("want a 400 about the context length, got %d %s", w.Code, w.Body.String())
		}
	}
}
