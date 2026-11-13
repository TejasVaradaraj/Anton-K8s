package main

// The sandbox agent's conversation. Two backends behind one endpoint
// (POST /chat):
//
//   - builtin: a minimal agent loop. The conversation lives in the workspace
//     (.anton/conversation.json); every turn sends the whole conversation to
//     the session router in the OpenAI chat format, the way a coding agent
//     re-sends its context. A large system prompt and simulated tool output
//     per turn reproduce a coding agent's context size and growth.
//   - opencode: OpenCode runs inside the sandbox (`opencode serve`) with its
//     data directory in the workspace and its model provider pointed at the
//     router. /chat forwards to its session API.
//
// Either way the agent's state is workspace state, so it survives exactly
// when the workspace survives: that is what the recovery test checks.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/TejasVaradaraj/anton-platform/internal/openai"
)

type chatReply struct {
	Reply            string  `json:"reply"`
	Turns            int     `json:"turns"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	RecomputedTokens int64   `json:"recomputed_tokens"`
	Replica          string  `json:"replica"`
	TTFTMs           float64 `json:"ttft_ms"`
	Backend          string  `json:"backend"`
}

type chatBackend interface {
	chat(text string) (chatReply, error)
	turns() int
}

// ---------- builtin agent ----------

type builtinAgent struct {
	mu         sync.Mutex
	onTurn     func() // persists the workspace right after a completed turn
	path       string
	router     string
	session    string
	model      string
	toolToks   int
	sysToks    int
	sharedToks int
}

func newBuiltinAgent(dir, router, session string) *builtinAgent {
	return &builtinAgent{
		path:    filepath.Join(dir, ".anton", "conversation.json"),
		router:  strings.TrimRight(router, "/"),
		session: session,
		model:   envStr("MODEL", "anton-mock"),
		sysToks: int(envFloat("SYSTEM_PROMPT_TOKENS", 8000)),
		// The part of the system prompt all sessions share; the rest is unique.
		sharedToks: int(envFloat("SHARED_PROMPT_TOKENS", 1000)),
		toolToks:   int(envFloat("TOOL_OUTPUT_TOKENS", 400)),
	}
}

// filler produces about n tokens of deterministic text (4 chars per token).
func filler(label string, n int) string {
	line := label + ": the agent's instructions, tool definitions, file contents and command output. "
	var b strings.Builder
	for b.Len() < n*4 {
		b.WriteString(line)
	}
	return b.String()[:n*4]
}

// systemPrompt is a short part every session shares (instructions, tool
// definitions) followed by a long part only this session has (its repository
// and files). On real GPUs an all-shared prompt made every session's cache
// look warm on any replica that had served any other session: about 6,000 of
// 7,400 prompt tokens were the same text (EKS run, 2026-11-13).
func (a *builtinAgent) systemPrompt() string {
	shared := min(a.sharedToks, a.sysToks)
	var b strings.Builder
	b.WriteString(filler("system", shared))
	for i := 0; b.Len() < a.sysToks*4; i++ {
		fmt.Fprintf(&b, "file %s/%d: this session's own repository contents and command output, line %d. ", a.session, i, i*7919)
	}
	return b.String()[:a.sysToks*4]
}

func (a *builtinAgent) load() []openai.Message {
	var msgs []openai.Message
	if b, err := os.ReadFile(a.path); err == nil {
		_ = json.Unmarshal(b, &msgs)
	}
	if len(msgs) == 0 {
		sys, _ := json.Marshal(a.systemPrompt())
		msgs = []openai.Message{{Role: "system", Content: sys}}
	}
	return msgs
}

func (a *builtinAgent) save(msgs []openai.Message) error {
	if err := os.MkdirAll(filepath.Dir(a.path), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(msgs)
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.path) // atomic: a kill mid-write never corrupts the conversation
}

// countTurns counts the agent's replies (tool output is also a user-role
// message, so user messages would double-count).
func countTurns(msgs []openai.Message) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "assistant" {
			n++
		}
	}
	return n
}

func (a *builtinAgent) turns() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return countTurns(a.load())
}

func (a *builtinAgent) chat(text string) (chatReply, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	msgs := a.load()
	content, _ := json.Marshal(text)
	msgs = append(msgs, openai.Message{Role: "user", Content: content})
	body, _ := json.Marshal(map[string]any{"model": a.model, "messages": msgs, "max_tokens": 256})

	start := time.Now()
	resp, err := http.Post(a.router+"/s/"+a.session+"/v1/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		return chatReply{}, err
	}
	defer resp.Body.Close()
	ttft := float64(time.Since(start).Microseconds()) / 1000
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return chatReply{}, fmt.Errorf("router: %d %s", resp.StatusCode, b)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage openai.Usage `json:"usage"`
		Anton struct {
			RecomputedTokens int64 `json:"recomputed_tokens"`
		} `json:"anton"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Choices) == 0 {
		return chatReply{}, fmt.Errorf("bad response from the model: %v", err)
	}
	reply := out.Choices[0].Message.Content
	r, _ := json.Marshal(reply)
	// Simulated tool output: what a coding agent appends after acting.
	tool, _ := json.Marshal(filler(fmt.Sprintf("tool output, turn %d", len(msgs)), a.toolToks))
	msgs = append(msgs, openai.Message{Role: "assistant", Content: r}, openai.Message{Role: "user", Content: tool})
	if err := a.save(msgs); err != nil {
		return chatReply{}, err
	}
	if a.onTurn != nil {
		a.onTurn() // a completed turn must survive even a hard kill a second later
	}
	cached := int64(0)
	if out.Usage.PromptTokensDetails != nil {
		cached = out.Usage.PromptTokensDetails.CachedTokens
	}
	recomputed := out.Anton.RecomputedTokens
	if recomputed == 0 {
		recomputed = out.Usage.PromptTokens - cached // real vLLM has no "anton" block
	}
	return chatReply{
		Reply: reply, Turns: countTurns(msgs), PromptTokens: out.Usage.PromptTokens, CachedTokens: cached,
		RecomputedTokens: recomputed, Replica: resp.Header.Get("X-Anton-Replica"), TTFTMs: ttft, Backend: "builtin",
	}, nil
}

