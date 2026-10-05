package db

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeLiveHome points the guard at a throwaway "real" home so the test never
// goes near ~/.staypoint, and re-homes HOME elsewhere the way isolated tests do.
func fakeLiveHome(t *testing.T) string {
	t.Helper()
	live := t.TempDir()
	prev := liveStateDirs
	liveStateDirs = []string{
		filepath.Join(live, ".staypoint"),
		filepath.Join(live, ".agent-mesh"),
	}
	t.Cleanup(func() { liveStateDirs = prev })
	t.Setenv("HOME", t.TempDir())
	return live
}

func TestOpenRefusesLiveDBUnderTest(t *testing.T) {
	live := fakeLiveHome(t)

	for _, p := range []string{
		filepath.Join(live, ".staypoint", "staypoint.db"),
		filepath.Join(live, ".staypoint", "mesh.db"),
		filepath.Join(live, ".agent-mesh", "agent-mesh.db"),
		filepath.Join(live, "other", "..", ".staypoint", "staypoint.db"),
	} {
		store, err := Open(p)
		if err == nil {
			store.Close()
			t.Errorf("Open(%q) = nil error, want refusal of the live database", p)
			continue
		}
		if _, statErr := os.Stat(filepath.Clean(p)); !os.IsNotExist(statErr) {
			t.Errorf("Open(%q) refused but still created the file (stat err %v)", p, statErr)
		}
	}
	if _, err := os.Stat(filepath.Join(live, ".staypoint")); !os.IsNotExist(err) {
		t.Errorf("refused Open still created the live data dir (stat err %v)", err)
	}
}

func TestOpenAllowsPathsOutsideLiveDirs(t *testing.T) {
	live := fakeLiveHome(t)

	for _, p := range []string{
		filepath.Join(t.TempDir(), "staypoint.db"),
		// A re-homed test's own ~/.staypoint is not the live one.
		filepath.Join(os.Getenv("HOME"), ".staypoint", "staypoint.db"),
		// Prefix match must stop at a path separator.
		filepath.Join(live, ".staypoint-copy", "staypoint.db"),
	} {
		store, err := Open(p)
		if err != nil {
			t.Errorf("Open(%q) refused: %v", p, err)
			continue
		}
		store.Close()
	}
}

func TestLiveStateDirsResolvedFromProcessHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	want := filepath.Join(home, ".staypoint")
	for _, d := range liveStateDirs {
		if d == want {
			return
		}
	}
	t.Errorf("liveStateDirs = %v, want it to include %s", liveStateDirs, want)
}
