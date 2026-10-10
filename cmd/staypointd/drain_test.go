package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
)

// launchd sends SIGTERM while a deploy drain is waiting on a live run. The
// shutdown must not cut the run: it escalates to a turn-boundary suspend
// (never to DrainNow, which cancels turns) and waits for the run to leave
// before the daemon exits.
func TestDrainForShutdown_SIGTERMDuringDrainWaitsForRuns(t *testing.T) {
	slots := orchestrator.NewRunSlots(9)
	slots.Wake = func(string, string) {}
	if err := slots.Acquire("task-live", orchestrator.SlotKey{}); err != nil {
		t.Fatal(err)
	}
	slots.StartDrain(orchestrator.DrainFinish) // the deploy's drain
	nowCh := slots.DrainNowChan()

	done := make(chan struct{})
	go func() {
		drainForShutdown(slots, orchestrator.NewDispatcher(), 5*time.Millisecond)
		close(done)
	}()

	time.Sleep(150 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("shutdown returned with a run still live")
	default:
	}
	if slots.Drain() != orchestrator.DrainBoundary {
		t.Fatalf("shutdown drain mode = %v, want boundary", slots.Drain())
	}
	select {
	case <-nowCh:
		t.Fatal("shutdown cut live turns (DrainNow)")
	default:
	}

	slots.Release("task-live") // the run reached its turn boundary
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not return after the last run left")
	}
}

// Run Now while the daemon drains for a deploy: the run is queued in SQLite
// (not refused, not run), and a restarted daemon loads and starts it.
func TestWireOnWake_RunNowDuringDrainQueuedAndRunsAfterRestart(t *testing.T) {
	orchestrator.GlobalDispatcher = orchestrator.NewDispatcher()
	slots := freshSlots(t, 9)
	var locked atomic.Bool
	stubQuota(t, &locked)
	store := openTestStore(t)
	slots.Store = orchestrator.SQLQueueStore{DB: store.DB()}
	dir := t.TempDir()
	const taskID = "drain-run-now-1"
	insertWakeTask(t, store.DB(), taskID)

	var calls atomic.Int32
	wireOnWake(store, dir, nil, completingAdapter(&calls), &stubWM{dir: dir})
	slots.StartDrain(orchestrator.DrainFinish)
	orchestrator.GlobalDispatcher.Wake(taskID, "run_now", "")
	drain(t)
	if calls.Load() != 0 {
		t.Fatal("a run started during the drain")
	}
	if pos := slots.Position(taskID); !pos.Queued || pos.Wait != orchestrator.WaitDrain {
		t.Fatalf("run during drain not queued as drain: %+v", pos)
	}

	// The new daemon: fresh in-memory state, same DB.
	slots2 := freshSlots(t, 9)
	slots2.Store = orchestrator.SQLQueueStore{DB: store.DB()}
	wireOnWake(store, dir, nil, completingAdapter(&calls), &stubWM{dir: dir})
	restoreRunQueue(context.Background(), store.DB(), slots2)
	drain(t)
	if calls.Load() == 0 {
		t.Fatal("queued run did not start on the restarted daemon")
	}
	if q, _ := orchestrator.LoadQueue(context.Background(), store.DB()); len(q) != 0 {
		t.Fatalf("started run still in the persisted queue: %+v", q)
	}
}

func TestDrainStatusFile_WrittenWhileDrainingRemovedAfter(t *testing.T) {
	dir := t.TempDir()
	slots := orchestrator.NewRunSlots(9)
	slots.Wake = func(string, string) {}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go writeDrainStatusFile(ctx, dir, slots)
	path := filepath.Join(dir, drainStatusFileName)

	slots.StartDrain(orchestrator.DrainFinish)
	var st drainStatusFile
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &st) == nil && st.Draining {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("drain.json not written while draining")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st.Label != "Draining for deploy: 0 runs left, 0 queued" || st.UpdatedAt.IsZero() {
		t.Fatalf("drain.json = %+v", st)
	}

	slots.CancelDrain()
	for {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline.Add(5 * time.Second)) {
			t.Fatal("drain.json left after the drain ended")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Board review item 8: a run refused for a live agent in its worktree is
// woken again once that agent exits, not dropped for good.
func TestWakeWhenAgentExits_WakesAfterExit(t *testing.T) {
	prev := orchestrator.GlobalDispatcher
	t.Cleanup(func() { orchestrator.GlobalDispatcher = prev })
	orchestrator.GlobalDispatcher = orchestrator.NewDispatcher()
	woke := make(chan string, 1)
	orchestrator.GlobalDispatcher.OnWake = func(id, _ string) { woke <- id }

	var alive atomic.Bool
	alive.Store(true)
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait(); alive.Store(false) }()
	still := &orchestrator.AgentStillRunningError{PIDs: []int{pid}}
	go wakeWhenAgentExits("task-w", "run_now", still, 20*time.Millisecond, time.Minute)

	select {
	case <-woke:
		t.Fatal("woken while the agent still runs")
	case <-time.After(200 * time.Millisecond):
	}
	_ = cmd.Process.Kill()
	select {
	case id := <-woke:
		if id != "task-w" {
			t.Fatalf("woke %q", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("not woken after the agent exited")
	}
	if !refusedBeforeStart(still) || refusedBeforeStart(os.ErrClosed) {
		t.Fatal("refusedBeforeStart misclassifies")
	}
}
