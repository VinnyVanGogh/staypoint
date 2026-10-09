//go:build windows

package archive

// lockDir is a no-op on Windows; the archiver only runs on the Board's Mac.
func lockDir(string) (func(), error) { return func() {}, nil }
