package shipreview_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

// task-2bdcdc74: Approve merges without touching the repo root checkout.

// commitOnBranch commits file=content on a new branch cut from main and
// pushes it, leaving the root checkout where it was.
func commitOnBranch(t *testing.T, repo, branch, file, content string) string {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "wt")
	gitIn(t, repo, "worktree", "add", "-b", branch, wt, "main")
	if err := os.WriteFile(filepath.Join(wt, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", file)
	gitIn(t, wt, "commit", "-m", "change "+file)
	sha := gitIn(t, wt, "rev-parse", "HEAD")
	gitIn(t, repo, "push", "origin", branch)
	gitIn(t, repo, "worktree", "remove", "--force", wt)
	return sha
}

func newApproveCard(t *testing.T, repo, taskID, branch, sha string) (*sql.DB, *shipreview.Card, func() (string, error)) {
	t.Helper()
	db := openTestDB(t)
	_, _ = db.Exec(`INSERT INTO tasks (id, name) VALUES (?, 'approve isolation')`, taskID)
	recordMainBase(t, db, repo, taskID)
	card, err := shipreview.CreateCard(db, taskID, branch, sha, []string{"1. Verify"}, "", repo, nil)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	return db, card, func() (string, error) {
		return shipreview.ApproveAndMerge(context.Background(), db, card, repo, "main")
	}
}

// rootState is what Approve must leave alone in the root checkout.
func rootState(t *testing.T, repo string) string {
	t.Helper()
	return gitIn(t, repo, "rev-parse", "HEAD") + "\n" + gitIn(t, repo, "status", "--porcelain=v1", "--untracked-files=all")
}

func assertNoMergeLeftovers(t *testing.T, repo string) {
	t.Helper()
	for _, f := range []string{"MERGE_HEAD", "MERGE_MSG", "AUTO_MERGE"} {
		if _, err := os.Stat(filepath.Join(repo, ".git", f)); err == nil {
			t.Errorf(".git/%s left behind", f)
		}
	}
	if out := gitIn(t, repo, "diff", "--name-only", "--diff-filter=U"); out != "" {
		t.Errorf("unmerged paths left in root: %s", out)
	}
	if n := strings.Count(gitIn(t, repo, "worktree", "list", "--porcelain"), "worktree "); n != 1 {
		t.Errorf("want only the root worktree, have %d:\n%s", n, gitIn(t, repo, "worktree", "list"))
	}
}

func bothMergeModes(t *testing.T, fn func(t *testing.T)) {
	t.Run("merge-tree", fn)
	t.Run("worktree-fallback", func(t *testing.T) {
		shipreview.ForceMergeWorktreeFallbackForTest(t)
		fn(t)
	})
}

func TestApprove_DirtyRootCheckoutStillMerges(t *testing.T) {
	bothMergeModes(t, func(t *testing.T) {
		repo, branch, sha := setupGitRepo(t)
		_, _, approve := newApproveCard(t, repo, "t-dirty", branch, sha)
		// The stray go.mod edit: a modified tracked file, plus an untracked one.
		if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("local edit\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("untracked clash\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		before := rootState(t, repo)

		merged, err := approve()
		if err != nil {
			t.Fatalf("Approve with dirty root: %v", err)
		}
		if got := gitIn(t, repo, "rev-parse", "origin/main"); got != merged {
			t.Fatalf("origin/main = %s, want %s", got, merged)
		}
		gitIn(t, repo, "merge-base", "--is-ancestor", sha, "origin/main")
		if after := rootState(t, repo); after != before {
			t.Fatalf("root checkout changed:\nbefore %s\nafter  %s", before, after)
		}
		if b, _ := os.ReadFile(filepath.Join(repo, "README.md")); string(b) != "local edit\n" {
			t.Fatalf("local edit lost: %q", b)
		}
		assertNoMergeLeftovers(t, repo)
	})
}

func TestApprove_StaleIndexLockInRootStillMerges(t *testing.T) {
	bothMergeModes(t, func(t *testing.T) {
		repo, branch, sha := setupGitRepo(t)
		_, _, approve := newApproveCard(t, repo, "t-lock", branch, sha)
		lock := filepath.Join(repo, ".git", "index.lock")
		if err := os.WriteFile(lock, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		merged, err := approve()
		if err != nil {
			t.Fatalf("Approve with stale index.lock: %v", err)
		}
		if got := gitIn(t, repo, "rev-parse", "origin/main"); got != merged {
			t.Fatalf("origin/main = %s, want %s", got, merged)
		}
		if _, err := os.Stat(lock); err != nil {
			t.Fatalf("Approve touched the root's index.lock: %v", err)
		}
	})
}

func TestApprove_ConflictReturnsFilesAndLeavesNothing(t *testing.T) {
	bothMergeModes(t, func(t *testing.T) {
		repo, branch, sha := setupGitRepo(t)
		db, card, approve := newApproveCard(t, repo, "t-conflict", branch, sha)
		// main gains a different feature.txt after the branch was cut.
		mainSHA := commitOnBranch(t, repo, "other", "feature.txt", "main's version\n")
		gitIn(t, repo, "push", "origin", mainSHA+":refs/heads/main")
		gitIn(t, repo, "fetch", "origin")
		before := rootState(t, repo)

		_, err := approve()
		var conflict *shipreview.MergeConflictError
		if !errors.As(err, &conflict) || !errors.Is(err, shipreview.ErrMergeConflict) {
			t.Fatalf("want MergeConflictError, got %v", err)
		}
		if conflict.Target != "main" || !reflect.DeepEqual(conflict.Files, []string{"feature.txt"}) {
			t.Fatalf("conflict = %+v, want main [feature.txt]", conflict)
		}
		if got := gitIn(t, repo, "rev-parse", "origin/main"); got != mainSHA {
			t.Fatalf("origin/main moved to %s on conflict", got)
		}
		if after := rootState(t, repo); after != before {
			t.Fatalf("root checkout changed:\nbefore %s\nafter  %s", before, after)
		}
		assertNoMergeLeftovers(t, repo)
		if got, _ := shipreview.GetCard(db, card.TaskID); got.Status != shipreview.StatusPending {
			t.Fatalf("card status = %q after conflict, want pending", got.Status)
		}

		// The next Approve is not wedged by the failed one: resolve on the
		// branch and it merges.
		fix := filepath.Join(t.TempDir(), "fix")
		gitIn(t, repo, "worktree", "add", "--detach", fix, sha)
		gitIn(t, fix, "merge", "-X", "theirs", "origin/main")
		fixed := gitIn(t, fix, "rev-parse", "HEAD")
		gitIn(t, repo, "worktree", "remove", "--force", fix)
		gitIn(t, repo, "update-ref", "refs/heads/"+branch, fixed)
		card.HeadSHA = fixed
		if _, err := approve(); err != nil {
			t.Fatalf("Approve after resolving: %v", err)
		}
	})
}

func TestApprove_ConcurrentSameRepoSerialized(t *testing.T) {
	repo, branchA, shaA := setupGitRepo(t)
	shaB := commitOnBranch(t, repo, "feature/b", "b.txt", "b\n")
	_, _, approveA := newApproveCard(t, repo, "t-a", branchA, shaA)
	_, _, approveB := newApproveCard(t, repo, "t-b", "feature/b", shaB)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, fn := range []func() (string, error){approveA, approveB} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = fn()
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Approve %d: %v", i, err)
		}
	}
	for _, sha := range []string{shaA, shaB} {
		gitIn(t, repo, "merge-base", "--is-ancestor", sha, "origin/main")
	}
	assertNoMergeLeftovers(t, repo)
}

func TestApprove_HeadMovedStill409(t *testing.T) {
	repo, branch, sha := setupGitRepo(t)
	_, card, approve := newApproveCard(t, repo, "t-moved", branch, sha)
	card.HeadSHA = strings.Repeat("0", 40)
	if _, err := approve(); !errors.Is(err, shipreview.ErrHeadMoved) {
		t.Fatalf("want ErrHeadMoved, got %v", err)
	}
}

// With the target not checked out anywhere the local branch follows the
// merge; checked out (the root on main) it is left for its owner to pull.
func TestApprove_LocalTargetAdvancesOnlyWhenNotCheckedOut(t *testing.T) {
	repo, branch, sha := setupGitRepo(t)
	before := gitIn(t, repo, "rev-parse", "main")
	_, _, approve := newApproveCard(t, repo, "t-local", branch, sha)
	merged, err := approve()
	if err != nil {
		t.Fatal(err)
	}
	if got := gitIn(t, repo, "rev-parse", "main"); got != before {
		t.Fatalf("checked-out main moved to %s", got)
	}

	repo2, branch2, sha2 := setupGitRepo(t)
	gitIn(t, repo2, "checkout", "--detach")
	_, _, approve2 := newApproveCard(t, repo2, "t-local2", branch2, sha2)
	if merged, err = approve2(); err != nil {
		t.Fatal(err)
	}
	if got := gitIn(t, repo2, "rev-parse", "main"); got != merged {
		t.Fatalf("local main = %s, want %s", got, merged)
	}
}
