package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
)

// liveRuns is the daemon's record of runs in flight (task-3387cad2). A var so
// tests can swap it.
var liveRuns = orchestrator.GlobalRunSlots.LiveRuns

// markLiveRuns sets Running and RunStartedAt on each task: running only while
// the task has a checkout_run_id and its run holds a slot in this process.
// Stage in_progress alone is not running: a crashed or stopped run can leave
// it behind.
func markLiveRuns(tasks []context.Task) {
	live := liveRuns()
	for i := range tasks {
		applyLiveRun(&tasks[i], live)
	}
}

func applyLiveRun(t *context.Task, live map[string]time.Time) {
	t.Running, t.RunStartedAt = false, ""
	if t.CheckoutRunID == "" {
		return
	}
	since, ok := live[t.ID]
	if !ok {
		return
	}
	t.Running = true
	if !since.IsZero() {
		t.RunStartedAt = since.UTC().Format(time.RFC3339Nano)
	}
}

// LiveRun is one run in flight, as GET /api/runs/live lists it.
type LiveRun struct {
	TaskID         string `json:"task_id"`
	RunID          string `json:"run_id"`
	Name           string `json:"name"`
	Organization   string `json:"organization"`
	Project        string `json:"project,omitempty"`
	ExecutionStage string `json:"execution_stage"`
	StartedAt      string `json:"started_at,omitempty"`
}

// defaultOrg is the organization a task with none is listed under, as in
// fleet.Aggregator.
const defaultOrg = "StayPoint"

// listLiveRuns returns every run in flight, oldest first. The web UI's
// Running badges, Running filter and sidebar counts all read this.
func listLiveRuns(db *sql.DB) ([]LiveRun, error) {
	live := liveRuns()
	out := []LiveRun{}
	if len(live) == 0 {
		return out, nil
	}
	rows, err := db.Query(`SELECT id, checkout_run_id, name, COALESCE(organization, ''), COALESCE(project, ''), execution_stage
		FROM tasks WHERE COALESCE(checkout_run_id, '') != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r LiveRun
		if err := rows.Scan(&r.TaskID, &r.RunID, &r.Name, &r.Organization, &r.Project, &r.ExecutionStage); err != nil {
			return nil, err
		}
		since, ok := live[r.TaskID]
		if !ok {
			continue
		}
		if r.Organization == "" {
			r.Organization = defaultOrg
		}
		if !since.IsZero() {
			r.StartedAt = since.UTC().Format(time.RFC3339Nano)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt < out[j].StartedAt })
	return out, nil
}

// LiveRuns handles GET /api/runs/live: {runs: [LiveRun]}.
func (h *TasksHandler) LiveRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := listLiveRuns(h.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"runs": runs})
}

// SeedLiveRun handles PUT /api/tasks/{id}/test/live-run (TestMode only). The
// UI suite's daemon has no runner, so {"live": true} stands in for one: it
// takes a run slot and checks the task out the way Harness.Claim does;
// {"live": false} releases both, as Harness.Release does.
func (h *TasksHandler) SeedLiveRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Live bool `json:"live"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	task, err := context.GetTask(h.db, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "task not found: "+err.Error())
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if req.Live {
		if err := orchestrator.GlobalRunSlots.Acquire(task.ID, orchestrator.SlotKey{Org: task.Organization}); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if _, err := h.db.Exec(`UPDATE tasks SET execution_stage = 'in_progress', checkout_run_id = ?, updated_at = ? WHERE id = ?`,
			"ui-e2e-"+task.ID, now, task.ID); err != nil {
			orchestrator.GlobalRunSlots.Release(task.ID)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		if _, err := h.db.Exec(`UPDATE tasks SET checkout_run_id = NULL, updated_at = ? WHERE id = ?`, now, task.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		orchestrator.GlobalRunSlots.Release(task.ID)
	}
	if h.hub != nil {
		h.hub.Publish("run.live", map[string]any{"task_id": task.ID, "live": req.Live})
	}
	writeJSON(w, map[string]any{"task_id": task.ID, "live": req.Live})
}
