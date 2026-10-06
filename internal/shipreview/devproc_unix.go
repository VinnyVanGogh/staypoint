//go:build !windows

package shipreview

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// devStopGrace is how long a stopped dev server's process group gets to exit
// on SIGTERM before it is SIGKILLed. Variable so tests can shorten it.
var devStopGrace = 3 * time.Second

// startDevInOwnGroup puts the dev server shell in its own process group, so
// stopping it reaches everything the dev command spawned (npm → node → vite),
// not just /bin/sh (STA-727).
func startDevInOwnGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killDevProcessGroup sends SIGTERM to the dev server's process group, waits
// up to devStopGrace for the group to exit, then SIGKILLs whatever is left.
// The group id is the shell's pid (Setpgid), and it stays valid as long as
// any member is alive, even after the shell itself has been reaped.
func killDevProcessGroup(p *os.Process) {
	pgid := p.Pid
	if pgid <= 0 {
		return
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		// ESRCH: the group is already gone. Anything else (EPERM, or a server
		// started before group launch existed) falls back to the shell alone.
		if err != syscall.ESRCH {
			_ = p.Kill()
		}
		return
	}
	deadline := time.Now().Add(devStopGrace)
	for time.Now().Before(deadline) {
		if syscall.Kill(-pgid, 0) == syscall.ESRCH {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}
