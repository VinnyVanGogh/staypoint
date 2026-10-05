//go:build !windows

package repoaccess

import (
	"io/fs"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// ownProcessGroup puts a probe in its own process group so a timeout kills
// it and anything it started.
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

func errnoName(e syscall.Errno) string {
	if n := unix.ErrnoName(e); n != "" {
		return n
	}
	return "errno " + e.Error()
}

func ownerOf(fi fs.FileInfo) *Owner {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return &Owner{UID: int(st.Uid), Mode: fi.Mode()}
}
