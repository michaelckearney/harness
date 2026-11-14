package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRmDeletesOnlyInTheWorktree(t *testing.T) {
	wt := t.TempDir()
	os.MkdirAll(filepath.Join(wt, ".harness"), 0o755)
	os.WriteFile(filepath.Join(wt, ".harness", "pr.md"), []byte("fix: x\n"), 0o644)
	os.MkdirAll(filepath.Join(wt, ".git"), 0o755)
	os.MkdirAll(filepath.Join(wt, "backend", "old"), 0o755)
	os.WriteFile(filepath.Join(wt, "backend", "demo.go"), []byte("package backend\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "backend", "old", "a.go"), nil, 0o644)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "keep"), nil, 0o644)
	os.Symlink(outside, filepath.Join(wt, "out"))
	cwd := filepath.Join(wt, "backend")

	for _, bad := range [][]string{{"../.git"}, {"../.harness/pr.md"}, {"../.."}, {".."}, {outside}, {"../out/keep"}, {"missing.go"}, {"demo.go", "../.git"}} {
		if _, err := remove(cwd, bad); err == nil {
			t.Errorf("rm %v was allowed", bad)
		}
	}
	// A refusal deletes nothing, even of the paths that were fine.
	if _, err := os.Stat(filepath.Join(cwd, "demo.go")); err != nil {
		t.Fatal("a refused rm deleted demo.go")
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal("rm reached outside the worktree")
	}
	out, err := remove(cwd, []string{"demo.go", "old"})
	if err != nil || !strings.Contains(out, "backend/demo.go, backend/old") {
		t.Fatalf("rm: %q %v", out, err)
	}
	for _, gone := range []string{"demo.go", "old"} {
		if _, err := os.Stat(filepath.Join(cwd, gone)); !os.IsNotExist(err) {
			t.Errorf("%s is still there", gone)
		}
	}
	if _, err := remove(t.TempDir(), []string{"x"}); err == nil {
		t.Error("rm worked outside a worktree")
	}
}
