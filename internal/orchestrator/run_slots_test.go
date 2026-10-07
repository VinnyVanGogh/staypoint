package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// useSlots swaps GlobalRunSlots for a fresh limiter with the given cap whose
// queue pump does not dispatch (tests drive Wake themselves), and restores the
// previous limiter on cleanup.
func useSlots(t *testing.T, max int) *RunSlots {
	t.Helper()
	prev := GlobalRunSlots
	s := NewRunSlots(max)
	s.Wake = func(string, string) {}
	GlobalRunSlots = s
	t.Cleanup(func() { GlobalRunSlots = prev })
	return s
}

func TestRunSlots_DefaultCaps(t *testing.T) {
	l := NewRunSlots(0).Limits()
	if l.Global != 9 || l.PerRepo != 3 || l.PerOrg != 3 {
		t.Fatalf("default limits = %+v, want global 9, per repo 3, per org 3", l)
	}
}

func TestRunSlots_GlobalCapHonored(t *testing.T) {
	s := NewRunSlots(2)
	s.Wake = func(string, string) {}
	if err := s.Acquire("a", SlotKey{Dir: "/r1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Acquire("b", SlotKey{Dir: "/r2"}); err != nil {
		t.Fatal(err)
	}
	err := s.Acquire("c", SlotKey{Dir: "/r3"})
	if !errors.Is(err, ErrConcurrencyCap) {
		t.Fatalf("third run in a third repo: got %v, want ErrConcurrencyCap", err)
	}
	s.Release("a")
	if err := s.Acquire("c", SlotKey{Dir: "/r3"}); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

func TestRunSlots_PerRepoCapOfOne(t *testing.T) {
	s := NewRunSlotsWithLimits(RunLimits{Global: 3, PerRepo: 1})
	s.Wake = func(string, string) {}
	if err := s.Acquire("a", SlotKey{Dir: "/repo"}); err != nil {
		t.Fatal(err)
	}
	err := s.Acquire("b", SlotKey{Dir: "/repo"})
	if !errors.Is(err, ErrRepoBusy) || !errors.Is(err, ErrConcurrencyCap) {
		t.Fatalf("same repo: got %v, want ErrRepoBusy (matching ErrConcurrencyCap)", err)
	}
}

func TestRunSlots_QueuedRunKeepsItsPlaceInRepo(t *testing.T) {
	s := NewRunSlotsWithLimits(RunLimits{Global: 3, PerRepo: 1})
	s.Wake = func(string, string) {}
	_ = s.Acquire("a", SlotKey{Dir: "/repo"})
	s.Enqueue("b", SlotKey{Dir: "/repo"}, "run_now", "repo")
	s.Release("a")
	// A newcomer must not jump the queued run for the same repo.
	if err := s.Acquire("c", SlotKey{Dir: "/repo"}); !errors.Is(err, ErrRepoBusy) {
		t.Fatalf("newcomer jumped queue: %v", err)
	}
	if err := s.Acquire("b", SlotKey{Dir: "/repo"}); err != nil {
		t.Fatalf("queued run should start: %v", err)
	}
	if s.Position("b").Queued {
		t.Fatal("started run must leave the queue")
	}
}

func TestRunSlots_PositionAndPumpOrder(t *testing.T) {
	s := NewRunSlots(1)
	var mu sync.Mutex
	var woke []string
	s.Wake = func(id, _ string) { mu.Lock(); woke = append(woke, id); mu.Unlock() }
	_ = s.Acquire("a", SlotKey{Dir: "/r1"})
	s.Enqueue("b", SlotKey{Dir: "/r2"}, "x", "slots")
	s.Enqueue("c", SlotKey{Dir: "/r3"}, "x", "slots")
	if p := s.Position("c"); !p.Queued || p.Ahead != 1 {
		t.Fatalf("position c = %+v, want queued with 1 ahead", p)
	}
	// Re-enqueue keeps the place.
	s.Enqueue("b", SlotKey{Dir: "/r2"}, "x", "slots")
	if p := s.Position("b"); p.Ahead != 0 {
		t.Fatalf("re-enqueue moved b to %d", p.Ahead)
	}
	s.Release("a")
	mu.Lock()
	defer mu.Unlock()
	if len(woke) != 1 || woke[0] != "b" {
		t.Fatalf("pump woke %v, want [b] (one free slot, FIFO)", woke)
	}
}

func TestRunSlots_OnChangeReportsQueue(t *testing.T) {
	s := NewRunSlots(1)
	s.Wake = func(string, string) {}
	var last []QueuedRun
	s.OnChange = func(q []QueuedRun) { last = q }
	s.Enqueue("b", SlotKey{Dir: "/r2"}, "x", "slots")
	if len(last) != 1 || last[0].TaskID != "b" {
		t.Fatalf("OnChange queue = %+v", last)
	}
	s.Dequeue("b")
	if len(last) != 0 {
		t.Fatalf("after dequeue: %+v", last)
	}
}

// TestParallelRuns_ThreeTasksTwoRepos is the STA-773 acceptance test: three
// tasks across two repos with a cap of 3 and max_runs_per_repo = 1 → two run
// at once; the third (same repo as the first) is refused, queued, and starts
// automatically when its repo frees up.
func TestParallelRuns_ThreeTasksTwoRepos(t *testing.T) {
	slots := useSlots(t, 3)
	slots.SetLimits(RunLimits{Global: 3, PerRepo: 1})
	db := openTestDB(t)
	insertTask(t, db, "p-a1", "/tmp/sta773-repo-a")
	insertTask(t, db, "p-b1", "/tmp/sta773-repo-b")
	insertTask(t, db, "p-a2", "/tmp/sta773-repo-a")

	db.SetMaxOpenConns(1) // in-memory sqlite: every conn must see the same DB
	h := &Harness{DB: db, RepoRoot: "/tmp"}

	release := map[string]chan struct{}{
		"p-a1": make(chan struct{}),
		"p-b1": make(chan struct{}),
		"p-a2": make(chan struct{}),
	}
	started := make(chan string, 8)
	var wg sync.WaitGroup
	results := make(chan error, 8)

	// run mirrors Harness.Run's claim lifecycle: Claim, work, deferred Release.
	// A capacity refusal is queued, as the daemon's OnWake does.
	run := func(taskID string) {
		defer wg.Done()
		runID := "run-" + taskID
		err := h.Claim(context.Background(), taskID, runID, "agent")
		if errors.Is(err, ErrConcurrencyCap) {
			slots.Enqueue(taskID, h.SlotKeyForTask(context.Background(), taskID), "test", "repo")
			return
		}
		if err != nil {
			results <- err
			return
		}
		defer h.Release(taskID, runID)
		started <- taskID
		<-release[taskID]
	}
	// Queued runs are re-dispatched through Wake, exactly as the daemon does.
	slots.Wake = func(taskID, _ string) { wg.Add(1); go run(taskID) }

	wg.Add(2)
	go run("p-a1")
	go run("p-b1")
	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case id := <-started:
			got[id] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("only %v started; want p-a1 and p-b1 in parallel", got)
		}
	}
	if !got["p-a1"] || !got["p-b1"] {
		t.Fatalf("started %v, want p-a1 and p-b1", got)
	}

	// Third task shares repo A: refused and queued, not dropped.
	wg.Add(1)
	run("p-a2")
	if p := slots.Position("p-a2"); !p.Queued || p.Ahead != 0 {
		t.Fatalf("p-a2 position = %+v, want queued", p)
	}
	select {
	case id := <-started:
		t.Fatalf("%s started while repo A busy", id)
	case <-time.After(100 * time.Millisecond):
	}

	// Freeing repo B must not start p-a2.
	close(release["p-b1"])
	select {
	case id := <-started:
		t.Fatalf("%s started after repo B freed; repo A still busy", id)
	case <-time.After(200 * time.Millisecond):
	}

	// Freeing repo A starts p-a2 automatically.
	close(release["p-a1"])
	select {
	case id := <-started:
		if id != "p-a2" {
			t.Fatalf("started %s, want p-a2", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued p-a2 never started after repo A freed")
	}
	close(release["p-a2"])
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("run error: %v", err)
		}
	}
	if n := slots.Active(); n != 0 {
		t.Fatalf("slots leaked: %d active", n)
	}
}
