package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Zero-kill deploys (task-db71fba9): reinstall-daemon.sh drains the running
// daemon before it swaps the binary. The stub curl (reinstall_script_test.go)
// plays the daemon's /api/daemon/drain.

const stubBoardToken = "board-secret-0123456789"

func (f *deployFixture) setLiveSeq(counts ...string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.state, "live_seq"), []byte(strings.Join(counts, "\n")+"\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *deployFixture) drainCalls() string {
	return f.read(filepath.Join(f.state, "drain_calls"))
}

func repeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// Failure cases first.

func TestReinstallDrain_OldDaemonWithoutDrainIsRefused(t *testing.T) {
	f := newDeployFixture(t)
	f.seedLiveBinary()
	out, code := f.run([]string{"STUB_DRAIN=old"})
	f.assertRefused(out, code, "drain-unsupported", "has no drain mode")
	if !strings.Contains(out, "--now") {
		t.Errorf("refusal does not say how to deploy anyway:\n%s", out)
	}
}

func TestReinstallDrain_WrongBoardTokenIsRefused(t *testing.T) {
	f := newDeployFixture(t)
	f.seedLiveBinary()
	out, code := f.run([]string{"STUB_DRAIN=forbidden"})
	f.assertRefused(out, code, "drain-forbidden", "refused the drain request")
}

func TestReinstallDrain_BadMaxWaitIsUsageError(t *testing.T) {
	for _, arg := range [][]string{{"--max-wait"}, {"--max-wait", "soon"}, {"--max-wait=1h30m"}} {
		f := newDeployFixture(t)
		f.seedLiveBinary()
		out, code := f.run(nil, arg...)
		if code != 2 {
			t.Fatalf("%v: exit code = %d, want 2\n%s", arg, code, out)
		}
		if got := f.installedBinary(); got != "old binary\n" {
			t.Errorf("%v: live binary was replaced", arg)
		}
		if f.drainCalls() != "" {
			t.Errorf("%v: drained on a usage error:\n%s", arg, f.drainCalls())
		}
	}
}

