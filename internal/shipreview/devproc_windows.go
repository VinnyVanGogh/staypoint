//go:build windows

package shipreview

import (
	"os"
	"os/exec"
)

// startDevInOwnGroup is a no-op on Windows; there is no process group to kill.
func startDevInOwnGroup(cmd *exec.Cmd) {}

// killDevProcessGroup kills the dev server shell only.
func killDevProcessGroup(p *os.Process) { _ = p.Kill() }
