package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// STA-856 end to end: wireOnWake -> harness -> routed adapter -> a fake agy that
// edits files in the run's worktree. The harness guard must revert code edits
// in a work repo and fail the run, and leave docs and personal repos alone.

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// guardWorktree is a git repo standing in for the run's worktree.
func guardWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	gitT(t, dir, "config", "user.name", "t")
	gitT(t, dir, "config", "user.email", "t@example.com")
	gitT(t, dir, "config", "commit.gpgsign", "false")
	for name, body := range map[string]string{"main.go": "package main\n", "README.md": "# app\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "init")
	return dir
}

// editingCLI is a fake claude/agy that logs its spawn and then runs edit (a
// shell snippet) in its working directory.
func editingCLI(t *testing.T, edit string) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "spawns.log")
	bin = filepath.Join(dir, "fakecli.sh")
	script := "#!/bin/sh\nprintf '%s|%s\\n' \"${CLAUDE_CONFIG_DIR}\" \"$(echo \"$*\" | tr '\\n' ' ')\" >> \"" + logPath + "\"\n" +
		edit + "\necho 'done'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logPath
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWake_WorkRepoGeminiCodeEditIsRevertedAndRunFails(t *testing.T) {
	wt := guardWorktree(t)
	bin, logPath := editingCLI(t, "echo '// gemini' >> main.go; echo 'package x' > extra.go")
	r := runWakeIn(t, workRepo(t), "planning", openPacer(), bin, logPath, wt)

	_, args := mustOneSpawn(t, r)
	if isClaude(args) {
		t.Fatalf("spawned %q, want agy", args)
	}
	if got := readFile(t, filepath.Join(wt, "main.go")); got != "package main\n" {
		t.Errorf("main.go = %q, want reverted", got)
	}
	if _, err := os.Stat(filepath.Join(wt, "extra.go")); !os.IsNotExist(err) {
		t.Errorf("extra.go left in the worktree: %v", err)
	}
	want := "Blocked: Gemini edited code in a work repo: extra.go, main.go (reverted)"
	if len(r.messages) != 1 || r.messages[0] != want {
		t.Errorf("error rows = %q, want [%q]", r.messages, want)
	}
	if r.stage != "error" {
		t.Errorf("execution_stage = %q, want error", r.stage)
	}
}

func TestWake_WorkRepoGeminiDocEditsAreAllowed(t *testing.T) {
	wt := guardWorktree(t)
	bin, logPath := editingCLI(t, "mkdir -p docs; echo '# x' > docs/x.md; echo 'more' >> README.md")
	r := runWakeIn(t, workRepo(t), "planning", openPacer(), bin, logPath, wt)

	if len(r.spawns) == 0 || isClaude(r.spawns[0]) {
		t.Fatalf("want an agy spawn, got %q", r.spawns)
	}
	for _, m := range r.messages {
		if strings.HasPrefix(m, "Blocked:") {
			t.Fatalf("doc-only turn blocked: %q", m)
		}
	}
	if got := readFile(t, filepath.Join(wt, "docs/x.md")); got != "# x\n" {
		t.Errorf("docs/x.md = %q", got)
	}
	if got := readFile(t, filepath.Join(wt, "README.md")); got != "# app\nmore\n" {
		t.Errorf("README.md = %q", got)
	}
}

func TestWake_PersonalRepoGeminiMayEditCode(t *testing.T) {
	wt := guardWorktree(t)
	bin, logPath := editingCLI(t, "echo '// gemini' >> main.go")
	r := runWakeIn(t, personalRepo(t), "planning", openPacer(), bin, logPath, wt)

	if len(r.spawns) == 0 || isClaude(r.spawns[0]) {
		t.Fatalf("want an agy spawn, got %q", r.spawns)
	}
	if got := readFile(t, filepath.Join(wt, "main.go")); got != "package main\n// gemini\n" {
		t.Errorf("personal repo edit reverted: main.go = %q", got)
	}
	for _, m := range r.messages {
		if strings.HasPrefix(m, "Blocked:") {
			t.Fatalf("personal repo turn blocked: %q", m)
		}
	}
}

// A Claude turn in a work repo is not subject to the Gemini guard.
func TestWake_WorkRepoClaudeMayEditCode(t *testing.T) {
	wt := guardWorktree(t)
	bin, logPath := editingCLI(t, "echo '// claude' >> main.go")
	r := runWakeIn(t, workRepo(t), "coding", openPacer(), bin, logPath, wt)

	if len(r.spawns) == 0 || !isClaude(r.spawns[0]) {
		t.Fatalf("want a claude spawn, got %q", r.spawns)
	}
	if got := readFile(t, filepath.Join(wt, "main.go")); got != "package main\n// claude\n" {
		t.Errorf("Claude edit reverted: main.go = %q", got)
	}
}
