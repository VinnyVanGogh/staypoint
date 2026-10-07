package db

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// openPre33 returns a DB path whose schema is everything up to 32: migration
// 33 is undone (tasks.origin dropped, ledger row removed) so the next Open
// runs it against rows the test seeds.
func openPre33(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "pre33.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	conn := store.DB()
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS idx_tasks_origin`,
		`ALTER TABLE tasks DROP COLUMN origin`,
		`DELETE FROM schema_versions WHERE version = 33`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("undo 33 (%s): %v", stmt, err)
		}
	}
	t.Cleanup(func() { store.Close() })
	return dbPath, conn
}

func seedTask(t *testing.T, conn *sql.DB, id, name, org, created string, extra ...string) {
	t.Helper()
	stage := "todo"
	status := "active"
	if len(extra) > 0 && extra[0] != "" {
		stage = extra[0]
	}
	if len(extra) > 1 && extra[1] != "" {
		status = extra[1]
	}
	if _, err := conn.Exec(`INSERT INTO tasks (id, name, repo_path, git_branch, status, account_role, organization, execution_stage, created_at, updated_at)
		VALUES (?, ?, '/repo', 'main', ?, 'personal', ?, ?, ?, ?)`, id, name, status, org, stage, created, created); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

func reopen(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store.DB()
}

func taskRow(t *testing.T, conn *sql.DB, id string) (status, stage, origin, org string) {
	t.Helper()
	if err := conn.QueryRow(`SELECT status, execution_stage, origin, COALESCE(organization, '') FROM tasks WHERE id = ?`, id).
		Scan(&status, &stage, &origin, &org); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return
}

func TestMigration33_MarksLegacyNormalizesOrgAndMergesDuplicates(t *testing.T) {
	dbPath, conn := openPre33(t)

	// Duplicate group "Dictated" x4: oldest keeper, one plain dup, one with a
	// work product, one with a ship card, one checked out by a run.
	seedTask(t, conn, "task-keep", "Dictated", "STA", "2026-01-01T00:00:00Z")
	seedTask(t, conn, "task-dup1", "Dictated ", "StayPoint", "2026-01-02T00:00:00Z")
	seedTask(t, conn, "task-wp", "Dictated", "StayPoint", "2026-01-03T00:00:00Z")
	seedTask(t, conn, "task-card", "Dictated", "StayPoint", "2026-01-04T00:00:00Z")
	seedTask(t, conn, "task-run", "Dictated", "StayPoint", "2026-01-05T00:00:00Z", "in_progress")
	// Case differs: not an exact duplicate.
	seedTask(t, conn, "task-case", "dictated", "StayPoint", "2026-01-06T00:00:00Z")
	// Done tasks are not merged.
	seedTask(t, conn, "task-done", "Dictated", "StayPoint", "2026-01-07T00:00:00Z", "done", "done")
	// A parent with a child is protected; the child is a lone title.
	seedTask(t, conn, "task-parent", "Parent dup", "Research", "2026-01-08T00:00:00Z")
	seedTask(t, conn, "task-parent2", "Parent dup", "Research", "2026-01-09T00:00:00Z")
	seedTask(t, conn, "task-child", "Child", "Research", "2026-01-10T00:00:00Z")
	for _, stmt := range []string{
		`UPDATE tasks SET parent_id = 'task-parent2' WHERE id = 'task-child'`,
		`INSERT INTO task_work_products (task_id, product_type, reference) VALUES ('task-wp', 'commit', 'abc123')`,
		`INSERT INTO ship_review_cards (id, task_id, branch, head_sha) VALUES ('card-1', 'task-card', 'b', 'sha')`,
		`UPDATE tasks SET checkout_run_id = 'run-1' WHERE id = 'task-run'`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("seed (%s): %v", stmt, err)
		}
	}

	conn = reopen(t, dbPath)

	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM schema_versions WHERE version = 33`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("migration 33 not recorded (n=%d err=%v)", n, err)
	}
	if err := conn.QueryRow(`SELECT COUNT(*) FROM tasks WHERE origin != 'legacy'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("every pre-existing task must be legacy, %d are not (err=%v)", n, err)
	}
	if _, _, _, org := taskRow(t, conn, "task-keep"); org != "StayPoint" {
		t.Errorf("organization STA not normalized: %q", org)
	}

	if status, _, _, _ := taskRow(t, conn, "task-keep"); status != "active" {
		t.Errorf("keeper status = %s, want active", status)
	}
	if status, stage, _, _ := taskRow(t, conn, "task-dup1"); status != "soft_deleted" || stage != "cancelled" {
		t.Errorf("plain duplicate = %s/%s, want soft_deleted/cancelled", status, stage)
	}
	var msg, author string
	if err := conn.QueryRow(`SELECT message, author FROM task_comments WHERE task_id = 'task-dup1'`).Scan(&msg, &author); err != nil {
		t.Fatalf("duplicate comment: %v", err)
	}
	if !strings.Contains(msg, "task-keep") || author != LegacyDedupeAuthor {
		t.Errorf("comment %q by %q must point at task-keep", msg, author)
	}
	for _, id := range []string{"task-wp", "task-card", "task-run", "task-case", "task-parent", "task-parent2", "task-child"} {
		if status, _, _, _ := taskRow(t, conn, id); status != "active" {
			t.Errorf("%s must be untouched, status = %s", id, status)
		}
	}
	if status, _, _, _ := taskRow(t, conn, "task-done"); status != "done" {
		t.Errorf("done task changed: %s", status)
	}

	// Idempotent: a second pass removes nothing.
	removed, err := MergeLegacyDuplicates(conn)
	if err != nil || removed != 0 {
		t.Fatalf("second merge removed %d (err=%v), want 0", removed, err)
	}

	// Tasks created after the migration default to native.
	seedTask(t, conn, "task-new", "Dictated", "StayPoint", "2026-02-01T00:00:00Z")
	if _, _, origin, _ := taskRow(t, conn, "task-new"); origin != "native" {
		t.Errorf("new task origin = %q, want native", origin)
	}
	removed, err = MergeLegacyDuplicates(conn)
	if err != nil || removed != 0 {
		t.Fatalf("native task with a legacy title must not be merged (removed %d, err=%v)", removed, err)
	}
}

// Re-running 33 when tasks.origin already exists (a branch build already
// added it) must not relabel tasks created since as legacy.
func TestMigration33_RerunKeepsNativeOrigins(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rerun.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	conn := store.DB()
	seedTask(t, conn, "task-native", "Native", "STA", "2026-03-01T00:00:00Z")
	if _, err := conn.Exec(`DELETE FROM schema_versions WHERE version = 33`); err != nil {
		t.Fatal(err)
	}
	store.Close()

	conn = reopen(t, dbPath)
	status, _, origin, org := taskRow(t, conn, "task-native")
	if origin != "native" || status != "active" {
		t.Errorf("re-run relabeled task: origin=%s status=%s", origin, status)
	}
	if org != "StayPoint" {
		t.Errorf("re-run must still normalize organization, got %q", org)
	}
}

func TestMigration35_SourceRefUnique(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "m35.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conn := store.DB()
	ins := `INSERT INTO tasks (id, name, repo_path, git_branch, account_role, source_ref, source_id, priority) VALUES (?, 'n', '', '', 'personal', ?, ?, 'high')`
	if _, err := conn.Exec(ins, "task-a", "STA-1", "uuid-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ins, "task-b", "STA-1", "uuid-1"); err == nil {
		t.Fatal("duplicate source_id accepted")
	}
	// Native tasks (empty source_id) are not constrained.
	for _, id := range []string{"task-c", "task-d"} {
		if _, err := conn.Exec(ins, id, "", ""); err != nil {
			t.Fatalf("native task %s: %v", id, err)
		}
	}
}
