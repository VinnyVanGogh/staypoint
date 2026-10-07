package orchestrator

import (
	"context"
	"errors"
	"testing"
)

// A parked (backlog) or closed (cancelled, done) task is never claimed, so no
// wake, queue re-dispatch or MCP call can start a run on it.
func TestClaim_RefusesNonRunnableStages(t *testing.T) {
	for _, stage := range []string{"backlog", "cancelled", "done", "rejected", "stopped"} {
		t.Run(stage, func(t *testing.T) {
			useSlots(t, 1)
			db := openTestDB(t)
			insertTask(t, db, "task-"+stage, "/tmp/repo")
			if _, err := db.Exec(`UPDATE tasks SET execution_stage = ? WHERE id = ?`, stage, "task-"+stage); err != nil {
				t.Fatal(err)
			}
			h := &Harness{DB: db}
			err := h.Claim(context.Background(), "task-"+stage, "run-1", "agent")
			if !errors.Is(err, ErrNotRunnable) {
				t.Fatalf("claim on %s: err = %v, want ErrNotRunnable", stage, err)
			}
			var checkout string
			_ = db.QueryRow(`SELECT COALESCE(checkout_run_id, '') FROM tasks WHERE id = ?`, "task-"+stage).Scan(&checkout)
			if checkout != "" {
				t.Fatalf("refused claim must not check the task out, got %q", checkout)
			}
		})
	}
}

func TestClaim_TodoAfterBacklogIsClaimable(t *testing.T) {
	useSlots(t, 1)
	db := openTestDB(t)
	insertTask(t, db, "task-parked", "/tmp/repo")
	if _, err := db.Exec(`UPDATE tasks SET execution_stage = 'todo' WHERE id = 'task-parked'`); err != nil {
		t.Fatal(err)
	}
	h := &Harness{DB: db}
	if err := h.Claim(context.Background(), "task-parked", "run-1", "agent"); err != nil {
		t.Fatalf("todo task must be claimable: %v", err)
	}
}
