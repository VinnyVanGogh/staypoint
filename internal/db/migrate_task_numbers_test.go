package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// fixtureTasks is a pre-46 database: two organizations, out-of-order inserts,
// a created_at tie, imported Paperclip labels (one equal to a number the
// renumber will hand out), a parent/child pair, a blocker and a cancelled task.
var fixtureTasks = []struct{ id, name, org, created, sourceRef, parent string }{
	{"task-00000005", "Fifth StayPoint", "StayPoint", "2026-10-05T00:00:00.000Z", "", ""},
	{"task-00000001", "First StayPoint", "StayPoint", "2026-10-01T00:00:00.000Z", "STA-775", ""},
	{"task-0000000b", "Tie b", "StayPoint", "2026-10-03T00:00:00.000Z", "", "task-00000001"},
	{"task-0000000a", "Tie a", "StayPoint", "2026-10-03T00:00:00.000Z", "STA-2", "task-00000001"},
	{"task-m0000002", "Second MAN", "Managed Solution", "2026-10-02T00:00:00.000Z", "MAN-602", ""},
	{"task-m0000001", "First MAN", "Managed Solution", "2026-09-30T00:00:00.000Z", "", ""},
	{"task-n0000001", "No org", "", "2026-10-04T00:00:00.000Z", "", ""},
	{"task-r0000001", "Research one", "Research", "2026-10-06T00:00:00.000Z", "", ""},
}

// Want: dense per key, created_at order, ties by id; no org files under STA.
var wantNumbers = map[string]struct {
	key string
	n   int
}{
	"task-00000001": {"STA", 1},
	"task-0000000a": {"STA", 2},
	"task-0000000b": {"STA", 3},
	"task-n0000001": {"STA", 4},
	"task-00000005": {"STA", 5},
	"task-m0000001": {"MAN", 1},
	"task-m0000002": {"MAN", 2},
	"task-r0000001": {"RES", 1},
}

