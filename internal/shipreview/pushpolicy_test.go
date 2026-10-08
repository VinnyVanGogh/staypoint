package shipreview_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

func commitFile(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", name)
	gitIn(t, dir, "commit", "-m", "add "+name)
	return gitIn(t, dir, "rev-parse", "HEAD")
}

// TestGetProjectPushPolicyDefaultsBranchOnly: no explicit policy (no row, a
// nil db, an empty path, a row inserted without push_policy, an empty stored
// value) reads as "branch_only" (Board decision 2026-10-08).
func TestGetProjectPushPolicyDefaultsBranchOnly(t *testing.T) {
	db := openTestDB(t)
	repo := t.TempDir()

	if got := shipreview.GetProjectPushPolicy(db, repo); got != shipreview.PushPolicyBranchOnly {
		t.Errorf("no row: got %q, want branch_only", got)
	}
	cfg, err := shipreview.GetProjectDevConfig(db, repo)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PushPolicy != shipreview.PushPolicyBranchOnly {
		t.Errorf("GetProjectDevConfig no row: got %q, want branch_only", cfg.PushPolicy)
	}
	if got := shipreview.GetProjectPushPolicy(nil, repo); got != shipreview.PushPolicyBranchOnly {
		t.Errorf("nil db: got %q, want branch_only", got)
	}
	if got := shipreview.GetProjectPushPolicy(db, ""); got != shipreview.PushPolicyBranchOnly {
		t.Errorf("empty path: got %q, want branch_only", got)
	}

	// A row created without naming push_policy takes the column default.
	if _, err := db.Exec(`INSERT INTO project_dev_configs (repo_path) VALUES (?)`, repo); err != nil {
		t.Fatal(err)
	}
	if got := shipreview.GetProjectPushPolicy(db, repo); got != shipreview.PushPolicyBranchOnly {
		t.Errorf("column default: got %q, want branch_only", got)
	}
}

