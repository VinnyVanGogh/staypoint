package db

import (
	"path/filepath"
	"testing"
)

// STA-861 migration 38: task_work_products accepts 'doc', and rebuilding the
// table keeps existing rows.
func TestMigration38_WorkProductDocType(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "wp.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	conn := store.DB()
	// Put the table back in its pre-38 shape with a row in it.
	for _, stmt := range []string{
		`INSERT INTO tasks (id, name, repo_path, git_branch, status, account_role) VALUES ('task-wp', 'wp', '/r', 'main', 'active', 'personal')`,
		`DROP TABLE task_work_products`,
		`CREATE TABLE task_work_products (id INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE, product_type TEXT NOT NULL CHECK (product_type IN ('pull_request', 'commit', 'branch', 'workspace_file')), reference TEXT NOT NULL, created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')))`,
		`INSERT INTO task_work_products (task_id, product_type, reference) VALUES ('task-wp', 'commit', 'abc123')`,
		`DELETE FROM schema_versions WHERE version = 38`,
		`DELETE FROM schema_migrations WHERE version = 38`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, err := conn.Exec(`INSERT INTO task_work_products (task_id, product_type, reference) VALUES ('task-wp', 'doc', 'x')`); err == nil {
		t.Fatal("pre-38 table should refuse 'doc'")
	}
	store.Close()

	store, err = Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store.Close()
	conn = store.DB()
	var ref string
	if err := conn.QueryRow(`SELECT reference FROM task_work_products WHERE task_id = 'task-wp' AND product_type = 'commit'`).Scan(&ref); err != nil || ref != "abc123" {
		t.Fatalf("existing row after rebuild: %q, %v", ref, err)
	}
	if _, err := conn.Exec(`INSERT INTO task_work_products (task_id, product_type, reference) VALUES ('task-wp', 'doc', 'https://example.com/report')`); err != nil {
		t.Fatalf("doc after 38: %v", err)
	}
	if _, err := conn.Exec(`INSERT INTO task_work_products (task_id, product_type, reference) VALUES ('task-wp', 'tweet', 'x')`); err == nil {
		t.Error("unknown type must still be refused")
	}
}
