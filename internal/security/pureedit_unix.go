//go:build unix

package security

import (
	"os"
	"syscall"
)

// singleLink reports whether fi is a file with exactly one hard link, so a
// write to it cannot change a file elsewhere.
func singleLink(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Nlink == 1
}
