package shipreview_test

// #245: a registered branch that differs from main/dev-server only in case
// ("Main", "MAIN", "Dev-Server") resolves to the protected branch on a
// case-insensitive filesystem (macOS APFS): `git update-ref -d refs/heads/Main`
// deletes the loose ref file refs/heads/main. Cards must refuse such names,
// and cleanup must never delete a ref that is (case-insensitively) a
// protected, default or target branch.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
	"github.com/VinnyVanGogh/staypoint/internal/workspace"
)

// caseInsensitiveFS reports whether dir's filesystem folds case, as the
// default macOS APFS volume does.
func caseInsensitiveFS(t *testing.T, dir string) bool {
	t.Helper()
	p := filepath.Join(dir, ".case-probe")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(p)
	_, err := os.Stat(filepath.Join(dir, ".CASE-PROBE"))
	return err == nil
}

func refExists(t *testing.T, repo, ref string) bool {
	t.Helper()
	for _, line := range splitLines(gitOut(t, repo, "for-each-ref", "--format=%(refname)", ref)) {
		if line == ref {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// The deletion itself, end to end, on a case-insensitive filesystem: an
// approved card whose branch is "Main" must never delete refs/heads/main.
func TestSec245_CleanupNeverDeletesCaseAliasOfMain(t *testing.T) {
	for _, alias := range []string{"Main", "MAIN", "mAiN"} {
		t.Run(alias, func(t *testing.T) {
			repo, card, mainSHA := mergedCard(t, "t-case-"+alias)
			if !caseInsensitiveFS(t, repo) {
				t.Skip("test filesystem is case-sensitive: refs/heads/" + alias + " cannot alias refs/heads/main here; the EqualFold/exact-ref logic is covered by TestSec245_CleanupRefusesCaseVariantNames")
			}
			card.Branch = alias
			err := shipreview.CleanupMergedBranch(context.Background(), repo, card, mainSHA)
			if !errors.Is(err, shipreview.ErrProtectedBranch) {
				t.Errorf("CleanupMergedBranch(%q) = %v, want ErrProtectedBranch", alias, err)
			}
			if !refExists(t, repo, "refs/heads/main") {
				t.Fatalf("refs/heads/main was deleted through the alias %q", alias)
			}
			if gitOut(t, repo, "ls-remote", "--heads", "origin", "refs/heads/main") == "" {
				t.Fatalf("origin main was deleted through the alias %q", alias)
			}
		})
	}
}

// Filesystem-independent: case variants of protected, default and target
// names are refused by name before any git ref is touched.
func TestSec245_CleanupRefusesCaseVariantNames(t *testing.T) {
	repo, card, mainSHA := mergedCard(t, "t-case-names")
	gitT(t, repo, "branch", "dev-server", "main")
	gitT(t, repo, "branch", "release", "main")
	ctx := context.Background()
	for _, c := range []struct{ branch, target string }{
		{"Main", ""}, {"MAIN", ""}, {"Master", ""}, {"Dev-Server", ""}, {"DEV-SERVER", ""},
		{"Release", "release"}, {"RELEASE", "release"},
	} {
		cc := *card
		cc.Branch, cc.TargetBranch = c.branch, c.target
		if err := shipreview.CleanupMergedBranch(ctx, repo, &cc, mainSHA); !errors.Is(err, shipreview.ErrProtectedBranch) {
			t.Errorf("CleanupMergedBranch(branch %q, target %q) = %v, want ErrProtectedBranch", c.branch, c.target, err)
		}
		if err := shipreview.DeleteBranch(ctx, repo, c.branch); c.target == "" && !errors.Is(err, shipreview.ErrProtectedBranch) {
			t.Errorf("DeleteBranch(%q) = %v, want ErrProtectedBranch", c.branch, err)
		}
	}
	for _, ref := range []string{"refs/heads/main", "refs/heads/dev-server", "refs/heads/release"} {
		if !refExists(t, repo, ref) {
			t.Fatalf("%s was deleted", ref)
		}
	}
}

// A name that only resolves through case folding is not that branch: the
// head lookup fails instead of returning the other branch's tip.
func TestSec245_CurrentBranchHEADRequiresExactCase(t *testing.T) {
	repo, branch, _ := setupGitRepo(t)
	ctx := context.Background()
	for _, alias := range []string{"Feature/Test-Ship", "FEATURE/TEST-SHIP", "Main"} {
		if sha, err := shipreview.CurrentBranchHEAD(ctx, repo, alias); err == nil {
			t.Errorf("CurrentBranchHEAD(%q) = %s; want an error (only %q / main exist)", alias, sha, branch)
		}
	}
	if _, err := shipreview.CurrentBranchHEAD(ctx, repo, branch); err != nil {
		t.Fatalf("exact name: %v", err)
	}
}

// Case variants of main and dev-server are refused when the card is built.
func TestSec245_CardRefusesCaseVariantRegisteredBranch(t *testing.T) {
	t.Run("main", func(t *testing.T) {
		for _, alias := range []string{"Main", "MAIN", "origin/Main"} {
			t.Run(alias, func(t *testing.T) {
				const taskID = "task-casemain"
				db := openTestDB(t)
				repo, _, _ := setupGitRepo(t)
				gitT(t, repo, "branch", "staypoint/"+taskID, "main")
				recordMainBase(t, db, repo, taskID)
				commitIn(t, repo, "later.txt", "later\n") // main moves past the base
				gitT(t, repo, "push", "origin", "main")
				registerWorkProduct(t, db, taskID, "branch", alias)

				card, err := shipreview.BuildAndStartCard(context.Background(), db, taskID, repo, []string{"1. Check"}, "", nil)
				if err == nil {
					t.Fatalf("card made for registered branch %q: %s@%s", alias, card.Branch, card.HeadSHA)
				}
				if _, gErr := shipreview.GetCard(db, taskID); !errors.Is(gErr, shipreview.ErrNoCard) {
					t.Fatalf("a card exists (err %v)", gErr)
				}
			})
		}
	})
	t.Run("dev-server", func(t *testing.T) {
		for _, alias := range []string{"Dev-Server", "DEV-SERVER"} {
			t.Run(alias, func(t *testing.T) {
				const taskID = "task-casedev"
				db := openTestDB(t)
				repo, _ := devServerRepo(t, db, taskID, true)
				gitT(t, repo, "checkout", "dev-server")
				commitIn(t, repo, "dev-later.txt", "later\n")
				gitT(t, repo, "push", "origin", "dev-server")
				gitT(t, repo, "checkout", "main")
				registerWorkProduct(t, db, taskID, "branch", alias)

				card, err := shipreview.BuildAndStartCard(context.Background(), db, taskID, repo, []string{"1. Check"}, "", nil)
				if err == nil {
					t.Fatalf("card made for registered branch %q: %s@%s", alias, card.Branch, card.HeadSHA)
				}
			})
		}
	})
}

// A registered branch that is just another name for the target's tip (a
// copy of main, or an alias the exact-ref check missed) is refused: Approve
// would merge nothing and cleanup would delete a ref equal to the target.
func TestSec245_CardRefusesBranchAtTargetOrDefaultTip(t *testing.T) {
	const taskID = "task-tipcopy"
	db := openTestDB(t)
	repo, _, _ := setupGitRepo(t)
	gitT(t, repo, "branch", "staypoint/"+taskID, "main")
	recordMainBase(t, db, repo, taskID)
	commitIn(t, repo, "later.txt", "later\n")
	gitT(t, repo, "push", "origin", "main")
	gitT(t, repo, "branch", "main-copy", "main")
	gitT(t, repo, "push", "origin", "main-copy")
	registerWorkProduct(t, db, taskID, "branch", "main-copy")

	if card, err := shipreview.BuildAndStartCard(context.Background(), db, taskID, repo, []string{"1. Check"}, "", nil); err == nil {
		t.Fatalf("card made for a branch at main's tip: %s@%s", card.Branch, card.HeadSHA)
	}

	// Same for a dev-server task whose registered branch sits at main (the
	// default branch) rather than its target.
	const devTask = "task-tipdefault"
	db2 := openTestDB(t)
	repo2, _ := devServerRepo(t, db2, devTask, true)
	gitT(t, repo2, "checkout", "main")
	commitIn(t, repo2, "main-later.txt", "x\n")
	gitT(t, repo2, "push", "origin", "main")
	gitT(t, repo2, "branch", "main-copy", "main")
	if err := workspace.RecordTaskTarget(context.Background(), db2, devTask, "dev-server"); err != nil {
		t.Fatal(err)
	}
	registerWorkProduct(t, db2, devTask, "branch", "main-copy")
	if card, err := shipreview.BuildAndStartCard(context.Background(), db2, devTask, repo2, []string{"1. Check"}, "", nil); err == nil {
		t.Fatalf("card made for a branch at the default branch's tip: %s@%s", card.Branch, card.HeadSHA)
	}
}

func TestSec245_ValidTargetBranchFollowsCheckRefFormat(t *testing.T) {
	for _, ok := range []string{"main", "dev-server", "release/2026-10", "feature/x_y", "fix/a.b", "v1.2.3", "a-b/c-d"} {
		if err := shipreview.ValidTargetBranch(ok); err != nil {
			t.Errorf("ValidTargetBranch(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{
		"HEAD", "@", "a^b", "a:b", "a*b", "a?b", "a[b", `a\b`, "a~1", "a@{1}", "a@{",
		".hidden", "a/.b", "a//b", "/a", "a.", "a.lock", "a/b.lock", "a\x01b", "a\x7fb", "-x",
		"a..b", "a b", "dev/",
	} {
		if shipreview.ValidTargetBranch(bad) == nil {
			t.Errorf("ValidTargetBranch(%q) accepted", bad)
		}
	}
}
