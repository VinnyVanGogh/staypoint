//go:build !windows

package procwatch

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestSleepHelper is the fake agent process: macOS hides the environment of
// its own platform binaries (/bin/sleep) from ps, but not of other programs
// (an agent CLI, or this test binary).
func TestSleepHelper(t *testing.T) {
	if os.Getenv("PROCWATCH_SLEEP") == "" {
		t.Skip("helper process")
	}
	time.Sleep(5 * time.Minute)
}

func startIn(t *testing.T, dir string, setpgid bool, env ...string) (*exec.Cmd, time.Time) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSleepHelper$")
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "PROCWATCH_SLEEP=1"), env...)
	if setpgid {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	return cmd, at
}

func TestAgentsInDir_MatchesOnlyMarkedHeadless(t *testing.T) {
	dir := t.TempDir()
	other, _ := startIn(t, dir, false, "STAYPOINT_TASK_ID=task-other")
	agent, _ := startIn(t, dir, false, "STAYPOINT_TASK_ID=task-x")
	deadline := time.Now().Add(15 * time.Second)
	var got []int
	for time.Now().Before(deadline) {
		var err error
		got, err = AgentsInDir(dir, "STAYPOINT_TASK_ID=task-x")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(got) != 1 || got[0] != agent.Process.Pid {
		out, _ := exec.Command("ps", "eww", "-o", "pid=,tty=,command=", "-p", strconv.Itoa(agent.Process.Pid)).CombinedOutput()
		t.Fatalf("AgentsInDir = %v, want [%d] (not %d)\nps: %s", got, agent.Process.Pid, other.Process.Pid, out)
	}
}

func TestVerifyAndStopGroup(t *testing.T) {
	cmd, at := startIn(t, t.TempDir(), true)
	pid := cmd.Process.Pid
	if ours, err := Verify(pid, at); err != nil || !ours {
		t.Fatalf("Verify(own group) = %v, %v; want true", ours, err)
	}
	if ours, err := Verify(pid, at.Add(-time.Hour)); err != nil || ours {
		t.Fatalf("Verify(wrong start time) = %v, %v; want false (pid reused)", ours, err)
	}
	if !StopGroup(pid, time.Second) {
		t.Fatal("StopGroup did not stop the group")
	}
	if ours, _ := Verify(pid, at); ours {
		t.Fatal("stopped group still verifies as alive")
	}
}
