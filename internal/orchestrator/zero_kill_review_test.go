package orchestrator

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
)

// Board review of 4136a01 (task-db71fba9): items 8-12.

// Item 9: a Board pause is recorded when the run pauses, so a daemon killed
// while the run waits for Resume leaves it for the Board.
func TestZeroKill_PausedRunNotAutoResumedAfterKill9(t *testing.T) {
	d := zkDB(t)
	ctx := context.Background()
	zkTask(t, d, "zk-bp", "")
	liveBegin(ctx, d, LiveRun{TaskID: "zk-bp", RunID: "old", NextTurn: 2})
	livePaused(d, "zk-bp", "old", true)
	_, _ = d.Exec(`UPDATE live_runs SET daemon_pid=1, daemon_id='previous-daemon'`) // kill -9 here

	rep := RecoverLiveRuns(ctx, d, nil)
	if len(rep.Resumed) != 0 || len(rep.Held) != 1 {
		t.Fatalf("recovery = %+v, want the Board-paused run held", rep)
	}
}

// Item 10: --now arriving as a turn finishes does not throw the finished
// turn away; the run suspends before the next one.
func TestZeroKill_DrainNowAfterTurnEndedKeepsTheTurn(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	d := zkDB(t)
	zkTask(t, d, "zk-r10", repo)
	s := useSlots(t, 9)
	h := zkHarness(t, d, repo)
	res, err := h.Run(context.Background(), "zk-r10", zkRunCfg(5, func(context.Context, string, string, []string, []string, io.Writer, io.Writer) error {
		s.StartDrain(DrainNow)
		return nil // the turn finished
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Disposition != SuspendedDisposition {
		t.Fatalf("disposition = %q", res.Disposition)
	}
	if lr := liveRun(context.Background(), d, "zk-r10"); lr == nil || lr.NextTurn != 1 {
		t.Fatalf("live run = %+v, want the finished turn kept (resume at turn 1)", lr)
	}
}

// Item 11: after Close, a wake runs inline (it only queues during the
// shutdown drain) instead of in a goroutine nobody waits for.
func TestDispatcher_CloseRunsLaterWakesInline(t *testing.T) {
	d := NewDispatcher()
	ran := false
	d.OnWake = func(string, string) { ran = true }
	d.Close()
	d.Wake("t", "r", "")
	if !ran {
		t.Fatal("wake after Close did not run before Wake returned")
	}
}

// Item 11: the shutdown drain cannot be cancelled, so a wake that arrives
// meanwhile can only queue.
func TestRunSlots_ShutdownDrainIsNotCancellable(t *testing.T) {
	s := NewRunSlots(3)
	s.Wake = func(string, string) {}
	s.StartShutdownDrain(DrainBoundary)
	s.CancelDrain()
	if s.Drain() != DrainBoundary {
		t.Fatalf("drain = %v after CancelDrain during shutdown", s.Drain())
	}
}

// Item 12: a suspended run whose task closed before it resumed loses its
// kept worktree on the next daemon start.
func TestZeroKill_ClosedTaskSuspendedWorktreeIsRemoved(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	d := zkDB(t)
	zkTask(t, d, "zk-c12", repo)
	s := useSlots(t, 9)
	h := zkHarness(t, d, repo)
	if _, err := h.Run(context.Background(), "zk-c12", zkRunCfg(5, func(context.Context, string, string, []string, []string, io.Writer, io.Writer) error {
		s.StartDrain(DrainBoundary)
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	lr := liveRun(context.Background(), d, "zk-c12")
	if lr == nil {
		t.Fatal("no suspended run")
	}
	if _, err := os.Stat(lr.Worktree); err != nil {
		t.Fatalf("suspended worktree missing: %v", err)
	}
	_, _ = d.Exec(`UPDATE tasks SET execution_stage='done' WHERE id='zk-c12'`)
	_, _ = d.Exec(`UPDATE live_runs SET daemon_id='previous-daemon'`)
	RecoverLiveRuns(context.Background(), d, nil)
	if _, err := os.Stat(lr.Worktree); !os.IsNotExist(err) {
		t.Fatalf("closed task's suspended worktree still there (err=%v)", err)
	}
}

// Item 8: the refusal names what to wait for, and is still ErrAgentStillRunning.
func TestAgentStillRunningError(t *testing.T) {
	var err error = &AgentStillRunningError{PIDs: []int{1 << 30}}
	if !errors.Is(err, ErrAgentStillRunning) {
		t.Fatal("not ErrAgentStillRunning")
	}
	var e *AgentStillRunningError
	if !errors.As(err, &e) || !e.Gone() {
		t.Fatal("a pid that does not exist is not gone")
	}
}
