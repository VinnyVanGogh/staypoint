package server

import (
	gocontext "context"
	"encoding/json"
	"net/http"
	

	"github.com/google/uuid"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
)

var validRunControlActions = map[string]bool{
	"pause":   true,
	"extend":  true,
	"resume":  true,
	"stop":    true,
	"message": true,
}

// RunControl handles POST /api/tasks/{id}/run-control
//
//	body: {"action":"pause"|"resume"|"stop"|"message","text":"..."}
func (h *TasksHandler) RunControl(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}

	task, err := context.GetTask(h.db, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	var req struct {
		Action string `json:"action"`
		Text   string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	req.Action = strings.TrimSpace(req.Action)
	if !validRunControlActions[req.Action] {
		writeError(w, http.StatusBadRequest, "action must be one of: pause, resume, stop, message")
		return
	}

	// Only allow control actions on in-progress or paused tasks.
	stage := task.ExecutionStage
	if stage == "" {
		stage = task.Status
	}
	if req.Action != "stop" && req.Action != "extend" {
		switch stage {
		case "in_progress", "paused":
			// allowed
		default:
			writeError(w, http.StatusConflict, "task is not in_progress or paused")
			return
		}
	}

	rc := orchestrator.GlobalRunControl

	switch req.Action {
	case "extend":
		// Increment max_turns by 50, update status
		_, err := h.db.Exec(`UPDATE tasks SET max_turns = max_turns + 50, execution_stage = 'in_progress', updated_at = CURRENT_TIMESTAMP WHERE id = ?`, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to extend task limits: "+err.Error())
			return
		}
		orchestrator.GlobalDispatcher.Wake(id, "run_now", "run_now:"+id+":"+uuid.New().String()[:8])
	case "pause":
		if err := rc.SetPause(id, true); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to set pause: "+err.Error())
			return
		}
	case "resume":
		if err := rc.SetPause(id, false); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to resume: "+err.Error())
			return
		}
	case "stop":
		if err := rc.SetStop(id); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to stop: "+err.Error())
			return
		}
	case "message":
		if strings.TrimSpace(req.Text) == "" {
			writeError(w, http.StatusBadRequest, "text is required for message action")
			return
		}
		if err := rc.InjectMessage(id, req.Text); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to inject message: "+err.Error())
			return
		}
	}

	if h.hub != nil {
		h.hub.Publish("run_control", map[string]string{
			"task_id": id,
			"action":  req.Action,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "action": req.Action})
}

type runControlState struct {
	Paused        bool `json:"paused"`
	StopRequested bool `json:"stop_requested"`
}

// GetRunControlState handles GET /api/tasks/{id}/run-control-state
// Returns the current pause/stop flags from the run_control table.
// With ?wait=true, long-polls up to 29 s waiting for the pause flag to clear
// or stop to be set (used by the PreToolUse hook to block at step boundaries).
func (h *TasksHandler) GetRunControlState(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}

	rc := orchestrator.GlobalRunControl
	wait := r.URL.Query().Get("wait") == "true"

	paused := rc.IsPaused(id)
	stop := rc.IsStopRequested(id)

	if !wait || !paused || stop {
		writeJSON(w, runControlState{Paused: paused, StopRequested: stop})
		return
	}

	// Long-poll: check every 500 ms for up to 29 s.
	ctx, cancel := gocontext.WithTimeout(r.Context(), 29*time.Second)
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			writeJSON(w, runControlState{Paused: rc.IsPaused(id), StopRequested: rc.IsStopRequested(id)})
			return
		case <-ticker.C:
			paused = rc.IsPaused(id)
			stop = rc.IsStopRequested(id)
			if !paused || stop {
				writeJSON(w, runControlState{Paused: paused, StopRequested: stop})
				return
			}
		}
	}
}
