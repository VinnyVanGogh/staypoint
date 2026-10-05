package db

import (
	"os"
	"path/filepath"
)

// liveStateDirs are the real StayPoint data directories, resolved from HOME
// once at process start. Tests that re-home with t.Setenv("HOME", ...) run
// later, so this still names the live install after they do.
var liveStateDirs = defaultLiveStateDirs()

func defaultLiveStateDirs() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, ".staypoint"),
		filepath.Join(home, ".agent-mesh"),
	}
}
