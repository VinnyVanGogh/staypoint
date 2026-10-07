package shipreview

import (
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
