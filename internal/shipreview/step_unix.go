//go:build !windows

package shipreview

import (
	"os/exec"
	"syscall"
)

// killStepGroupOnCancel runs a setup step in its own process group and makes
// context cancellation kill the whole group, so a cancelled `a; b` step does
// not leave b running in a worktree that is about to be removed.
func killStepGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
