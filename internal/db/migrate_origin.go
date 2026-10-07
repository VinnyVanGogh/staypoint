package db

import (
	"database/sql"
	"fmt"
)

// LegacyDedupeAuthor is the task_comments author of the comment left on a
// legacy duplicate soft-deleted by migration 33.
const LegacyDedupeAuthor = "staypoint-migration"

// migrateTaskOrigins is migration 33 (backlog stage + task origins):
//
//  1. adds tasks.origin (native | paperclip_import | legacy) and marks every
//     task that already exists as legacy;
//  2. normalizes organization 'STA' to 'StayPoint';
//  3. merges exact duplicate titles among active legacy tasks.
//
// Every step is idempotent. Step 1 marks rows legacy only when this run adds
// the column, so a re-run never relabels tasks created after it.
func migrateTaskOrigins(conn *sql.DB) error {
	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var has int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name = 'origin'`).Scan(&has); err != nil {
		return fmt.Errorf("inspect tasks.origin: %w", err)
	}
	if has == 0 {
		if _, err := tx.Exec(`ALTER TABLE tasks ADD COLUMN origin TEXT NOT NULL DEFAULT 'native'`); err != nil {
			return fmt.Errorf("add tasks.origin: %w", err)
		}
		if _, err := tx.Exec(`UPDATE tasks SET origin = 'legacy'`); err != nil {
			return fmt.Errorf("mark legacy tasks: %w", err)
		}
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_tasks_origin ON tasks (origin)`); err != nil {
		return fmt.Errorf("index tasks.origin: %w", err)
	}
	if err := NormalizeTaskOrganizations(tx); err != nil {
		return err
	}
	if _, err := MergeLegacyDuplicates(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// execQuerier is the subset of *sql.DB / *sql.Tx the cleanup steps use.
type execQuerier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
}

// organizationAliases maps short or legacy organization names to the
// canonical StayPoint organization name.
var organizationAliases = map[string]string{
	"STA": "StayPoint",
}

// NormalizeTaskOrganizations rewrites aliased organization names. Idempotent.
func NormalizeTaskOrganizations(q execQuerier) error {
	for from, to := range organizationAliases {
		if _, err := q.Exec(`UPDATE tasks SET organization = ? WHERE organization = ?`, to, from); err != nil {
			return fmt.Errorf("normalize organization %s: %w", from, err)
		}
	}
	return nil
}

// protectedTaskWhere matches tasks a duplicate merge must never touch: any
// recorded work product (commit, branch, PR, file), a ship review card, a
// worktree base, a live checkout or running stage, or child tasks.
const protectedTaskWhere = `(
	EXISTS (SELECT 1 FROM task_work_products w WHERE w.task_id = t.id)
	OR EXISTS (SELECT 1 FROM ship_review_cards c WHERE c.task_id = t.id)
	OR EXISTS (SELECT 1 FROM task_worktree_bases b WHERE b.task_id = t.id)
	OR EXISTS (SELECT 1 FROM tasks k WHERE k.parent_id = t.id AND k.status != 'soft_deleted')
	OR COALESCE(t.checkout_run_id, '') != ''
	OR t.execution_stage IN ('in_progress', 'paused')
)`

// MergeLegacyDuplicates soft-deletes active legacy tasks whose trimmed name
// exactly matches an older active legacy task. The oldest task of each group
// is the keeper; each removed duplicate gets a comment naming it. Protected
// tasks (protectedTaskWhere) are never removed. Returns the number removed.
// Idempotent: a second run finds no groups.
func MergeLegacyDuplicates(q execQuerier) (int, error) {
	rows, err := q.Query(`
		SELECT t.id, TRIM(t.name), ` + protectedTaskWhere + `
		FROM tasks t
		WHERE t.status = 'active' AND t.origin = 'legacy'
		  AND TRIM(t.name) IN (
		      SELECT TRIM(name) FROM tasks
		      WHERE status = 'active' AND origin = 'legacy'
		      GROUP BY TRIM(name) HAVING COUNT(*) > 1)
		ORDER BY TRIM(t.name), t.created_at ASC, t.rowid ASC`)
	if err != nil {
		return 0, fmt.Errorf("find legacy duplicates: %w", err)
	}
	type dup struct {
		id, keeper string
	}
	var drop []dup
	keeper := map[string]string{}
	for rows.Next() {
		var id, name string
		var protected bool
		if err := rows.Scan(&id, &name, &protected); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan legacy duplicate: %w", err)
		}
		k, seen := keeper[name]
		if !seen {
			keeper[name] = id
			continue
		}
		if !protected {
			drop = append(drop, dup{id: id, keeper: k})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, d := range drop {
		if _, err := q.Exec(`UPDATE tasks SET status = 'soft_deleted', execution_stage = 'cancelled',
			deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			WHERE id = ?`, d.id); err != nil {
			return 0, fmt.Errorf("soft-delete duplicate %s: %w", d.id, err)
		}
		msg := fmt.Sprintf("Duplicate of %s (same title, older). Soft-deleted by the legacy cleanup; continue on %s.", d.keeper, d.keeper)
		if _, err := q.Exec(`INSERT INTO task_comments (task_id, author, message) VALUES (?, ?, ?)`, d.id, LegacyDedupeAuthor, msg); err != nil {
			return 0, fmt.Errorf("comment on duplicate %s: %w", d.id, err)
		}
	}
	return len(drop), nil
}
