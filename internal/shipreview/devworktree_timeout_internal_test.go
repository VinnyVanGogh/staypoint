package shipreview

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
)

// STA-685: Start dev sat on "Creating dev worktree" while git hung in the
// client repo. The cleanup calls before `worktree add` must give up after the
// short git timeout and report it, not wait out gitOutput's 60s each.
func TestEnsureDevWorktree_GitHangFailsFast(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(gitexec.TimeoutEnv, "300ms")

	repo := t.TempDir()
	wt := filepath.Join(t.TempDir(), "devserver-task-x")
	start := time.Now()
	err := ensureDevWorktree(context.Background(), repo, wt, "deadbeef")
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("ensureDevWorktree took %s with git hanging", took)
	}
	if !gitexec.IsTimeout(err) {
		t.Fatalf("err = %v, want a git timeout", err)
	}
}
