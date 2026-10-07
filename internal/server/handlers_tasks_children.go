package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/VinnyVanGogh/staypoint/internal/context"
)

// createChildTask serves POST /api/tasks with parent_id set (STA-820). The
// child inherits repo, branch, org and project from the parent and gets the
// parent's handoff stored by the daemon. work_kind defaults to coding.
func (h *TasksHandler) createChildTask(w http.ResponseWriter, parentID, name, workKind, handoff, description string, maxBudgetUSD float64, maxTurns int, allowDeep bool) {
	if workKind == "" {
		workKind = "coding"
	}
	task, err := context.CreateChildTask(h.db, context.ChildTaskOptions{
		ParentID:      parentID,
		Name:          name,
		WorkKind:      workKind,
		Handoff:       handoff,
		Description:   description,
		MaxBudgetUSD:  maxBudgetUSD,
		MaxTurns:      maxTurns,
		BoardOverride: allowDeep,
	})
	if err != nil {
		switch {
		case errors.Is(err, context.ErrInvalidWorkKind):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, context.ErrParentNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, context.ErrChildLimit), errors.Is(err, context.ErrDepthLimit):
			writeError(w, http.StatusConflict, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "failed to create child task: "+err.Error())
		}
		return
	}

	if h.hub != nil {
		h.hub.Publish("task_created", task)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(task)
}
