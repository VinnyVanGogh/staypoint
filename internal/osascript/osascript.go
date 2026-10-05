// Package osascript runs AppleScript through osascript and reports failures
// with the exit status and stderr instead of dropping them.
//
// From the launchd daemon, osascript output is attributed to osascript/Script
// Editor. Notifications are silently discarded when that app lacks
// notification permission or Focus is on, and dialogs fail when there is no GUI
// session, so callers must look at the error.
package osascript

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// binary is the osascript executable. Tests point it at a fake.
var binary = "osascript"

// Error is a failed osascript run.
type Error struct {
	ExitCode int    // -1 when osascript never started or was killed
	Stderr   string // trimmed
	Err      error
}

func (e *Error) Error() string {
	var b strings.Builder
	if e.ExitCode >= 0 {
		fmt.Fprintf(&b, "osascript exit %d", e.ExitCode)
	} else {
		fmt.Fprintf(&b, "osascript: %v", e.Err)
	}
	if e.Stderr != "" {
		b.WriteString(": ")
		b.WriteString(e.Stderr)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

func newError(err error, stderr *bytes.Buffer) error {
	code := -1
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() >= 0 {
		code = ee.ExitCode()
	}
	return &Error{ExitCode: code, Stderr: strings.TrimSpace(stderr.String()), Err: err}
}

// Run runs script and waits for osascript to exit.
func Run(ctx context.Context, script string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, binary, "-e", script)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return newError(err, &stderr)
	}
	return nil
}

// Start runs a script that stays up while it waits on the user, such as a
// modal display dialog. It waits up to grace for osascript to fail: a denied
// permission or missing GUI session exits at once, so a process still running
// after grace has put its dialog on screen and Start returns nil. If osascript
// later exits with an error, that error goes to late (when non-nil).
func Start(script string, grace time.Duration, late func(error)) error {
	var stderr bytes.Buffer
	cmd := exec.Command(binary, "-e", script)
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return newError(err, &stderr)
	}
	done := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		if err != nil {
			err = newError(err, &stderr)
		}
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(grace):
	}
	go func() {
		if err := <-done; err != nil && late != nil {
			late(err)
		}
	}()
	return nil
}

// Quote returns s as an AppleScript string literal.
func Quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
