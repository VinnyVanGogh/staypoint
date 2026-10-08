package db

import (
	"path/filepath"
	"strings"
	"testing"
)

// decisionLogV39 is decision_log as migration 39 created it.
const decisionLogV39 = `CREATE TABLE decision_log (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	subject_kind   TEXT NOT NULL,
	subject_id     TEXT NOT NULL,
	advisor        TEXT NOT NULL,
	model          TEXT NOT NULL DEFAULT '',
	recommendation TEXT NOT NULL DEFAULT '',
	reason         TEXT NOT NULL DEFAULT '',
	latency_ms     INTEGER NOT NULL DEFAULT 0,
	error          TEXT NOT NULL DEFAULT '',
	final_decision TEXT NOT NULL DEFAULT '',
	decided_by     TEXT NOT NULL DEFAULT '',
	decided_at     TEXT,
	created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
)`

// STA-433.1 migration 42: decision_log gains the shadow-decision columns and a
// (subject_kind, created_at) index; rows written before 42 keep their values
// and read empty strings in the new columns.
func TestMigration42_DecisionLogShadowColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "dl.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	conn := store.DB()
	for _, stmt := range []string{
		`DROP TABLE decision_log`,
		decisionLogV39,
		`CREATE INDEX idx_decision_log_subject ON decision_log (subject_kind, subject_id, advisor, id)`,
		`INSERT INTO decision_log (subject_kind, subject_id, advisor, recommendation, final_decision, decided_by)
			VALUES ('security_gate', 'gate-1', 'together', 'approved', 'approved', 'board')`,
		`DELETE FROM schema_versions WHERE version = 42`,
		`DELETE FROM schema_migrations WHERE version = 42`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	store.Close()

	store, err = Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store.Close()
	conn = store.DB()

	var rec, key, task, question, options, pick string
	if err := conn.QueryRow(`SELECT recommendation, decision_key, task_id, question, options_json, pick
		FROM decision_log WHERE subject_id = 'gate-1'`).Scan(&rec, &key, &task, &question, &options, &pick); err != nil {
		t.Fatalf("old row after 42: %v", err)
	}
	if rec != "approved" || key != "" || task != "" || question != "" || options != "" || pick != "" {
		t.Fatalf("old row = rec %q key %q task %q question %q options %q pick %q", rec, key, task, question, options, pick)
	}

	// New columns are NOT NULL: an explicit NULL is refused, omitted means ''.
	if _, err := conn.Exec(`INSERT INTO decision_log (subject_kind, subject_id, advisor, pick) VALUES ('governance', 't', 'current', NULL)`); err == nil {
		t.Error("pick must be NOT NULL")
	}
	if _, err := conn.Exec(`INSERT INTO decision_log (subject_kind, subject_id, advisor, decision_key, task_id, question, options_json, pick)
		VALUES ('governance', 'task-1', 'current', 'k1', 'task-1', 'move to done?', '["yes","no"]', 'yes')`); err != nil {
		t.Fatalf("insert with new columns: %v", err)
	}

	var idxSQL string
	if err := conn.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_decision_log_kind_created'`).Scan(&idxSQL); err != nil {
		t.Fatalf("kind/created index missing: %v", err)
	}
	var plan string
	if err := conn.QueryRow(`EXPLAIN QUERY PLAN SELECT id FROM decision_log WHERE subject_kind = 'governance' ORDER BY created_at DESC LIMIT 10`).Scan(new(int), new(int), new(int), &plan); err != nil {
		t.Fatal(err)
	}
	if want := "idx_decision_log_kind_created"; !strings.Contains(plan, want) {
		t.Errorf("query plan %q does not use %s", plan, want)
	}
}

// Rerunning 42 on a DB that already has the columns (ledger lost, schema kept)
// must not fail on duplicate columns.
func TestMigration42_Rerun(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "dl.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DELETE FROM schema_versions WHERE version = 42`,
		`DELETE FROM schema_migrations WHERE version = 42`,
	} {
		if _, err := store.DB().Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	store.Close()
	store, err = Open(dbPath)
	if err != nil {
		t.Fatalf("reopen with 42 rerun: %v", err)
	}
	store.Close()
}
