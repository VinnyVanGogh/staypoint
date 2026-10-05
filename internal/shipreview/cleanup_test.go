package shipreview_test

// STA-637: the task branch is deleted after Approve & merge, never before, and
// never the target or default branch.

import (
	"context"
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
	db := openTestDB(t)
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
	return repoDir, card, mainSHA
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
	// A worktree and a dev worktree that a path-like ID would sweep away.
	other := filepath.Join(repoDir, ".worktrees", "task-keep")
	gitIn(t, repoDir, "worktree", "add", "--detach", other, "main")
	for _, id := range []string{"", "/", ".", "..", "../x", "a/b", `a\b`} {
		c := *card
		c.TaskID = id
		if err := shipreview.CleanupMergedBranch(context.Background(), repoDir, &c, mainSHA); err == nil {
			t.Errorf("task id %q: want error, got nil", id)
		}
	}
	if !remoteHasBranch(t, repoDir, card.Branch) {
		t.Error("branch deleted despite invalid task id")
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("unrelated worktree removed by an invalid task id: %v", err)
	}
}

// STA-649: the ancestor checks run before any worktree is removed, so an
// unmerged agent worktree keeps its uncommitted edits.
func TestCleanupMergedBranchKeepsUnmergedAgentWorktree(t *testing.T) {
	repoDir, card, mainSHA := mergedCard(t, "t-agentwt")
	agentWT := filepath.Join(repoDir, ".worktrees", card.TaskID)
	gitIn(t, repoDir, "worktree", "add", agentWT, card.Branch)

	// The agent kept working after the review: one local commit, one
	// uncommitted edit. Neither reached main.
	if err := os.WriteFile(filepath.Join(agentWT, "late.txt"), []byte("late\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, agentWT, "add", ".")
	gitIn(t, agentWT, "commit", "-m", "late commit")
	dirty := filepath.Join(agentWT, "wip.txt")
	if err := os.WriteFile(dirty, []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := shipreview.CleanupMergedBranch(context.Background(), repoDir, card, mainSHA)
	if err == nil || !strings.Contains(err.Error(), "not in main") {
		t.Fatalf("want 'not in main' error, got %v", err)
	}
	if _, err := os.Stat(dirty); err != nil {
		t.Errorf("uncommitted work in unmerged agent worktree lost: %v", err)
	}
	if !localHasBranch(repoDir, card.Branch) || !remoteHasBranch(t, repoDir, card.Branch) {
		t.Error("branch deleted although the agent worktree holds unmerged work")
	}
}

// STA-649: a remote branch that moved past main blocks the whole cleanup,
// including the worktree removal that used to happen first.
func TestCleanupMergedBranchRemoteAheadRemovesNothing(t *testing.T) {
	repoDir, card, mainSHA := mergedCard(t, "t-remoteahead")
	agentWT := filepath.Join(repoDir, ".worktrees", card.TaskID)
	gitIn(t, repoDir, "worktree", "add", agentWT, card.Branch)
	dirty := filepath.Join(agentWT, "wip.txt")
	if err := os.WriteFile(dirty, []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Someone else pushes to the task branch from another clone.
	bare := gitIn(t, repoDir, "remote", "get-url", "origin")
	other := filepath.Join(t.TempDir(), "other")
	gitIn(t, filepath.Dir(other), "clone", "-q", "-b", card.Branch, bare, other)
	if err := os.WriteFile(filepath.Join(other, "remote.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, other, "add", ".")
	gitIn(t, other, "commit", "-m", "pushed elsewhere")
	gitIn(t, other, "push", "origin", card.Branch)

	err := shipreview.CleanupMergedBranch(context.Background(), repoDir, card, mainSHA)
	if err == nil || !strings.Contains(err.Error(), "remote branch") {
		t.Fatalf("want remote-branch refusal, got %v", err)
	}
	if _, err := os.Stat(dirty); err != nil {
		t.Errorf("agent worktree removed before the remote check: %v", err)
	}
	if !localHasBranch(repoDir, card.Branch) || !remoteHasBranch(t, repoDir, card.Branch) {
		t.Error("branch deleted although the remote holds unmerged work")
	}
}

// STA-649: a failing fetch --prune aborts cleanup instead of deciding on a
// stale view of the remote.
func TestCleanupMergedBranchFetchFailureDeletesNothing(t *testing.T) {
	repoDir, card, mainSHA := mergedCard(t, "t-fetchfail")
	agentWT := filepath.Join(repoDir, ".worktrees", card.TaskID)
	gitIn(t, repoDir, "worktree", "add", agentWT, card.Branch)
	bare := gitIn(t, repoDir, "remote", "get-url", "origin")
	gitIn(t, repoDir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))

	err := shipreview.CleanupMergedBranch(context.Background(), repoDir, card, mainSHA)
	if err == nil || !strings.Contains(err.Error(), "fetch --prune") {
		t.Fatalf("want fetch --prune error, got %v", err)
	}
	if _, err := os.Stat(agentWT); err != nil {
		t.Errorf("agent worktree removed despite fetch failure: %v", err)
	}
	if !localHasBranch(repoDir, card.Branch) {
		t.Error("local branch deleted despite fetch failure")
	}

	// Remote reachable again: the retry cleans everything up.
	gitIn(t, repoDir, "remote", "set-url", "origin", bare)
	if err := shipreview.CleanupMergedBranch(context.Background(), repoDir, card, mainSHA); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if remoteHasBranch(t, repoDir, card.Branch) || localHasBranch(repoDir, card.Branch) {
		t.Error("branch survived the retry")
	}
}

// STA-649: an async dev-server setup still running at approve is cancelled, so
// it neither recreates devserver-<id> nor starts the server afterwards.
func TestCleanupMergedBranchCancelsInFlightDevSetup(t *testing.T) {
	db := openTestDB(t)
	taskID := "t-devsetup"
	if _, err := db.Exec(`INSERT INTO tasks (id, name) VALUES (?, 'dev')`, taskID); err != nil {
		t.Fatal(err)
	}
	repoDir, branch, featureSHA := setupGitRepo(t)
	card, err := shipreview.CreateCard(db, taskID, branch, featureSHA, []string{"1. Verify"}, "", repoDir, nil)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}

	devWT := filepath.Join(repoDir, ".worktrees", "devserver-"+taskID)
	started := filepath.Join(t.TempDir(), "server-started")
	stepRunning := filepath.Join(t.TempDir(), "step-running")
	cfg := &shipreview.ProjectDevConfig{
		RepoPath:   repoDir,
		DevCommand: "touch " + started + "; sleep 60",
		DevURL:     "http://localhost:39999",
		SetupSteps: []string{"touch " + stepRunning + "; sleep 30"},
	}
	if _, err := shipreview.StartDevServerAsync(db, card, cfg, repoDir, nil); err != nil {
		t.Fatalf("StartDevServerAsync: %v", err)
	}
	waitFor(t, 10*time.Second, "setup step to start", func() bool {
		_, err := os.Stat(stepRunning)
		return err == nil
	})

	mainSHA, err := shipreview.ApproveAndMerge(context.Background(), db, card, repoDir, "main")
	if err != nil {
		t.Fatalf("ApproveAndMerge: %v", err)
	}
	if err := shipreview.CleanupMergedBranch(context.Background(), repoDir, card, mainSHA); err != nil {
		t.Fatalf("CleanupMergedBranch: %v", err)
	}

	// The setup has unwound by now; give a late recreation a chance to show.
	waitFor(t, 10*time.Second, "dev state to leave starting", func() bool {
		c, err := shipreview.GetCard(db, taskID)
		return err == nil && c.DevState != shipreview.DevStateStarting
	})
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(devWT); !os.IsNotExist(err) {
		t.Errorf("devserver worktree recreated after cleanup (stat err %v)", err)
	}
	if _, err := os.Stat(started); !os.IsNotExist(err) {
		t.Error("dev server started after the review was approved and cleaned up")
	}
	if remoteHasBranch(t, repoDir, branch) || localHasBranch(repoDir, branch) {
		t.Error("branch not deleted")
	}
}

// STA-649 review: the leased local delete keeps branch -D's refusal to delete
// a branch checked out in some other worktree.
func TestCleanupMergedBranchKeepsBranchCheckedOutElsewhere(t *testing.T) {
	repoDir, card, mainSHA := mergedCard(t, "t-elsewhere")
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	gitIn(t, repoDir, "worktree", "add", elsewhere, card.Branch)

	err := shipreview.CleanupMergedBranch(context.Background(), repoDir, card, mainSHA)
	if err == nil || !strings.Contains(err.Error(), "checked out in") {
		t.Fatalf("want checked-out refusal, got %v", err)
	}
	if !localHasBranch(repoDir, card.Branch) {
		t.Error("local branch deleted while checked out in another worktree")
	}
	if got := gitIn(t, elsewhere, "symbolic-ref", "HEAD"); got != "refs/heads/"+card.Branch {
		t.Errorf("other worktree HEAD = %q", got)
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
