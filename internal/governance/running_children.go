package governance

import (
	"database/sql"
	"strconv"
	"strings"
)

// tasks.max_running_children caps how many children of one parent hold a run
// at once. It is enforced in the claim SQL, so it holds whatever origin a
// child was created with (a child created by a spoofed CLI session still waits
// for a sibling to finish). 0 means no limit.
const (
	SettingMaxRunningChildren = "tasks.max_running_children"
	DefaultMaxRunningChildren = 3
)

// RunningChildLimit reads tasks.max_running_children, falling back to the
// default when unset or unparsable.
func RunningChildLimit(db *sql.DB) int {
	var raw string
	if err := db.QueryRow(`SELECT value FROM settings_kv WHERE key = ?`, SettingMaxRunningChildren).Scan(&raw); err != nil {
		return DefaultMaxRunningChildren
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return DefaultMaxRunningChildren
	}
	return n
}

// SiblingsUnderRunLimitSQL is a WHERE fragment true when the tasks row
// referenced as table (e.g. "tasks") has no parent, or fewer than limit of its
// siblings are running (checked out and in_progress). Requiring in_progress
// keeps a stale checkout (a run that crashed after its task left
// in_progress, which RecoveryScan does not clear) from holding the parent
// busy forever. limit <= 0 disables the check.
func SiblingsUnderRunLimitSQL(table string, limit int) string {
	if limit <= 0 {
		return "1=1"
	}
	return `(COALESCE(` + table + `.parent_id, '') = '' OR (SELECT COUNT(*) FROM tasks rs WHERE rs.parent_id = ` + table +
		`.parent_id AND rs.id != ` + table + `.id AND rs.checkout_run_id IS NOT NULL AND rs.execution_stage = 'in_progress') < ` + strconv.Itoa(limit) + `)`
}

// ParentAtRunLimit reports whether taskID has a parent whose running children
// already reach limit.
func ParentAtRunLimit(db *sql.DB, taskID string, limit int) bool {
	if limit <= 0 {
		return false
	}
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM tasks t JOIN tasks rs ON rs.parent_id = t.parent_id
		WHERE t.id = ? AND COALESCE(t.parent_id, '') != '' AND rs.id != t.id AND rs.checkout_run_id IS NOT NULL
		  AND rs.execution_stage = 'in_progress'`, taskID).Scan(&n)
	return err == nil && n >= limit
}
