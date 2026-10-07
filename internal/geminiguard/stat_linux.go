//go:build linux

package geminiguard

import "syscall"

// statExtra returns the inode change time and inode number. A process cannot
// set ctime back, so an unchanged ctime proves the file was not touched.
func statExtra(sys any) (ctimeNs int64, ino uint64, ok bool) {
	st, isStat := sys.(*syscall.Stat_t)
	if !isStat {
		return 0, 0, false
	}
	return st.Ctim.Nano(), uint64(st.Ino), true
}

// statLinks returns the hard-link count and device id (0, 0 when unknown).
func statLinks(sys any) (nlink, dev uint64) {
	st, isStat := sys.(*syscall.Stat_t)
	if !isStat {
		return 0, 0
	}
	return uint64(st.Nlink), uint64(st.Dev) //nolint:unconvert // width differs by arch
}
