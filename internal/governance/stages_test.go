package governance_test

import (
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/governance"
)

func TestStages_CancelledAndBacklogEdges(t *testing.T) {
	for _, pair := range [][2]string{
		{"backlog", "cancelled"}, {"todo", "backlog"}, {"todo", "cancelled"},
		{"blocked", "backlog"}, {"cancelled", "backlog"}, {"cancelled", "todo"},
		{"in_review", "cancelled"},
	} {
		if !governance.IsValidTransition(pair[0], pair[1]) {
			t.Errorf("expected valid: %s→%s", pair[0], pair[1])
		}
	}
	for _, pair := range [][2]string{{"backlog", "in_progress"}, {"cancelled", "in_progress"}, {"done", "cancelled"}} {
		if governance.IsValidTransition(pair[0], pair[1]) {
			t.Errorf("expected invalid: %s→%s", pair[0], pair[1])
		}
	}
}

func TestStages_Runnable(t *testing.T) {
	for _, s := range []string{"backlog", "done", "cancelled", "rejected"} {
		if governance.IsRunnableStage(s) {
			t.Errorf("%s must not be runnable", s)
		}
	}
	for _, s := range []string{"todo", "in_progress", "in_review", "blocked", "paused", "capped", "stopped"} {
		if !governance.IsRunnableStage(s) {
			t.Errorf("%s must be runnable", s)
		}
	}
	if got := governance.NonRunnableStagesSQL(); got != "'backlog', 'done', 'cancelled', 'rejected'" {
		t.Errorf("NonRunnableStagesSQL = %s", got)
	}
}

// A governance transition to cancelled closes the task; leaving it reopens.
func TestExecuteTransition_CancelledStatus(t *testing.T) {
	conn := openTestDB(t)
	insertTask(t, conn, "task-c")
	if err := governance.ExecuteTransition(conn, "task-c", "todo", "cancelled", "board"); err != nil {
		t.Fatal(err)
	}
	var status, stage string
	_ = conn.QueryRow(`SELECT status, execution_stage FROM tasks WHERE id = 'task-c'`).Scan(&status, &stage)
	if status != "soft_deleted" || stage != "cancelled" {
		t.Fatalf("cancel: %s/%s", status, stage)
	}
	if err := governance.ExecuteTransition(conn, "task-c", "cancelled", "backlog", "board"); err != nil {
		t.Fatal(err)
	}
	_ = conn.QueryRow(`SELECT status, execution_stage FROM tasks WHERE id = 'task-c'`).Scan(&status, &stage)
	if status != "active" || stage != "backlog" {
		t.Fatalf("reopen: %s/%s", status, stage)
	}
}
