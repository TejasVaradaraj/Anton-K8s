// Package openai holds the small part of the OpenAI chat-completions API that
// Anton touches. The router stays a pass-through: it forwards request bodies
// untouched to vLLM (or the mock), and only reads two things from them, the
// session and an estimate of the prompt length for its affinity cost rule.
package openai

import (
	"encoding/json"
	"net/http"
	"strings"
)

// SessionHeader carries the agent session on requests from clients that can
// set headers. Clients that cannot use the path form /s/{session}/v1/...
const SessionHeader = "X-Session-ID"

type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content,omitempty"` // a string or a list of parts
	// Tool calls and results count toward the context too.
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// Request is the subset of a chat-completions request Anton reads. Legacy
// benchmark requests carry SessionID and ContextTokens instead of messages.
type Request struct {
	Model         string    `json:"model,omitempty"`
	Messages      []Message `json:"messages,omitempty"`
	Stream        bool      `json:"stream,omitempty"`
	MaxTokens     int       `json:"max_tokens,omitempty"`
	SessionID     string    `json:"session_id,omitempty"`
	ContextTokens int64     `json:"context_tokens,omitempty"`
}

func (r *Request) IsChat() bool { return len(r.Messages) > 0 }

// EstimateTokens approximates the prompt length without a tokenizer: about
// four characters per token for English and code, plus a few tokens of
// framing per message. The router only uses it to price a re-prefill, where
// being within ~20% is enough; real token counts come back in usage.
func EstimateTokens(msgs []Message) int64 {
	var chars int64
	for _, m := range msgs {
		chars += int64(len(contentText(m.Content)) + len(m.ToolCalls) + len(m.Role))
	}
	return chars/4 + int64(4*len(msgs))
}

func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return string(raw)
}

// SessionFromPath extracts the session from /s/{session}/v1/... and returns
// the remaining path ("/v1/..."). ok is false for paths without the prefix.
func SessionFromPath(path string) (session, rest string, ok bool) {
	if !strings.HasPrefix(path, "/s/") {
		return "", path, false
	}
	tail := path[len("/s/"):]
	i := strings.Index(tail, "/")
	if i <= 0 {
		return "", path, false
	}
	return tail[:i], tail[i:], true
}

// Session finds the session for a request: path form first, then header,
// then the legacy body field.
func Session(r *http.Request, body *Request) string {
	if s, _, ok := SessionFromPath(r.URL.Path); ok {
		return s
	}
	if s := r.Header.Get(SessionHeader); s != "" {
		return s
	}
	if body != nil {
		return body.SessionID
	}
	return ""
}

// Usage is the usage block of a response; vLLM fills CachedTokens when
// prefix caching is on, which is how Anton sees KV-cache hits on real replicas.
type Usage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}
