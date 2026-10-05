//go:build windows

package repoaccess

import (
	"io/fs"
	"os/exec"
	"strconv"
	"syscall"
)

func ownProcessGroup(cmd *exec.Cmd) {}

func killProbe(cmd *exec.Cmd) { _ = cmd.Process.Kill() }

func errnoName(e syscall.Errno) string { return "errno " + strconv.Itoa(int(e)) }

func ownerOf(fi fs.FileInfo) *Owner { return nil }
