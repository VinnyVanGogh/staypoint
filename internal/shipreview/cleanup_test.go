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
	for _, id := range []string{"", "..", "../x", "a/b"} {
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
