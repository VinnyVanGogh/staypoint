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
	t.Cleanup(func() { statPath, devConfigStatTimeout = oldStat, oldTimeout })
}
