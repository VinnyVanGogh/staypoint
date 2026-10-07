//go:build !darwin && !linux

package geminiguard

// statExtra is unavailable here: every file is content-checked.
func statExtra(any) (int64, uint64, bool) { return 0, 0, false }

// statLinks is unavailable here.
func statLinks(any) (uint64, uint64) { return 0, 0 }
