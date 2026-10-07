package trackgate

import "testing"

// STA-861: an attached task is interactive. Attaching parks an unclaimed todo
// task in backlog (never claimed or woken) and leaves other stages alone.
func TestAttach_ParksTodoTaskInBacklog(t *testing.T) {
	conn := openStore(t)
	for _, tc := range []struct {
		id, stage, checkout, want string
	}{
		{"task-todo", "todo", "", "backlog"},
		{"task-backlog", "backlog", "", "backlog"},
		{"task-review", "in_review", "", "in_review"},
		{"task-running", "in_progress", "run-1", "in_progress"},
	} {
		insertTask(t, conn, tc.id, workRepo, "Managed Solution", "active")
		var checkout any
		if tc.checkout != "" {
			checkout = tc.checkout
		}
		if _, err := conn.Exec(`UPDATE tasks SET execution_stage = ?, checkout_run_id = ? WHERE id = ?`, tc.stage, checkout, tc.id); err != nil {
			t.Fatal(err)
		}
		if err := Attach(conn, "sess-"+tc.id, tc.id, ClientClaude, workRepo); err != nil {
			t.Fatalf("%s: attach: %v", tc.id, err)
		}
		var stage string
		if err := conn.QueryRow(`SELECT execution_stage FROM tasks WHERE id = ?`, tc.id).Scan(&stage); err != nil {
			t.Fatal(err)
		}
		if stage != tc.want {
			t.Errorf("%s: stage after attach = %s, want %s", tc.id, stage, tc.want)
		}
		// The gate still sees the session as attached to an active task.
		if got, err := SessionTask(conn, "sess-"+tc.id); err != nil || got != tc.id {
			t.Errorf("%s: SessionTask = %q, %v", tc.id, got, err)
		}
	}
}
