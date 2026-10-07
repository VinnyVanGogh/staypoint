package security

import (
	"fmt"
	"os"
)

// fdPath reads the open file's path from /proc.
func fdPath(f *os.File) (string, error) {
	return os.Readlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
}