// Ctrl-C while waiting must hand the daemon back to normal work (DELETE) and
// leave the old binary in place.
func TestReinstallDrain_InterruptCancelsDrain(t *testing.T) {
	f := newDeployFixture(t)
	f.seedLiveBinary()
	f.setLiveSeq(repeat("1", 2000)...)
	cmd := exec.Command("bash", filepath.Join(f.repo, "scripts", "reinstall-daemon.sh"))
	cmd.Dir = f.repo
	cmd.Env = f.env
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(f.read(filepath.Join(f.state, "binary_during_drain")), "old binary") {
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("script never polled the drain:\n%s", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = cmd.Process.Signal(os.Interrupt)
	_ = cmd.Wait()
	if !strings.Contains(f.drainCalls(), "DELETE") {
		t.Errorf("interrupted deploy did not cancel the drain:\n%s\n%s", f.drainCalls(), out.String())
	}
	if got := f.installedBinary(); got != "old binary\n" {
		t.Errorf("interrupted deploy replaced the live binary")
	}
}

func TestReinstallDrain_WaitsForLiveRunsBeforeSwap(t *testing.T) {
	f := newDeployFixture(t)
	f.seedLiveBinary()
	f.setLiveSeq("2", "2", "1", "0")
	out, code := f.run(nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(f.drainCalls(), `"mode":"finish"`) {
		t.Errorf("did not ask the daemon to drain (finish):\n%s", f.drainCalls())
	}
	if strings.Contains(f.drainCalls(), "DELETE") {
		t.Errorf("a successful deploy cancelled its drain:\n%s", f.drainCalls())
	}
	during := f.read(filepath.Join(f.state, "binary_during_drain"))
	if n := strings.Count(during, "old binary"); n != 4 {
		t.Errorf("polls saw the old binary %d times, want all 4 (swap must wait):\n%q", n, during)
	}
	if got := f.installedBinary(); got == "old binary\n" {
		t.Errorf("binary not swapped after the drain")
	}
	if !strings.Contains(out, "Draining for deploy: 2 run(s) left (task-a, task-b), 2 queued") {
		t.Errorf("progress line missing:\n%s", out)
	}
	log := f.deployLog()
	if !strings.Contains(log, "result=installed") || !strings.Contains(log, "drain=ok") {
		t.Errorf("deploys.log lacks the drained install:\n%s", log)
	}
	plist := f.read(filepath.Join(f.home, "Library", "LaunchAgents", "com.staypoint.daemon.plist"))
	if !strings.Contains(plist, "<key>ExitTimeOut</key>\n    <integer>86400</integer>") {
		t.Errorf("plist lacks the long ExitTimeOut:\n%s", plist)
	}
}

func TestReinstallDrain_MaxWaitSuspendsAtTurnBoundary(t *testing.T) {
	f := newDeployFixture(t)
	f.seedLiveBinary()
	// ~3s of one live run, then idle: past --max-wait 1.
	f.setLiveSeq(append(repeat("1", 60), "0")...)
	out, code := f.run(nil, "--max-wait", "1")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	calls := f.drainCalls()
	if !strings.Contains(calls, `"mode":"finish"`) || !strings.Contains(calls, `"mode":"boundary"`) {
		t.Errorf("want a finish drain escalated to boundary:\n%s", calls)
	}
	if strings.Count(calls, `"mode":"boundary"`) != 1 {
		t.Errorf("boundary requested more than once:\n%s", calls)
	}
	if !strings.Contains(f.deployLog(), "drain=suspended") {
		t.Errorf("deploys.log lacks drain=suspended:\n%s", f.deployLog())
	}
}

func TestReinstallDrain_NowModeAsksForNow(t *testing.T) {
	f := newDeployFixture(t)
	f.seedLiveBinary()
	f.setLiveSeq("1", "0")
	out, code := f.run(nil, "--now")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(f.drainCalls(), `"mode":"now"`) {
		t.Errorf("--now did not ask for an immediate suspend:\n%s", f.drainCalls())
	}
	if !strings.Contains(f.deployLog(), "drain=now") {
		t.Errorf("deploys.log lacks drain=now:\n%s", f.deployLog())
	}
}

func TestReinstallDrain_NowDeploysOverOldDaemon(t *testing.T) {
	f := newDeployFixture(t)
	f.seedLiveBinary()
	out, code := f.run([]string{"STUB_DRAIN=old"}, "--now")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(f.deployLog(), "drain=unsupported") {
		t.Errorf("deploys.log lacks drain=unsupported:\n%s", f.deployLog())
	}
}

func TestReinstallDrain_NoDaemonRunningSkipsDrain(t *testing.T) {
	f := newDeployFixture(t)
	f.seedLiveBinary()
	out, code := f.run([]string{"STUB_DOWN=1"})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if f.drainCalls() != "" {
		t.Errorf("drained a daemon that was not running:\n%s", f.drainCalls())
	}
	if !strings.Contains(f.deployLog(), "drain=not-running") {
		t.Errorf("deploys.log lacks drain=not-running:\n%s", f.deployLog())
	}
}

// The Board token authorises the drain; it must never be on a command line
// (any local process can read argv) or in the output, and neither may a
// tokenized Web UI URL.
func TestReinstallDrain_TokensNeverInArgvOrOutput(t *testing.T) {
	f := newDeployFixture(t)
	f.seedLiveBinary()
	f.setLiveSeq("0")
	out, code := f.run(nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if args := f.read(filepath.Join(f.state, "curl_args")); strings.Contains(args, stubBoardToken) {
		t.Errorf("board token passed in curl argv:\n%s", args)
	}
	if !strings.Contains(f.read(filepath.Join(f.state, "curl_stdin")), "Cookie: staypoint_board="+stubBoardToken) {
		t.Errorf("drain request did not carry the Board cookie on stdin")
	}
	if strings.Contains(out, stubBoardToken) || strings.Contains(out, "?token=") {
		t.Errorf("output leaks a token or tokenized URL:\n%s", out)
	}
}
