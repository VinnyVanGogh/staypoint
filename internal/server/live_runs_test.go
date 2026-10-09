package server_test

import (
	"fmt"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
)

// task-3387cad2: lists show a Running badge only while a run is in flight
// (checkout_run_id set AND the daemon holds the task's run slot), never for
// stage in_progress alone. The sidebar counts read the same source.
func TestLiveRuns_RunningMeansSlotAndCheckout(t *testing.T) {
	fakePaperclip(t)
	database := setupTestDB(t)
	if _, err := database.Exec(`INSERT INTO tasks (id, name, repo_path, git_branch, organization, project, status, account_role, execution_stage, checkout_run_id, origin) VALUES
		('lr-live', 'live run', '/repo/x', 'main', 'General', 'P', 'active', 'personal', 'in_progress', 'run-1', 'native'),
		('lr-stale', 'stale checkout', '/repo/x', 'main', 'General', 'P', 'active', 'personal', 'in_progress', 'run-2', 'native'),
		('lr-stage', 'stage only', '/repo/x', 'main', '', 'P', 'active', 'personal', 'in_progress', NULL, 'native'),
		('lr-todo', 'todo', '/repo/x', 'main', 'General', 'P', 'active', 'personal', 'todo', NULL, 'native')`); err != nil {
		t.Fatal(err)
	}
	// lr-stage holds a slot but has no checkout: not running either.
	for _, id := range []string{"lr-live", "lr-stage"} {
		if err := orchestrator.GlobalRunSlots.Acquire(id, orchestrator.SlotKey{Org: "lr-test-" + id}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { orchestrator.GlobalRunSlots.Release(id) })
	}
	srv, token := startTestServer(t, database)
	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	var live struct {
		Runs []struct {
			TaskID       string `json:"task_id"`
			RunID        string `json:"run_id"`
			Organization string `json:"organization"`
			StartedAt    string `json:"started_at"`
		} `json:"runs"`
	}
	getHiddenJSON(t, base+"/api/runs/live", token, &live)
	if len(live.Runs) != 1 || live.Runs[0].TaskID != "lr-live" || live.Runs[0].RunID != "run-1" ||
		live.Runs[0].Organization != "General" || live.Runs[0].StartedAt == "" {
		t.Fatalf("/api/runs/live = %+v, want only lr-live in General with a start time", live.Runs)
	}

	type task struct {
		ID           string `json:"id"`
		Running      bool   `json:"running"`
		RunStartedAt string `json:"run_started_at"`
	}
	var list struct {
		Tasks []task `json:"tasks"`
	}
	getHiddenJSON(t, base+"/api/tasks?status=all", token, &list)
	got := map[string]task{}
	for _, tk := range list.Tasks {
		got[tk.ID] = tk
	}
	if !got["lr-live"].Running || got["lr-live"].RunStartedAt == "" {
		t.Errorf("lr-live: %+v, want running with a start time", got["lr-live"])
	}
	for _, id := range []string{"lr-stale", "lr-stage", "lr-todo"} {
		if got[id].Running {
			t.Errorf("%s reported running", id)
		}
	}

	var running struct {
		Tasks []task `json:"tasks"`
	}
	getHiddenJSON(t, base+"/api/tasks?status=all&running=1", token, &running)
	if len(running.Tasks) != 1 || running.Tasks[0].ID != "lr-live" {
		t.Errorf("?running=1 = %+v, want only lr-live", running.Tasks)
	}
	getHiddenJSON(t, base+"/api/tasks?status=all&running=0", token, &running)
	for _, tk := range running.Tasks {
		if tk.ID == "lr-live" {
			t.Error("?running=0 listed lr-live")
		}
	}

	var one struct {
		Task task `json:"task"`
	}
	getHiddenJSON(t, base+"/api/tasks/lr-live", token, &one)
	if !one.Task.Running {
		t.Error("GET /api/tasks/lr-live: running = false")
	}
	getHiddenJSON(t, base+"/api/tasks/lr-stale", token, &one)
	if one.Task.Running {
		t.Error("GET /api/tasks/lr-stale: running = true")
	}

	// The sidebar's number: General has exactly the one live run.
	var ov struct {
		Organizations []struct {
			Name       string `json:"name"`
			TaskCounts struct {
				Running int `json:"running"`
			} `json:"task_counts"`
		} `json:"organizations"`
	}
	getHiddenJSON(t, base+"/api/fleet/overview", token, &ov)
	found := false
	for _, o := range ov.Organizations {
		if o.Name == "General" {
			found = true
			if o.TaskCounts.Running != 1 {
				t.Errorf("General running = %d, want 1", o.TaskCounts.Running)
			}
		}
	}
	if !found {
		t.Error("no General org in fleet overview")
	}
}
