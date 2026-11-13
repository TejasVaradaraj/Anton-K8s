// Package kv is the small HTTP contract between Anton and an inference
// replica / cache tier. The mock replica implements it today; a thin adapter
// in front of vLLM + LMCache would implement the same four calls.
package kv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

type SessionCache struct {
	SessionID        string `json:"session_id"`
	Tokens           int64  `json:"tokens"`
	Bytes            int64  `json:"bytes"`
	LastAccessUnixMs int64  `json:"last_access_unix_ms"`
	Hits             int    `json:"hits"`
}

type ReplicaInfo struct {
	Replica            string         `json:"replica"`
	InFlight           int            `json:"in_flight"`
	Concurrency        int            `json:"concurrency"`
	PrefillMsPerToken  float64        `json:"prefill_ms_per_token"`
	PrefillCurve       string         `json:"prefill_curve,omitempty"` // "tokens:seconds,..." measured on the real GPU
	BytesPerToken      int64          `json:"bytes_per_token"`
	OffloadBytesPerSec float64        `json:"offload_bytes_per_sec"`
	RestoreBytesPerSec float64        `json:"restore_bytes_per_sec"`
	Sessions           []SessionCache `json:"sessions"`
}

type CompletionRequest struct {
	SessionID     string `json:"session_id"`
	ContextTokens int64  `json:"context_tokens"`
}

type CompletionResponse struct {
	Replica          string  `json:"replica"`
	CachedTokens     int64   `json:"cached_tokens"`
	RecomputedTokens int64   `json:"recomputed_tokens"`
	PrefillMs        float64 `json:"prefill_ms"`
	QueueMs          float64 `json:"queue_ms"`
}

type OffloadRequest struct {
	SessionID string `json:"session_id"`
	Delete    bool   `json:"delete"`
}

type Block struct {
	Tokens int64 `json:"tokens"`
	Bytes  int64 `json:"bytes"`
	// TokenIDs is the session's prompt as token IDs (real vLLM replicas only).
	// LMCache keys KV blocks by token content, so a replica that never saw the
	// session can prefetch its cache from these IDs.
	TokenIDs []int `json:"token_ids,omitempty"`
}

var ErrNotFound = errors.New("not found")

var httpClient = &http.Client{Timeout: 5 * time.Minute}

// Do sends a JSON request and decodes a JSON reply. A 404 becomes ErrNotFound.
func Do(ctx context.Context, method, url string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %d %s", method, url, resp.StatusCode, b)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func Sessions(ctx context.Context, base string) (*ReplicaInfo, error) {
	var info ReplicaInfo
	return &info, Do(ctx, http.MethodGet, base+"/kv/sessions", nil, &info)
}

func Offload(ctx context.Context, base, session string, del bool) error {
	return Do(ctx, http.MethodPost, base+"/kv/offload", OffloadRequest{SessionID: session, Delete: del}, nil)
}

func Restore(ctx context.Context, base, session string) error {
	return Do(ctx, http.MethodPost, base+"/kv/restore", OffloadRequest{SessionID: session}, nil)
}
