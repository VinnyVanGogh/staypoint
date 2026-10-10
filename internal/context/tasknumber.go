package context

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/taskref"
)

// nextTaskNumber reserves the next number for an organization key inside tx.
// The counter row is written first, so the transaction takes SQLite's write
// lock before it reads anything: two concurrent creates queue on the lock and
// get distinct numbers, and a number is never handed out twice (the counter
// only moves forward, even when its newest task is deleted). A key with no
// counter yet starts after its highest numbered task.
func nextTaskNumber(tx *sql.Tx, key string) (int, error) {
	var n int
	err := tx.QueryRow(`
		INSERT INTO task_number_counters (org_key, last_number)
		VALUES (?, COALESCE((SELECT MAX(number) FROM tasks WHERE org_key = ?), 0) + 1)
		ON CONFLICT(org_key) DO UPDATE SET last_number = last_number + 1
		RETURNING last_number`, key, key).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("reserve task number for %s: %w", key, err)
	}
	return n, nil
}

// ErrTaskRefNotFound means a reference (STA-123) names no task, by number or
// by legacy label.
var ErrTaskRefNotFound = errors.New("task reference not found")

// ResolveTaskRef maps a reference to the internal task id. The current
// numbering wins: STA-775 is the task numbered 775. Only when no task holds
// that number does it fall back to a legacy label, the Paperclip identifier an
// imported task keeps in source_ref. legacy reports that fallback.
func ResolveTaskRef(db *sql.DB, ref string) (id string, legacy bool, err error) {
	key, n, ok := taskref.Parse(ref)
	if !ok {
		return "", false, fmt.Errorf("%w: %q is not a reference like STA-123", ErrTaskRefNotFound, ref)
	}
	err = db.QueryRow(`SELECT id FROM tasks WHERE org_key = ? AND number = ?`, key, n).Scan(&id)
	if err == nil {
		return id, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	// Oldest first: a label imported twice (archive + open) resolves stably.
	err = db.QueryRow(`SELECT id FROM tasks WHERE source_ref = ? COLLATE NOCASE ORDER BY created_at ASC, id ASC LIMIT 1`,
		taskref.Format(key, n)).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("%w: %s", ErrTaskRefNotFound, taskref.Format(key, n))
	}
	return "", false, err
}

// ResolveTaskID returns the internal id for anything a caller may type where
// a task id goes: a reference (STA-123, or a legacy label) becomes its task
// id; everything else (task-xxxxxxxx, an id prefix, a name) is returned
// unchanged for the caller's own lookup.
func ResolveTaskID(db *sql.DB, s string) string {
	s = strings.TrimSpace(s)
	if _, _, ok := taskref.Parse(s); !ok {
		return s
	}
	if id, _, err := ResolveTaskRef(db, s); err == nil {
		return id
	}
	return s
}

// fillRef sets the reference fields derived from the stored key and number.
func (t *Task) fillRef(key string, number int, slug string) {
	if key == "" || number <= 0 {
		return
	}
	t.OrgKey, t.Number, t.Slug = key, number, slug
	t.Identifier = taskref.Format(key, number)
	t.URL = taskref.URL(t.Identifier, slug)
}

// Ref is the task's reference (STA-123), or its id when it has none.
func (t *Task) Ref() string {
	if t.Identifier != "" {
		return t.Identifier
	}
	return t.ID
}
