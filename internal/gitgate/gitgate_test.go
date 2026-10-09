package gitgate_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/gitgate"
)

// ----------------------------------------------------------------------------
// Test-repo helpers
// ----------------------------------------------------------------------------

// initRepo creates a bare temp git repository, configures a fake identity,
// and returns its path.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "git", "init", "-b", "main")
	run(t, dir, "git", "config", "user.email", "test@test.com")
	run(t, dir, "git", "config", "user.name", "Tester")
	return dir
}

// cloneRepo creates a local clone of src and wires it up with an origin remote.
// The clone's default branch is "main".
func cloneRepo(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "git", "clone", src, ".")
	run(t, dir, "git", "config", "user.email", "test@test.com")
	run(t, dir, "git", "config", "user.name", "Tester")
	return dir
}

// commit writes a file and creates a commit, returning the short SHA.
func commit(t *testing.T, dir, msg, filename string) string {
	t.Helper()
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(msg), 0644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "add", filename)
	run(t, dir, "git", "commit", "-m", msg)
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cmd %v in %s: %v\n%s", args, dir, err, out)
	}
}

func runNoFail(dir string, args ...string) (string, error) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// ----------------------------------------------------------------------------
// PreFlight tests
// ----------------------------------------------------------------------------

func TestPreFlight_CleanBranch(t *testing.T) {
	// origin: one commit on main
	origin := initRepo(t)
	commit(t, origin, "initial", "a.txt")

	clone := cloneRepo(t, origin)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r, err := gitgate.PreFlight(ctx, clone, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK {
		t.Errorf("expected OK, got errors: %v", r.Errors)
	}
}

func TestPreFlight_DirtyWorktree(t *testing.T) {
	origin := initRepo(t)
	commit(t, origin, "initial", "a.txt")
	clone := cloneRepo(t, origin)

	// Modify a tracked file without committing
	if err := os.WriteFile(filepath.Join(clone, "a.txt"), []byte("dirty"), 0644); err != nil {
		t.Fatal(err)
	}
	run(t, clone, "git", "add", "a.txt") // stage it

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r, err := gitgate.PreFlight(ctx, clone, "main")
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Error("expected FAIL on dirty worktree, got OK")
	}
	if !containsAny(r.Errors, "dirty", "uncommitted") {
		t.Errorf("unexpected error messages: %v", r.Errors)
	}
}

func TestPreFlight_BehindUpstream(t *testing.T) {
	origin := initRepo(t)
	commit(t, origin, "initial", "a.txt")
	clone := cloneRepo(t, origin)

	// Add a commit to origin that the clone doesn't have
	commit(t, origin, "origin-only", "b.txt")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r, err := gitgate.PreFlight(ctx, clone, "main")
	if err != nil {
		t.Fatal(err)
	}
	// Should succeed after fast-forward
	if !r.OK {
		t.Errorf("expected OK after fast-forward, got errors: %v", r.Errors)
	}
	if r.Behind != 1 {
		t.Errorf("expected Behind=1, got %d", r.Behind)
	}
	// Verify clone was actually updated
	head, _ := runNoFail(clone, "git", "log", "--oneline", "-1")
	if !strings.Contains(head, "origin-only") {
		t.Errorf("clone was not fast-forwarded, HEAD: %s", head)
	}
}

