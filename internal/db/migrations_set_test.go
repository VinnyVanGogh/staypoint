package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// STA-744: parallel PRs each append a migration. If the higher number merges
// (and runs) first, the lower one merged later must still be applied.

// tableMigration creates table t<version> and counts how often it ran.
func tableMigration(version int, runs map[int]int) Migration {
	return Migration{
		Version: version,
		Name:    fmt.Sprintf("create_t%d", version),
		Up: func(conn *sql.DB) error {
			runs[version]++
			_, err := conn.Exec(fmt.Sprintf("CREATE TABLE t%d (id INTEGER);", version))
			return err
		},
	}
}

func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	conn.SetMaxOpenConns(1)
	t.Cleanup(func() { conn.Close() })
	return conn
}

func appliedMigrations(t *testing.T, conn *sql.DB) map[int]string {
	t.Helper()
	rows, err := conn.Query(`SELECT version, name FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	defer rows.Close()
	got := map[int]string{}
	for rows.Next() {
		var v int
		var name string
		if err := rows.Scan(&v, &name); err != nil {
			t.Fatalf("scan schema_migrations: %v", err)
		}
		got[v] = name
	}
	return got
}

func tableExists(t *testing.T, conn *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
		t.Fatalf("sqlite_master: %v", err)
	}
	return n == 1
}

func TestApplyMigrationSet_LowerVersionMergedLaterIsApplied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gap.db")
	conn := openRaw(t, path)
	runs := map[int]int{}

	// Branch builds of #192 (26) and #196 (28) already migrated this DB.
	if err := applyMigrationSet(path, conn, []Migration{tableMigration(26, runs), tableMigration(28, runs)}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	// #193's 27 merges afterwards.
	later := []Migration{tableMigration(26, runs), tableMigration(27, runs), tableMigration(28, runs)}
	if err := applyMigrationSet(path, conn, later); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	if !tableExists(t, conn, "t27") {
		t.Fatalf("migration 27 was skipped because 28 was already applied")
	}
	if want := (map[int]int{26: 1, 27: 1, 28: 1}); !reflect.DeepEqual(runs, want) {
		t.Errorf("runs = %v, want each migration exactly once %v", runs, want)
	}
	want := map[int]string{26: "create_t26", 27: "create_t27", 28: "create_t28"}
	if got := appliedMigrations(t, conn); !reflect.DeepEqual(got, want) {
		t.Errorf("schema_migrations = %v, want %v", got, want)
	}
}

func TestApplyMigrationSet_AppliesInVersionOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "order.db")
	conn := openRaw(t, path)
	var order []int
	mk := func(v int) Migration {
		return Migration{Version: v, Name: fmt.Sprintf("m%d", v), Up: func(*sql.DB) error {
			order = append(order, v)
			return nil
		}}
	}
	// Slice order after a messy merge need not match version order.
	if err := applyMigrationSet(path, conn, []Migration{mk(3), mk(1), mk(2)}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if want := []int{1, 2, 3}; !reflect.DeepEqual(order, want) {
		t.Errorf("applied order = %v, want %v", order, want)
	}
}

func TestApplyMigrationSet_DuplicateVersionFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dup.db")
	conn := openRaw(t, path)
	runs := map[int]int{}
	a := tableMigration(26, runs)
	b := Migration{Version: 26, Name: "other_26", Up: func(*sql.DB) error { runs[-26]++; return nil }}

	err := applyMigrationSet(path, conn, []Migration{tableMigration(25, runs), a, b})
	if err == nil {
		t.Fatalf("expected an error for duplicate migration version 26")
	}
	for _, want := range []string{"duplicate migration version 26", "create_t26", "other_26"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
	if len(runs) != 0 {
		t.Errorf("no migration may run when the set is invalid, ran %v", runs)
	}
}

func TestOpen_DuplicateVersionFailsStartup(t *testing.T) {
	orig := Migrations
	defer func() { Migrations = orig }()
	last := orig[len(orig)-1]
	Migrations = append(append([]Migration{}, orig...), Migration{Version: last.Version, Name: "clash", Up: func(*sql.DB) error { return nil }})

	store, err := Open(filepath.Join(t.TempDir(), "dup_open.db"))
	if err == nil {
		store.Close()
		t.Fatalf("Open must fail when two migrations share version %d", last.Version)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("duplicate migration version %d", last.Version)) {
		t.Errorf("unexpected error: %v", err)
	}
}

// Catches two PRs that picked the same migration number, in CI.
func TestMigrations_VersionsUnique(t *testing.T) {
	if err := validateMigrations(Migrations); err != nil {
		t.Fatal(err)
	}
}

func TestValidateMigrations_RejectsNonPositiveVersion(t *testing.T) {
	err := validateMigrations([]Migration{{Version: 0, Name: "zero"}})
	if err == nil {
		t.Fatalf("expected an error for version 0")
	}
}

// A DB migrated by the old runner has only schema_versions rows. They must be
// carried into schema_migrations so nothing reruns.
func TestApplyMigrationSet_BackfillsFromLegacySchemaVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	conn := openRaw(t, path)
	if _, err := conn.Exec(`
		CREATE TABLE schema_versions (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		);
		INSERT INTO schema_versions (version, applied_at) VALUES (1, '2026-01-01T00:00:00.000Z'), (2, '2026-01-02T00:00:00.000Z'), (3, '2026-01-03T00:00:00.000Z');
		CREATE TABLE t1 (id INTEGER); CREATE TABLE t2 (id INTEGER); CREATE TABLE t3 (id INTEGER);
	`); err != nil {
		t.Fatalf("seed legacy db: %v", err)
	}

	runs := map[int]int{}
	ms := []Migration{tableMigration(1, runs), tableMigration(2, runs), tableMigration(3, runs), tableMigration(4, runs)}
	if err := applyMigrationSet(path, conn, ms); err != nil {
		t.Fatalf("apply on legacy db: %v", err)
	}

	if want := (map[int]int{4: 1}); !reflect.DeepEqual(runs, want) {
		t.Errorf("runs = %v, want only the new migration 4 to run", runs)
	}
	want := map[int]string{1: "create_t1", 2: "create_t2", 3: "create_t3", 4: "create_t4"}
	if got := appliedMigrations(t, conn); !reflect.DeepEqual(got, want) {
		t.Errorf("schema_migrations = %v, want %v", got, want)
	}
	var appliedAt string
	if err := conn.QueryRow(`SELECT applied_at FROM schema_migrations WHERE version = 2`).Scan(&appliedAt); err != nil {
		t.Fatalf("read applied_at: %v", err)
	}
	if appliedAt != "2026-01-02T00:00:00.000Z" {
		t.Errorf("backfilled applied_at = %q, want the legacy timestamp", appliedAt)
	}
	// The legacy table keeps being written so an older binary still sees the DB as current.
	var maxLegacy int
	if err := conn.QueryRow(`SELECT MAX(version) FROM schema_versions`).Scan(&maxLegacy); err != nil {
		t.Fatalf("read schema_versions: %v", err)
	}
	if maxLegacy != 4 {
		t.Errorf("schema_versions max = %d, want 4", maxLegacy)
	}
}

// The live DB was left at 1..26,28 with 27 never run. Backfill must copy the
// recorded set, not assume 1..MAX, or 27 is marked applied without running.
func TestApplyMigrationSet_BackfillKeepsLegacyGap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy_gap.db")
	conn := openRaw(t, path)
	if _, err := conn.Exec(`
		CREATE TABLE schema_versions (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		);
		INSERT INTO schema_versions (version) VALUES (1), (3);
		CREATE TABLE t1 (id INTEGER); CREATE TABLE t3 (id INTEGER);
	`); err != nil {
		t.Fatalf("seed legacy db: %v", err)
	}

	runs := map[int]int{}
	if err := applyMigrationSet(path, conn, []Migration{tableMigration(1, runs), tableMigration(2, runs), tableMigration(3, runs)}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if want := (map[int]int{2: 1}); !reflect.DeepEqual(runs, want) {
		t.Errorf("runs = %v, want only the missing migration 2", runs)
	}
	if !tableExists(t, conn, "t2") {
		t.Errorf("migration 2 should have been applied")
	}
}

// STA-744 Board: the live DB, migrated by main at 36ef95b, records 1..27 in
// schema_versions only. Opening it with the ledger must mark 1..27 applied
// and rerun nothing.
func TestOpen_BackfillsDBMigratedByOldRunner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "at27.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := store.DB().Exec(`DROP TABLE IF EXISTS schema_migrations`); err != nil {
		t.Fatalf("drop ledger: %v", err)
	}
	store.Close()

	orig := Migrations
	defer func() { Migrations = orig }()
	runs := 0
	Migrations = make([]Migration, len(orig))
	for i, m := range orig {
		up := m.Up
		m.Up = func(conn *sql.DB) error { runs++; return up(conn) }
		Migrations[i] = m
	}

	store, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store.Close()
	if runs != 0 {
		t.Errorf("%d migrations reran on a DB that had them all", runs)
	}
	got := appliedMigrations(t, store.DB())
	if len(got) != len(orig) {
		t.Errorf("schema_migrations has %d rows, want %d", len(got), len(orig))
	}
	for _, m := range orig {
		if got[m.Version] != m.Name {
			t.Errorf("schema_migrations[%d] = %q, want %q", m.Version, got[m.Version], m.Name)
		}
	}
	if _, ok := got[27]; !ok {
		t.Errorf("version 27 missing from schema_migrations: %v", got)
	}
}
