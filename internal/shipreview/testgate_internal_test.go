package shipreview

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/testgate"
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
	_, _, err := ChangeForGate(context.Background(), dir, head, "")
	if err == nil || !strings.Contains(err.Error(), "no merge-base with main") || strings.Contains(err.Error(), "master") {
		t.Fatalf("err = %v, want main's merge-base error", err)
	}
}

func TestChangeForGateFallsBackToMaster(t *testing.T) {
	dir, head := gateRepo(t, "master")
	if _, _, err := ChangeForGate(context.Background(), dir, head, ""); err != nil {
		t.Fatalf("master repo: %v", err)
	}
}

// STA-791: a deleted file needs no new test. A delete-only change has no
// sources and no warnings; a rename still counts its new path.
func TestChangeForGateSkipsDeletedFiles(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(p, body string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	write("migrations/005_to_delete.sql", "SELECT 1;\n")
	write("old.go", "package x\n")
	write("moved.go", "package x\n\nfunc Moved() {}\n")
	run("add", "-A")
	run("commit", "-q", "-m", "base")

	run("checkout", "-q", "-b", "delete-only")
	run("rm", "-q", "migrations/005_to_delete.sql", "old.go")
	run("commit", "-q", "-m", "delete")
	files, lines, err := ChangeForGate(context.Background(), dir, run("rev-parse", "HEAD"), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("delete-only change: files=%v, want none", files)
	}
	r := testgate.Evaluate(testgate.Input{Files: files, ChangedLines: lines})
	if r.Blocking || len(r.Warnings) != 0 || len(r.Sources) != 0 {
		t.Fatalf("delete-only change: blocking=%v warnings=%v sources=%v", r.Blocking, r.Warnings, r.Sources)
	}

	run("checkout", "-q", "-b", "rename", "main")
	run("mv", "moved.go", "renamed.go")
	run("commit", "-q", "-m", "rename")
	files, _, err = ChangeForGate(context.Background(), dir, run("rev-parse", "HEAD"), "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(files, ",") != "renamed.go" {
		t.Fatalf("rename: files=%v, want [renamed.go]", files)
	}
}
