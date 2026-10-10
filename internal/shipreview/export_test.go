package shipreview

import (
	"context"
	"os"
	"testing"
	"time"
)

// SetDevConfigStatForTest swaps the stat lookupDevConfig uses to compare repo
// paths, and its stat deadline, for the rest of the test.
func SetDevConfigStatForTest(t testing.TB, stat func(string) (os.FileInfo, error), timeout time.Duration) {
	t.Helper()
	oldStat, oldTimeout := statPath, devConfigStatTimeout
	statPath, devConfigStatTimeout = stat, timeout
	clearDevConfigStatCache()
	t.Cleanup(func() {
		statPath, devConfigStatTimeout = oldStat, oldTimeout
		clearDevConfigStatCache()
	})
}

// SetDevConfigStatTTLForTest sets how long lookups reuse a stat result, for
// the rest of the test. Zero disables reuse.
func SetDevConfigStatTTLForTest(t testing.TB, ttl time.Duration) {
	t.Helper()
	old := devConfigStatTTL
	devConfigStatTTL = ttl
	clearDevConfigStatCache()
	t.Cleanup(func() {
		devConfigStatTTL = old
		clearDevConfigStatCache()
	})
}

func clearDevConfigStatCache() {
	devConfigStatCache.Lock()
	clear(devConfigStatCache.m)
	devConfigStatCache.Unlock()
}

// ForceMergeWorktreeFallbackForTest makes Approve merge in a scratch worktree,
// as on git older than 2.38, for the rest of the test.
func ForceMergeWorktreeFallbackForTest(t testing.TB) {
	t.Helper()
	old := mergeTreeSupported
	mergeTreeSupported = func(context.Context, string) bool { return false }
	t.Cleanup(func() { mergeTreeSupported = old })
}
