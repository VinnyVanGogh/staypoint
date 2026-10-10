package shipreview_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
	"github.com/VinnyVanGogh/staypoint/internal/testgate"
	"github.com/VinnyVanGogh/staypoint/internal/workspace"
)

// staleMainRepo builds the repo the test gate saw on 2026-10-09: the repo
// root's local main is stale, origin/main moved on with unrelated commits, and
// the task branch was cut from origin/main with two commits of its own.
func staleMainRepo(t *testing.T) (dir, originMain, head string) {
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
	commit := func(p, body, msg string) {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", p)
		run("commit", "-q", "-m", msg)
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	commit("README.md", "x\n", "base")
	// origin/main gains unrelated work; local main never pulls it.
	run("checkout", "-q", "-b", "upstream")
	commit("internal/paperclip/import_api.go", "package paperclip\n", "unrelated 1")
	commit("internal/checklist/seed.go", "package checklist\n", "unrelated 2")
	originMain = run("rev-parse", "HEAD")
	run("update-ref", "refs/remotes/origin/main", originMain)
	run("checkout", "-q", "-b", "staypoint/task-x", originMain)
	commit("internal/feat/feat.go", "package feat\n", "feat")
	commit("internal/feat/feat_test.go", "package feat\n", "feat test")
	head = run("rev-parse", "HEAD")
	run("branch", "-q", "-D", "upstream")
	return dir, originMain, head
}

var twoCommitFiles = []string{"internal/feat/feat.go", "internal/feat/feat_test.go"}

// task-a5c42165: a stale local main made the gate judge every commit since
// it (213 test files, unrelated sources). origin/main is the base.
func TestChangeForGatePrefersOriginOverStaleMain(t *testing.T) {
	dir, _, head := staleMainRepo(t)
	files, _, err := shipreview.ChangeForGate(context.Background(), dir, head, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(files, twoCommitFiles) {
		t.Fatalf("files = %v, want only the branch's %v", files, twoCommitFiles)
	}
}

// The card's recorded task base is what the gate measures from, so it agrees
// with "Files changed".
func TestChangeForCardUsesRecordedBase(t *testing.T) {
	dir, originMain, head := staleMainRepo(t)
	db := openTestDB(t)
	ensureTaskBaseTable(t, db)
	ctx := context.Background()
	if err := workspace.RecordTaskBase(ctx, db, dir, "task-a1b2c3d4", originMain); err != nil {
		t.Fatal(err)
	}
	// Even with origin/main gone, the recorded base gives the right change.
	if out, err := exec.Command("git", "-C", dir, "update-ref", "-d", "refs/remotes/origin/main").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	card := &shipreview.Card{TaskID: "task-a1b2c3d4", HeadSHA: head}
	files, _, err := shipreview.ChangeForCard(ctx, db, dir, card)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(files, twoCommitFiles) {
		t.Fatalf("files = %v, want %v", files, twoCommitFiles)
	}

	// A moved pin is tampering: fail closed, never fall back to a guess.
	if out, err := exec.Command("git", "-C", dir, "update-ref", workspace.BaseRef("task-a1b2c3d4"), head).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if _, _, err := shipreview.ChangeForCard(ctx, db, dir, card); !errors.Is(err, workspace.ErrTaskBaseTampered) {
		t.Fatalf("tampered base: err = %v, want ErrTaskBaseTampered", err)
	}
}

// A task with no recorded base falls back to the merge-base with origin/main.
func TestChangeForCardFallsBackWithoutRecordedBase(t *testing.T) {
	dir, _, head := staleMainRepo(t)
	db := openTestDB(t)
	ensureTaskBaseTable(t, db)
	files, _, err := shipreview.ChangeForCard(context.Background(), db, dir, &shipreview.Card{TaskID: "task-0000beef", HeadSHA: head})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(files, twoCommitFiles) {
		t.Fatalf("files = %v, want %v", files, twoCommitFiles)
	}
}

// ScriptsAt reads the scripts a workflow calls, and theirs, at the head, so
// the gate sees `go test` inside scripts/*.sh.
func TestScriptsAtFollowsWorkflowScripts(t *testing.T) {
	dir := t.TempDir()
	write := func(p, body string) {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@t")
	git("config", "user.name", "t")
	write(".github/workflows/ci.yml", "jobs:\n  t:\n    steps:\n      - run: scripts/ci.sh\n      - run: scripts/missing.sh\n")
	write("scripts/ci.sh", "#!/bin/sh\n./scripts/unit.sh -race\n")
	write("scripts/unit.sh", "#!/bin/sh\ngo test \"$@\" ./...\n")
	git("add", "-A")
	git("commit", "-q", "-m", "ci")
	head := git("rev-parse", "HEAD")

	ctx := context.Background()
	wf, err := shipreview.WorkflowsAt(ctx, dir, head)
	if err != nil {
		t.Fatal(err)
	}
	scripts := shipreview.ScriptsAt(ctx, dir, head, wf)
	if len(scripts) != 2 || scripts["scripts/unit.sh"] == "" || scripts["scripts/ci.sh"] == "" {
		t.Fatalf("scripts = %v, want ci.sh and unit.sh", scripts)
	}
	r := testgate.Evaluate(testgate.Input{Files: []string{"a.go", "a_test.go"}, CI: testgate.CIState{Workflows: wf, Scripts: scripts}})
	if r.Blocking || len(r.TestWorkflows) != 1 {
		t.Fatalf("CI via nested scripts: blocking=%v TestWorkflows=%v warnings=%+v", r.Blocking, r.TestWorkflows, r.Warnings)
	}
}
