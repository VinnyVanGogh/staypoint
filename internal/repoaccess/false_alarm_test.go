package repoaccess_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/repoaccess"
)

// Each test here is a way the checker could tell the Board "macOS permission
// needed" when macOS is not the problem. A false alarm trains the Board to
// ignore the real one.

// On Linux, /bin/sh is dash, and its cd error ("can't cd to <path>") is the
// same for a missing path and a denied one. A probe that fails without saying
// why must not be read as a permission denial.
func TestProbe_UnexplainedFailureIsNotPermission(t *testing.T) {
	dashLike := func(path string) *exec.Cmd {
		return exec.Command("/bin/sh", "-c", `echo "staypoint-repo-probe: 1: cd: can't cd to $1" >&2; exit 2`, "probe", path)
	}
	r := repoaccess.Probe(context.Background(), filepath.Join(t.TempDir(), "gone"), 3*time.Second, dashLike)
	if r.Status == repoaccess.StatusOK {
		t.Fatalf("a failed probe reported ok")
	}
	if r.PermissionNeeded() {
		t.Fatalf("status = %q (%s): an unexplained failure was reported as a macOS permission problem", r.Status, r.Detail)
	}
}

// A fork/exec failure (e.g. EAGAIN under heavy load) says nothing about the repo.
func TestProbe_StartFailureIsNotPermission(t *testing.T) {
	noBinary := func(path string) *exec.Cmd {
		return exec.Command(filepath.Join(t.TempDir(), "no-such-probe-binary"), path)
	}
	r := repoaccess.Probe(context.Background(), t.TempDir(), 3*time.Second, noBinary)
	if r.Status == repoaccess.StatusOK {
		t.Fatalf("a probe that never started reported ok")
	}
	if r.PermissionNeeded() {
		t.Fatalf("status = %q (%s): a start failure was reported as a macOS permission problem", r.Status, r.Detail)
	}
}

// Daemon shutdown cancels ctx while probes are running. Those probes did not
// time out; they were abandoned. The Board must not hear "access lost".
func TestChecker_CancelledCheckDoesNotNotify(t *testing.T) {
	slow := func(path string) *exec.Cmd { return exec.Command("sleep", "5") }
	rec := &recorder{}
	c := &repoaccess.Checker{
		Timeout: 3 * time.Second,
		Command: slow,
		Notify:  rec.notify,
		Publish: rec.publish,
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	start := time.Now()
	c.Check(ctx, []string{t.TempDir(), t.TempDir()})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Check took %v after cancel; it should return promptly", elapsed)
	}

	notices, published := rec.snapshot()
	if len(notices) != 0 || len(published) != 0 {
		t.Fatalf("cancelled check notified the Board: notices=%q published=%q", notices, published)
	}
	if snap := c.Snapshot(); snap.Checked || len(snap.Inaccessible) != 0 {
		t.Fatalf("cancelled check recorded results: %+v", snap)
	}
}
