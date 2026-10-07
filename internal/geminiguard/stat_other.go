//go:build !darwin && !linux

package geminiguard

// statExtra is unavailable here: every file is content-checked.
func statExtra(any) (int64, uint64, bool) { return 0, 0, false }
