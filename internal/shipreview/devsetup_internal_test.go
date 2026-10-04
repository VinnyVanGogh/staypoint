package shipreview

import (
	"context"
	"os"
	"testing"
	"time"
)

// STA-648 #4: a setup that outlives the cleanup wait is still marked canceled,
// so it can never register a dev server afterwards.
func TestCancelSetupsTimeoutStillBlocksLaunch(t *testing.T) {
	m := &procManager{
		procs:     make(map[string]*os.Process),
		repoPaths: make(map[string]string),
		worktrees: make(map[string]string),
		setups:    make(map[string]map[*devSetup]struct{}),
	}
	ctx, s := m.beginSetup("t1")

	if m.cancelSetups(context.Background(), "t1", 10*time.Millisecond) {
		t.Fatal("cancelSetups reported done while the setup was still running")
	}
	if ctx.Err() == nil {
		t.Error("setup context not canceled")
	}
	if m.storeIfActive(s, "t1", &os.Process{Pid: -1}, "/repo", "/repo/.worktrees/devserver-t1") {
		t.Error("canceled setup was allowed to register its process")
	}
	if m.has("t1") {
		t.Error("process registered for canceled setup")
	}

	m.endSetup("t1", s)
	if !m.cancelSetups(context.Background(), "t1", time.Second) {
		t.Error("cancelSetups with no setups in flight should report done")
	}
	if len(m.setups) != 0 {
		t.Errorf("setups not cleared: %v", m.setups)
	}
}
