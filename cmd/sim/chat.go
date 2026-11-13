package main

// OpenAI-format replies for the mock replica, so agents and the router see
// the same API a vLLM server exposes. The text is a placeholder; the timing
// and the usage block (including cached_tokens, as vLLM reports it) are the
// point.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/TejasVaradaraj/anton-platform/internal/kv"
	"github.com/TejasVaradaraj/anton-platform/internal/openai"
)

const mockModel = "anton-mock"

func models(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   []map[string]any{{"id": mockModel, "object": "model", "owned_by": "anton"}},
	})
}

func usage(res kv.CompletionResponse, prompt, completion int64) map[string]any {
	return map[string]any{
		"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": prompt + completion,
		"prompt_tokens_details": map[string]any{"cached_tokens": res.CachedTokens},
	}
}

func reply(res kv.CompletionResponse, prompt int64) string {
	return fmt.Sprintf("[mock model on %s] %d prompt tokens: %d from the KV cache, %d recomputed in %.0f ms.",
		res.Replica, prompt, res.CachedTokens, res.RecomputedTokens, res.PrefillMs)
}

func writeChat(w http.ResponseWriter, in *openai.Request, res kv.CompletionResponse, prompt int64) {
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	text := reply(res, prompt)
	completion := int64(len(text) / 4)
	model := in.Model
	if model == "" {
		model = mockModel
	}
	// Anton's own numbers ride along for the API and benchmark; OpenAI
	// clients ignore unknown fields.
	anton := map[string]any{"replica": res.Replica, "cached_tokens": res.CachedTokens,
		"recomputed_tokens": res.RecomputedTokens, "prefill_ms": res.PrefillMs, "queue_ms": res.QueueMs}

	if !in.Stream {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": text}}},
			"usage": usage(res, prompt, completion), "anton": anton,
		})
		return
	}
	// Server-sent events, chunked the way OpenAI-compatible servers stream.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	send(chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	for _, word := range strings.SplitAfter(text, " ") {
		send(chunk(map[string]any{"content": word}, nil))
	}
	last := chunk(map[string]any{}, "stop")
	last["usage"], last["anton"] = usage(res, prompt, completion), anton
	send(last)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
