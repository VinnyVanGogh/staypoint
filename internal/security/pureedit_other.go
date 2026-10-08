//go:build !unix

package security

import "os"

// singleLink cannot read the link count here: never relax.
func singleLink(os.FileInfo) bool { return false }
