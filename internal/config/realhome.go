package config

import (
	"errors"
	"os/user"
	"path/filepath"
)

// RealHomeDir is the account's home directory from the user database, not
// $HOME. A process told HOME=/tmp/x would otherwise load /tmp/x's
// config.toml (Board review #2 H1); on darwin os/user asks the directory
// service through libSystem, with or without cgo.
func RealHomeDir() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	if u.HomeDir == "" || !filepath.IsAbs(u.HomeDir) {
		return "", errors.New("user database has no absolute home directory for the current user")
	}
	return filepath.Clean(u.HomeDir), nil
}
