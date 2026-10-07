package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A project that ships to dev-server cuts its task branches from
// origin/dev-server, not origin/main, and records that target.
func TestCreate_BranchesFromTargetBranch(t *testing.T) {
	repo, other := repoWithOrigin(t)
	db := testDB(t)
	gitIn(t, other, "checkout", "-b", "dev-server")
	devTip := commitFile(t, other, "dev.txt", "dev only\n", "dev-server work")
	gitIn(t, other, "push", "origin", "dev-server")

	wm := NewWorktreeManager(repo, db)
	wm.TargetBranch = func(context.Context, string) (string, error) { return "dev-server", nil }
	wt, err := wm.Create("task-target", "sess")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := gitIn(t, wt, "rev-parse", "HEAD"); got != devTip {
		t.Fatalf("worktree HEAD = %s, want origin/dev-server %s", got, devTip)
	}
	if _, err := os.Stat(filepath.Join(wt, "dev.txt")); err != nil {
		t.Fatalf("dev-server file missing from task worktree: %v", err)
	}
	ctx := context.Background()
	if got, err := RecordedTaskBase(ctx, db, "task-target"); err != nil || got != devTip {
		t.Fatalf("recorded base = %q (err %v), want %s", got, err, devTip)
	}
	if got, err := RecordedTaskTarget(ctx, db, "task-target"); err != nil || got != "dev-server" {
		t.Fatalf("recorded target = %q (err %v), want dev-server", got, err)
	}
}

// With no target configured the default branch is cut and recorded.
func TestCreate_DefaultTargetRecorded(t *testing.T) {
	repo, _ := repoWithOrigin(t)
	db := testDB(t)
	wm := NewWorktreeManager(repo, db)
	wm.TargetBranch = func(context.Context, string) (string, error) { return "", nil }
	if _, err := wm.Create("task-default", "sess"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got, _ := RecordedTaskTarget(context.Background(), db, "task-default"); got != "main" {
		t.Fatalf("recorded target = %q, want main", got)
	}
}

// A configured target that does not exist, or a resolver error, makes no
// worktree: it never falls back to main.
func TestCreate_MissingTargetFailsClosed(t *testing.T) {
	cases := map[string]func(context.Context, string) (string, error){
		"missing branch": func(context.Context, string) (string, error) { return "dev-server", nil },
		"resolver error": func(context.Context, string) (string, error) { return "", errors.New("config unreadable") },
		"flag-like name": func(context.Context, string) (string, error) { return "--upload-pack=x", nil },
	}
	for name, resolve := range cases {
		t.Run(name, func(t *testing.T) {
			repo, _ := repoWithOrigin(t)
			db := testDB(t)
			wm := NewWorktreeManager(repo, db)
			wm.TargetBranch = resolve
			if _, err := wm.Create("task-missing", "sess"); err == nil {
				t.Fatal("Create succeeded; want an error")
			}
			if _, err := os.Stat(filepath.Join(repo, ".worktrees", "task-missing")); !os.IsNotExist(err) {
				t.Fatalf("worktree left behind (stat err %v)", err)
			}
			if got, _ := RecordedTaskBase(context.Background(), db, "task-missing"); got != "" {
				t.Fatalf("base recorded for a failed create: %s", got)
			}
		})
	}
}