func seedPre46(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "numbers.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	conn := store.DB()
	revertTaskNumbers(t, conn)
	for _, f := range fixtureTasks {
		var parent any
		if f.parent != "" {
			parent = f.parent
		}
		var org any
		if f.org != "" {
			org = f.org
		}
		if _, err := conn.Exec(`INSERT INTO tasks (id, name, repo_path, organization, created_at, source_ref, parent_id)
			VALUES (?, ?, '/tmp/r', ?, ?, ?, ?)`, f.id, f.name, org, f.created, f.sourceRef, parent); err != nil {
			t.Fatalf("seed %s: %v", f.id, err)
		}
	}
	for _, stmt := range []string{
		`UPDATE tasks SET status = 'soft_deleted' WHERE id = 'task-00000005'`,
		`INSERT INTO task_relations (task_id, blocks_id) VALUES ('task-0000000a', 'task-0000000b')`,
		`INSERT INTO task_comments (task_id, author, message) VALUES ('task-0000000b', 'board', 'hi')`,
		`INSERT INTO task_work_products (task_id, product_type, reference) VALUES ('task-m0000002', 'branch', 'staypoint/task-m0000002')`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	store.Close()
	return dbPath
}

// revertTaskNumbers is migration 46's reverse: drop what it added and forget
// it ran. Nothing else changes, so the old ids and labels are all still there.
func revertTaskNumbers(t *testing.T, conn *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS idx_tasks_org_number`,
		`DROP INDEX IF EXISTS idx_tasks_source_ref`,
		`DROP TABLE IF EXISTS task_number_map`,
		`DROP TABLE IF EXISTS task_number_counters`,
		`ALTER TABLE tasks DROP COLUMN org_key`,
		`ALTER TABLE tasks DROP COLUMN number`,
		`ALTER TABLE tasks DROP COLUMN slug`,
		`DELETE FROM schema_versions WHERE version = 46`,
		`DELETE FROM schema_migrations WHERE version = 46`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

func readNumbers(t *testing.T, conn *sql.DB) map[string]struct {
	key string
	n   int
} {
	t.Helper()
	got := map[string]struct {
		key string
		n   int
	}{}
	rows, err := conn.Query(`SELECT id, org_key, number FROM tasks`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, key string
		var n int
		if err := rows.Scan(&id, &key, &n); err != nil {
			t.Fatal(err)
		}
		got[id] = struct {
			key string
			n   int
		}{key, n}
	}
	return got
}

func TestMigration46_RenumbersDensePerOrgAndKeepsRelations(t *testing.T) {
	dbPath := seedPre46(t)
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen (runs 46): %v", err)
	}
	defer store.Close()
	conn := store.DB()

	got := readNumbers(t, conn)
	if len(got) != len(wantNumbers) {
		t.Fatalf("numbered %d tasks, want %d: %v", len(got), len(wantNumbers), got)
	}
	for id, w := range wantNumbers {
		if got[id] != w {
			t.Errorf("%s = %s-%d, want %s-%d", id, got[id].key, got[id].n, w.key, w.n)
		}
	}

	// Every relation still points at the same internal ids.
	var parentOK, relOK, commentOK, productOK int
	conn.QueryRow(`SELECT COUNT(*) FROM tasks WHERE parent_id = 'task-00000001'`).Scan(&parentOK)
	conn.QueryRow(`SELECT COUNT(*) FROM task_relations WHERE task_id = 'task-0000000a' AND blocks_id = 'task-0000000b'`).Scan(&relOK)
	conn.QueryRow(`SELECT COUNT(*) FROM task_comments WHERE task_id = 'task-0000000b'`).Scan(&commentOK)
	conn.QueryRow(`SELECT COUNT(*) FROM task_work_products WHERE task_id = 'task-m0000002' AND reference = 'staypoint/task-m0000002'`).Scan(&productOK)
	if parentOK != 2 || relOK != 1 || commentOK != 1 || productOK != 1 {
		t.Fatalf("relations changed: children=%d relation=%d comment=%d product=%d", parentOK, relOK, commentOK, productOK)
	}

	// The legacy label is kept as-is next to the new number, and mapped.
	var ref, legacy string
	conn.QueryRow(`SELECT source_ref FROM tasks WHERE id = 'task-00000001'`).Scan(&ref)
	conn.QueryRow(`SELECT legacy_ref FROM task_number_map WHERE task_id = 'task-00000001'`).Scan(&legacy)
	if ref != "STA-775" || legacy != "STA-775" {
		t.Fatalf("legacy label source_ref=%q map=%q, want STA-775", ref, legacy)
	}
	var mapped int
	conn.QueryRow(`SELECT COUNT(*) FROM task_number_map`).Scan(&mapped)
	if mapped != len(wantNumbers) {
		t.Fatalf("task_number_map has %d rows, want %d", mapped, len(wantNumbers))
	}

	// Counters sit at the highest number per key.
	for key, want := range map[string]int{"STA": 5, "MAN": 2, "RES": 1} {
		var last int
		conn.QueryRow(`SELECT last_number FROM task_number_counters WHERE org_key = ?`, key).Scan(&last)
		if last != want {
			t.Errorf("counter %s = %d, want %d", key, last, want)
		}
	}

	var slug string
	conn.QueryRow(`SELECT slug FROM tasks WHERE id = 'task-m0000001'`).Scan(&slug)
	if slug != "first-man" {
		t.Fatalf("slug = %q, want first-man", slug)
	}

	// The unique index refuses a duplicate (org, number).
	if _, err := conn.Exec(`UPDATE tasks SET number = 1 WHERE id = 'task-m0000002'`); err == nil {
		t.Fatal("duplicate MAN-1 accepted; want unique index violation")
	}
}

func TestMigration46_IdempotentAndReversible(t *testing.T) {
	dbPath := seedPre46(t)
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	conn := store.DB()
	first := readNumbers(t, conn)

	// Re-running numbers nothing new.
	if err := migrateTaskNumbers(conn); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if again := readNumbers(t, conn); len(again) != len(first) {
		t.Fatalf("rerun changed numbering: %v -> %v", first, again)
	} else {
		for id, v := range first {
			if again[id] != v {
				t.Fatalf("rerun renumbered %s: %v -> %v", id, v, again[id])
			}
		}
	}

	// Revert, then migrate again: the same numbers come back.
	revertTaskNumbers(t, conn)
	store.Close()
	store, err = Open(dbPath)
	if err != nil {
		t.Fatalf("reopen after revert: %v", err)
	}
	defer store.Close()
	for id, v := range readNumbers(t, store.DB()) {
		if first[id] != v {
			t.Fatalf("after revert+migrate %s = %v, want %v", id, v, first[id])
		}
	}
}

// A task inserted with no number after 46 ran (an older binary) is numbered
// after the highest existing number on the next run, never reusing one, even
// when that number's task was deleted.
func TestMigration46_LateRowsNumberAfterCounter(t *testing.T) {
	dbPath := seedPre46(t)
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conn := store.DB()
	if _, err := conn.Exec(`DELETE FROM tasks WHERE id = 'task-00000005'`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO tasks (id, name, repo_path, organization) VALUES ('task-late0001', 'Late', '/tmp/r', 'StayPoint')`); err != nil {
		t.Fatal(err)
	}
	if err := migrateTaskNumbers(conn); err != nil {
		t.Fatal(err)
	}
	if got := readNumbers(t, conn)["task-late0001"]; got.key != "STA" || got.n != 6 {
		t.Fatalf("late task = %s-%d, want STA-6 (STA-5 was deleted, never reused)", got.key, got.n)
	}
}
