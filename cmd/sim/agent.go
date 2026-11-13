package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// runAgent is the sandbox stub. It stands in for a coding agent: it keeps
// appending to a journal in its workspace, and that workspace is what must
// survive hibernation and node loss. The first journal line carries a random
// workspace ID, so a restored sandbox can prove it is the same workspace.
func runAgent() {
	session := envStr("SESSION_ID", "unknown")
	tier := envStr("TIER_URL", "http://anton-tier:9000")
	dir := envStr("WORKDIR", "/workspace")
	// PERSIST_WORKSPACE=0 is the baseline where a sandbox is an ordinary pod.
	persist := envStr("PERSIST_WORKSPACE", "1") == "1"
	journal := filepath.Join(dir, "journal.log")
	os.MkdirAll(dir, 0o755)

	var mu sync.Mutex
	// A workspace volume (PVC mode) that already holds a journal was kept or
	// restored from a VolumeSnapshot; it is newer than any tier tarball.
	restored, source := false, ""
	if _, err := os.Stat(journal); err == nil {
		restored, source = true, "volume"
	} else if !persist {
		// nothing to restore from
	} else if resp, err := http.Get(tier + "/ws/" + session); err == nil {
		if resp.StatusCode == http.StatusOK {
			data, _ := io.ReadAll(resp.Body)
			if err := untar(dir, data); err == nil {
				restored, source = true, "tier"
			}
		}
		resp.Body.Close()
	}
	if !restored {
		os.WriteFile(journal, []byte(fmt.Sprintf("workspace %d\n", time.Now().UnixNano())), 0o644)
	}

	snapshot := func() error {
		if !persist {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		data, err := tarDir(dir)
		if err != nil {
			return err
		}
		req, _ := http.NewRequest(http.MethodPut, tier+"/ws/"+session, bytes.NewReader(data))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}

	go func() { // the "work": one journal line every two seconds, a file every ten steps
		for step := 0; ; step++ {
			time.Sleep(2 * time.Second)
			mu.Lock()
			if f, err := os.OpenFile(journal, os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
				fmt.Fprintf(f, "step %s\n", time.Now().Format(time.RFC3339))
				f.Close()
			}
			if step%10 == 9 {
				writeWorkFile(dir, step/10)
			}
			mu.Unlock()
			if step%5 == 4 {
				snapshot() // periodic snapshot bounds the loss on a hard kill to ~10s
			}
		}
	}()

	agent := newChatBackend(dir, envStr("ROUTER_URL", "http://anton-router:8080"), session, func() {
		if err := snapshot(); err != nil {
			log.Println("post-turn snapshot failed:", err)
		}
	})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /chat", handleChat(agent))
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		lines, first := 0, ""
		if f, err := os.Open(journal); err == nil {
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				if lines == 0 {
					first = sc.Text()
				}
				lines++
			}
			f.Close()
		}
		json.NewEncoder(w).Encode(map[string]any{"session": session, "workspace": first, "lines": lines,
			"restored": restored, "source": source, "turns": agent.turns()})
	})
	mux.HandleFunc("GET /files", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"files": listFiles(dir, 500)})
	})
	mux.HandleFunc("GET /files/content", func(w http.ResponseWriter, r *http.Request) {
		target, ok := insideDir(dir, r.URL.Query().Get("path"))
		if !ok {
			http.Error(w, "path outside the workspace", http.StatusBadRequest)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		f, err := os.Open(target)
		if err != nil {
			http.Error(w, "no such file", http.StatusNotFound)
			return
		}
		defer f.Close()
		const limit = 64 << 10
		data, _ := io.ReadAll(io.LimitReader(f, limit+1))
		truncated := len(data) > limit
		if truncated {
			data = data[:limit]
		}
		json.NewEncoder(w).Encode(map[string]any{"path": r.URL.Query().Get("path"), "content": string(data), "truncated": truncated})
	})
	mux.HandleFunc("POST /snapshot", func(w http.ResponseWriter, _ *http.Request) {
		if err := snapshot(); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	go func() { // graceful stop: save the workspace before the pod goes away
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		<-ch
		if err := snapshot(); err != nil {
			log.Println("final snapshot failed:", err)
		}
		os.Exit(0)
	}()
	log.Printf("agent for session %s (restored=%v from %q)", session, restored, source)
	log.Fatal(http.ListenAndServe(":8080", mux))
}

func tarDir(dir string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: 0o644, Size: int64(len(data))}); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), tw.Close()
}

func untar(dir string, data []byte) error {
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.Clean("/"+h.Name)) // Clean("/"+name) blocks ../ escapes
		if !strings.HasPrefix(target, dir) {
			continue
		}
		os.MkdirAll(filepath.Dir(target), 0o755)
		body, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, body, 0o644); err != nil {
			return err
		}
	}
}

// writeWorkFile stands in for the code an agent writes: one small file per
// ten steps under src/, keeping the newest twenty.
func writeWorkFile(dir string, n int) {
	src := filepath.Join(dir, "src")
	os.MkdirAll(src, 0o755)
	body := fmt.Sprintf("# step %d\n# written by the sandbox agent at %s\n\ndef step_%d():\n    return %d\n",
		n, time.Now().Format(time.RFC3339), n, n)
	os.WriteFile(filepath.Join(src, fmt.Sprintf("step_%03d.py", n)), []byte(body), 0o644)
	if old := n - 20; old >= 0 {
		os.Remove(filepath.Join(src, fmt.Sprintf("step_%03d.py", old)))
	}
}

type fileEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Dir  bool   `json:"dir"`
}

func listFiles(dir string, max int) []fileEntry {
	out := []fileEntry{}
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || path == dir {
			return nil
		}
		if len(out) >= max {
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(dir, path)
		out = append(out, fileEntry{Path: filepath.ToSlash(rel), Size: info.Size(), Dir: info.IsDir()})
		return nil
	})
	return out
}

// insideDir resolves a client-supplied relative path and refuses anything
// that would escape the workspace (../, absolute paths, symlinks out).
func insideDir(dir, rel string) (string, bool) {
	if rel == "" {
		return "", false
	}
	sep := string(filepath.Separator)
	clean := filepath.Join(dir, filepath.Clean("/"+rel))
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		root = dir
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		// Not on disk (yet): resolve its parent instead, so a symlinked
		// directory still cannot smuggle the path out.
		parent, perr := filepath.EvalSymlinks(filepath.Dir(clean))
		if perr != nil {
			return clean, clean == dir || strings.HasPrefix(clean, dir+sep)
		}
		resolved = filepath.Join(parent, filepath.Base(clean))
	}
	return resolved, resolved == root || strings.HasPrefix(resolved, root+sep)
}
