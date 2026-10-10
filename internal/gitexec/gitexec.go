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

// DefaultTimeout bounds a quick git call (a read such as rev-parse, status or
// worktree prune) whose context has no deadline.
const DefaultTimeout = 10 * time.Second

// DefaultSlowTimeout bounds a git call that may touch the network or write a
// whole tree (see TimeoutFor) when its context has no deadline. A worktree
// holding node_modules can take longer than DefaultTimeout just to delete
// (STA-710).
const DefaultSlowTimeout = 2 * time.Minute

// TimeoutEnv overrides DefaultTimeout (a Go duration, e.g. "30s").
const TimeoutEnv = "STAYPOINT_GIT_TIMEOUT"

// SlowTimeoutEnv overrides DefaultSlowTimeout (a Go duration, e.g. "5m").
const SlowTimeoutEnv = "STAYPOINT_GIT_SLOW_TIMEOUT"

// waitDelay is how long Run waits for a killed git's pipes to close. Without
// it, a grandchild that inherited stdout keeps the call blocked after git dies.
const waitDelay = 2 * time.Second

// ErrTimeout is wrapped by every error from a git call that ran out of time.
var ErrTimeout = errors.New("git timed out")

// IsTimeout reports whether err came from a git call that ran out of time.
func IsTimeout(err error) bool { return errors.Is(err, ErrTimeout) }

// Timeout returns the deadline for a quick git call: DefaultTimeout, or
// TimeoutEnv when set.
func Timeout() time.Duration { return envDuration(TimeoutEnv, DefaultTimeout) }

// SlowTimeout returns the deadline for a slow git call: DefaultSlowTimeout, or
// SlowTimeoutEnv when set.
func SlowTimeout() time.Duration { return envDuration(SlowTimeoutEnv, DefaultSlowTimeout) }

func envDuration(name string, def time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

// slowCommands may talk to a remote or rewrite the working tree, so they get
// SlowTimeout. Everything else is a local read or a small ref/config write.
var slowCommands = map[string]bool{
	"clone": true, "fetch": true, "pull": true, "push": true, "ls-remote": true,
	"checkout": true, "switch": true, "restore": true, "reset": true, "clean": true,
	"merge": true, "merge-tree": true, "rebase": true, "cherry-pick": true, "revert": true, "stash": true,
	"commit": true, "gc": true, "repack": true, "submodule": true, "lfs": true,
}

// slowWorktreeCommands are the `git worktree` subcommands that write or delete
// a whole tree. `worktree list` and `worktree prune` stay quick.
var slowWorktreeCommands = map[string]bool{"add": true, "remove": true, "move": true, "repair": true}

// TimeoutFor returns the deadline Command applies to git with args when the
// caller's context has none: SlowTimeout for commands that touch the network
// or a whole working tree (fetch, push, worktree add/remove, ...), Timeout for
// the rest.
func TimeoutFor(args ...string) time.Duration {
	sub, rest := subcommand(args)
	if slowCommands[sub] {
		return SlowTimeout()
	}
	if sub == "worktree" && len(rest) > 0 && slowWorktreeCommands[rest[0]] {
		return SlowTimeout()
	}
	return Timeout()
}

// subcommand skips git's global options (-C <dir>, -c <k=v>, --git-dir=...)
// and returns the subcommand and the arguments after it.
func subcommand(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree" || a == "--namespace":
			i++ // the option's value
		case strings.HasPrefix(a, "-"):
		default:
			return a, args[i+1:]
		}
	}
	return "", nil
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

// Command returns git with args, bounded by ctx's deadline, or by
// TimeoutFor(args...) counted from this call when ctx has none.
func Command(ctx context.Context, args ...string) *Cmd {
	cancel := context.CancelFunc(func() {})
	if _, ok := ctx.Deadline(); !ok {
		ctx, cancel = context.WithTimeout(ctx, TimeoutFor(args...))
	}
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args come from daemon code, not shell input
	cmd.WaitDelay = waitDelay
	return &Cmd{Cmd: cmd, ctx: ctx, cancel: cancel, args: args, start: time.Now()}
}

// Run starts git and waits for it. A git call that writes the repo's shared
// state holds that repo's lock while it runs (see repolock.go).
func (c *Cmd) Run() error {
	defer c.cancel()
	unlock, err := lockRepo(c.ctx, c.Dir, c.args)
	if err != nil {
		return err
	}
	defer unlock()
	return c.wrap(c.Cmd.Run())
}

// Output runs git and returns its stdout, locking like Run.
func (c *Cmd) Output() ([]byte, error) {
	defer c.cancel()
	unlock, err := lockRepo(c.ctx, c.Dir, c.args)
	if err != nil {
		return nil, err
	}
	defer unlock()
	out, err := c.Cmd.Output()
	return out, c.wrap(err)
}

// CombinedOutput runs git and returns stdout and stderr together, locking
// like Run.
func (c *Cmd) CombinedOutput() ([]byte, error) {
	defer c.cancel()
	unlock, err := lockRepo(c.ctx, c.Dir, c.args)
	if err != nil {
		return nil, err
	}
	defer unlock()
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
