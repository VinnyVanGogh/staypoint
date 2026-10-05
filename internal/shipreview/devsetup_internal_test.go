package shipreview

import (
	"testing"
	"time"
)

func currentSetup(taskID string) *devSetup {
	devServerManager.mu.Lock()
	defer devServerManager.mu.Unlock()
	return devServerManager.setups[taskID]
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// STA-649 review: overlapping restarts must leave exactly one live setup, and
// it must be the one cancelSetup (what CleanupMergedBranch calls) reaches.
// Before the fix, B registered while A waited on X, then A overwrote B and B
// survived the cleanup's cancel.
func TestBeginSetupOverlappingRestartsLeaveNoOrphan(t *testing.T) {
	const id = "task-overlap"
	x := devServerManager.beginSetup(id)

	aCh := make(chan *devSetup, 1)
	go func() { aCh <- devServerManager.beginSetup(id) }() // supersedes x, waits on x.done
	<-x.ctx.Done()

	bCh := make(chan *devSetup, 1)
	var a *devSetup
	waitUntil(t, "A to register", func() bool { a = currentSetup(id); return a != nil && a != x })
	go func() { bCh <- devServerManager.beginSetup(id) }() // supersedes A, waits on A.done
	var b *devSetup
	waitUntil(t, "B to register", func() bool { b = currentSetup(id); return b != nil && b != a })

	if x.ctx.Err() == nil || a.ctx.Err() == nil {
		t.Fatalf("superseded setups still live: x=%v a=%v", x.ctx.Err(), a.ctx.Err())
	}
	if b.ctx.Err() != nil {
		t.Fatal("latest setup cancelled")
	}

	// X and A unwind; neither may unregister or overwrite B.
	devServerManager.endSetup(id, x)
	if got := <-aCh; got != a {
		t.Fatal("beginSetup returned a different setup than it registered")
	}
	devServerManager.endSetup(id, a)
	if got := <-bCh; got != b {
		t.Fatal("beginSetup returned a different setup than it registered")
	}
	if currentSetup(id) != b {
		t.Fatal("latest setup no longer registered")
	}

	devServerManager.cancelSetup(id, 10*time.Millisecond)
	if b.ctx.Err() == nil {
		t.Fatal("cleanup's cancelSetup did not reach the latest setup")
	}
	if devServerManager.storeIfActive(id, b, nil, "", "") {
		t.Fatal("storeIfActive accepted a server from a cancelled setup")
	}
	devServerManager.endSetup(id, b)
	if currentSetup(id) != nil {
		t.Fatal("setup left registered after it ended")
	}
}
