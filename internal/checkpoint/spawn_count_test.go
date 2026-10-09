package checkpoint

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// countGitSpawns puts a logging git shim first on PATH and returns a func
// that reports the git subcommands run since the last call. The shim and its
// log live under t.TempDir; the real git is untouched.
func countGitSpawns(t *testing.T) func() []string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	shimDir := t.TempDir()
	logPath := filepath.Join(shimDir, "spawns.log")
	script := fmt.Sprintf("#!/bin/sh\necho \"$1\" >> %q\nexec %q \"$@\"\n", logPath, realGit)
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		data, _ := os.ReadFile(logPath)
		_ = os.Remove(logPath)
		return strings.Fields(string(data))
	}
}

// The task page's whole-run diff starts with the pre-run lookup and then runs
// two diffs. Each git spawn costs 100ms+ on a loaded machine, so the page
// paid for ten of them (STA-775). With one rev-parse per call and the
// pre-run ref passed straight to git, the diffs need no for-each-ref.
func TestWholeRunDiffGitSpawns(t *testing.T) {
	dir := setupTestGitRepo(t)
	ctx := context.Background()
	taskID := "task-spawns"

	if err := os.WriteFile(filepath.Join(dir, "file_a.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	pre, err := CreateCheckpoint(ctx, CreateOptions{WorkDir: dir, SessionID: "run-1", Message: "pre-run " + taskID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file_a.txt"), []byte("v3"), 0o644); err != nil {
		t.Fatal(err)
	}

	spawns := countGitSpawns(t)

	id, ref, err := FindPreRunCheckpointRef(ctx, dir, taskID)
	if err != nil || id != pre.ID || ref != pre.Ref {
		t.Fatalf("FindPreRunCheckpointRef = %q, %q, %v; want %q, %q", id, ref, err, pre.ID, pre.Ref)
	}
	if got := spawns(); strings.Join(got, " ") != "rev-parse for-each-ref" {
		t.Errorf("pre-run lookup spawned %v, want [rev-parse for-each-ref]", got)
	}

	stat, err := DiffCheckpoint(ctx, dir, ref)
	if err != nil || !strings.Contains(stat, "file_a.txt") {
		t.Fatalf("DiffCheckpoint(ref) = %q, %v", stat, err)
	}
	if got := spawns(); strings.Join(got, " ") != "rev-parse diff" {
		t.Errorf("stat diff spawned %v, want [rev-parse diff]", got)
	}

	files, err := DiffCheckpointFiles(ctx, dir, ref)
	if err != nil || len(files) != 1 || files[0].Path != "file_a.txt" {
		t.Fatalf("DiffCheckpointFiles(ref) = %+v, %v", files, err)
	}
	if got := spawns(); strings.Join(got, " ") != "rev-parse diff" {
		t.Errorf("numstat diff spawned %v, want [rev-parse diff]", got)
	}
}

// getGitPaths reads both paths from one rev-parse; both must still be right
// from a subdirectory and from a linked worktree, whose git dir is not
// <root>/.git.
func TestGetGitPathsOneSpawn(t *testing.T) {
	dir := setupTestGitRepo(t)
	ctx := context.Background()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", dir, "worktree", "add", "-q", wt).CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v: %s", err, out)
	}
	resolve := func(p string) string {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	spawns := countGitSpawns(t)
	for _, tc := range []struct{ dir, root, gitDir string }{
		{dir, dir, filepath.Join(dir, ".git")},
		{sub, dir, filepath.Join(dir, ".git")},
		{wt, wt, filepath.Join(dir, ".git", "worktrees", "wt")},
	} {
		root, gitDir, err := getGitPaths(ctx, tc.dir)
		if err != nil {
			t.Fatalf("getGitPaths(%s): %v", tc.dir, err)
		}
		if resolve(root) != resolve(tc.root) || resolve(gitDir) != resolve(tc.gitDir) {
			t.Errorf("getGitPaths(%s) = %q, %q; want %q, %q", tc.dir, root, gitDir, tc.root, tc.gitDir)
		}
		if got := spawns(); len(got) != 1 {
			t.Errorf("getGitPaths(%s) spawned %v, want one rev-parse", tc.dir, got)
		}
	}

	if _, _, err := getGitPaths(ctx, t.TempDir()); err == nil {
		t.Error("getGitPaths outside a repo: want error")
	}
}
