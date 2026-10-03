package main

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/config"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestHookPushPolicy: the hook resolves a task worktree to its main checkout's
// push_policy row, and fails closed everywhere else.
func TestHookPushPolicy(t *testing.T) {
	oldCfg := cfg
	t.Cleanup(func() { cfg = oldCfg })

	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(repo, ".worktrees", "task-1")
	runGit(t, repo, "worktree", "add", "-q", "-b", "staypoint/task-1", wt)
	plain := t.TempDir()

	// No config at all: never.
	cfg = nil
	if got := hookPushPolicy(wt); got != "never" {
		t.Errorf("nil cfg: got %q", got)
	}

	cfg = config.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.DBPath = filepath.Join(cfg.DataDir, "staypoint.db")

	// Database missing: never.
	if got := hookPushPolicy(wt); got != "never" {
		t.Errorf("missing db: got %q", got)
	}

	// A database without the push_policy column (daemon not yet migrated):
	// never, and the hook must not add the column.
	raw, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE project_dev_configs (repo_path TEXT PRIMARY KEY, dev_command TEXT, dev_url TEXT, setup_steps_json TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO project_dev_configs (repo_path) VALUES (?)`, repo); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if got := hookPushPolicy(wt); got != "never" {
		t.Errorf("unmigrated db: got %q", got)
	}
	ro, err := db.OpenReadOnly(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if db.HasColumn(ro, "project_dev_configs", "push_policy") {
		t.Error("hook migrated the database")
	}
	ro.Close()
	if err := os.Remove(cfg.DBPath); err != nil {
		t.Fatal(err)
	}

	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := shipreview.UpsertProjectDevConfig(store.DB(), &shipreview.ProjectDevConfig{RepoPath: repo, PushPolicy: shipreview.PushPolicyBranchOnly}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	for _, tc := range []struct {
		dir, want, desc string
	}{
		{"", "never", "empty dir"},
		{"relative/path", "never", "relative dir"},
		{plain, "never", "not a git repo"},
		{repo, "branch_only", "main checkout"},
		{wt, "branch_only", "task worktree resolves to its main checkout"},
		{filepath.Join(wt, "sub"), "branch_only", "subdir of worktree"},
	} {
		if tc.dir != "" && filepath.IsAbs(tc.dir) {
			_ = os.MkdirAll(tc.dir, 0o755)
		}
		if got := hookPushPolicy(tc.dir); got != tc.want {
			t.Errorf("%s (%s): got %q, want %q", tc.desc, tc.dir, got, tc.want)
		}
	}
}
