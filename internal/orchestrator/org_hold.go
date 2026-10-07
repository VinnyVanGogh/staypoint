package orchestrator

import (
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/VinnyVanGogh/staypoint/internal/governance"
)

// WakeHeld reports whether taskID's organization is on a Board hold. When it
// is, the refused wake is logged (daemon log and the task's activity as
// "wake_held") so the Board can see what the hold stopped. Claim enforces the
// hold on its own; this only makes the refusal visible before any run work.
func WakeHeld(db *sql.DB, taskID, reason string) bool {
	if db == nil {
		return false
	}
	held, org := governance.TaskOrgHeld(db, taskID)
	if !held {
		return false
	}
	slog.Info("wake held: organization on hold", slog.String("task", taskID),
		slog.String("reason", reason), slog.String("organization", org))
	_, _ = db.Exec(`INSERT INTO activity_log (task_id, event_type, details) VALUES (?, 'wake_held', ?)`,
		taskID, fmt.Sprintf("held: %s wake refused, organization %q is on hold", reason, org))
	return true
}
