package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The router counts every path under ~/Documents/dev/worktrees as work. For
// the Open PR default only the remote decides there (STA-717 review).
func TestShipReviewIsWorkRepo_WorktreesDirUsesRemote(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mk := func(name, remote string) string {
		t.Helper()
		dir := filepath.Join(home, "Documents", "dev", "worktrees", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", remote}} {
			if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v %s", args, err, out)
			}
		}
		return dir
	}
	personal := mk("staypoint-feature", "https://github.com/VinnyVanGogh/staypoint.git")
	work := mk("azure-intake", "https://github.com/ManagedSolution-Automation/mansol_apps_production.git")

	if shipReviewIsWorkRepo(personal) {
		t.Errorf("personal worktree %s counted as work", personal)
	}
	if !shipReviewIsWorkRepo(work) {
		t.Errorf("Managed Solution worktree %s not counted as work", work)
	}
	other := filepath.Join(home, "Documents", "dev", "work-repos", "x")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if !shipReviewIsWorkRepo(other) {
		t.Errorf("~/Documents/dev/work-repos path %s must stay work", other)
	}
}
