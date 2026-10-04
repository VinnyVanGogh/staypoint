package shipreview

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// STA-649: the remote delete is leased to the tip CleanupMergedBranch checked,
// so a push that lands after the check is never discarded.
func TestDeleteRemoteBranchLeasedRefusesMovedTip(t *testing.T) {
	base := t.TempDir()
	bare := filepath.Join(base, "bare.git")
	work := filepath.Join(base, "work")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(base, "init", "-q", "--bare", "-b", "main", bare)
	git(base, "init", "-q", "-b", "main", work)
	git(work, "commit", "-q", "--allow-empty", "-m", "init")
	git(work, "remote", "add", "origin", bare)
	git(work, "push", "-q", "origin", "main", "main:refs/heads/feature")
	checked := git(work, "rev-parse", "HEAD")

	// The branch moves on the remote after the check.
	git(work, "commit", "-q", "--allow-empty", "-m", "late")
	moved := git(work, "rev-parse", "HEAD")
	git(work, "push", "-q", "origin", "HEAD:refs/heads/feature")

	if err := deleteRemoteBranchLeased(context.Background(), work, "feature", checked); err == nil {
		t.Fatal("leased delete succeeded although the remote tip moved")
	}
	if got := git(bare, "rev-parse", "refs/heads/feature"); got != moved {
		t.Fatalf("remote feature = %s, want untouched %s", got, moved)
	}

	if err := deleteRemoteBranchLeased(context.Background(), work, "feature", moved); err != nil {
		t.Fatalf("leased delete at the current tip: %v", err)
	}
	if out := git(bare, "branch", "--list", "feature"); out != "" {
		t.Fatalf("feature still on remote: %q", out)
	}
}
