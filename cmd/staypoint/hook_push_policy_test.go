package main

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/config"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/security"
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
// push_policy row, reads no row as branch_only, and fails closed everywhere
// else.
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

	// repo stores "pr" (distinct from both the default and the fail-closed
	// value, so worktree resolution is observable), denied stores an explicit
	// "never", defaulted has a row created without push_policy, and
	// unconfigured has no row at all.
	denied := newGitRepo(t)
	defaulted := newGitRepo(t)
	unconfigured := newGitRepo(t)
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*shipreview.ProjectDevConfig{
		{RepoPath: repo, PushPolicy: shipreview.PushPolicyPR},
		{RepoPath: denied, PushPolicy: shipreview.PushPolicyNever},
	} {
		if err := shipreview.UpsertProjectDevConfig(store.DB(), c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().Exec(`INSERT INTO project_dev_configs (repo_path, dev_command, dev_url, setup_steps_json) VALUES (?, '', '', '[]')`, defaulted); err != nil {
		t.Fatal(err)
	}
	store.Close()

	for _, tc := range []struct {
		dir, want, desc string
	}{
		{"", "never", "empty dir"},
		{"relative/path", "never", "relative dir"},
		{plain, "never", "not a git repo"},
		{repo, "pr", "main checkout"},
		{wt, "pr", "task worktree resolves to its main checkout"},
		{filepath.Join(wt, "sub"), "pr", "subdir of worktree"},
		{denied, "never", "explicit never"},
		{defaulted, "branch_only", "row without push_policy takes the column default"},
		{unconfigured, "branch_only", "no row"},
	} {
		if tc.dir != "" && filepath.IsAbs(tc.dir) {
			_ = os.MkdirAll(tc.dir, 0o755)
		}
		if got := hookPushPolicy(tc.dir); got != tc.want {
			t.Errorf("%s (%s): got %q, want %q", tc.desc, tc.dir, got, tc.want)
		}
	}
}

// newGitRepo creates a git repo with one commit on main.
func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	return dir
}

// TestHookPushPolicyNoRowAllowsBranchPush: with the hook's real resolver, a
// repo that has no project_dev_configs row may push a feature branch and
// dev-server, but a push to main/master stays Red, as does a force or delete
// push (Board decision 2026-10-08). An explicit "never" row still blocks
// every push.
func TestHookPushPolicyNoRowAllowsBranchPush(t *testing.T) {
	oldCfg := cfg
	t.Cleanup(func() { cfg = oldCfg })
	cfg = config.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.DBPath = filepath.Join(cfg.DataDir, "staypoint.db")

	unconfigured := newGitRepo(t)
	denied := newGitRepo(t)
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := shipreview.UpsertProjectDevConfig(store.DB(), &shipreview.ProjectDevConfig{RepoPath: denied, PushPolicy: shipreview.PushPolicyNever}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	for _, tc := range []struct {
		cwd, cmd string
		red      bool
		desc     string
	}{
		{unconfigured, "git push origin main", true, "no row: main is Red"},
		{unconfigured, "git push origin master", true, "no row: master is Red"},
		{unconfigured, "git push origin HEAD:refs/heads/main", true, "no row: refspec to main is Red"},
		{unconfigured, "git push --force origin staypoint/task-x", true, "no row: force push is Red"},
		{unconfigured, "git push --delete origin staypoint/task-x", true, "no row: delete push is Red"},
		{unconfigured, "git push --mirror origin", true, "no row: mirror push is Red"},
		{denied, "git push origin staypoint/task-x", true, "explicit never: branch push is Red"},
		{unconfigured, "git push origin staypoint/task-x", false, "no row: feature branch is not Red"},
		{unconfigured, "git push origin dev-server", false, "no row: dev-server is not Red"},
	} {
		c := &security.Classifier{CWD: tc.cwd, PushPolicyFor: hookPushPolicy}
		got := c.Classify(tc.cmd)
		if (got.Tier == security.Red) != tc.red {
			t.Errorf("[%s] %q: got %s (%v), want red=%v", tc.desc, tc.cmd, got.Tier, got.Reasons, tc.red)
		}
	}
}
