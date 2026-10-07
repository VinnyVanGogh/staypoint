//go:build unix

package security

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// readResolved opens an already-resolved path without following a final
// symlink (one swapped in after resolution makes the open fail).
func readResolved(resolved string) ([]byte, string, error) {
	fd, err := syscall.Open(resolved, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	f := os.NewFile(uintptr(fd), resolved)
	defer f.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return nil, "", err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return nil, "", ErrNotRegular
	}
	if st.Size > maxScriptBytes {
		return nil, "", ErrTooLarge
	}
	real, err := fdPath(f)
	if err != nil {
		return nil, "", fmt.Errorf("locate opened script: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxScriptBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > maxScriptBytes {
		return nil, "", ErrTooLarge
	}
	return data, filepath.Clean(real), nil
}