// A resumed task branch has a commit from an earlier run and main has moved
// on: --ff-only cannot catch it up, so PreFlight merges main in.
func TestPreFlight_DivergedBranchMerges(t *testing.T) {
	origin := initRepo(t)
	commit(t, origin, "initial", "a.txt")
	clone := cloneRepo(t, origin)

	commit(t, clone, "task work", "task.txt")
	commit(t, origin, "main moved", "b.txt")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r, err := gitgate.PreFlight(ctx, clone, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK {
		t.Fatalf("expected OK after merging main, got errors: %v", r.Errors)
	}
	if r.Ahead != 1 || r.Behind != 1 {
		t.Errorf("ahead/behind = %d/%d, want 1/1", r.Ahead, r.Behind)
	}
	for _, f := range []string{"task.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(clone, f)); err != nil {
			t.Errorf("%s missing after merge: %v", f, err)
		}
	}
	// The task commit is kept, not rewritten: a pushed branch needs no force-push.
	if _, err := runNoFail(clone, "git", "merge-base", "--is-ancestor", "origin/main", "HEAD"); err != nil {
		t.Errorf("origin/main is not an ancestor of HEAD after merge")
	}
	parents, _ := runNoFail(clone, "git", "rev-list", "--parents", "-n1", "HEAD")
	if n := len(strings.Fields(parents)); n != 3 {
		t.Errorf("HEAD should be a merge commit (2 parents), got %q", parents)
	}
}

// A diverged branch whose commits conflict with main fails with the
// conflicting files listed and the merge aborted.
func TestPreFlight_DivergedBranchConflictAborts(t *testing.T) {
	origin := initRepo(t)
	commit(t, origin, "initial", "a.txt")
	clone := cloneRepo(t, origin)

	commit(t, clone, "task edit", "a.txt")
	before, _ := runNoFail(clone, "git", "rev-parse", "HEAD")
	commit(t, origin, "main edit", "a.txt")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r, err := gitgate.PreFlight(ctx, clone, "main")
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Fatal("expected failure on conflicting merge")
	}
	if len(r.Conflicts) != 1 || r.Conflicts[0] != "a.txt" {
		t.Errorf("Conflicts = %v, want [a.txt]", r.Conflicts)
	}
	if !containsAny(r.Errors, "conflicts in: a.txt") {
		t.Errorf("error should name the conflicting file, got %v", r.Errors)
	}
	after, _ := runNoFail(clone, "git", "rev-parse", "HEAD")
	if after != before {
		t.Errorf("HEAD moved: %s -> %s", before, after)
	}
	if _, err := runNoFail(clone, "git", "rev-parse", "-q", "--verify", "MERGE_HEAD"); err == nil {
		t.Error("merge left in progress (MERGE_HEAD exists); want it aborted")
	}
	if status, _ := runNoFail(clone, "git", "status", "--porcelain"); status != "" {
		t.Errorf("worktree dirty after abort: %q", status)
	}
}

// ----------------------------------------------------------------------------
// PostFlight tests
// ----------------------------------------------------------------------------

func TestPostFlight_UncommittedChanges(t *testing.T) {
	origin := initRepo(t)
	commit(t, origin, "initial", "a.txt")
	clone := cloneRepo(t, origin)

	if err := os.WriteFile(filepath.Join(clone, "a.txt"), []byte("dirty"), 0644); err != nil {
		t.Fatal(err)
	}
	run(t, clone, "git", "add", "a.txt")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r, err := gitgate.PostFlight(ctx, clone, "main")
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Error("expected FAIL on uncommitted changes")
	}
}

func TestPostFlight_UnpushedCommits(t *testing.T) {
	origin := initRepo(t)
	commit(t, origin, "initial", "a.txt")
	clone := cloneRepo(t, origin)

	// Commit locally without pushing
	commit(t, clone, "local-only", "c.txt")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r, err := gitgate.PostFlight(ctx, clone, "main")
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Error("expected FAIL on unpushed commits")
	}
	if !containsAny(r.Errors, "unpushed") {
		t.Errorf("unexpected error messages: %v", r.Errors)
	}
}

