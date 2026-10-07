//go:build darwin

package geminiguard

import "syscall"

// statExtra returns the inode change time and inode number. A process cannot
// set ctime back, so an unchanged ctime proves the file was not touched.
func statExtra(sys any) (ctimeNs int64, ino uint64, ok bool) {
	st, isStat := sys.(*syscall.Stat_t)
	if !isStat {
		return 0, 0, false
	}
	return st.Ctimespec.Nano(), st.Ino, true
}
