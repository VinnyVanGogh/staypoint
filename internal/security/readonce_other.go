//go:build !darwin && !linux

package security

import (
	"errors"
	"os"
)

// fdPath is unsupported here: fail closed (scripts stay opaque).
func fdPath(*os.File) (string, error) { return "", errors.New("fd path lookup unsupported") }
