package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/geminiapproval"
	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
	"github.com/VinnyVanGogh/staypoint/internal/workspace"
)

// STA-864: tasks whose directory is not a git repo (or that have no repo)
// run in place with a warning instead of failing to create a worktree.

// plainDir is an existing directory that is not a git repository.
func plainDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "personal-mac-utils")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if workspace.InGitRepo(dir) {
		t.Skip("temp dir is inside a git repository on this machine")
	}
	return dir
}

// failingWM fails the test if the harness tries to create a worktree.
type failingWM struct{ called atomic.Bool }

func (f *failingWM) CreateContext(context.Context, string, string) (string, error) {
	f.called.Store(true)
	return "", errors.New("worktree must not be created for a non-git task")
}
func (f *failingWM) PruneContext(context.Context, string) error            { return nil }
func (f *failingWM) PruneWorktreeDirContext(context.Context, string) error { return nil }

type plainRun struct {
	cwd      string
	steps    []string // "kind|status|title"
	stage    string
	products []string
	store    *db.Store
}

// runPlainTask wakes taskID (repo_path as given) through wireOnWake with an
// adapter that records its cwd, writes notes.txt there and completes.
func runPlainTask(t *testing.T, taskID, repoPath string) plainRun {
	t.Helper()
	orchestrator.GlobalDispatcher = orchestrator.NewDispatcher()
	store := openTestStore(t)
	if _, err := store.DB().Exec(
		`INSERT INTO tasks (id, name, repo_path, execution_stage, assignee_agent_id) VALUES (?, 'plain task', ?, 'todo', 'agent-plain')`,
		taskID, repoPath,
	); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	var cwd atomic.Value
	stub := func(_ context.Context, dir string, _ string, _ []string, _ []string, stdout, _ io.Writer) error {
		cwd.Store(dir)
		if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("done\n"), 0o644); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "[[TASK_COMPLETE]]")
		return nil
	}
	wm := &failingWM{}
	harnessRepo := t.TempDir()
	wireOnWake(store, harnessRepo, nil, stub, wm)
	orchestrator.GlobalDispatcher.Wake(taskID, "test_plain", "test:"+taskID)
	done := make(chan struct{})
	go func() { orchestrator.GlobalDispatcher.Drain(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("dispatcher did not drain")
	}
	if wm.called.Load() {
		t.Fatal("harness tried to create a worktree for a non-git task")
	}

	r := plainRun{store: store}
	r.cwd, _ = cwd.Load().(string)
	rows, err := store.DB().Query(`SELECT kind, status, title FROM run_steps WHERE task_id=? ORDER BY seq`, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k, s, ti string
		if err := rows.Scan(&k, &s, &ti); err != nil {
			t.Fatal(err)
		}
		r.steps = append(r.steps, k+"|"+s+"|"+ti)
	}
	rows.Close()
	_ = store.DB().QueryRow(`SELECT execution_stage FROM tasks WHERE id=?`, taskID).Scan(&r.stage)
	prows, err := store.DB().Query(`SELECT product_type || ':' || reference FROM task_work_products WHERE task_id=?`, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for prows.Next() {
		var p string
		_ = prows.Scan(&p)
		r.products = append(r.products, p)
	}
	prows.Close()
	return r
}

func assertPlainRun(t *testing.T, r plainRun, taskID, dir string) {
	t.Helper()
	if r.cwd != dir {
		t.Fatalf("adapter ran in %q, want %q", r.cwd, dir)
	}
	if got := readFile(t, filepath.Join(dir, "notes.txt")); got != "done\n" {
		t.Fatalf("edit not in place: notes.txt = %q", got)
	}
	want := "message|done|" + workspace.NonGitWarning
	found := false
	for _, s := range r.steps {
		if s == want {
			found = true
		}
		if strings.HasPrefix(s, "checkpoint|") {
			t.Errorf("non-git run wrote a checkpoint step: %q", s)
		}
	}
	if !found {
		t.Fatalf("no warning row %q; steps: %q", want, r.steps)
	}
	if r.stage != "in_review" {
		t.Errorf("stage = %q, want in_review", r.stage)
	}
	if len(r.products) != 1 || r.products[0] != "workspace_file:"+dir {
		t.Errorf("work products = %q, want the directory", r.products)
	}
	if _, err := shipreview.GetCard(r.store.DB(), taskID); !errors.Is(err, shipreview.ErrNoCard) {
		t.Errorf("ship review card for a non-git task: err = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		t.Errorf("a .git appeared in %s: %v", dir, err)
	}
}

func TestWake_NonGitDirRunsInPlaceWithWarning(t *testing.T) {
	dir := plainDir(t)
	const taskID = "nongit-dir-task"
	r := runPlainTask(t, taskID, dir)
	assertPlainRun(t, r, taskID, dir)
}

func TestWake_EmptyRepoPathUsesScratchDir(t *testing.T) {
	const taskID = "nongit-scratch-task"
	want, err := workspace.ScratchDir(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(want, os.Getenv(workspace.ScratchRootEnv)) {
		t.Fatalf("scratch dir %q escaped the test root", want)
	}
	r := runPlainTask(t, taskID, "")
	assertPlainRun(t, r, taskID, want)
}

// A docs-shaped kind may still route to Gemini in a non-git dir, but a turn
// that writes a .go file fails the run, and the row says it was not reverted.
func TestWake_NonGitGeminiNonCodeTurnWritingGoFails(t *testing.T) {
	dir := plainDir(t)
	bin, logPath := editingCLI(t, "echo 'package x' > extra.go")
	r := runWakeIn(t, dir, "planning", openPacer(), bin, logPath, t.TempDir())

	if len(r.spawns) == 0 || isClaude(r.spawns[0]) {
		t.Fatalf("want an agy spawn, got %q", r.spawns)
	}
	if r.stage != "error" {
		t.Errorf("stage = %q, want error", r.stage)
	}
	want := "Blocked: Gemini changed non-doc files in a folder that is not a git repository: extra.go (not reverted)"
	if len(r.messages) != 1 || r.messages[0] != want {
		t.Fatalf("error rows = %q, want [%q]", r.messages, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "extra.go")); err != nil {
		t.Errorf("extra.go should be left as is (nothing to revert to): %v", err)
	}
}

// Doc-only Gemini edits in a non-git dir are fine.
func TestWake_NonGitGeminiDocEditAllowed(t *testing.T) {
	dir := plainDir(t)
	bin, logPath := editingCLI(t, "echo '# notes' > NOTES.md")
	r := runWakeIn(t, dir, "planning", openPacer(), bin, logPath, t.TempDir())
	for _, m := range r.messages {
		if strings.HasPrefix(m, "Blocked:") {
			t.Fatalf("doc edit blocked: %q", m)
		}
	}
}

// Kinds that may write code never route to Gemini in a non-git dir, even
// when every Claude seat is locked.
func TestWake_NonGitCodingNeverSpawnsGemini(t *testing.T) {
	dir := plainDir(t)
	bin, logPath := fakeCLI(t, "")
	pacer := lockedPacer(map[router.PoolID]string{router.PoolPersonalClaude: "weekly limit", router.PoolWorkClaude: "weekly limit"})
	r := runWakeIn(t, dir, "coding", pacer, bin, logPath, t.TempDir())
	for _, s := range r.spawns {
		if s != "" && !isClaude(s) {
			t.Fatalf("spawned Gemini for code work in a non-git dir: %q", s)
		}
	}
}

func TestBarGeminiOutsideGit(t *testing.T) {
	gem := router.RouteSlot{Family: router.FamilyGemini, Model: "gemini-3.1-pro-high", PoolID: router.PoolGeminiNative}
	cl := router.RouteSlot{Family: router.FamilyClaude, Model: "opus", PoolID: router.PoolPersonalClaude, Seat: router.SeatPersonal}
	route := router.KindRoute{Kind: router.WorkKindCoding, Candidates: []router.RouteSlot{cl, gem}}

	dir := plainDir(t)
	got := barGeminiOutsideGit(route, dir, "bar-task")
	if len(got.Candidates) != 1 || got.Candidates[0] != cl {
		t.Fatalf("non-git coding candidates = %+v, want Claude only", got.Candidates)
	}
	if len(route.Candidates) != 2 {
		t.Fatal("input route was modified")
	}
	if got := barGeminiOutsideGit(route, "", "bar-task"); len(got.Candidates) != 1 {
		t.Fatalf("scratch dir coding candidates = %+v", got.Candidates)
	}
	planning := route
	planning.Kind = router.WorkKindPlanning
	if got := barGeminiOutsideGit(planning, dir, "bar-task"); len(got.Candidates) != 2 {
		t.Fatalf("planning should keep Gemini: %+v", got.Candidates)
	}
	wt := guardWorktree(t)
	if got := barGeminiOutsideGit(route, wt, "bar-task"); len(got.Candidates) != 2 {
		t.Fatalf("git repo route changed: %+v", got.Candidates)
	}
}

// provider=gemini on a code kind in a non-git dir: the Touch ID gate asks
// for nothing and an existing approval is neither consumed nor applied. The
// run is Claude only (an approval can never let Gemini write code where
// nothing can be reverted).
func TestWake_NonGitGeminiCodeApprovalNeverApplies(t *testing.T) {
	dir := plainDir(t)
	f := newGateFixture(t, dir, "coding", "gemini")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	scope := geminiapproval.Scope{TaskID: f.taskID, Repo: dir}
	if _, err := f.store.DB().Exec(
		`INSERT INTO security_gate_requests (id, cmdline, run_id, status, created_at, decided_at) VALUES ('pre-approved', ?, ?, 'approved', ?, ?)`,
		scope.Cmdline(), geminiapproval.RunID, now, now); err != nil {
		t.Fatal(err)
	}
	bin, logPath := editingCLI(t, "echo 'package x' > extra.go")
	r := runWakeIn(t, dir, "", openPacer(), bin, logPath, t.TempDir())
	if len(r.spawns) != 1 || !isClaude(r.spawns[0]) {
		t.Fatalf("non-git code run: want Claude only, got %q", r.spawns)
	}
	var used int
	_ = f.store.DB().QueryRow(`SELECT COUNT(1) FROM gemini_code_grant_uses`).Scan(&used)
	if used != 0 {
		t.Errorf("approval consumed by a non-git run")
	}
	if n := len(f.requests()); n != 1 {
		t.Errorf("gate requests = %d, want only the pre-existing one", n)
	}
	if br := f.blockReason(); br != "" {
		t.Errorf("non-git run held for approval: %q", br)
	}
}
