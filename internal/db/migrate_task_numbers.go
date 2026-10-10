package db

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/taskref"
)

// migrateTaskNumbers gives every task a per-organization reference
// (tasks.org_key + tasks.number, shown as STA-123) and a stored URL slug.
//
// Existing tasks are numbered per organization key in created_at order, ties
// broken by id, starting at 1. Each assignment is also written to
// task_number_map (task id -> key, number, legacy label), the record of what
// the renumber did. task_number_counters holds the last number handed out per
// key, so a new task never reuses a number, even one whose task was deleted.
//
// Nothing is rewritten: the task id stays the primary key that parent_id,
// task_relations, comments, cards, gate requests, run steps and branch names
// point at, and source_ref keeps the Paperclip label (STA-775), which still
// resolves as an alias. Reverting is dropping the three columns, the two
// tables and the index.
//
// Idempotent: only rows with no number are numbered, after the highest number
// already held for their key.
func migrateTaskNumbers(conn *sql.DB) error {
	for _, stmt := range []string{
		`ALTER TABLE tasks ADD COLUMN org_key TEXT;`,
		`ALTER TABLE tasks ADD COLUMN number INTEGER;`,
		`ALTER TABLE tasks ADD COLUMN slug TEXT NOT NULL DEFAULT '';`,
	} {
		if _, err := conn.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS task_number_counters (
			org_key     TEXT PRIMARY KEY,
			last_number INTEGER NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS task_number_map (
			task_id     TEXT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
			org_key     TEXT NOT NULL,
			number      INTEGER NOT NULL,
			legacy_ref  TEXT NOT NULL DEFAULT '',
			assigned_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		);`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	if err := numberUnnumberedTasks(tx); err != nil {
		return err
	}
	for _, stmt := range []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_org_number ON tasks (org_key, number) WHERE number IS NOT NULL;`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_source_ref ON tasks (source_ref) WHERE source_ref != '';`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func numberUnnumberedTasks(tx *sql.Tx) error {
	last := map[string]int{}
	rows, err := tx.Query(`SELECT org_key, MAX(number) FROM tasks WHERE number IS NOT NULL GROUP BY org_key`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			rows.Close()
			return err
		}
		last[k] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// A counter can be ahead of the tasks (its newest task was deleted).
	if rows, err = tx.Query(`SELECT org_key, last_number FROM task_number_counters`); err != nil {
		return err
	}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			rows.Close()
			return err
		}
		if n > last[k] {
			last[k] = n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	type pending struct{ id, org, name, sourceRef string }
	var todo []pending
	rows, err = tx.Query(`SELECT id, COALESCE(organization, ''), name, COALESCE(source_ref, '')
		FROM tasks WHERE number IS NULL ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.org, &p.name, &p.sourceRef); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, p := range todo {
		key := taskref.OrgKey(p.org)
		last[key]++
		n := last[key]
		if _, err := tx.Exec(`UPDATE tasks SET org_key = ?, number = ?, slug = ? WHERE id = ?`,
			key, n, taskref.Slugify(p.name), p.id); err != nil {
			return fmt.Errorf("number %s: %w", p.id, err)
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO task_number_map (task_id, org_key, number, legacy_ref) VALUES (?, ?, ?, ?)`,
			p.id, key, n, p.sourceRef); err != nil {
			return fmt.Errorf("map %s: %w", p.id, err)
		}
	}
	for k, n := range last {
		if _, err := tx.Exec(`INSERT INTO task_number_counters (org_key, last_number) VALUES (?, ?)
			ON CONFLICT(org_key) DO UPDATE SET last_number = MAX(last_number, excluded.last_number)`, k, n); err != nil {
			return err
		}
	}
	return nil
}
