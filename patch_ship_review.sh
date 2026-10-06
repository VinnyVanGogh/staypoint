cat << 'INNEREOF' >> internal/server/handlers_ship_review.go

// AIReview handles POST /api/tasks/{id}/ship-review/ai-review (Board action)
// It resets the task's cap by extending turns, marks the card sent_back with a
// review request comment, and wakes the agent.
func (h *ShipReviewHandler) AIReview(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	card, _, ok := h.requireCard(w, taskID)
	if !ok {
		return
	}

	comment := "Please review your code changes (the diff) against my original request. List out what you changed, carefully ensure you didn't miss any requirements or introduce bugs, and fix any issues you find."

	// Extend task budget to allow it to run again
	_, err := h.db.Exec(`UPDATE tasks SET max_turns = max_turns + 50, execution_stage = 'in_progress', updated_at = CURRENT_TIMESTAMP WHERE id = ?`, taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to extend task limits: "+err.Error())
		return
	}

	// Send back card
	if err := shipreview.SendBack(h.db, card, comment); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	shipreview.StopDevServer(h.db, card)
	_ = context.AddTaskComment(h.db, taskID, "board", comment)
	_ = governance.LogEvent(h.db, taskID, "board", governance.AuditBoardAction, nil, nil,
		map[string]any{"action": "ai_review", "ip": r.RemoteAddr, "user_agent": r.UserAgent()})

	h.hub.Publish("ship_review_sent_back", map[string]any{
		"task_id": taskID,
		"comment": comment,
	})
	
	// Wake the dispatcher
	orchestrator.GlobalDispatcher.Wake(taskID, "run_now", "run_now:"+taskID+":"+uuid.New().String()[:8])

	writeJSON(w, map[string]string{"status": "ok"})
}
INNEREOF
