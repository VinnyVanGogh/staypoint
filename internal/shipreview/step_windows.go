//go:build windows

package shipreview

import "os/exec"

// killStepGroupOnCancel keeps exec's default cancellation (kill the shell).
func killStepGroupOnCancel(cmd *exec.Cmd) {}
