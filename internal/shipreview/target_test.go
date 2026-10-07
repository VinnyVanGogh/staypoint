package shipreview_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
	"github.com/VinnyVanGogh/staypoint/internal/workspace"
)

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitIn(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", name)
	gitT(t, dir, "commit", "-m", "add "+name)
	return gitOut(t, dir, "rev-parse", "HEAD")
}

func registerWorkProduct(t *testing.T, db *sql.DB, taskID, typ, ref string) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS task_work_products (
		id INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT NOT NULL, product_type TEXT NOT NULL,
		reference TEXT NOT NULL, created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO task_work_products (task_id, product_type, reference) VALUES (?, ?, ?)`, taskID, typ, ref); err != nil {
		t.Fatal(err)
	}
}

// devServerRepo is the 2026-10-07 shape: origin has main and dev-server
// (ahead of main), the task branch staypoint/<task> has no commits, and the
// agent's work is on fix/user-lifecycle-dev-wiring cut from dev-server.
// The task's base and target are recorded as targetBase / target.
func devServerRepo(t *testing.T, db *sql.DB, taskID string, recordDevServer bool) (repo, fixSHA string) {
	t.Helper()
	repo, _, _ = setupGitRepo(t)
	gitT(t, repo, "checkout", "-b", "dev-server", "main")
	commitIn(t, repo, "dev-only.txt", "dev\n")
	gitT(t, repo, "push", "origin", "dev-server")
	gitT(t, repo, "checkout", "-b", "fix/user-lifecycle-dev-wiring", "dev-server")
	fixSHA = commitIn(t, repo, "lifecycle.go", "package x\n")
	gitT(t, repo, "push", "origin", "fix/user-lifecycle-dev-wiring")
	gitT(t, repo, "checkout", "main")

	ensureTaskBaseTable(t, db)
	ctx := context.Background()
	if recordDevServer {
		base := gitOut(t, repo, "rev-parse", "dev-server")
		gitT(t, repo, "branch", "staypoint/"+taskID, base)
		if err := workspace.RecordTaskBase(ctx, db, repo, taskID, base); err != nil {
			t.Fatal(err)
		}
		if err := workspace.RecordTaskTarget(ctx, db, taskID, "dev-server"); err != nil {
			t.Fatal(err)
		}
	} else {
		gitT(t, repo, "branch", "staypoint/"+taskID, "main")
		recordMainBase(t, db, repo, taskID)
	}
	return repo, fixSHA
}

// A dev-server task whose work is on a registered branch gets a card for
// that branch, measured against the task's base, merging into dev-server.
// Approve merges it into dev-server and leaves main alone.
func TestBuildAndStartCard_DevServerWorkProductBranch(t *testing.T) {
	const taskID = "task-0cdd4de2"
	db := openTestDB(t)
	repo, fixSHA := devServerRepo(t, db, taskID, true)
	registerWorkProduct(t, db, taskID, "branch", "fix/user-lifecycle-dev-wiring")
	mainBefore := gitOut(t, repo, "rev-parse", "origin/main")

	ctx := context.Background()
	card, err := shipreview.BuildAndStartCard(ctx, db, taskID, repo, []string{"1. Check"}, "", nil)
	if err != nil {
		t.Fatalf("BuildAndStartCard: %v", err)
	}
	if card.Branch != "fix/user-lifecycle-dev-wiring" || card.HeadSHA != fixSHA {
		t.Fatalf("card = %s@%s, want fix/user-lifecycle-dev-wiring@%s", card.Branch, card.HeadSHA, fixSHA)
	}
	if card.TargetBranch != "dev-server" {
		t.Fatalf("target = %q, want dev-server", card.TargetBranch)
	}
	if !reflect.DeepEqual(card.FilesChanged, []string{"lifecycle.go"}) {
		t.Fatalf("files = %v, want [lifecycle.go]", card.FilesChanged)
	}

	merged, err := shipreview.ApproveAndMerge(ctx, db, card, repo, card.TargetBranch)
	if err != nil {
		t.Fatalf("ApproveAndMerge: %v", err)
	}
	if got := gitOut(t, repo, "rev-parse", "origin/dev-server"); got != merged {
		t.Fatalf("origin/dev-server = %s, want merge %s", got, merged)
	}
	gitT(t, repo, "merge-base", "--is-ancestor", fixSHA, "origin/dev-server")
	if got := gitOut(t, repo, "rev-parse", "origin/main"); got != mainBefore {
		t.Fatalf("origin/main moved to %s; Approve must not touch main", got)
	}

	// Cleanup deletes the registered branch, never dev-server, and leaves
	// the task's own worktree branch alone.
	if err := shipreview.CleanupMergedBranch(ctx, repo, card, merged); err != nil {
		t.Fatalf("CleanupMergedBranch: %v", err)
	}
	if out, _ := exec.Command("git", "-C", repo, "ls-remote", "--heads", "origin", "fix/user-lifecycle-dev-wiring").Output(); len(strings.TrimSpace(string(out))) != 0 {
		t.Fatalf("registered branch still on origin: %s", out)
	}
	gitT(t, repo, "rev-parse", "--verify", "refs/remotes/origin/dev-server")
}

// The incident as it happened: a task cut from main before targets were
// recorded, in a work repo with dev-server. The card is made (over-reporting
// the dev-server delta is safe; hiding is not) and targets dev-server.
func TestBuildAndStartCard_LegacyMainBaseWorkRepo(t *testing.T) {
	const taskID = "task-legacy"
	db := openTestDB(t)
	repo, _ := devServerRepo(t, db, taskID, false)
	registerWorkProduct(t, db, taskID, "branch", "origin/fix/user-lifecycle-dev-wiring")
	prev := shipreview.IsWorkRepo
	shipreview.IsWorkRepo = func(string) bool { return true }
	t.Cleanup(func() { shipreview.IsWorkRepo = prev })

	card, err := shipreview.BuildAndStartCard(context.Background(), db, taskID, repo, []string{"1. Check"}, "", nil)
	if err != nil {
		t.Fatalf("BuildAndStartCard: %v", err)
	}
	if card.TargetBranch != "dev-server" {
		t.Fatalf("target = %q, want dev-server (never main for a work repo with dev-server)", card.TargetBranch)
	}
	if !reflect.DeepEqual(card.FilesChanged, []string{"dev-only.txt", "lifecycle.go"}) {
		t.Fatalf("files = %v, want both the dev-server delta and the fix", card.FilesChanged)
	}
}

// The task's own branch wins when it has changes, even with a registered
// branch.
func TestBuildAndStartCard_TaskBranchWinsWhenChanged(t *testing.T) {
	const taskID = "task-own"
	db := openTestDB(t)
	repo, _ := devServerRepo(t, db, taskID, true)
	registerWorkProduct(t, db, taskID, "branch", "fix/user-lifecycle-dev-wiring")
	gitT(t, repo, "checkout", "staypoint/"+taskID)
	own := commitIn(t, repo, "own.txt", "own\n")
	gitT(t, repo, "checkout", "main")

	card, err := shipreview.BuildAndStartCard(context.Background(), db, taskID, repo, []string{"1. Check"}, "", nil)
	if err != nil {
		t.Fatalf("BuildAndStartCard: %v", err)
	}
	if card.Branch != "staypoint/"+taskID || card.HeadSHA != own || card.TargetBranch != "dev-server" {
		t.Fatalf("card = %s@%s → %s, want staypoint/%s@%s → dev-server", card.Branch, card.HeadSHA, card.TargetBranch, taskID, own)
	}
}

// Registered branches a card must never ship or delete are refused.
func TestBuildAndStartCard_RefusesProtectedWorkProductBranch(t *testing.T) {
	for _, ref := range []string{"main", "dev-server", "origin/dev-server", "staypoint/task-other", "--delete"} {
		t.Run(ref, func(t *testing.T) {
			const taskID = "task-protected"
			db := openTestDB(t)
			repo, _ := devServerRepo(t, db, taskID, true)
			gitT(t, repo, "branch", "staypoint/task-other", "fix/user-lifecycle-dev-wiring")
			registerWorkProduct(t, db, taskID, "branch", ref)
			_, err := shipreview.BuildAndStartCard(context.Background(), db, taskID, repo, []string{"1. Check"}, "", nil)
			if err == nil {
				t.Fatalf("card made for registered branch %q", ref)
			}
			if _, gErr := shipreview.GetCard(db, taskID); !errors.Is(gErr, shipreview.ErrNoCard) {
				t.Fatalf("a card exists (err %v)", gErr)
			}
		})
	}
}

// Approve never merges into an unnamed or invalid target.
func TestApproveAndMerge_RequiresTarget(t *testing.T) {
	db := openTestDB(t)
	repo, branch, sha := setupGitRepo(t)
	card := &shipreview.Card{ID: "c", TaskID: "t", Branch: branch, HeadSHA: sha}
	for _, target := range []string{"", "-x", "refs/heads/main", branch} {
		if _, err := shipreview.ApproveAndMerge(context.Background(), db, card, repo, target); err == nil {
			t.Fatalf("ApproveAndMerge into %q succeeded", target)
		}
	}
}

func TestValidTargetBranch(t *testing.T) {
	for _, ok := range []string{"", "main", "dev-server", "release/2026-10"} {
		if err := shipreview.ValidTargetBranch(ok); err != nil {
			t.Errorf("ValidTargetBranch(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"-x", "refs/heads/dev", "origin/dev-server", "staypoint/task-1", "a..b", "a b", "dev/"} {
		if shipreview.ValidTargetBranch(bad) == nil {
			t.Errorf("ValidTargetBranch(%q) accepted", bad)
		}
	}
}

// The configured target wins; a work repo with origin/dev-server defaults to
// it; anything else is the default branch.
func TestProjectTargetBranch(t *testing.T) {
	db := openTestDB(t)
	repo, _ := devServerRepo(t, db, "task-cfg", true)
	ctx := context.Background()
	prev := shipreview.IsWorkRepo
	t.Cleanup(func() { shipreview.IsWorkRepo = prev })

	shipreview.IsWorkRepo = func(string) bool { return false }
	if got, err := shipreview.ProjectTargetBranch(ctx, db, repo); err != nil || got != "" {
		t.Fatalf("personal repo target = %q (%v), want default", got, err)
	}
	shipreview.IsWorkRepo = func(string) bool { return true }
	if got, err := shipreview.ProjectTargetBranch(ctx, db, repo); err != nil || got != "dev-server" {
		t.Fatalf("work repo target = %q (%v), want dev-server", got, err)
	}
	if err := shipreview.UpsertProjectDevConfig(db, &shipreview.ProjectDevConfig{RepoPath: repo, TargetBranch: "release"}); err != nil {
		t.Fatal(err)
	}
	if got, err := shipreview.ProjectTargetBranch(ctx, db, repo); err != nil || got != "release" {
		t.Fatalf("configured target = %q (%v), want release", got, err)
	}
}

// A card whose target is not the branch the task was cut from lists what
// the merge would land there, not only the diff against the base: work cut
// from dev-server but merging into main also lands dev-server's commits.
func TestBuildAndStartCard_ListsWhatLandsOnTarget(t *testing.T) {
	const taskID = "task-landing"
	db := openTestDB(t)
	repo, _ := devServerRepo(t, db, taskID, true)
	if err := workspace.RecordTaskTarget(context.Background(), db, taskID, "main"); err != nil {
		t.Fatal(err)
	}
	registerWorkProduct(t, db, taskID, "branch", "fix/user-lifecycle-dev-wiring")

	card, err := shipreview.BuildAndStartCard(context.Background(), db, taskID, repo, []string{"1. Check"}, "", nil)
	if err != nil {
		t.Fatalf("BuildAndStartCard: %v", err)
	}
	if card.TargetBranch != "main" {
		t.Fatalf("target = %q, want main", card.TargetBranch)
	}
	if !reflect.DeepEqual(card.FilesChanged, []string{"dev-only.txt", "lifecycle.go"}) {
		t.Fatalf("files = %v, want dev-only.txt (lands on main) and lifecycle.go", card.FilesChanged)
	}
}

// A registered branch that is the project's configured target is refused
// even when the task itself targets another branch: Approve would delete it.
func TestBuildAndStartCard_RefusesProjectTargetAsWorkProduct(t *testing.T) {
	const taskID = "task-release"
	db := openTestDB(t)
	repo, _ := devServerRepo(t, db, taskID, true)
	gitT(t, repo, "branch", "release", "fix/user-lifecycle-dev-wiring")
	if err := shipreview.UpsertProjectDevConfig(db, &shipreview.ProjectDevConfig{RepoPath: repo, TargetBranch: "release"}); err != nil {
		t.Fatal(err)
	}
	registerWorkProduct(t, db, taskID, "branch", "release")
	_, err := shipreview.BuildAndStartCard(context.Background(), db, taskID, repo, []string{"1. Check"}, "", nil)
	if !errors.Is(err, shipreview.ErrProtectedBranch) {
		t.Fatalf("err = %v, want ErrProtectedBranch", err)
	}
}
