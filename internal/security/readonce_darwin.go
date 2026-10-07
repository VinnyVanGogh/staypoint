package security

import (
	"bytes"
	"os"
	"syscall"
	"unsafe"
)

// fdPath asks the kernel for the path of the open file (F_GETPATH).
func fdPath(f *os.File) (string, error) {
	buf := make([]byte, 1024) // MAXPATHLEN
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_GETPATH, uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return "", errno
	}
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		buf = buf[:i]
	}
	return string(buf), nil
}
