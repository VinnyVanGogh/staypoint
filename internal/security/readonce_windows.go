package security

import "errors"

// readResolved is unsupported on Windows: scripts stay opaque (fail closed).
func readResolved(string) ([]byte, string, error) {
	return nil, "", errors.New("script reading unsupported on windows")
}
