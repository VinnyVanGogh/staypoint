//go:build windows

package procwatch

import (
	"errors"
	"time"
)

// Windows has no process groups to record; the daemon runs on macOS.

func GroupAlive(int) bool { return false }

func PidAlive(int) bool { return false }

// Supported is false: the harness must not act on these stubs (a turn guard
// would read every agent as exited and cut its turn).
const Supported = false

func PidRunning(int) bool { return false }

func signalGroup(int, bool) {}

func leaderStart(int) (time.Time, bool, error) {
	return time.Time{}, false, errors.New("not supported on windows")
}

func AgentsInDir(string, string) ([]int, error) { return nil, nil }
