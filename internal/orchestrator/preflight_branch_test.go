package orchestrator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/gitgate"
	"github.com/VinnyVanGogh/staypoint/internal/workspace"
)

func gitOutT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", name)
	gitT(t, dir, "commit", "-q", "-m", name)
}

func TestPreflightBranch_UsesRecordedTaskTarget(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec(`INSERT INTO task_worktree_bases (task_id, repo_path, base_sha, target_branch) VALUES ('task-dev', '/r', 'abc', 'dev-server')`); err != nil {
		t.Fatal(err)
	}
	if got := preflightBranch(context.Background(), db, "/r", "task-dev"); got != "dev-server" {
		t.Fatalf("preflightBranch = %q, want dev-server", got)
	}
}

func TestPreflightBranch_FallsBackToMain(t *testing.T) {
	db := openTestDB(t)
	repo := t.TempDir()
	initGitRepo(t, repo)
	if got := preflightBranch(context.Background(), db, repo, "task-none"); got != "main" {
		t.Fatalf("preflightBranch = %q, want main", got)
	}
}

// A task cut from dev-server, in a repo where dev-server and main have
// diverged, must fast-forward against dev-server. Against main (the old
// hardcoded branch) pre-flight fails and the run ends with zero turns.
func TestPreflightBranch_DevServerTaskFastForwards(t *testing.T) {
	ctx := context.Background()
	origin := t.TempDir()
	initGitRepo(t, origin)
	gitT(t, origin, "checkout", "-q", "-b", "dev-server")
	commitFile(t, origin, "dev.txt", "dev 1\n")
	gitT(t, origin, "checkout", "-q", "main")
	commitFile(t, origin, "main.txt", "main only\n")

	clone := filepath.Join(t.TempDir(), "clone")
	gitT(t, origin, "clone", "-q", origin, clone)
	gitT(t, clone, "config", "user.email", "test@test.com")
	gitT(t, clone, "config", "user.name", "Test")
	wt := filepath.Join(t.TempDir(), "wt")
	gitT(t, clone, "worktree", "add", "-q", "--no-track", "-b", "staypoint/task-dev", wt, "origin/dev-server")
	base := gitOutT(t, wt, "rev-parse", "HEAD")

	// dev-server moves on after the task was cut.
	gitT(t, origin, "checkout", "-q", "dev-server")
	commitFile(t, origin, "dev2.txt", "dev 2\n")

	db := openTestDB(t)
	if err := workspace.RecordTaskBase(ctx, db, clone, "task-dev", base); err != nil {
		t.Fatal(err)
	}
	if err := workspace.RecordTaskTarget(ctx, db, "task-dev", "dev-server"); err != nil {
		t.Fatal(err)
	}

	if r, err := gitgate.PreFlight(ctx, wt, "main"); err != nil || r.OK {
		t.Fatalf("pre-flight against main should fail for a dev-server task (the old bug); got ok=%v err=%v", r != nil && r.OK, err)
	}

	branch := preflightBranch(ctx, db, clone, "task-dev")
	if branch != "dev-server" {
		t.Fatalf("preflightBranch = %q, want dev-server", branch)
	}
	r, err := gitgate.PreFlight(ctx, wt, branch)
	if err != nil || !r.OK {
		t.Fatalf("pre-flight against %s failed: err=%v result=%+v", branch, err, r)
	}
	if r.Behind != 1 {
		t.Fatalf("behind = %d, want 1 (fast-forwarded the new dev-server commit)", r.Behind)
	}
	if _, err := os.Stat(filepath.Join(wt, "dev2.txt")); err != nil {
		t.Fatalf("worktree was not fast-forwarded to origin/dev-server: %v", err)
	}
}
