package openai

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestSessionFromPath(t *testing.T) {
	for path, want := range map[string][2]string{
		"/s/core-agent-1b13/v1/chat/completions": {"core-agent-1b13", "/v1/chat/completions"},
		"/s/x/v1/models":                         {"x", "/v1/models"},
	} {
		s, rest, ok := SessionFromPath(path)
		if !ok || s != want[0] || rest != want[1] {
			t.Errorf("%s -> %q %q %v", path, s, rest, ok)
		}
	}
	for _, path := range []string{"/v1/chat/completions", "/s/", "/s//v1"} {
		if _, _, ok := SessionFromPath(path); ok {
			t.Errorf("%s should not parse", path)
		}
	}
}

func TestSessionPrecedence(t *testing.T) {
	r := httptest.NewRequest("POST", "/s/from-path/v1/chat/completions", nil)
	r.Header.Set(SessionHeader, "from-header")
	if got := Session(r, &Request{SessionID: "from-body"}); got != "from-path" {
		t.Fatalf("path should win, got %s", got)
	}
	r = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set(SessionHeader, "from-header")
	if got := Session(r, &Request{SessionID: "from-body"}); got != "from-header" {
		t.Fatalf("header should win over body, got %s", got)
	}
}

func TestEstimateTokensHandlesStringsAndParts(t *testing.T) {
	var req Request
	body := `{"messages":[
	  {"role":"system","content":"` + string(make([]byte, 0)) + `You are a coding agent."},
	  {"role":"user","content":[{"type":"text","text":"0123456789012345678901234567890123456789"}]},
	  {"role":"assistant","tool_calls":[{"id":"1","type":"function","function":{"name":"read","arguments":"{}"}}]}
	]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	got := EstimateTokens(req.Messages)
	if got < 20 || got > 60 {
		t.Fatalf("estimate %d out of the expected range", got)
	}
	if !req.IsChat() || (&Request{SessionID: "s", ContextTokens: 5}).IsChat() {
		t.Fatal("IsChat")
	}
}
