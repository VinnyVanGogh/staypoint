// Package procwatch tracks the agent CLI processes a run spawns, so the daemon
// can find and stop them after it restarts (task-db71fba9; task-ae1414b0).
//
// Every agent CLI starts in its own process group (adapter.setProcessGroup),
// with the CLI as the group leader, so its pid is also the group id. The
// harness records that pid and the time it started. A later daemon stops the
// group only when it can show the group is still the one it recorded: the
// leader's start time matches, or the leader is gone but members of the group
// are left (a group id is not reused while any member is alive).
package procwatch

import (
	"context"
	"errors"
	"time"
)

type spawnHookKey struct{}

// WithSpawnHook returns a ctx whose agent spawns report their pid to fn.
func WithSpawnHook(ctx context.Context, fn func(pid int)) context.Context {
	return context.WithValue(ctx, spawnHookKey{}, fn)
}

// Spawned reports pid to the spawn hook in ctx, if any. The adapter calls it
// right after an agent CLI starts.
func Spawned(ctx context.Context, pid int) {
	if fn, ok := ctx.Value(spawnHookKey{}).(func(int)); ok && fn != nil && pid > 0 {
		fn(pid)
	}
}

// startSlack is how far a leader's start time may be from the recorded one.
// ps reports start times to the second.
const startSlack = 3 * time.Second

// ErrUnverified means a recorded group is alive but could not be checked
// against the record (ps failed), so it must be neither killed nor ignored.
var ErrUnverified = errors.New("process group alive but could not be verified")

// Verify reports whether the group led by pid, recorded as started at
// startedAt, is still running and is the same group. It returns
// ErrUnverified when the group is alive but the check itself failed.
func Verify(pid int, startedAt time.Time) (bool, error) {
	if pid <= 1 || !GroupAlive(pid) {
		return false, nil
	}
	start, found, err := leaderStart(pid)
	if err != nil {
		return false, ErrUnverified
	}
	if !found {
		// Leader gone, members left: the group id cannot have been reused.
		return true, nil
	}
	if startedAt.IsZero() {
		return false, ErrUnverified
	}
	d := start.Sub(startedAt)
	if d < 0 {
		d = -d
	}
	return d <= startSlack, nil
}

// StopGroup sends SIGTERM to the group, waits up to grace for it to exit,
// then sends SIGKILL. It returns true when the group is gone.
func StopGroup(pgid int, grace time.Duration) bool {
	if pgid <= 1 {
		return true
	}
	signalGroup(pgid, false)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !GroupAlive(pgid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	signalGroup(pgid, true)
	for i := 0; i < 40; i++ {
		if !GroupAlive(pgid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return !GroupAlive(pgid)
}
