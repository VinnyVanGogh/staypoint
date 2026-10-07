package orchestrator

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// nogitHarness is a harness for a task whose repo_path is a plain (non-git)
// temp directory holding one code file.
func nogitHarness(t *testing.T, taskID string) (*Harness, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db := openTestDB(t)
	insertTask(t, db, taskID, dir)
	h := &Harness{DB: db, RepoRoot: dir, WM: &noopWorktreeManager{}, Interceptor: NewInterceptor(db)}
	return h, dir
}

func wantNoGitBlock(t *testing.T, result *RunResult) {
	t.Helper()
	if result.Disposition != "error" || !strings.Contains(result.DiagnosticMsg, "not a git repository") {
		t.Fatalf("non-git guard did not fail the run: disposition=%q msg=%q", result.Disposition, result.DiagnosticMsg)
	}
}

// A Gemini turn that edits code and then panics (stream parser, tee writer)
// must still be checked: the panic used to unwind past CheckNoGit, so the
// edit was never reported and the next run snapshotted it as the baseline.
func TestRun_NoGitGuardCheckedAfterAdapterPanic(t *testing.T) {
	const taskID = "nogit-panic"
	h, dir := nogitHarness(t, taskID)
	var result *RunResult
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("adapter panic escaped the harness before the guard ran: %v", r)
			}
		}()
		result, err = h.Run(context.Background(), taskID, RunConfig{
			MaxTurns:         1,
			Provider:         "gemini",
			MaxWallclock:     10 * time.Second,
			SkipGitPreflight: true,
			RunAdapter: func(_ context.Context, cwd, _ string, _, _ []string, _, _ io.Writer) error {
				_ = os.WriteFile(filepath.Join(cwd, "main.go"), []byte("package main\n// pwned\n"), 0o644)
				panic("stream parser blew up")
			},
		})
	}()
	if err != nil {
		t.Fatal(err)
	}
	wantNoGitBlock(t, result)
	if got, _ := os.ReadFile(filepath.Join(dir, "main.go")); !strings.Contains(string(got), "pwned") {
		t.Fatalf("test setup: edit not on disk")
	}
}

// Snapshot failure before the turn: the Gemini turn cannot be verified, so
// the run fails even though nothing non-doc changed.
func TestRun_NoGitGuardSnapshotErrorFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads 0000 dirs")
	}
	const taskID = "nogit-snaperr"
	h, dir := nogitHarness(t, taskID)
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	result, err := h.Run(context.Background(), taskID, RunConfig{
		MaxTurns:         1,
		Provider:         "gemini",
		MaxWallclock:     10 * time.Second,
		SkipGitPreflight: true,
		RunAdapter: func(context.Context, string, string, []string, []string, io.Writer, io.Writer) error {
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantNoGitBlock(t, result)
}

// A Claude-routed turn that fell back to Gemini mid-turn is checked: the
// spawn tracker, not the planned provider, decides.
func TestRun_NoGitGuardCatchesMidRunGeminiFallback(t *testing.T) {
	const taskID = "nogit-fallback"
	h, _ := nogitHarness(t, taskID)
	spawned := false
	result, err := h.Run(context.Background(), taskID, RunConfig{
		MaxTurns:         1,
		Provider:         "claude",
		MaxWallclock:     10 * time.Second,
		SkipGitPreflight: true,
		TurnUsedGemini: func() bool {
			v := spawned
			spawned = false
			return v
		},
		RunAdapter: func(_ context.Context, cwd, _ string, _, _ []string, _, _ io.Writer) error {
			spawned = true // claude seat locked; gemini streamed instead
			return os.WriteFile(filepath.Join(cwd, "main.go"), []byte("package main\n// edit\n"), 0o644)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wantNoGitBlock(t, result)
}

// A panicking turn with nothing for the guard to block still ends the run
// (no further turn re-snapshots over late writes) instead of crashing.
func TestRun_AdapterPanicStopsRun(t *testing.T) {
	const taskID = "panic-stop"
	h, _ := nogitHarness(t, taskID)
	calls := 0
	result, err := h.Run(context.Background(), taskID, RunConfig{
		MaxTurns:         3,
		Provider:         "claude",
		MaxWallclock:     10 * time.Second,
		SkipGitPreflight: true,
		RunAdapter: func(context.Context, string, string, []string, []string, io.Writer, io.Writer) error {
			calls++
			panic("boom")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || result.Disposition != "error" || !strings.Contains(result.DiagnosticMsg, "crashed") {
		t.Fatalf("calls=%d disposition=%q msg=%q", calls, result.Disposition, result.DiagnosticMsg)
	}
}
