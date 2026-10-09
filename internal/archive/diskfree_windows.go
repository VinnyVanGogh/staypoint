//go:build windows

package archive

import "errors"

func diskFree(string) (uint64, error) { return 0, errors.New("not supported") }