// ---------- OpenCode ----------

type opencodeAgent struct {
	mu      sync.Mutex
	onTurn  func()
	dir     string
	url     string
	idFile  string
	session string
}

// startOpenCode writes the workspace's opencode.json (provider = the session
// router, path-scoped to this session) and starts `opencode serve` with its
// state under the workspace.
func startOpenCode(dir, router, session string) (*opencodeAgent, error) {
	model := envStr("MODEL", "anton-mock")
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		// OpenCode 1.19.5 reads "provider" (singular) with "npm" and "options".
		// It silently drops an unknown "providers" key and then fails every
		// message with ProviderModelNotFoundError (found on EKS, 2026-11-13).
		"provider": map[string]any{
			"anton": map[string]any{
				"name": "Anton router",
				"npm":  "@ai-sdk/openai-compatible",
				"options": map[string]any{
					"baseURL": strings.TrimRight(router, "/") + "/s/" + session + "/v1",
					"apiKey":  "anton", // the router ignores it; the SDK wants one
				},
				// Without limits OpenCode asks for 32000 output tokens, and a
				// server with a 32768-token window (vLLM) rejects every request:
				// prompt + max_tokens must fit the window (found on EKS, 2026-11-13).
				"models": map[string]any{model: map[string]any{
					"name": model,
					"limit": map[string]any{
						"context": int(envFloat("MODEL_CONTEXT", 32768)),
						"output":  int(envFloat("MODEL_MAX_OUTPUT", 4096)),
					},
				}},
			},
		},
		"model": "anton/" + model,
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "opencode.json"), b, 0o644); err != nil {
		return nil, err
	}
	port := envStr("OPENCODE_PORT", "4096")
	cmd := exec.Command(envStr("OPENCODE_BIN", "opencode"), "serve", "--hostname", "127.0.0.1", "--port", port)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"XDG_DATA_HOME="+filepath.Join(dir, ".anton", "share"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, ".anton", "config"),
		"XDG_STATE_HOME="+filepath.Join(dir, ".anton", "state"),
		"HOME="+dir,
		// Sandboxes have no internet egress (default-deny NetworkPolicy), and
		// OpenCode otherwise spends its start-up waiting on models.dev.
		"OPENCODE_DISABLE_MODELS_FETCH=1",
		"OPENCODE_DISABLE_AUTOUPDATE=1",
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start opencode: %w", err)
	}
	a := &opencodeAgent{dir: dir, url: "http://127.0.0.1:" + port, idFile: filepath.Join(dir, ".anton", "opencode-session")}
	// Every probe has its own timeout: OpenCode accepts connections before it
	// can answer, and a probe without one hung forever on the first request
	// (found on kind, 2026-11-13), so the sandbox never became ready.
	probe := &http.Client{Timeout: 2 * time.Second}
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); {
		if r, err := probe.Get(a.url + "/session"); err == nil {
			r.Body.Close()
			return a, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil, fmt.Errorf("opencode serve did not come up on %s", a.url)
}

func (a *opencodeAgent) ensureSession() (string, error) {
	if a.session != "" {
		return a.session, nil
	}
	if b, err := os.ReadFile(a.idFile); err == nil && len(bytes.TrimSpace(b)) > 0 {
		a.session = string(bytes.TrimSpace(b)) // resume the conversation that survived in the workspace
		return a.session, nil
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Post(a.url+"/session", "application/json", strings.NewReader(`{"title":"anton"}`))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var s struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil || s.ID == "" {
		return "", fmt.Errorf("opencode: could not create a session")
	}
	os.MkdirAll(filepath.Dir(a.idFile), 0o755)
	if err := os.WriteFile(a.idFile, []byte(s.ID), 0o644); err != nil {
		return "", err
	}
	a.session = s.ID
	return s.ID, nil
}

func (a *opencodeAgent) turns() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	id, err := a.ensureSession()
	if err != nil {
		return 0
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Get(a.url + "/session/" + id + "/message")
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var msgs []struct {
		Info struct {
			Role string `json:"role"`
		} `json:"info"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&msgs)
	n := 0
	for _, m := range msgs {
		if m.Info.Role == "user" {
			n++
		}
	}
	return n
}

func (a *opencodeAgent) chat(text string) (chatReply, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id, err := a.ensureSession()
	if err != nil {
		return chatReply{}, err
	}
	body, _ := json.Marshal(map[string]any{"parts": []map[string]any{{"type": "text", "text": text}}})
	start := time.Now()
	resp, err := http.Post(a.url+"/session/"+id+"/message", "application/json", bytes.NewReader(body))
	if err != nil {
		return chatReply{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return chatReply{}, fmt.Errorf("opencode: %d %s", resp.StatusCode, b)
	}
	var out struct {
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return chatReply{}, err
	}
	if a.onTurn != nil {
		a.onTurn()
	}
	var reply strings.Builder
	for _, p := range out.Parts {
		if p.Type == "text" {
			reply.WriteString(p.Text)
		}
	}
	out2 := chatReply{Reply: reply.String(), TTFTMs: float64(time.Since(start).Microseconds()) / 1000, Backend: "opencode"}
	out2.Turns, out2.PromptTokens, out2.CachedTokens = a.turnUsage(id)
	out2.RecomputedTokens = out2.PromptTokens - out2.CachedTokens
	return out2, nil
}

// turnUsage reads the conversation back from OpenCode: the number of user
// turns, and the token usage of the FIRST model call of the latest turn. That
// call is the one that meets the KV cache as the turn found it; later calls in
// the same turn (after tool results) are always warm from the first.
// OpenCode reports input tokens without the cached ones, so prompt = input +
// cache.read. Zeroes when OpenCode cannot be read: usage is reporting only.
func (a *opencodeAgent) turnUsage(id string) (turns int, prompt, cached int64) {
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Get(a.url + "/session/" + id + "/message")
	if err != nil {
		return 0, 0, 0
	}
	defer resp.Body.Close()
	var msgs []struct {
		Info struct {
			ID       string `json:"id"`
			Role     string `json:"role"`
			ParentID string `json:"parentID"`
			Tokens   struct {
				Input int64 `json:"input"`
				Cache struct {
					Read int64 `json:"read"`
				} `json:"cache"`
			} `json:"tokens"`
		} `json:"info"`
	}
	if json.NewDecoder(resp.Body).Decode(&msgs) != nil {
		return 0, 0, 0
	}
	lastUser := ""
	for _, m := range msgs {
		if m.Info.Role == "user" {
			turns++
			lastUser = m.Info.ID
		}
	}
	for _, m := range msgs {
		if m.Info.Role == "assistant" && m.Info.ParentID == lastUser {
			return turns, m.Info.Tokens.Input + m.Info.Tokens.Cache.Read, m.Info.Tokens.Cache.Read
		}
	}
	return turns, 0, 0
}

// newChatBackend picks the agent from AGENT (builtin by default). onTurn
// runs after every completed turn (the sandbox snapshots its workspace).
func newChatBackend(dir, router, session string, onTurn func()) chatBackend {
	if envStr("AGENT", "builtin") == "opencode" {
		a, err := startOpenCode(dir, router, session)
		if err == nil {
			a.onTurn = onTurn
			return a
		}
		log.Printf("opencode unavailable, falling back to the builtin agent: %v", err)
	}
	b := newBuiltinAgent(dir, router, session)
	b.onTurn = onTurn
	return b
}

func handleChat(b chatBackend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.Content) == "" {
			http.Error(w, "need content", http.StatusBadRequest)
			return
		}
		out, err := b.chat(in.Content)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	}
}
