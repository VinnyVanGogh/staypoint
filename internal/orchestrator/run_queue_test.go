package orchestrator

import (
	"errors"
	"sync"
	"testing"
)

// A newcomer in a fresh repo must not take the slot a queued run is waiting
// for: queued runs start first, in order.
func TestRunSlots_NewcomerDoesNotStealFreedSlot(t *testing.T) {
	s := NewRunSlots(1)
	s.Wake = func(string, string) {}
	_ = s.Acquire("a", SlotKey{Dir: "/r1"})
	s.Enqueue("b", SlotKey{Dir: "/r2"}, "x", WaitSlots)
	s.Release("a")
	if err := s.Acquire("c", SlotKey{Dir: "/r3"}); !errors.Is(err, ErrConcurrencyCap) {
		t.Fatalf("newcomer took the slot reserved for queued b: %v", err)
	}
	if err := s.Acquire("b", SlotKey{Dir: "/r2"}); err != nil {
		t.Fatalf("queued b should start: %v", err)
	}
}

// A run waiting on its quota pool does not hold back runs on other pools.
func TestRunSlots_QuotaWaitDoesNotReserveSlot(t *testing.T) {
	s := NewRunSlots(1)
	s.Wake = func(string, string) {}
	s.Enqueue("b", SlotKey{Dir: "/r2"}, "x", WaitQuota)
	if err := s.Acquire("c", SlotKey{Dir: "/r3"}); err != nil {
		t.Fatalf("quota-waiting run blocked a newcomer on another pool: %v", err)
	}
}

// Back-to-back releases must not dispatch the same queued run twice while
// the first dispatch has not reached Acquire yet.
func TestRunSlots_PumpDoesNotDoubleDispatch(t *testing.T) {
	s := NewRunSlots(2)
	var mu sync.Mutex
	woke := map[string]int{}
	s.Wake = func(id, _ string) { mu.Lock(); woke[id]++; mu.Unlock() }
	_ = s.Acquire("a", SlotKey{Dir: "/r1"})
	_ = s.Acquire("b", SlotKey{Dir: "/r2"})
	s.Enqueue("c", SlotKey{Dir: "/r3"}, "x", WaitSlots)
	s.Release("a")
	s.Release("b")
	mu.Lock()
	n := woke["c"]
	mu.Unlock()
	if n != 1 {
		t.Fatalf("c dispatched %d times, want 1", n)
	}
	// Once the dispatch resolves (re-queued), a later pump may retry it.
	s.Enqueue("c", SlotKey{Dir: "/r3"}, "x", WaitQuota)
	s.Pump()
	mu.Lock()
	n = woke["c"]
	mu.Unlock()
	if n != 2 {
		t.Fatalf("c dispatched %d times after re-queue, want 2", n)
	}
}

// A quota-waiting run at the head of the queue must not hold back the next
// run when a slot frees: it is probed, and the next run is dispatched too.
func TestRunSlots_PumpProbesQuotaWaitWithoutSpendingSlot(t *testing.T) {
	s := NewRunSlots(1)
	var mu sync.Mutex
	var woke []string
	s.Wake = func(id, _ string) { mu.Lock(); woke = append(woke, id); mu.Unlock() }
	_ = s.Acquire("a", SlotKey{Dir: "/r1"})
	s.Enqueue("q", SlotKey{Dir: "/r2"}, "x", WaitQuota)
	s.Enqueue("b", SlotKey{Dir: "/r3"}, "x", WaitSlots)
	s.Release("a")
	mu.Lock()
	defer mu.Unlock()
	if len(woke) != 2 || woke[0] != "q" || woke[1] != "b" {
		t.Fatalf("pump woke %v, want [q b]", woke)
	}
}

func TestWaitFor(t *testing.T) {
	if got := WaitFor(ErrRepoBusy); got != WaitRepo {
		t.Fatalf("WaitFor(ErrRepoBusy) = %q", got)
	}
	if got := WaitFor(ErrConcurrencyCap); got != WaitSlots {
		t.Fatalf("WaitFor(ErrConcurrencyCap) = %q", got)
	}
}