// TestGetProjectPushPolicyFailsClosed: an explicit "never", an unknown stored
// value and a lookup error all read as "never".
func TestGetProjectPushPolicyFailsClosed(t *testing.T) {
	db := openTestDB(t)
	repo := t.TempDir()

	if err := shipreview.UpsertProjectDevConfig(db, &shipreview.ProjectDevConfig{RepoPath: repo, PushPolicy: shipreview.PushPolicyNever}); err != nil {
		t.Fatal(err)
	}
	if got := shipreview.GetProjectPushPolicy(db, repo); got != shipreview.PushPolicyNever {
		t.Errorf("explicit never: got %q, want never", got)
	}
	if _, err := db.Exec(`DELETE FROM project_dev_configs WHERE repo_path = ?`, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO project_dev_configs (repo_path, push_policy) VALUES (?, 'anything_goes')`, repo); err != nil {
		t.Fatal(err)
	}
	if got := shipreview.GetProjectPushPolicy(db, repo); got != shipreview.PushPolicyNever {
		t.Errorf("unknown value: got %q, want never", got)
	}
	cfg, err := shipreview.GetProjectDevConfig(db, repo)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PushPolicy != shipreview.PushPolicyNever {
		t.Errorf("GetProjectDevConfig unknown value: got %q, want never", cfg.PushPolicy)
	}

	// A lookup error (here: the table is gone) fails closed.
	if _, err := db.Exec(`DROP TABLE project_dev_configs`); err != nil {
		t.Fatal(err)
	}
	if got := shipreview.GetProjectPushPolicy(db, repo); got != shipreview.PushPolicyNever {
		t.Errorf("lookup error: got %q, want never", got)
	}
}

// TestGetProjectPushPolicyUnverifiedFailsClosed: a path that cannot be stat'ed
// cannot be ruled out as an alias of a live_credentials project, so it reads
// as "never" even though it has no row of its own. With no live project the
// same path is just unconfigured: "branch_only".
func TestGetProjectPushPolicyUnverifiedFailsClosed(t *testing.T) {
	db := openTestDB(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	if got := shipreview.GetProjectPushPolicy(db, missing); got != shipreview.PushPolicyBranchOnly {
		t.Errorf("no live project: got %q, want branch_only", got)
	}
	live := t.TempDir()
	if err := shipreview.UpsertProjectDevConfig(db, &shipreview.ProjectDevConfig{RepoPath: live, LiveCredentials: true, PushPolicy: shipreview.PushPolicyPR}); err != nil {
		t.Fatal(err)
	}
	if got := shipreview.GetProjectPushPolicy(db, missing); got != shipreview.PushPolicyNever {
		t.Errorf("unverified path: got %q, want never", got)
	}
}

// TestGetProjectPushPolicyRoundTrip: each policy survives upsert, and an
// upsert with no policy stores "branch_only".
func TestGetProjectPushPolicyRoundTrip(t *testing.T) {
	db := openTestDB(t)
	for _, p := range []shipreview.PushPolicy{shipreview.PushPolicyNever, shipreview.PushPolicyBranchOnly, shipreview.PushPolicyPR, ""} {
		repo := t.TempDir()
		if err := shipreview.UpsertProjectDevConfig(db, &shipreview.ProjectDevConfig{RepoPath: repo, PushPolicy: p}); err != nil {
			t.Fatalf("upsert %q: %v", p, err)
		}
		want := p
		if want == "" {
			want = shipreview.PushPolicyBranchOnly
		}
		if got := shipreview.GetProjectPushPolicy(db, repo); got != want {
			t.Errorf("policy %q: got %q, want %q", p, got, want)
		}
		cfgs, err := shipreview.ListProjectDevConfigs(db)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, c := range cfgs {
			if c.RepoPath == repo {
				found = true
				if c.PushPolicy != want {
					t.Errorf("list %q: got %q, want %q", p, c.PushPolicy, want)
				}
			}
		}
		if !found {
			t.Errorf("list: %s missing", repo)
		}
	}
}

// TestGetProjectPushPolicyMatchesAlias: a path reaching the configured repo
// through a symlink gets that repo's policy (STA-767 directory matching).
func TestGetProjectPushPolicyMatchesAlias(t *testing.T) {
	db := openTestDB(t)
	repo := t.TempDir()
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(repo, link); err != nil {
		t.Skip("symlink:", err)
	}
	if err := shipreview.UpsertProjectDevConfig(db, &shipreview.ProjectDevConfig{RepoPath: repo, PushPolicy: shipreview.PushPolicyBranchOnly}); err != nil {
		t.Fatal(err)
	}
	if got := shipreview.GetProjectPushPolicy(db, link); got != shipreview.PushPolicyBranchOnly {
		t.Errorf("alias: got %q, want branch_only", got)
	}
}

// TestPushBranchRefusesProtected: the Board push never touches main, master,
// dev-server, the remote default branch in any spelling, or the merge target,
// and origin's main is unchanged afterwards.
func TestPushBranchRefusesProtected(t *testing.T) {
	repoDir, _, _ := setupGitRepo(t)
	ctx := context.Background()
	mainBefore := gitIn(t, repoDir, "ls-remote", "origin", "refs/heads/main")
	mainSHA := gitIn(t, repoDir, "rev-parse", "main")

	for _, b := range []string{"main", "Main", "MAIN", "master", shipreview.WorkTargetBranch, "HEAD"} {
		err := shipreview.PushBranch(ctx, repoDir, b, "", mainSHA)
		if !errors.Is(err, shipreview.ErrProtectedBranch) {
			t.Errorf("branch %q: want ErrProtectedBranch, got %v", b, err)
		}
	}

	gitIn(t, repoDir, "checkout", "-b", "release")
	sha := commitFile(t, repoDir, "release.txt")
	gitIn(t, repoDir, "checkout", "main")
	if err := shipreview.PushBranch(ctx, repoDir, "release", "release", sha); !errors.Is(err, shipreview.ErrProtectedBranch) {
		t.Errorf("merge target: want ErrProtectedBranch, got %v", err)
	}
	if err := shipreview.PushBranch(ctx, repoDir, "-delete", "", sha); err == nil {
		t.Error("option-like branch name: want error, got nil")
	}

	if after := gitIn(t, repoDir, "ls-remote", "origin", "refs/heads/main"); after != mainBefore {
		t.Errorf("origin main changed: %q -> %q", mainBefore, after)
	}
	if out := gitIn(t, repoDir, "ls-remote", "--heads", "origin", "release"); out != "" {
		t.Errorf("merge target was pushed: %q", out)
	}
}

// TestPushBranchHeadMoved: a branch that advanced past the reviewed SHA, or
// an empty SHA, is refused and nothing is pushed.
func TestPushBranchHeadMoved(t *testing.T) {
	repoDir, branch, featureSHA := setupGitRepo(t)
	ctx := context.Background()

	gitIn(t, repoDir, "checkout", branch)
	commitFile(t, repoDir, "extra.txt")
	gitIn(t, repoDir, "checkout", "main")

	if err := shipreview.PushBranch(ctx, repoDir, branch, "", featureSHA); !errors.Is(err, shipreview.ErrHeadMoved) {
		t.Errorf("moved head: want ErrHeadMoved, got %v", err)
	}
	if err := shipreview.PushBranch(ctx, repoDir, branch, "", ""); !errors.Is(err, shipreview.ErrHeadMoved) {
		t.Errorf("empty sha: want ErrHeadMoved, got %v", err)
	}
	remote := shipreview.GetBranchRemoteInfo(ctx, repoDir, branch)
	if remote.RemoteSHA != featureSHA {
		t.Errorf("origin moved to %q, want %q", remote.RemoteSHA, featureSHA)
	}
}

// TestPushBranchRefusesDivergedRemote: the push is never forced, so a remote
// branch holding commits the reviewed head lacks is not overwritten.
func TestPushBranchRefusesDivergedRemote(t *testing.T) {
	repoDir, branch, featureSHA := setupGitRepo(t)
	ctx := context.Background()

	// Someone else pushes to origin/<branch>.
	other := filepath.Join(t.TempDir(), "other")
	remoteURL := gitIn(t, repoDir, "remote", "get-url", "origin")
	gitIn(t, filepath.Dir(other), "clone", "-q", "-b", branch, remoteURL, other)
	gitIn(t, other, "config", "user.email", "t@t.com")
	gitIn(t, other, "config", "user.name", "test")
	theirs := commitFile(t, other, "theirs.txt")
	gitIn(t, other, "push", "-q", "origin", branch)

	// Locally the branch diverges.
	gitIn(t, repoDir, "checkout", branch)
	ours := commitFile(t, repoDir, "ours.txt")
	gitIn(t, repoDir, "checkout", "main")
	if ours == featureSHA {
		t.Fatal("setup: local branch did not move")
	}

	if err := shipreview.PushBranch(ctx, repoDir, branch, "", ours); err == nil {
		t.Fatal("diverged remote: want push rejected, got nil")
	}
	if got := shipreview.GetBranchRemoteInfo(ctx, repoDir, branch).RemoteSHA; got != theirs {
		t.Errorf("remote overwritten: got %q, want %q", got, theirs)
	}
}

// TestPushBranchPushesReviewedSHA: a new task branch is pushed at exactly the
// reviewed SHA and reported as pushed.
func TestPushBranchPushesReviewedSHA(t *testing.T) {
	repoDir, _, _ := setupGitRepo(t)
	ctx := context.Background()

	gitIn(t, repoDir, "checkout", "-b", "staypoint/task-push")
	sha := commitFile(t, repoDir, "task.txt")
	gitIn(t, repoDir, "checkout", "main")

	if info := shipreview.GetBranchRemoteInfo(ctx, repoDir, "staypoint/task-push"); info.Pushed {
		t.Fatalf("branch on origin before push: %+v", info)
	}
	if err := shipreview.PushBranch(ctx, repoDir, "staypoint/task-push", "main", sha); err != nil {
		t.Fatalf("push: %v", err)
	}
	info := shipreview.GetBranchRemoteInfo(ctx, repoDir, "staypoint/task-push")
	if !info.Pushed || info.RemoteSHA != sha {
		t.Errorf("after push: got %+v, want pushed at %s", info, sha)
	}
}
