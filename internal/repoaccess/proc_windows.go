//go:build windows

package repoaccess

import "os/exec"

func ownProcessGroup(cmd *exec.Cmd) {}

func killProbe(cmd *exec.Cmd) { _ = cmd.Process.Kill() }
