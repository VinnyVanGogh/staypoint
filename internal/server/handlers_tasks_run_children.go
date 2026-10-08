package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
	"github.com/google/uuid"
)

// Run all children (task-f6777004): one Board action, one passkey, moves a
// parent's backlog/todo children to todo in priority order and wakes them, so
// the run queue starts them. tasks.max_running_children still caps how many
// run at once (the claim refuses the rest and they wait in the queue). The
// route is wrapped in WrapBoardAction; each child also goes through the same
// checks SetStage applies: the leave-backlog Board gate and the org hold.

// runChildResult is one child's outcome.
type runChildResult struct {
	TaskID string `json:"task_id"`
	Name   string `json:"name"`
	From   string `json:"from"`
	Result string `json:"result"` // queued, skipped, failed
	Reason string `json:"reason,omitempty"`
}

// childrenByPriority lists the parent's live children, highest priority
// first, then oldest first.
func childrenByPriority(h *TasksHandler, parentID string) ([]string, error) {
	rows, err := h.db.Query(`
		SELECT id FROM tasks
		WHERE parent_id = ? AND status != 'soft_deleted'
		ORDER BY CASE LOWER(COALESCE(priority, 'medium'))
		           WHEN 'critical' THEN 0 WHEN 'urgent' THEN 0 WHEN 'high' THEN 1
		           WHEN 'low' THEN 3 ELSE 2 END,
		         created_at, id`, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RunChildren handles POST /api/tasks/{id}/run-children (Board action).
func (h *TasksHandler) RunChildren(w http.ResponseWriter, r *http.Request) {
	parent, err := context.GetTask(h.db, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	// The route's WrapBoardAction already demanded the Board session and
	// passkey; fail closed if this handler is ever mounted without it.
	if !h.isBoardRequest(r) {
		writeBoardError(w, "board_session_required", "forbidden: Run all children is a Board action")
		return
	}
	ids, err := childrenByPriority(h, parent.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list children: "+err.Error())
		return
	}
	results := make([]runChildResult, 0, len(ids))
	queued := 0
	for _, id := range ids {
		child, err := context.GetTask(h.db, id)
		if err != nil {
			results = append(results, runChildResult{TaskID: id, Result: "failed", Reason: err.Error()})
			continue
		}
		res := runChildResult{TaskID: child.ID, Name: child.Name, From: child.ExecutionStage}
		switch child.ExecutionStage {
		case governance.StageBacklog, governance.StageTodo:
		default:
			res.Result, res.Reason = "skipped", "stage is "+child.ExecutionStage
			results = append(results, res)
			continue
		}
		if child.Status == "done" {
			res.Result, res.Reason = "skipped", "task is done"
			results = append(results, res)
			continue
		}
		if held, org := governance.TaskOrgHeld(h.db, child.ID); held {
			res.Result, res.Reason = "skipped", "organization "+org+" is on hold"
			results = append(results, res)
			continue
		}
		// requireBoardToLeave for a batch: the passkey was asserted once for
		// the whole request, so a gated child (agent-created, prod-targeting
		// included) passes on the Board session that request carried.
		if context.RequiresBoardToLeave(child, governance.StageTodo) && !h.isBoardRequest(r) {
			res.Result, res.Reason = "failed", "board_session_required"
			results = append(results, res)
			continue
		}
		if child.ExecutionStage != governance.StageTodo {
			if err := context.SetTaskExecutionStageWithOptions(h.db, child.ID, governance.StageTodo, context.DoneOptions{BoardStage: true}); err != nil {
				reason := err.Error()
				if errors.Is(err, context.ErrNoRepo) {
					reason = "no repo: " + reason
				}
				res.Result, res.Reason = "failed", reason
				results = append(results, res)
				continue
			}
			if h.hub != nil {
				h.hub.Publish("task_stage_changed", map[string]string{"task_id": child.ID, "stage": governance.StageTodo})
			}
		}
		// Wake in priority order: what the claim refuses for the
		// max_running_children cap waits in the run queue in this order.
		orchestrator.GlobalDispatcher.Wake(child.ID, "run_children", "run_children:"+child.ID+":"+uuid.New().String()[:8])
		res.Result = "queued"
		queued++
		results = append(results, res)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"task_id":  parent.ID,
		"queued":   queued,
		"children": results,
	})
}
