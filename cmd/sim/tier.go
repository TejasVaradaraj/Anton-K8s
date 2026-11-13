package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"

	"github.com/TejasVaradaraj/anton-platform/internal/kv"
)

// runTier is the cache tier: where KV blocks and workspace snapshots go so
// they outlive any one pod. It keeps everything in memory, so it must run on
// a node that is never disrupted (the control-plane node in this setup).
func runTier() {
	var mu sync.Mutex
	blocks := map[string]kv.Block{}
	workspaces := map[string][]byte{}
	mux := http.NewServeMux()

	mux.HandleFunc("PUT /blocks/{id}", func(w http.ResponseWriter, r *http.Request) {
		var b kv.Block
		if json.NewDecoder(r.Body).Decode(&b) != nil {
			http.Error(w, "bad block", http.StatusBadRequest)
			return
		}
		mu.Lock()
		blocks[r.PathValue("id")] = b
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
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
	mux.HandleFunc("GET /blocks", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(blocks)
	})
	mux.HandleFunc("DELETE /blocks/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		delete(blocks, r.PathValue("id"))
		delete(workspaces, r.PathValue("id"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /blocks", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		blocks, workspaces = map[string]kv.Block{}, map[string][]byte{}
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /ws/{id}", func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		workspaces[r.PathValue("id")] = data
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /ws/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		data, ok := workspaces[r.PathValue("id")]
		mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	log.Println("cache tier listening on :9000")
	log.Fatal(http.ListenAndServe(":9000", mux))
}
