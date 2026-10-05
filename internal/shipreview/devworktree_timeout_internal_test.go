package shipreview

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// STA-710: after STA-685, recreating a dev worktree ran `git worktree remove
// --force` under the quick git timeout, and deleting a worktree that holds
// node_modules took longer than that. The recreate must delete the old tree
// outside git's quick timeout and still finish well inside the slow one.
func TestEnsureDevWorktree_RecreatesBigTreeWithinTimeout(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "package.json")
	git("commit", "-q", "-m", "init")
	sha := git("rev-parse", "HEAD")

	wt := filepath.Join(repo, ".worktrees", "devserver-task-big")
	if err := ensureDevWorktree(context.Background(), repo, wt, sha); err != nil {
		t.Fatalf("first create: %v", err)
	}

	// A node_modules-like tree: thousands of small files in nested packages.
	// Deleting it takes well over the quick timeout set below.
	for p := 0; p < 150; p++ {
		dir := filepath.Join(wt, "node_modules", fmt.Sprintf("pkg-%d", p), "lib")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for f := 0; f < 20; f++ {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("m%d.js", f)), []byte("module.exports = 1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Setenv(gitexec.TimeoutEnv, "500ms")
	t.Setenv(gitexec.SlowTimeoutEnv, "60s")
	start := time.Now()
	if err := ensureDevWorktree(context.Background(), repo, wt, sha); err != nil {
		t.Fatalf("recreate after %s: %v", time.Since(start), err)
	}
	if took := time.Since(start); took > 60*time.Second {
		t.Fatalf("recreate took %s", took)
	}

	if _, err := os.Stat(filepath.Join(wt, "node_modules")); !os.IsNotExist(err) {
		t.Errorf("node_modules survived the recreate (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "package.json")); err != nil {
		t.Errorf("recreated worktree missing package.json: %v", err)
	}
	if got := git("-C", wt, "rev-parse", "HEAD"); got != sha {
		t.Errorf("worktree HEAD = %s, want %s", got, sha)
	}
	if n := strings.Count(git("worktree", "list", "--porcelain"), "devserver-task-big"); n != 1 {
		t.Errorf("devserver worktree registered %d times, want 1", n)
	}
}