func TestPostFlight_Clean(t *testing.T) {
	origin := initRepo(t)
	commit(t, origin, "initial", "a.txt")
	clone := cloneRepo(t, origin)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r, err := gitgate.PostFlight(ctx, clone, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !r.OK {
		t.Errorf("expected OK on clean synced clone, errors: %v", r.Errors)
	}
}

// ----------------------------------------------------------------------------
// MainContains tests
// ----------------------------------------------------------------------------

func TestMainContains_RegularMerge(t *testing.T) {
	origin := initRepo(t)
	sha1 := commit(t, origin, "first", "a.txt")
	sha2 := commit(t, origin, "second", "b.txt")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Fetch full SHAs
	full1, _ := runNoFail(origin, "git", "rev-parse", sha1)
	full2, _ := runNoFail(origin, "git", "rev-parse", sha2)

	// Set up origin/main ref by creating a remote-tracking ref
	// Since this is the bare origin, we need to simulate origin/main:
	// We'll clone and check MainContains on the clone.
	clone := cloneRepo(t, origin)

	missing, err := gitgate.MainContains(ctx, clone, full1, full2)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Errorf("expected all SHAs in main, got missing: %v", missing)
	}
}

func TestMainContains_GenuinelyUnmerged(t *testing.T) {
	origin := initRepo(t)
	commit(t, origin, "initial", "a.txt")
	clone := cloneRepo(t, origin)

	// Create a local commit that has NOT been pushed
	localSHA := commit(t, clone, "local-only", "x.txt")
	fullLocal, _ := runNoFail(clone, "git", "rev-parse", localSHA)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	missing, err := gitgate.MainContains(ctx, clone, fullLocal)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) == 0 {
		t.Error("expected local-only SHA to be missing from main")
	}
}

func TestMainContains_SquashMerged(t *testing.T) {
	// Set up origin with a squash-merged commit:
	// 1. origin has "initial" on main
	// 2. Clone adds a feature commit ("feature work")
	// 3. origin gets a squash commit with the same subject ("feature work")
	// 4. MainContains on the clone's feature commit should report it as merged

	origin := initRepo(t)
	commit(t, origin, "initial", "a.txt")
	clone := cloneRepo(t, origin)

	// Feature commit on clone (not pushed)
	featureSHA := commit(t, clone, "feature work", "feature.txt")
	fullFeature, _ := runNoFail(clone, "git", "rev-parse", featureSHA)

	// Squash-merge equivalent on origin: same subject, different SHA
	commit(t, origin, "feature work", "squashed.txt")

	// Update clone's remote refs
	run(t, clone, "git", "fetch", "origin")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	missing, err := gitgate.MainContains(ctx, clone, fullFeature)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Errorf("expected squash-equivalent SHA to be reported as merged, got missing: %v", missing)
	}
}

// TestPostFlight_PushedWithoutUpstream verifies that a branch pushed via
// `git push origin HEAD` (no -u flag) is NOT reported as having unpushed commits
// even though @{u} is unset (STA-572).
func TestPostFlight_PushedWithoutUpstream(t *testing.T) {
	// Create a bare "remote" and clone it.
	bare := t.TempDir()
	run(t, bare, "git", "init", "--bare", "-b", "main")
	clone := cloneRepo(t, bare)

	// Initial commit on main.
	commit(t, clone, "init", "README.md")
	run(t, clone, "git", "push", "origin", "main")

	// Create a feature branch, add a commit, push WITHOUT -u.
	run(t, clone, "git", "checkout", "-b", "feature/no-upstream")
	commit(t, clone, "feature work", "feature.txt")
	run(t, clone, "git", "push", "origin", "HEAD") // no -u

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := gitgate.PostFlight(ctx, clone, "main")
	if err != nil {
		t.Fatal(err)
	}

	// Should pass (no unpushed commits) even without upstream tracking.
	for _, e := range result.Errors {
		if strings.Contains(strings.ToLower(e), "unpushed") {
			t.Errorf("branch pushed without -u should not report unpushed commits; got error: %s", e)
		}
	}
}

// ----------------------------------------------------------------------------
// Helper
// ----------------------------------------------------------------------------

func containsAny(strs []string, keywords ...string) bool {
	for _, s := range strs {
		sl := strings.ToLower(s)
		for _, kw := range keywords {
			if strings.Contains(sl, strings.ToLower(kw)) {
				return true
			}
		}
	}
	return false
}
