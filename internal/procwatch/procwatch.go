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

// startSlack is how much later than the recorded start time a leader may
// have started and still be the recorded process. The record is taken from ps
// at spawn (or just after spawn), so the real start is never later than it;
// ps reports start times to the second.
const startSlack = time.Second

// ErrUnverified means a recorded group is alive but could not be checked
// against the record (ps failed), so it must be neither killed nor ignored.
var ErrUnverified = errors.New("process group alive but could not be verified")

// procStart reports when process pid started (see leaderStart). Swappable in
// tests.
var procStart = leaderStart

// StartTime returns when process pid started, as ps reports it. ok is false
// when it could not be read. Record it at spawn so Verify compares ps against
// ps.
func StartTime(pid int) (time.Time, bool) {
	t, found, err := procStart(pid)
	return t, found && err == nil
}

// Verify reports whether the group led by pid, recorded as started at
// startedAt, is still running and is the same group. It returns
// ErrUnverified when the group is alive but the check itself failed.
//
// The check is one-sided: a reused pid always belongs to a process that
// started after the recorded one, so a leader that started at or before the
// record (plus startSlack) is the recorded process. Load or a DST change
// between the record and the check cannot make the real process look reused.
func Verify(pid int, startedAt time.Time) (bool, error) {
	if pid <= 1 || !GroupAlive(pid) {
		return false, nil
	}
	start, found, err := procStart(pid)
	if err != nil {
		return false, ErrUnverified
	}
	if !found {
		if PidAlive(pid) {
			// ps saw nothing, yet the leader exists: not a verdict.
			return false, ErrUnverified
		}
		// Leader gone, members left: the group id cannot have been reused.
		return true, nil
	}
	if startedAt.IsZero() {
		return false, ErrUnverified
	}
	return !start.After(startedAt.Add(startSlack)), nil
}

// SameProcess reports whether pid is alive and is the process recorded as
// started at startedAt (same one-sided rule as Verify). A zero startedAt
// falls back to whether pid is alive.
func SameProcess(pid int, startedAt time.Time) bool {
	if !PidRunning(pid) {
		return false
	}
	if startedAt.IsZero() {
		return true
	}
	start, found, err := procStart(pid)
	if err != nil || !found {
		return true // alive and unreadable: do not take its runs from it
	}
	return !start.After(startedAt.Add(startSlack))
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
