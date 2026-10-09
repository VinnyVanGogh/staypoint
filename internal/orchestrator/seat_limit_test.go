package orchestrator

import (
	"context"
	"database/sql"
	"io"
	"strings"
	"testing"
)

// fakeSeatLimit mimics adapter.SeatLimitError without importing the adapter.
type fakeSeatLimit struct{ all bool }

func (e fakeSeatLimit) Error() string        { return "work seat locked (You've hit your session limit)" }
func (e fakeSeatLimit) SeatsExhausted() bool { return e.all }

func seatLimitHarness(t *testing.T, taskID string) (*Harness, *sql.DB) {
	t.Helper()
	d := openTestDB(t)
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)
	if _, err := d.Exec(`INSERT INTO tasks (id, name, repo_path, execution_stage) VALUES (?, ?, ?, ?)`,
		taskID, "Seat limit task", repoDir, "todo"); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	return &Harness{DB: d, RepoRoot: repoDir, WM: &fixedPathWM{path: repoDir}, Interceptor: NewInterceptor(d)}, d
}

func countEvents(t *testing.T, d *sql.DB, taskID, event string) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM activity_log WHERE task_id=? AND event_type=?`, taskID, event).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", event, err)
	}
	return n
}

// Both seats out: the run ends as a quota wait (task stays in_progress, caller
// re-queues), not "adapter failed N turns in a row".
func TestHarness_AllSeatsOutIsQuotaWait(t *testing.T) {
	h, d := seatLimitHarness(t, "task-seat-wait")
	calls := 0
	result, err := h.Run(context.Background(), "task-seat-wait", RunConfig{
		MaxTurns: 5,
		RunAdapter: func(ctx context.Context, cwd, provider string, rawArgs, extraEnv []string, stdout, stderr io.Writer) error {
			calls++
			return fakeSeatLimit{all: true}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.QuotaWait || result.Disposition != "in_progress" {
		t.Fatalf("want quota wait in_progress, got wait=%v disposition=%q msg=%q", result.QuotaWait, result.Disposition, result.DiagnosticMsg)
	}
	if calls != 1 {
		t.Errorf("a quota wait must end the run after one turn, got %d", calls)
	}
	if strings.Contains(result.DiagnosticMsg, "failed") || !strings.Contains(result.DiagnosticMsg, "Waiting for a Claude seat") {
		t.Errorf("diagnostic must read as a wait: %q", result.DiagnosticMsg)
	}
	var n int
	_ = d.QueryRow(`SELECT count(*) FROM run_errors WHERE task_id=?`, "task-seat-wait").Scan(&n)
	if n != 0 {
		t.Errorf("a quota wait is not a run error, got %d run_errors rows", n)
	}
	var stage string
	_ = d.QueryRow(`SELECT execution_stage FROM tasks WHERE id=?`, "task-seat-wait").Scan(&stage)
	if stage != "in_progress" {
		t.Errorf("execution_stage = %q, want in_progress", stage)
	}
}

// A seat that runs out mid-turn never counts toward the consecutive-failure
// stop: the run keeps going (next turn re-routes) and each turn's seat is
// recorded.
func TestHarness_MidTurnSeatLimitIsNotAFailure(t *testing.T) {
	h, d := seatLimitHarness(t, "task-seat-mid")
	seats := []string{"Claude Opus · work seat", "Claude Opus · personal seat", "Claude Opus · personal seat"}
	turn := 0
	result, err := h.Run(context.Background(), "task-seat-mid", RunConfig{
		MaxTurns: 3,
		TurnSeat: func() string { return seats[turn-1] },
		RunAdapter: func(ctx context.Context, cwd, provider string, rawArgs, extraEnv []string, stdout, stderr io.Writer) error {
			turn++
			_, _ = stdout.Write([]byte("working\n"))
			return fakeSeatLimit{all: false}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if turn != 3 || result.Disposition == "error" || result.QuotaWait {
		t.Fatalf("want 3 turns, no error, no wait; got turns=%d disposition=%q msg=%q", turn, result.Disposition, result.DiagnosticMsg)
	}
	if n := countEvents(t, d, "task-seat-mid", "seat_limit"); n != 3 {
		t.Errorf("seat_limit events = %d, want 3", n)
	}
	rows, err := d.Query(`SELECT details FROM activity_log WHERE task_id=? AND event_type='turn_seat' ORDER BY id`, "task-seat-mid")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		got = append(got, s)
	}
	want := []string{"turn 0 ran on Claude Opus · work seat", "turn 1 ran on Claude Opus · personal seat", "turn 2 ran on Claude Opus · personal seat"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("turn_seat = %q, want %q", got, want)
	}
}
