package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/db"
)

// silentHarness returns a harness whose worktree manager and interceptor need
// no git repo.
func silentHarness(t *testing.T, taskID string) *Harness {
	t.Helper()
	db := openTestDB(t)
	insertTask(t, db, taskID, "/tmp")
	h := &Harness{
		DB:          db,
		RepoRoot:    "/tmp",
		WM:          &noopWorktreeManager{},
		Interceptor: NewInterceptor(db),
	}
	h.Interceptor.Guards = nil
	return h
}

func lastHarnessComment(t *testing.T, h *Harness, taskID string) string {
	t.Helper()
	var msg string
	_ = h.DB.QueryRow(`SELECT message FROM task_comments WHERE task_id=? AND author='harness' ORDER BY id DESC LIMIT 1`, taskID).Scan(&msg)
	return msg
}

// TestRun_SilentRunMarkedFailed is the STA-775 item 3 regression: a run whose
// adapter turns exit cleanly but print nothing (a Gemini run did 2 turns with
// only wake/route/checkpoint steps) must end failed with a plain reason that
// carries the exit code and stderr tail, not sit in_progress looking idle.
func TestRun_SilentRunMarkedFailed(t *testing.T) {
	const taskID = "silent-task"
	h := silentHarness(t, taskID)

	var steps []RunStep
	sr := NewStepRecorder(h.DB, func(ev string, data any) {
		if s, ok := data.(RunStep); ok && ev == "run.step" {
			steps = append(steps, s)
		}
	}, "silent-run", taskID)

	result, err := h.Run(context.Background(), taskID, RunConfig{
		MaxTurns:         2,
		AgentID:          "tester",
		MaxWallclock:     10 * time.Second,
		SkipGitPreflight: true,
		StepRecorder:     sr,
		ParseDelta:       func([]byte) ([]StepDelta, error) { return nil, nil },
		RunAdapter: func(_ context.Context, _, _ string, _, _ []string, stdout, stderr io.Writer) error {
			// A stream init line the parser ignores, and nothing else.
			fmt.Fprintln(stdout, `{"type":"init","session_id":"x"}`)
			fmt.Fprintln(stderr, "loaded cached credentials")
			fmt.Fprintln(stderr, "warning: model returned an empty response")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Turns != 2 {
		t.Fatalf("turns = %d, want 2", result.Turns)
	}
	if result.Disposition != "error" {
		t.Fatalf("disposition = %q, want error", result.Disposition)
	}
	var stage string
	_ = h.DB.QueryRow(`SELECT execution_stage FROM tasks WHERE id=?`, taskID).Scan(&stage)
	if stage != "error" {
		t.Fatalf("execution_stage = %q, want error", stage)
	}

	msg := lastHarnessComment(t, h, taskID)
	if !strings.Contains(msg, "Run ended with no output (exit 0") ||
		!strings.Contains(msg, "warning: model returned an empty response") {
		t.Fatalf("diagnostic comment = %q", msg)
	}

	// The timeline shows why, ahead of the terminal state row.
	var note, state *RunStep
	for i := range steps {
		switch {
		case steps[i].Kind == StepMessage && strings.HasPrefix(steps[i].Title, "Run ended with no output"):
			note = &steps[i]
		case steps[i].Kind == StepState:
			state = &steps[i]
		}
	}
	if note == nil || note.Status != "error" {
		t.Fatalf("no error message step for the silent run; steps: %+v", steps)
	}
	if !strings.Contains(note.Body, "warning: model returned an empty response") {
		t.Fatalf("message step body = %q, want stderr tail", note.Body)
	}
	if state == nil || state.Title != "Finished: error" || state.Seq < note.Seq {
		t.Fatalf("state step = %+v, want Finished: error after the message", state)
	}
}

// A silent run that the adapter also failed keeps its exit code in the reason.
func TestRun_SilentRunReportsExitCode(t *testing.T) {
	const taskID = "silent-exit-task"
	h := silentHarness(t, taskID)
	result, err := h.Run(context.Background(), taskID, RunConfig{
		MaxTurns:         1,
		AgentID:          "tester",
		MaxWallclock:     10 * time.Second,
		SkipGitPreflight: true,
		RunAdapter: func(_ context.Context, _, _ string, _, _ []string, _, stderr io.Writer) error {
			fmt.Fprintln(stderr, "fatal: no such model")
			return &exitErr{code: 3}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != "error" {
		t.Fatalf("disposition = %q, want error", result.Disposition)
	}
	if msg := lastHarnessComment(t, h, taskID); !strings.Contains(msg, "exit 3") || !strings.Contains(msg, "fatal: no such model") {
		t.Fatalf("diagnostic comment = %q", msg)
	}
}

// A run that answered (no file changes) is not silent: its final message is
// kept and the disposition is unchanged.
func TestRun_AnsweredRunIsNotSilent(t *testing.T) {
	const taskID = "answered-task"
	h := silentHarness(t, taskID)
	result, err := h.Run(context.Background(), taskID, RunConfig{
		MaxTurns:         1,
		AgentID:          "tester",
		MaxWallclock:     10 * time.Second,
		SkipGitPreflight: true,
		RunAdapter: func(_ context.Context, _, _ string, _, _ []string, stdout, _ io.Writer) error {
			fmt.Fprintln(stdout, `{"type":"assistant","message":{"content":[{"type":"text","text":"The mailbox type is set per license."}]}}`)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition == "error" {
		t.Fatalf("answered run marked error")
	}
	if msg := lastHarnessComment(t, h, taskID); strings.Contains(msg, "no output") {
		t.Fatalf("answered run got a no-output diagnostic: %q", msg)
	}
}

// A run stopped by the Board keeps "stopped" even when it was silent.
func TestRun_SilentStoppedRunStaysStopped(t *testing.T) {
	const taskID = "silent-stop-task"
	h := silentHarness(t, taskID)
	// The minimal test schema has no run_control table; back the control
	// flags with a full store.
	store, err := db.Open(t.TempDir() + "/rc.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err := store.DB().Exec(`INSERT INTO tasks (id, name, repo_path) VALUES (?, 'stop', '/tmp')`, taskID); err != nil {
		t.Fatal(err)
	}
	rc := NewRunControl(store.DB())
	result, err := h.Run(context.Background(), taskID, RunConfig{
		MaxTurns:         3,
		AgentID:          "tester",
		MaxWallclock:     10 * time.Second,
		SkipGitPreflight: true,
		RunControl:       rc,
		RunAdapter: func(context.Context, string, string, []string, []string, io.Writer, io.Writer) error {
			_ = rc.SetStop(taskID) // the Board presses Stop mid-turn
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != "stopped" {
		t.Fatalf("disposition = %q, want stopped", result.Disposition)
	}
}

func TestNoOutputMessage(t *testing.T) {
	if got := noOutputMessage(0, ""); got != "Run ended with no output (exit 0, no stderr)." {
		t.Fatalf("empty stderr: %q", got)
	}
	long := strings.Repeat("x", 1000) + "\nlast line"
	got := noOutputMessage(2, long)
	if !strings.HasPrefix(got, "Run ended with no output (exit 2, stderr: …") || !strings.HasSuffix(got, "last line).") {
		t.Fatalf("long stderr: %q", got)
	}
	if len(got) > 400 {
		t.Fatalf("message not tail-limited: %d bytes", len(got))
	}
}

// exitErr is an error carrying a process exit code, like *exec.ExitError.
type exitErr struct{ code int }

func (e *exitErr) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e *exitErr) ExitCode() int { return e.code }

func TestExitCodeFrom_ExitCoder(t *testing.T) {
	if got := exitCodeFrom(fmt.Errorf("wrapped: %w", &exitErr{code: 7})); got != 7 {
		t.Fatalf("exitCodeFrom = %d, want 7", got)
	}
	if got := exitCodeFrom(errors.New("plain")); got != 1 {
		t.Fatalf("plain error exit = %d, want 1", got)
	}
}
