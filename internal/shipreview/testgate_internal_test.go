package shipreview

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func gateRepo(t *testing.T, branch string) (dir, head string) {
	t.Helper()
	dir = t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", branch)
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("commit", "-q", "--allow-empty", "-m", "base")
	run("checkout", "-q", "-b", "feature")
	run("commit", "-q", "--allow-empty", "-m", "change")
	return dir, run("rev-parse", "HEAD")
}

// STA-766 review nit: with neither main nor master, the error must say why
// main failed, not master's fallback "unknown revision".
func TestChangeForGateReportsMainsError(t *testing.T) {
	dir, head := gateRepo(t, "trunk")
	_, _, err := ChangeForGate(context.Background(), dir, head)
	if err == nil || !strings.Contains(err.Error(), "no merge-base with main") || strings.Contains(err.Error(), "master") {
		t.Fatalf("err = %v, want main's merge-base error", err)
	}
}

func TestChangeForGateFallsBackToMaster(t *testing.T) {
	dir, head := gateRepo(t, "master")
	if _, _, err := ChangeForGate(context.Background(), dir, head); err != nil {
		t.Fatalf("master repo: %v", err)
	}
}
