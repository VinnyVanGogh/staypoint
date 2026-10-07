package orchestrator

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock for the turn watch.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 6, 20, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// recordSteps returns a StepRecorder that collects run.step rows.
func recordSteps(h *Harness, taskID string) (*StepRecorder, func() []RunStep) {
	var mu sync.Mutex
	var steps []RunStep
	sr := NewStepRecorder(h.DB, func(ev string, data any) {
		if s, ok := data.(RunStep); ok && ev == "run.step" {
			mu.Lock()
			steps = append(steps, s)
			mu.Unlock()
		}
	}, "watch-run", taskID)
	return sr, func() []RunStep {
		mu.Lock()
		defer mu.Unlock()
		return append([]RunStep(nil), steps...)
	}
}

func findStep(steps []RunStep, prefix string) *RunStep {
	for i := range steps {
		if strings.HasPrefix(steps[i].Title, prefix) {
			return &steps[i]
		}
	}
	return nil
}

// A turn that keeps working for longer than the old 30 minute cap (50
// simulated minutes, output every 10) is not stopped: there is no fixed
// time cap, only the stall check.
func TestRun_LongActiveTurnNotKilled(t *testing.T) {
	const taskID = "long-active-task"
	h := silentHarness(t, taskID)
	clk := newFakeClock()
	sr, steps := recordSteps(h, taskID)

	var killed bool
	result, err := h.Run(context.Background(), taskID, RunConfig{
		MaxTurns:         1,
		AgentID:          "tester",
		SkipGitPreflight: true,
		StepRecorder:     sr,
		ParseDelta:       func([]byte) ([]StepDelta, error) { return nil, nil },
		Clock:            clk.Now,
		watchPoll:        time.Millisecond,
		RunAdapter: func(ctx context.Context, _, _ string, _, _ []string, stdout, _ io.Writer) error {
			for i := 0; i < 5; i++ {
				clk.Advance(10 * time.Minute)
				fmt.Fprintf(stdout, "{\"type\":\"progress\",\"n\":%d}\n", i)
				time.Sleep(20 * time.Millisecond) // let the watch poll
				if ctx.Err() != nil {
					killed = true
					return ctx.Err()
				}
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if killed {
		t.Fatal("active turn was cancelled")
	}
	if result.Disposition == "capped" || strings.HasPrefix(result.DiagnosticMsg, "Stopped:") {
		t.Fatalf("disposition = %q diag = %q, want the turn left to finish", result.Disposition, result.DiagnosticMsg)
	}
	if s := findStep(steps(), "Stopped:"); s != nil {
		t.Fatalf("unexpected stop row %+v", s)
	}
	if got := clk.Now().Sub(newFakeClock().Now()); got < 30*time.Minute {
		t.Fatalf("simulated turn ran %s, want past the old 30m cap", got)
	}
}

// A turn with no output at all is stopped by the stall timeout, the run
// fails with a "Stopped: no activity for 20m" row, not a silent "capped".
func TestRun_SilentTurnKilledByStallTimeout(t *testing.T) {
	const taskID = "stalled-task"
	h := silentHarness(t, taskID)
	clk := newFakeClock()
	sr, steps := recordSteps(h, taskID)

	started := make(chan struct{})
	go func() {
		<-started
		for i := 0; i < 50; i++ {
			clk.Advance(time.Minute)
			time.Sleep(2 * time.Millisecond)
		}
	}()

	result, err := h.Run(context.Background(), taskID, RunConfig{
		MaxTurns:         3,
		AgentID:          "tester",
		SkipGitPreflight: true,
		StepRecorder:     sr,
		ParseDelta:       func([]byte) ([]StepDelta, error) { return nil, nil },
		Clock:            clk.Now,
		watchPoll:        time.Millisecond,
		RunAdapter: func(ctx context.Context, _, _ string, _, _ []string, _, _ io.Writer) error {
			close(started)
			select {
			case <-ctx.Done():
				return &exitErr{code: 1}
			case <-time.After(10 * time.Second):
				return fmt.Errorf("stall watch never fired")
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != "error" {
		t.Fatalf("disposition = %q, want error", result.Disposition)
	}
	if result.Turns != 1 {
		t.Fatalf("turns = %d, want 1 (no retry after a stall)", result.Turns)
	}
	row := findStep(steps(), "Stopped: no activity for 20m")
	if row == nil || row.Status != "error" {
		t.Fatalf("no stall row; steps: %+v", steps())
	}
	if s := findStep(steps(), "Run ended with no output"); s != nil {
		t.Fatalf("stall should not also report a silent run: %+v", s)
	}
	if msg := lastHarnessComment(t, h, taskID); !strings.Contains(msg, "no activity for 20m") {
		t.Fatalf("diagnostic comment = %q", msg)
	}
	if GlobalActiveTurns.Get(taskID) != nil {
		t.Fatal("active turn not cleared after the run")
	}
}

// Silence while the Board holds the run (paused) is not a stall.
func TestTurnWatch_BoardWaitIsNotStall(t *testing.T) {
	clk := newFakeClock()
	var cancelled bool
	waiting := true
	w := startTurnWatch(RunConfig{Clock: clk.Now, watchPoll: time.Hour}, func() bool { return waiting }, func() { cancelled = true })
	clk.Advance(45 * time.Minute)
	if w.check() {
		t.Fatal("stalled while the Board was holding the run")
	}
	waiting = false
	clk.Advance(19 * time.Minute)
	if w.check() {
		t.Fatal("stalled before 20m of silence after the hold")
	}
	clk.Advance(time.Minute)
	if !w.check() {
		t.Fatal("not stalled after 20m of silence")
	}
	if r := w.Stop(); r != turnStopStall || cancelled {
		t.Fatalf("reason = %q cancelled = %v", r, cancelled)
	}
}

// turn_timeout, when set, stops even an active turn; stall_timeout 0 (off)
// never stops one.
func TestTurnWatch_TurnTimeoutAndStallOff(t *testing.T) {
	clk := newFakeClock()
	w := startTurnWatch(RunConfig{Clock: clk.Now, watchPoll: time.Hour, TurnTimeout: time.Hour, StallTimeout: -1}, nil, func() {})
	for i := 0; i < 5; i++ {
		clk.Advance(10 * time.Minute)
		w.Touch()
		if w.check() {
			t.Fatalf("stopped at %d0m", i+1)
		}
	}
	clk.Advance(10 * time.Minute)
	if !w.check() {
		t.Fatal("turn_timeout did not stop the turn at 1h")
	}
	if r := w.Stop(); r != turnStopTimeout {
		t.Fatalf("reason = %q", r)
	}
	if title, _ := w.stopRow(turnStopTimeout); title != "Stopped: turn ran longer than 1h" {
		t.Fatalf("title = %q", title)
	}
	if startTurnWatch(RunConfig{StallTimeout: -1}, nil, func() {}) != nil {
		t.Fatal("watch started with both limits off")
	}
}
