// Package gitexec runs git with a deadline the daemon can rely on.
//
// A git child can block forever without using any CPU: on macOS, a launchd
// daemon that touches a repo under ~/Documents, ~/Desktop or ~/Downloads waits
// in open() until someone answers a privacy prompt (STA-685). Every git call
// the daemon makes goes through Command so that wait becomes an error instead
// of a hung request.
package gitexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultTimeout bounds a git call whose context has no deadline.
const DefaultTimeout = 10 * time.Second

// TimeoutEnv overrides DefaultTimeout (a Go duration, e.g. "30s").
const TimeoutEnv = "STAYPOINT_GIT_TIMEOUT"

// waitDelay is how long Run waits for a killed git's pipes to close. Without
// it, a grandchild that inherited stdout keeps the call blocked after git dies.
const waitDelay = 2 * time.Second

// ErrTimeout is wrapped by every error from a git call that ran out of time.
var ErrTimeout = errors.New("git timed out")

// IsTimeout reports whether err came from a git call that ran out of time.
func IsTimeout(err error) bool { return errors.Is(err, ErrTimeout) }

// Timeout returns the deadline applied when the caller's context has none.
func Timeout() time.Duration {
	if v := os.Getenv(TimeoutEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return DefaultTimeout
}

// Cmd is an exec.Cmd for git. Set Dir, Env, Stdin, Stdout and Stderr on it as
// usual; Run, Output and CombinedOutput translate a missed deadline into an
// error wrapping ErrTimeout.
type Cmd struct {
	*exec.Cmd
	ctx    context.Context
	cancel context.CancelFunc
	args   []string
	start  time.Time
}

// Command returns git with args, bounded by ctx's deadline, or by Timeout()
// counted from this call when ctx has none.
func Command(ctx context.Context, args ...string) *Cmd {
	cancel := context.CancelFunc(func() {})
	if _, ok := ctx.Deadline(); !ok {
		ctx, cancel = context.WithTimeout(ctx, Timeout())
	}
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args come from daemon code, not shell input
	cmd.WaitDelay = waitDelay
	return &Cmd{Cmd: cmd, ctx: ctx, cancel: cancel, args: args, start: time.Now()}
}

// Run starts git and waits for it.
func (c *Cmd) Run() error {
	defer c.cancel()
	return c.wrap(c.Cmd.Run())
}

// Output runs git and returns its stdout.
func (c *Cmd) Output() ([]byte, error) {
	defer c.cancel()
	out, err := c.Cmd.Output()
	return out, c.wrap(err)
}

// CombinedOutput runs git and returns stdout and stderr together.
func (c *Cmd) CombinedOutput() ([]byte, error) {
	defer c.cancel()
	out, err := c.Cmd.CombinedOutput()
	return out, c.wrap(err)
}

// Wait waits for a git started with Start.
func (c *Cmd) Wait() error {
	defer c.cancel()
	return c.wrap(c.Cmd.Wait())
}

func (c *Cmd) wrap(err error) error {
	if err == nil {
		return nil
	}
	ctxErr := c.ctx.Err()
	if ctxErr == nil {
		return err
	}
	dir := c.Dir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return fmt.Errorf("%w: git %s blocked for %s in %s (possible causes: macOS privacy prompt pending, file provider, network mount): %v",
			ErrTimeout, strings.Join(c.args, " "), time.Since(c.start).Round(time.Millisecond), dir, err)
	}
	return fmt.Errorf("git %s in %s: %w", strings.Join(c.args, " "), dir, ctxErr)
}
