package shipreview_test

import (
	"context"
	"database/sql"
	"os/exec"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/workspace"
)

// recordMainBase records main's tip as taskID's base, as the daemon does when
// it creates the task worktree (STA-774). Cards are only created against a
// recorded base.
func recordMainBase(t *testing.T, db *sql.DB, repoDir, taskID string) string {
	t.Helper()
	ensureTaskBaseTable(t, db)
	out, err := exec.Command("git", "-C", repoDir, "rev-parse", "refs/heads/main").Output()
	if err != nil {
		t.Fatalf("rev-parse main: %v", err)
	}
	base := strings.TrimSpace(string(out))
	if err := workspace.RecordTaskBase(context.Background(), db, repoDir, taskID, base); err != nil {
		t.Fatalf("RecordTaskBase: %v", err)
	}
	return base
}

func ensureTaskBaseTable(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS task_worktree_bases (
		task_id    TEXT PRIMARY KEY,
		repo_path  TEXT NOT NULL DEFAULT '',
		base_sha   TEXT NOT NULL,
		target_branch TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
		updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
	)`); err != nil {
		t.Fatalf("create task_worktree_bases: %v", err)
	}
}
