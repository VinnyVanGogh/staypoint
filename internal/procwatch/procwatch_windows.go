//go:build windows

package procwatch

import (
	"errors"
	"time"
)

// Windows has no process groups to record; the daemon runs on macOS.

func GroupAlive(int) bool { return false }

func PidAlive(int) bool { return false }

func signalGroup(int, bool) {}

func leaderStart(int) (time.Time, bool, error) {
	return time.Time{}, false, errors.New("not supported on windows")
}

func AgentsInDir(string, string) ([]int, error) { return nil, nil }
