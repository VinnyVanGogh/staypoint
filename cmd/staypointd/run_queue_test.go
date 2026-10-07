package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

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
	taskQuotaLocked = func(*sql.DB, string) bool { return locked.Load() }
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
	if err := slots.Acquire("blocker", "/elsewhere"); err != nil {
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

	slots.Enqueue("gone-task", dir, "run_now", orchestrator.WaitRepo)
	slots.Pump()
	drain(t)
	if slots.Position("gone-task").Queued {
		t.Fatal("deleted task still queued")
	}
}
