package shipreview_test

// STA-637: the task branch is deleted after Approve & merge, never before, and
// never the target or default branch.

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v (in %s): %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func remoteHasBranch(t *testing.T, repoDir, branch string) bool {
	t.Helper()
	return gitIn(t, repoDir, "ls-remote", "--heads", "origin", "refs/heads/"+branch) != ""
}

func localHasBranch(repoDir, branch string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	cmd.Dir = repoDir
	return cmd.Run() == nil
}

// mergedCard sets up a repo whose feature branch has been approved and merged.
func mergedCard(t *testing.T, taskID string) (repoDir string, card *shipreview.Card, mainSHA string) {
	t.Helper()
	_, repoDir, card, mainSHA = mergedCardDB(t, taskID)
	return repoDir, card, mainSHA
}

func mergedCardDB(t *testing.T, taskID string) (db *sql.DB, repoDir string, card *shipreview.Card, mainSHA string) {
	t.Helper()
	db = openTestDB(t)
	// One connection: each new connection to ":memory:" is a fresh, empty DB.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`INSERT INTO tasks (id, name) VALUES (?, 'cleanup')`, taskID); err != nil {
		t.Fatal(err)
	}
	repoDir, branch, featureSHA := setupGitRepo(t)
	card, err := shipreview.CreateCard(db, taskID, branch, featureSHA, []string{"1. Verify"}, "", repoDir, nil)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	mainSHA, err = shipreview.ApproveAndMerge(context.Background(), db, card, repoDir, "main")
	if err != nil {
		t.Fatalf("ApproveAndMerge: %v", err)
	}
	return db, repoDir, card, mainSHA
}

func TestCleanupMergedBranchDeletesRemoteLocalAndWorktrees(t *testing.T) {
	repoDir, card, mainSHA := mergedCard(t, "t-clean")
	ctx := context.Background()

	// Agent worktree holding the branch, plus a leftover dev-server worktree.
	agentWT := filepath.Join(repoDir, ".worktrees", card.TaskID)
	devWT := filepath.Join(repoDir, ".worktrees", "devserver-"+card.TaskID)
	gitIn(t, repoDir, "worktree", "add", agentWT, card.Branch)
	gitIn(t, repoDir, "worktree", "add", "--detach", devWT, card.HeadSHA)

	if err := shipreview.CleanupMergedBranch(ctx, repoDir, card, mainSHA); err != nil {
		t.Fatalf("CleanupMergedBranch: %v", err)
	}

	if remoteHasBranch(t, repoDir, card.Branch) {
		t.Errorf("remote branch %q still present", card.Branch)
	}
	if localHasBranch(repoDir, card.Branch) {
		t.Errorf("local branch %q still present", card.Branch)
	}
	for _, wt := range []string{agentWT, devWT} {
		if _, err := os.Stat(wt); !os.IsNotExist(err) {
			t.Errorf("worktree %s still present (stat err %v)", wt, err)
		}
	}
	if !remoteHasBranch(t, repoDir, "main") {
		t.Error("main must survive cleanup")
	}

	// Retrying after success is a no-op.
	if err := shipreview.CleanupMergedBranch(ctx, repoDir, card, mainSHA); err != nil {
		t.Errorf("second CleanupMergedBranch: %v", err)
	}
}

func TestCleanupMergedBranchRefusesDefaultBranch(t *testing.T) {
	repoDir, card, mainSHA := mergedCard(t, "t-default")
	ctx := context.Background()

	// Make "develop" the remote default branch, so the guard is exercised by
	// origin/HEAD rather than by the hard-coded main/master names.
	gitIn(t, repoDir, "branch", "develop", "main")
	gitIn(t, repoDir, "push", "origin", "develop")
	gitIn(t, repoDir, "remote", "set-head", "origin", "develop")

	for _, branch := range []string{"main", "master", "develop"} {
		c := *card
		c.Branch = branch
		err := shipreview.CleanupMergedBranch(ctx, repoDir, &c, mainSHA)
		if !errors.Is(err, shipreview.ErrProtectedBranch) {
			t.Errorf("CleanupMergedBranch(%q): want ErrProtectedBranch, got %v", branch, err)
		}
	}
	for _, branch := range []string{"main", "develop"} {
		if !remoteHasBranch(t, repoDir, branch) {
			t.Errorf("protected branch %q was deleted from the remote", branch)
		}
		if !localHasBranch(repoDir, branch) {
			t.Errorf("protected branch %q was deleted locally", branch)
		}
	}
	// The real task branch is untouched by the refused calls.
	if !remoteHasBranch(t, repoDir, card.Branch) {
		t.Errorf("task branch %q deleted by a refused call", card.Branch)
	}
}

