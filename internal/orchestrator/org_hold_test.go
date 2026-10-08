package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/governance"
)

// Org hold: while an organization is held nothing in it is claimed, whatever
// woke it; other organizations are unaffected; lifting the hold restores it.
func TestClaim_RefusesHeldOrganization(t *testing.T) {
	db := openTestDB(t)
	insertTask(t, db, "task-held", "/repo/a")
	insertTask(t, db, "task-free", "/repo/b")
	if _, err := db.Exec(`UPDATE tasks SET organization = CASE id WHEN 'task-held' THEN 'Managed Solution' ELSE 'Personal' END`); err != nil {
		t.Fatal(err)
	}
	if err := governance.SetOrgHold(db, "managed solution", true); err != nil {
		t.Fatal(err)
	}
	h := &Harness{DB: db}
	ctx := context.Background()

	if err := h.Claim(ctx, "task-held", "run-1", "agent"); !errors.Is(err, ErrOrgHeld) {
		t.Fatalf("claim in held org: err = %v, want ErrOrgHeld", err)
	}
	var stage string
	_ = db.QueryRow(`SELECT execution_stage FROM tasks WHERE id = 'task-held'`).Scan(&stage)
	if stage == "in_progress" {
		t.Fatal("refused claim still moved the task to in_progress")
	}
	if !WakeHeld(db, "task-held", "assignment") {
		t.Error("WakeHeld: want true for a held org")
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM activity_log WHERE task_id = 'task-held' AND event_type = 'wake_held'`).Scan(&n)
	if n != 1 {
		t.Errorf("wake_held activity rows = %d, want 1", n)
	}

	if WakeHeld(db, "task-free", "assignment") {
		t.Error("WakeHeld: other org reported held")
	}
	if err := h.Claim(ctx, "task-free", "run-2", "agent"); err != nil {
		t.Fatalf("claim in other org: %v", err)
	}
	h.Release("task-free", "run-2")

	if err := governance.SetOrgHold(db, "Managed Solution", false); err != nil {
		t.Fatal(err)
	}
	if err := h.Claim(ctx, "task-held", "run-3", "agent"); err != nil {
		t.Fatalf("claim after the hold was lifted: %v", err)
	}
	h.Release("task-held", "run-3")
}
