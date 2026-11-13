package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInsideDirRefusesEscapes(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("x"), 0o644)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o644)
	os.Symlink(outside, filepath.Join(dir, "link"))

	for rel, want := range map[string]bool{
		"ok.txt":          true,
		"/ok.txt":         true,  // treated as relative to the workspace
		"../ok.txt":       true,  // cleaned back inside
		"../../etc/hosts": true,  // remapped to <workspace>/etc/hosts, never the real one
		"link/secret":     false, // symlink pointing outside
		"":                false,
	} {
		if _, ok := insideDir(dir, rel); ok != want {
			t.Errorf("insideDir(%q) = %v, want %v", rel, ok, want)
		}
	}
}

func TestListFilesAndWorkFiles(t *testing.T) {
	dir := t.TempDir()
	for n := 0; n < 25; n++ {
		writeWorkFile(dir, n)
	}
	files := listFiles(dir, 500)
	py := 0
	for _, f := range files {
		if !f.Dir {
			py++
		}
	}
	if py != 20 {
		t.Fatalf("kept %d work files, want the newest 20", py)
	}
	if got := listFiles(dir, 3); len(got) != 3 {
		t.Fatalf("listFiles cap: got %d entries", len(got))
	}
}
