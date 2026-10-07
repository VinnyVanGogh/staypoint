package db

import (
	"path/filepath"
	"testing"
)

// STA-716 / STA-735: branch builds of parallel PRs migrate the live DB, so it
// can hold a higher version (another PR's 28) while this build's 27 has never
// run. A migration that is missing from schema_versions must still be applied,
// not skipped because MAX(version) is already past it.
func TestOpen_AppliesMigrationBelowMaxThatNeverRan(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gap.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	conn := store.DB()
	// Undo migration 27 and record a later version from some other branch,
	// leaving the DB as a pre-STA-744 binary would (no schema_migrations).
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS schema_migrations`,
		`ALTER TABLE board_webauthn_credentials DROP COLUMN flags_user_present`,
		`ALTER TABLE board_webauthn_credentials DROP COLUMN flags_user_verified`,
		`ALTER TABLE board_webauthn_credentials DROP COLUMN flags_backup_eligible`,
		`ALTER TABLE board_webauthn_credentials DROP COLUMN flags_backup_state`,
		`ALTER TABLE board_webauthn_credentials DROP COLUMN attestation_type`,
		`ALTER TABLE board_webauthn_credentials DROP COLUMN transports`,
		`DELETE FROM schema_versions WHERE version = 27`,
		`INSERT INTO schema_versions (version) VALUES (999)`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("seed gap (%s): %v", stmt, err)
		}
	}
	store.Close()

	store, err = Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store.Close()
	conn = store.DB()

	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM schema_versions WHERE version = 27`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("migration 27 not recorded after reopen (count=%d, err=%v)", n, err)
	}
	if err := conn.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('board_webauthn_credentials') WHERE name = 'flags_backup_eligible'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("flags_backup_eligible missing after reopen (count=%d, err=%v)", n, err)
	}
	// The unknown higher version is left alone.
	if err := conn.QueryRow(`SELECT COUNT(*) FROM schema_versions WHERE version = 999`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("version 999 row changed (count=%d, err=%v)", n, err)
	}
}

// Applying by "not yet recorded" relies on each version appearing once.
func TestMigrations_VersionsUniqueAndAscending(t *testing.T) {
	for i := 1; i < len(Migrations); i++ {
		if Migrations[i].Version <= Migrations[i-1].Version {
			t.Errorf("migration %q has version %d after %q version %d; versions must be unique and ascending",
				Migrations[i].Name, Migrations[i].Version, Migrations[i-1].Name, Migrations[i-1].Version)
		}
	}
}
