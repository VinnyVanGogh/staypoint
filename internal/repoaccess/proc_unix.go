//go:build !windows

package repoaccess

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup puts the probe in its own process group so a timeout kills
// the shell and whichever of its children is stuck in open().
func ownProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func killProbe(cmd *exec.Cmd) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}
