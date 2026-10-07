package geminiguard

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/checkpoint"
)

func TestIsDocPath(t *testing.T) {
	cases := map[string]bool{
		"README.md":               true,
		"docs/x.md":               true,
		"docs/diagram.png":        true,
		"doc/api/thing.go":        false,
		".staypoint/plan.json":    true,
		"docs/guide.pdf":          true,
		"deck.pptx":               true,
		"specs/report.docx":       true,
		"talk.key":                true,
		"notes.odt":               true,
		"slides.odp":              true,
		"docs/mkdocs.yml":         false,
		"config.yaml":             false,
		"app.json":                false,
		"pyproject.toml":          false,
		".env":                    false,
		".env.local":              false,
		"setup.ini":               false,
		"db/migrations/001.sql":   false,
		"scripts/deploy.sh":       false,
		"Dockerfile":              false,
		"docs/Dockerfile":         false,
		".github/README.md":       false,
		"main_test.go":            false,
		"analysis.ipynb":          false,
		"requirements.txt":        false,
		"requirements-dev.txt":    false,
		"CMakeLists.txt":          false,
		"notes/CHANGELOG.MDX":     true,
		"a/b/guide.rst":           true,
		"manual.adoc":             true,
		"LICENSE.txt":             true,
		"main.go":                 false,
		"src/docs/x.go":           false,
		"docs":                    false,
		"Makefile":                false,
		".github/workflows/x.yml": false,
		"../docs/x.md":            false,
		"/abs/README.md":          false,
		".git/config":             false,
		"docs/.git/x.md":          false,
		"":                        false,
	}
	for p, want := range cases {
		if got := IsDocPath(p); got != want {
			t.Errorf("IsDocPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// repo builds a git repo with main.go and README.md committed, plus an
// uncommitted edit from an earlier (Claude) turn, and returns a pre-turn
// snapshot backed by a daemon checkpoint.
func repo(t *testing.T) (string, *Snapshot) {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "config", "user.name", "t")
	run(t, dir, "config", "user.email", "t@example.com")
	write(t, dir, "main.go", "package main\n")
	write(t, dir, "README.md", "# app\n")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "init")
	write(t, dir, "util.go", "package main // earlier turn\n")
	return dir, snap(t, dir)
}

func snap(t *testing.T, dir string) *Snapshot {
	t.Helper()
	ctx := context.Background()
	cp, err := checkpoint.CreateCheckpoint(ctx, checkpoint.CreateOptions{WorkDir: dir, SessionID: "run-test", Message: "turn"})
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	s, err := Take(ctx, dir, cp.CommitSHA)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	return s
}

func TestEnforce_CodeEditIsRevertedAndBlocked(t *testing.T) {
	dir, pre := repo(t)
	write(t, dir, "main.go", "package main\n// gemini was here\n")
	write(t, dir, "util.go", "package main // gemini rewrote\n")
	res := Enforce(context.Background(), pre)
	if !res.Violated() || strings.Join(res.Blocked, ",") != "main.go,util.go" {
		t.Fatalf("blocked = %v err = %v", res.Blocked, res.Err)
	}
	if res.Err != nil || len(res.Unresolved) != 0 {
		t.Fatalf("revert failed: %v %v", res.Unresolved, res.Err)
	}
	if got := read(t, dir, "main.go"); got != "package main\n" {
		t.Errorf("main.go not reverted: %q", got)
	}
	// Restores to the pre-turn checkpoint, not HEAD: the earlier turn's edit survives.
	if got := read(t, dir, "util.go"); got != "package main // earlier turn\n" {
		t.Errorf("util.go = %q, want the pre-turn content", got)
	}
	want := "Blocked: Gemini edited code: main.go, util.go (reverted)"
	if res.Title() != want {
		t.Errorf("title = %q, want %q", res.Title(), want)
	}
}

func TestEnforce_DocsOnlyTurnIsAllowed(t *testing.T) {
	dir, pre := repo(t)
	write(t, dir, "docs/x.md", "# design\n")
	write(t, dir, "README.md", "# app\n\nmore\n")
	write(t, dir, ".staypoint/plan.md", "plan\n")
	res := Enforce(context.Background(), pre)
	if res.Violated() {
		t.Fatalf("doc-only turn blocked: %v %v", res.Blocked, res.Err)
	}
	if read(t, dir, "docs/x.md") != "# design\n" || read(t, dir, "README.md") != "# app\n\nmore\n" {
		t.Error("doc edits were not kept")
	}
}

func TestEnforce_NewCodeFileIsDeleted(t *testing.T) {
	dir, pre := repo(t)
	write(t, dir, "pkg/new.go", "package pkg\n")
	write(t, dir, "docs/ok.md", "fine\n")
	res := Enforce(context.Background(), pre)
	if strings.Join(res.Blocked, ",") != "pkg/new.go" || res.Err != nil {
		t.Fatalf("blocked = %v err = %v", res.Blocked, res.Err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pkg/new.go")); !os.IsNotExist(err) {
		t.Errorf("new code file left behind: %v", err)
	}
	if read(t, dir, "docs/ok.md") != "fine\n" {
		t.Error("doc file alongside the violation was removed")
	}
}

func TestEnforce_DeletedCodeFileIsRestored(t *testing.T) {
	dir, pre := repo(t)
	_ = os.Remove(filepath.Join(dir, "main.go"))
	res := Enforce(context.Background(), pre)
	if strings.Join(res.Blocked, ",") != "main.go" || res.Err != nil {
		t.Fatalf("blocked = %v err = %v", res.Blocked, res.Err)
	}
	if read(t, dir, "main.go") != "package main\n" {
		t.Error("deleted file not restored")
	}
}

func TestEnforce_TouchWithoutChangeIsAllowed(t *testing.T) {
	dir, pre := repo(t)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "main.go"), later, later); err != nil {
		t.Fatal(err)
	}
	if res := Enforce(context.Background(), pre); res.Violated() {
		t.Fatalf("touch flagged: %v %v", res.Blocked, res.Err)
	}
}

// A commit made during the turn is rolled back even after the worktree is
// reset to look untouched: the branch, not the worktree, is what ships.
func TestEnforce_CommittedCodeIsRolledBack(t *testing.T) {
	dir, pre := repo(t)
	head := run(t, dir, "rev-parse", "HEAD")
	write(t, dir, "main.go", "package main\n// sneaky\n")
	run(t, dir, "commit", "-q", "-am", "gemini")
	run(t, dir, "show", "HEAD~1:main.go") // sanity
	write(t, dir, "main.go", "package main\n")
	res := Enforce(context.Background(), pre)
	if strings.Join(res.Blocked, ",") != "main.go" || res.Err != nil {
		t.Fatalf("blocked = %v err = %v", res.Blocked, res.Err)
	}
	if got := run(t, dir, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD = %s, want rolled back to %s", got, head)
	}
}

// Ignore rules live in agent-writable files; a file hidden by them is still seen.
func TestEnforce_IgnoredCodeFileIsStillSeen(t *testing.T) {
	dir, pre := repo(t)
	write(t, dir, ".git/info/exclude", "*.go\n")
	write(t, dir, "hidden.go", "package main\n")
	res := Enforce(context.Background(), pre)
	if strings.Join(res.Blocked, ",") != "hidden.go" {
		t.Fatalf("blocked = %v err = %v", res.Blocked, res.Err)
	}
	if _, err := os.Stat(filepath.Join(dir, "hidden.go")); !os.IsNotExist(err) {
		t.Errorf("ignored code file left behind: %v", err)
	}
}

// Moving the checkpoint refs cannot change what the guard compares against:
// it holds the checkpoint SHA in memory.
func TestEnforce_IgnoresAgentMovedCheckpointRefs(t *testing.T) {
	dir, pre := repo(t)
	write(t, dir, "main.go", "package main\n// evil\n")
	run(t, dir, "add", "-A")
	evil := run(t, dir, "write-tree")
	c := run(t, dir, "commit-tree", evil, "-m", "fake checkpoint")
	run(t, dir, "update-ref", "refs/staypoint/checkpoints/latest", c)
	res := Enforce(context.Background(), pre)
	if strings.Join(res.Blocked, ",") != "main.go" {
		t.Fatalf("blocked = %v err = %v", res.Blocked, res.Err)
	}
	if read(t, dir, "main.go") != "package main\n" {
		t.Error("main.go not restored to the daemon's checkpoint")
	}
}

func TestEnforce_NoGitRepoFailsClosedOnCode(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "notes.md", "a\n")
	pre, err := Take(context.Background(), dir, "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, dir, "x.go", "package x\n")
	write(t, dir, "notes.md", "b\n")
	res := Enforce(context.Background(), pre)
	if strings.Join(res.Blocked, ",") != "x.go" {
		t.Fatalf("blocked = %v", res.Blocked)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.go")); !os.IsNotExist(err) {
		t.Error("x.go not removed")
	}
}
