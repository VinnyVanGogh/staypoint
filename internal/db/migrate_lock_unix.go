//go:build !windows

package db

import (
	"os"

	"golang.org/x/sys/unix"
)

// flock, not fcntl: flock locks belong to the open file description, so two
// Opens in the same process also exclude each other.
func lockFile(f *os.File) error {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX)
		if err != unix.EINTR {
			return err
		}
	}
}

func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