func TestCleanupMergedBranchKeepsUnmergedWork(t *testing.T) {
	repoDir, card, mainSHA := mergedCard(t, "t-unmerged")

	// A commit lands on the task branch after the merge.
	gitIn(t, repoDir, "checkout", card.Branch)
	if err := os.WriteFile(filepath.Join(repoDir, "late.txt"), []byte("late\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repoDir, "add", ".")
	gitIn(t, repoDir, "commit", "-m", "late commit")
	gitIn(t, repoDir, "push", "origin", card.Branch)
	gitIn(t, repoDir, "checkout", "main")

	err := shipreview.CleanupMergedBranch(context.Background(), repoDir, card, mainSHA)
	if err == nil || !strings.Contains(err.Error(), "commits not in main") {
		t.Fatalf("want 'commits not in main' error, got %v", err)
	}
	if !localHasBranch(repoDir, card.Branch) {
		t.Error("local branch with unmerged work was deleted")
	}
	if !remoteHasBranch(t, repoDir, card.Branch) {
		t.Error("remote branch with unmerged work was deleted")
	}
}

func TestCleanupMergedBranchRejectsBadTaskID(t *testing.T) {
	repoDir, card, mainSHA := mergedCard(t, "t-badid")
	// "/" is the dangerous one: filepath.Base("/") == "/" and joining it onto
	// .worktrees yields .worktrees itself (STA-648).
	for _, id := range []string{"", "..", "../x", "a/b", "/", `\`, `a\b`, ".hidden"} {
		c := *card
		c.TaskID = id
		if err := shipreview.CleanupMergedBranch(context.Background(), repoDir, &c, mainSHA); err == nil {
			t.Errorf("task id %q: want error, got nil", id)
		}
	}
	if !remoteHasBranch(t, repoDir, card.Branch) {
		t.Error("branch deleted despite invalid task id")
	}
}

// commitOn makes a commit touching name in dir and returns its SHA.
func commitOn(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", name)
	gitIn(t, dir, "commit", "-m", "add "+name)
	return gitIn(t, dir, "rev-parse", "HEAD")
}

// STA-648 #2: unmerged local work is detected before any worktree is removed,
// so the agent worktree survives with its uncommitted changes.
func TestCleanupMergedBranchKeepsWorktreeWithUnmergedLocalWork(t *testing.T) {
	repoDir, card, mainSHA := mergedCard(t, "t-keepwt")
	agentWT := filepath.Join(repoDir, ".worktrees", card.TaskID)
	devWT := filepath.Join(repoDir, ".worktrees", "devserver-"+card.TaskID)
	gitIn(t, repoDir, "worktree", "add", agentWT, card.Branch)
	gitIn(t, repoDir, "worktree", "add", "--detach", devWT, card.HeadSHA)

	commitOn(t, agentWT, "unpushed.txt")
	dirty := filepath.Join(agentWT, "uncommitted.txt")
	if err := os.WriteFile(dirty, []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := shipreview.CleanupMergedBranch(context.Background(), repoDir, card, mainSHA)
	if err == nil || !strings.Contains(err.Error(), "local branch") || !strings.Contains(err.Error(), "commits not in main") {
		t.Fatalf("want local 'commits not in main' error, got %v", err)
	}
	if _, err := os.Stat(dirty); err != nil {
		t.Errorf("uncommitted change in agent worktree lost: %v", err)
	}
	if _, err := os.Stat(devWT); err != nil {
		t.Errorf("nothing may be removed when a tip is unmerged; dev worktree gone: %v", err)
	}
	if !localHasBranch(repoDir, card.Branch) {
		t.Error("local branch deleted")
	}
	if !remoteHasBranch(t, repoDir, card.Branch) {
		t.Error("remote branch deleted despite unmerged local work")
	}
}

// STA-648 #2/#3: a push to the task branch that this clone never fetched is
// seen via the remote itself, and blocks all cleanup.
func TestCleanupMergedBranchKeepsUnfetchedRemoteWork(t *testing.T) {
	repoDir, card, mainSHA := mergedCard(t, "t-unfetched")
	agentWT := filepath.Join(repoDir, ".worktrees", card.TaskID)
	gitIn(t, repoDir, "worktree", "add", agentWT, card.Branch)

	// Someone else pushes to the branch from another clone.
	other := filepath.Join(t.TempDir(), "other")
	origin := gitIn(t, repoDir, "remote", "get-url", "origin")
	gitIn(t, filepath.Dir(other), "clone", "-q", "-b", card.Branch, origin, other)
	pushed := commitOn(t, other, "elsewhere.txt")
	gitIn(t, other, "push", "-q", "origin", card.Branch)

	err := shipreview.CleanupMergedBranch(context.Background(), repoDir, card, mainSHA)
	if err == nil || !strings.Contains(err.Error(), "remote branch") || !strings.Contains(err.Error(), pushed) {
		t.Fatalf("want remote 'commits not in main' error naming %s, got %v", pushed, err)
	}
	if got := gitIn(t, repoDir, "ls-remote", "origin", "refs/heads/"+card.Branch); !strings.HasPrefix(got, pushed) {
		t.Errorf("remote branch = %q, want tip %s", got, pushed)
	}
	if _, err := os.Stat(agentWT); err != nil {
		t.Errorf("agent worktree removed despite unmerged remote work: %v", err)
	}
	if !localHasBranch(repoDir, card.Branch) {
		t.Error("local branch deleted despite unmerged remote work")
	}
}

// STA-648 #3: the remote delete carries a lease on the tip that was checked.
func TestDeleteRemoteBranchAtHonoursLease(t *testing.T) {
	repoDir, card, _ := mergedCard(t, "t-lease")
	ctx := context.Background()
	checked := gitIn(t, repoDir, "rev-parse", "refs/heads/"+card.Branch)

	// A push lands after the tip was read.
	gitIn(t, repoDir, "checkout", "-q", card.Branch)
	newer := commitOn(t, repoDir, "racer.txt")
	gitIn(t, repoDir, "push", "-q", "origin", card.Branch)
	gitIn(t, repoDir, "checkout", "-q", "main")

	if err := shipreview.DeleteRemoteBranchAt(ctx, repoDir, card.Branch, checked); err == nil {
		t.Fatal("delete with stale lease succeeded")
	}
	if got := gitIn(t, repoDir, "ls-remote", "origin", "refs/heads/"+card.Branch); !strings.HasPrefix(got, newer) {
		t.Fatalf("remote branch = %q, want it kept at %s", got, newer)
	}

	if err := shipreview.DeleteRemoteBranchAt(ctx, repoDir, card.Branch, newer); err != nil {
		t.Fatalf("delete with current lease: %v", err)
	}
	if remoteHasBranch(t, repoDir, card.Branch) {
		t.Error("remote branch still present after leased delete")
	}

	for _, b := range []string{"main", "master"} {
		if err := shipreview.DeleteRemoteBranchAt(ctx, repoDir, b, newer); !errors.Is(err, shipreview.ErrProtectedBranch) {
			t.Errorf("DeleteRemoteBranchAt(%q): want ErrProtectedBranch, got %v", b, err)
		}
	}
}

// STA-648 #4: a Start Dev setup still running at approve is canceled, so it
// cannot recreate devserver-<id> after cleanup.
func TestCleanupMergedBranchCancelsInFlightDevSetup(t *testing.T) {
	db, repoDir, card, mainSHA := mergedCardDB(t, "t-devrace")
	defer shipreview.SetDevSetupWait(10 * time.Second)()
	devWT := filepath.Join(repoDir, ".worktrees", "devserver-"+card.TaskID)

	cfg := &shipreview.ProjectDevConfig{
		RepoPath:   repoDir,
		DevCommand: "sleep 9999",
		DevURL:     "http://127.0.0.1:9999",
		SetupSteps: []string{"sleep 60"},
	}
	if _, err := shipreview.StartDevServerAsync(db, card, cfg, repoDir, nil); err != nil {
		t.Fatalf("StartDevServerAsync: %v", err)
	}
	waitFor(t, "dev worktree to appear", func() bool {
		_, err := os.Stat(devWT)
		return err == nil
	})

	start := time.Now()
	if err := shipreview.CleanupMergedBranch(context.Background(), repoDir, card, mainSHA); err != nil {
		t.Fatalf("CleanupMergedBranch: %v", err)
	}
	if d := time.Since(start); d > 8*time.Second {
		t.Errorf("cleanup took %v; setup step was not canceled", d)
	}
	waitFor(t, "setup to report canceled", func() bool {
		c, err := shipreview.GetCard(db, card.TaskID)
		return err == nil && c.DevState == shipreview.DevStateIdle &&
			len(c.DevLog) > 0 && c.DevLog[len(c.DevLog)-1] == "Dev server setup canceled"
	})
	if _, err := os.Stat(devWT); !os.IsNotExist(err) {
		t.Errorf("dev worktree %s recreated after cleanup (stat err %v)", devWT, err)
	}
	if out := gitIn(t, repoDir, "worktree", "list"); strings.Contains(out, "devserver-") {
		t.Errorf("dev worktree still registered:\n%s", out)
	}
	if remoteHasBranch(t, repoDir, card.Branch) {
		t.Errorf("remote branch %q still present", card.Branch)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
