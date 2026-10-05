package db

import (
	"fmt"
	"os"
)

// lockMigrations takes an exclusive cross-process lock on <dbPath>.migrate.lock
// so only one opener checks and applies pending migrations at a time. Without
// it the daemon and a hook process opening during a deploy both ran the same
// Up, and the loser restored the backup over the winner's live DB (STA-759).
// The lock file is left in place: removing it would let a waiter lock an
// unlinked inode while a new opener locks a fresh file.
func lockMigrations(dbPath string) (unlock func(), err error) {
	f, err := os.OpenFile(dbPath+".migrate.lock", os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open migration lock: %w", err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("failed to acquire migration lock: %w", err)
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}
