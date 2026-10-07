package shipreview_test

// PR #249 re-review F1: `git update-ref -d refs/heads/<b>` follows symbolic
// refs, and a loose ref reached through a filesystem symlink (a symlinked
// ref file, or a symlinked directory such as refs/heads/loop -> .) is
// another branch's file. Neither may be shipped, resolved or deleted.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

func gitDir(t *testing.T, repo string) string {
	t.Helper()
	return gitOut(t, repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
}

// aliases builds, in repo, three names that git resolves to main.
func aliases(t *testing.T, repo string) []string {
	t.Helper()
	gd := gitDir(t, repo)
	gitT(t, repo, "symbolic-ref", "refs/heads/fix/sym", "refs/heads/main")
	if err := os.Symlink("main", filepath.Join(gd, "refs", "heads", "lnk")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".", filepath.Join(gd, "refs", "heads", "loop")); err != nil {
		t.Fatal(err)
	}
	return []string{"fix/sym", "lnk", "loop/main"}
}

func TestSec249_CleanupNeverDeletesThroughSymrefOrSymlink(t *testing.T) {
	repo, card, mainSHA := mergedCard(t, "t-sym")
	ctx := context.Background()
	for _, alias := range aliases(t, repo) {
		cc := *card
		cc.Branch = alias
		if err := shipreview.CleanupMergedBranch(ctx, repo, &cc, mainSHA); !errors.Is(err, shipreview.ErrProtectedBranch) {
			t.Errorf("CleanupMergedBranch(%q) = %v, want ErrProtectedBranch", alias, err)
		}
		if !refExists(t, repo, "refs/heads/main") {
			t.Fatalf("refs/heads/main deleted through %q", alias)
		}
		if err := shipreview.DeleteBranch(ctx, repo, alias); !errors.Is(err, shipreview.ErrProtectedBranch) {
			t.Errorf("DeleteBranch(%q) = %v, want ErrProtectedBranch", alias, err)
		}
	}
}

func TestSec249_CurrentBranchHEADRefusesSymrefAndSymlink(t *testing.T) {
	repo, _, _ := setupGitRepo(t)
	for _, alias := range aliases(t, repo) {
		if sha, err := shipreview.CurrentBranchHEAD(context.Background(), repo, alias); err == nil {
			t.Errorf("CurrentBranchHEAD(%q) = %s, want an error", alias, sha)
		}
	}
}
