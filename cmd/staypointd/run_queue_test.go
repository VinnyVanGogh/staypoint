package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/adapter"
	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
)

// freshSlots swaps in a run limiter with the given cap for one test.
func freshSlots(t *testing.T, max int) *orchestrator.RunSlots {
	t.Helper()
	prev := orchestrator.GlobalRunSlots
	s := orchestrator.NewRunSlots(max)
	orchestrator.GlobalRunSlots = s
	t.Cleanup(func() { orchestrator.GlobalRunSlots = prev })
	return s
}

func stubQuota(t *testing.T, locked *atomic.Bool) {
	t.Helper()
	prev := taskQuotaLocked
	taskQuotaLocked = func(*sql.DB, string, string) bool { return locked.Load() }
	t.Cleanup(func() { taskQuotaLocked = prev })
}

func drain(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() { orchestrator.GlobalDispatcher.Drain(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("dispatcher did not drain")
	}
}

func insertWakeTask(t *testing.T, dbConn *sql.DB, id string) {
	t.Helper()
	if _, err := dbConn.Exec(
		`INSERT INTO tasks (id, name, repo_path, execution_stage, assignee_agent_id) VALUES (?, 'q task', '', 'todo', 'agent-q')`, id,
	); err != nil {
		t.Fatalf("insert task: %v", err)
	}
}

func completingAdapter(calls *atomic.Int32) orchestrator.AdapterRunFunc {
	return func(_ context.Context, _ string, _ string, _ []string, _ []string, stdout, _ io.Writer) error {
		calls.Add(1)
		fmt.Fprintln(stdout, "[[TASK_COMPLETE]]")
		return nil
	}
}

// TestWireOnWake_RefusedRunIsQueuedAndAutoStarts: before STA-773 a run refused
// by the cap was logged and dropped. Now it is queued and starts on its own
// once the slot frees.
func TestWireOnWake_RefusedRunIsQueuedAndAutoStarts(t *testing.T) {
	orchestrator.GlobalDispatcher = orchestrator.NewDispatcher()
	slots := freshSlots(t, 1)
	var locked atomic.Bool
	stubQuota(t, &locked)

	store := openTestStore(t)
	dir := t.TempDir()
	const taskID = "queue-refused-task-1"
	insertWakeTask(t, store.DB(), taskID)

	var calls atomic.Int32
	wireOnWake(store, dir, nil, completingAdapter(&calls), &stubWM{dir: dir})

	// Another run elsewhere holds the only slot.
	if err := slots.Acquire("blocker", orchestrator.SlotKey{Dir: "/elsewhere"}); err != nil {
		t.Fatal(err)
	}
	orchestrator.GlobalDispatcher.Wake(taskID, "run_now", "")
	drain(t)

	if calls.Load() != 0 {
		t.Fatal("run started while the cap was full")
	}
	pos := slots.Position(taskID)
	if !pos.Queued || pos.Wait != orchestrator.WaitSlots {
		t.Fatalf("refused run not queued: %+v", pos)
	}

	slots.Release("blocker")
	drain(t)
	if calls.Load() == 0 {
		t.Fatal("queued run did not start after the slot freed")
	}
	if slots.Position(taskID).Queued {
		t.Fatal("started run still queued")
	}
	if n := slots.Active(); n != 0 {
		t.Fatalf("slots leaked: %d active", n)
	}
}

// TestWireOnWake_QuotaLockedRunWaits: a run whose provider pool is locked
// waits in the queue without taking a slot, and starts once the pool unlocks.
func TestWireOnWake_QuotaLockedRunWaits(t *testing.T) {
	orchestrator.GlobalDispatcher = orchestrator.NewDispatcher()
	slots := freshSlots(t, 3)
	var locked atomic.Bool
	locked.Store(true)
	stubQuota(t, &locked)

	store := openTestStore(t)
	dir := t.TempDir()
	const taskID = "queue-quota-task-1"
	insertWakeTask(t, store.DB(), taskID)

	var calls atomic.Int32
	wireOnWake(store, dir, nil, completingAdapter(&calls), &stubWM{dir: dir})

	orchestrator.GlobalDispatcher.Wake(taskID, "run_now", "")
	drain(t)
	if calls.Load() != 0 {
		t.Fatal("run started while its quota pool was locked")
	}
	if pos := slots.Position(taskID); !pos.Queued || pos.Wait != orchestrator.WaitQuota {
		t.Fatalf("quota-locked run not queued: %+v", pos)
	}
	if n := slots.Active(); n != 0 {
		t.Fatalf("quota-locked run holds a slot: %d active", n)
	}

	locked.Store(false)
	slots.Pump() // the daemon's retry ticker
	drain(t)
	if calls.Load() == 0 {
		t.Fatal("run did not start after the pool unlocked")
	}
}

// TestWireOnWake_SeatsRunOutMidRunWaitsForReset: the 2026-10-09 incident. A
// run whose seats all answer with their limit mid-run is not an error: it is
// queued on quota (work -> personal -> wait) and resumes once a seat resets.
func TestWireOnWake_SeatsRunOutMidRunWaitsForReset(t *testing.T) {
	orchestrator.GlobalDispatcher = orchestrator.NewDispatcher()
	slots := freshSlots(t, 3)
	var locked atomic.Bool
	stubQuota(t, &locked)

	store := openTestStore(t)
	dir := t.TempDir()
	const taskID = "queue-seat-limit-1"
	insertWakeTask(t, store.DB(), taskID)

	var calls atomic.Int32
	limited := func(_ context.Context, _ string, _ string, _ []string, _ []string, stdout, _ io.Writer) error {
		if calls.Add(1) == 1 {
			// Both seats answer with their limit; the pacer now shows them out.
			locked.Store(true)
			return &adapter.SeatLimitError{AllLocked: true, Seats: []string{
				"work seat locked (You've hit your session limit · resets 4:20am)",
				"personal seat locked (You've hit your session limit · resets 4:20am)",
			}}
		}
		fmt.Fprintln(stdout, "[[TASK_COMPLETE]]")
		return nil
	}
	wireOnWake(store, dir, nil, limited, &stubWM{dir: dir})

	orchestrator.GlobalDispatcher.Wake(taskID, "run_now", "")
	drain(t)
	if calls.Load() != 1 {
		t.Fatalf("want one turn before the wait, got %d", calls.Load())
	}
	if pos := slots.Position(taskID); !pos.Queued || pos.Wait != orchestrator.WaitQuota {
		t.Fatalf("seat-limited run not queued on quota: %+v", pos)
	}
	var stage string
	_ = store.DB().QueryRow(`SELECT execution_stage FROM tasks WHERE id=?`, taskID).Scan(&stage)
	if stage == "error" {
		t.Fatalf("a quota wait must not mark the task errored")
	}

	locked.Store(false) // a seat reset
	slots.Pump()
	drain(t)
	if calls.Load() < 2 {
		t.Fatal("run did not resume after a seat reset")
	}
}

// TestWireOnWake_DuplicateWakeOfRunningTaskIsQuiet: with a cap of 1, a second
// Run Now on a running task hit the global cap and was dropped quietly. With
// parallel slots it reaches ErrAlreadyClaimed instead; that must not post a
// "Finished: error" state (and run.state SSE) over the live run.
func TestWireOnWake_DuplicateWakeOfRunningTaskIsQuiet(t *testing.T) {
	orchestrator.GlobalDispatcher = orchestrator.NewDispatcher()
	slots := freshSlots(t, 3)
	var locked atomic.Bool
	stubQuota(t, &locked)
	store := openTestStore(t)
	dir := t.TempDir()
	const taskID = "queue-dup-task-1"
	insertWakeTask(t, store.DB(), taskID)
	var calls atomic.Int32
	wireOnWake(store, dir, nil, completingAdapter(&calls), &stubWM{dir: dir})

	// The task's live run holds its slot.
	if err := slots.Acquire(taskID, orchestrator.SlotKey{Dir: orchestrator.RepoKey(dir)}); err != nil {
		t.Fatal(err)
	}
	orchestrator.GlobalDispatcher.Wake(taskID, "run_now", "")
	drain(t)
	if calls.Load() != 0 {
		t.Fatal("duplicate wake started a second run")
	}
	var n int
	_ = store.DB().QueryRow(`SELECT COUNT(*) FROM run_steps WHERE task_id=?`, taskID).Scan(&n)
	if n != 0 {
		t.Fatalf("duplicate wake wrote %d timeline steps, want 0", n)
	}
	slots.Release(taskID)
}

// TestWireOnWake_MissingQueuedTaskLeavesQueue: a queued task deleted before
// its turn must not keep its place (it would block its repo forever).
func TestWireOnWake_MissingQueuedTaskLeavesQueue(t *testing.T) {
	orchestrator.GlobalDispatcher = orchestrator.NewDispatcher()
	slots := freshSlots(t, 3)
	var locked atomic.Bool
	stubQuota(t, &locked)
	store := openTestStore(t)
	dir := t.TempDir()
	wireOnWake(store, dir, nil, completingAdapter(new(atomic.Int32)), &stubWM{dir: dir})

	slots.Enqueue("gone-task", orchestrator.SlotKey{Dir: dir}, "run_now", orchestrator.WaitRepo)
	slots.Pump()
	drain(t)
	if slots.Position("gone-task").Queued {
		t.Fatal("deleted task still queued")
	}
}
