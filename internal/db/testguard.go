package db

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// refuseLiveDBUnderTest stops a test binary from opening, and so migrating,
// the live database. A test that resolves the default path without
// re-homing HOME used to apply its branch's migrations to
// ~/.staypoint/staypoint.db, leaving gaps in the live schema_versions.
func refuseLiveDBUnderTest(dbPath string) error {
	if !testing.Testing() {
		return nil
	}
	target := pathForms(dbPath)
	for _, dir := range liveStateDirs {
		for _, d := range pathForms(dir) {
			for _, p := range target {
				if p == d || strings.HasPrefix(p, d+string(filepath.Separator)) {
					return fmt.Errorf("refusing to open live StayPoint database %s under go test: set HOME to t.TempDir() or pass a temp path", dbPath)
				}
			}
		}
	}
	return nil
}

// pathForms returns p as an absolute clean path and, when an ancestor
// exists, with that ancestor's symlinks resolved (/var -> /private/var).
func pathForms(p string) []string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return []string{filepath.Clean(p)}
	}
	forms := []string{abs}
	rest := ""
	for dir := abs; ; dir = filepath.Dir(dir) {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			if r := filepath.Join(resolved, rest); r != abs {
				forms = append(forms, r)
			}
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		rest = filepath.Join(filepath.Base(dir), rest)
	}
	return forms
}
