package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	gctx "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
)

// GovernanceHandler handles all /api/tasks/{id}/governance* endpoints.
type GovernanceHandler struct {
	db  *sql.DB
	hub *EventHub
}

func NewGovernanceHandler(db *sql.DB, hub *EventHub) *GovernanceHandler {
	return &GovernanceHandler{db: db, hub: hub}
}

// GetGovernance handles GET /api/tasks/{id}/governance
func (h *GovernanceHandler) GetGovernance(w http.ResponseWriter, r *http.Request) {
	task, err := gctx.GetTask(h.db, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	snap, err := governance.GetSnapshot(h.db, task.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, snap)
}

// SetGovernance handles POST /api/tasks/{id}/governance
func (h *GovernanceHandler) SetGovernance(w http.ResponseWriter, r *http.Request) {
	task, err := gctx.GetTask(h.db, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	var req struct {
		ApprovalThreshold int    `json:"approval_threshold"`
		RequireReview     bool   `json:"require_review"`
		WatchdogAgentID   string `json:"watchdog_agent_id"`
		WatchdogPrompt    string `json:"watchdog_prompt"`
		ActorID           string `json:"actor_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.ApprovalThreshold < 0 {
		req.ApprovalThreshold = 0
	}
	if req.ActorID == "" {
		req.ActorID = "system"
	}

	cfg, err := governance.SetGovernanceConfig(h.db, task.ID, req.ActorID, req.ApprovalThreshold, req.RequireReview, req.WatchdogAgentID, req.WatchdogPrompt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if h.hub != nil {
		h.hub.Publish("governance_updated", map[string]string{"task_id": task.ID})
	}
	writeJSON(w, cfg)
}

// AddReviewer handles POST /api/tasks/{id}/reviewers
func (h *GovernanceHandler) AddReviewer(w http.ResponseWriter, r *http.Request) {
	task, err := gctx.GetTask(h.db, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	var req struct {
		ReviewerID   string `json:"reviewer_id"`
		ReviewerType string `json:"reviewer_type"`
		ActorID      string `json:"actor_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.ReviewerID == "" {
		writeError(w, http.StatusBadRequest, "reviewer_id is required")
		return
	}
	if req.ActorID == "" {
		req.ActorID = "system"
	}

	if err := governance.AssignReviewer(h.db, task.ID, req.ReviewerID, req.ReviewerType, req.ActorID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if h.hub != nil {
		h.hub.Publish("reviewer_assigned", map[string]string{"task_id": task.ID, "reviewer_id": req.ReviewerID})
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]string{"status": "ok"})
}

// RemoveReviewer handles DELETE /api/tasks/{id}/reviewers/{rid}
func (h *GovernanceHandler) RemoveReviewer(w http.ResponseWriter, r *http.Request) {
	task, err := gctx.GetTask(h.db, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	rid := r.PathValue("rid")
	if rid == "" {
		writeError(w, http.StatusBadRequest, "reviewer id required")
		return
	}
	actorID := r.URL.Query().Get("actor_id")
	if actorID == "" {
		actorID = "system"
	}
	if err := governance.RemoveReviewer(h.db, task.ID, rid, actorID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// AddApprover handles POST /api/tasks/{id}/approvers
func (h *GovernanceHandler) AddApprover(w http.ResponseWriter, r *http.Request) {
	task, err := gctx.GetTask(h.db, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	var req struct {
		ApproverID   string `json:"approver_id"`
		ApproverType string `json:"approver_type"`
		ActorID      string `json:"actor_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.ApproverID == "" {
		writeError(w, http.StatusBadRequest, "approver_id is required")
		return
	}
	if req.ActorID == "" {
		req.ActorID = "system"
	}

	if err := governance.AssignApprover(h.db, task.ID, req.ApproverID, req.ApproverType, req.ActorID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if h.hub != nil {
		h.hub.Publish("approver_assigned", map[string]string{"task_id": task.ID, "approver_id": req.ApproverID})
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]string{"status": "ok"})
}

// RemoveApprover handles DELETE /api/tasks/{id}/approvers/{aid}
func (h *GovernanceHandler) RemoveApprover(w http.ResponseWriter, r *http.Request) {
	task, err := gctx.GetTask(h.db, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	aid := r.PathValue("aid")
	if aid == "" {
		writeError(w, http.StatusBadRequest, "approver id required")
		return
	}
	actorID := r.URL.Query().Get("actor_id")
	if actorID == "" {
		actorID = "system"
	}
	if err := governance.RemoveApprover(h.db, task.ID, aid, actorID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// SetWatchdog handles POST /api/tasks/{id}/watchdog
func (h *GovernanceHandler) SetWatchdog(w http.ResponseWriter, r *http.Request) {
	task, err := gctx.GetTask(h.db, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	var req struct {
		WatchdogAgentID string `json:"watchdog_agent_id"`
		WatchdogPrompt  string `json:"watchdog_prompt"`
		ActorID         string `json:"actor_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.ActorID == "" {
		req.ActorID = "system"
	}

	// Load existing config to preserve threshold/require_review.
	existing, _ := governance.GetGovernanceConfig(h.db, task.ID)
	threshold := 1
	requireReview := false
	if existing != nil {
		threshold = existing.ApprovalThreshold
		requireReview = existing.RequireReview
	}

	cfg, err := governance.SetGovernanceConfig(h.db, task.ID, req.ActorID, threshold, requireReview, req.WatchdogAgentID, req.WatchdogPrompt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = governance.LogEvent(h.db, task.ID, req.ActorID, governance.AuditWatchdogAssigned, nil, nil, map[string]string{
		"watchdog_agent_id": req.WatchdogAgentID,
	})
	if h.hub != nil {
		h.hub.Publish("watchdog_assigned", map[string]string{"task_id": task.ID, "watchdog_agent_id": req.WatchdogAgentID})
	}
	writeJSON(w, cfg)
}

// SubmitReview handles POST /api/tasks/{id}/review
func (h *GovernanceHandler) SubmitReview(w http.ResponseWriter, r *http.Request) {
	task, err := gctx.GetTask(h.db, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	var req struct {
		ReviewerID string `json:"reviewer_id"`
		Decision   string `json:"decision"`
		Notes      string `json:"notes"`
		ActorID    string `json:"actor_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.ReviewerID == "" {
		writeError(w, http.StatusBadRequest, "reviewer_id is required")
		return
	}
	if req.Decision == "" {
		writeError(w, http.StatusBadRequest, "decision is required (approved|rejected|changes_requested)")
		return
	}
	if req.ActorID == "" {
		req.ActorID = req.ReviewerID
	}

	rd, err := governance.SubmitReview(h.db, task.ID, req.ReviewerID, req.Decision, req.Notes, req.ActorID)
	if errors.Is(err, governance.ErrNotAssigned) {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.hub != nil {
		h.hub.Publish("review_submitted", map[string]string{"task_id": task.ID, "decision": req.Decision})
	}
	writeJSON(w, rd)
}

// SubmitApproval handles POST /api/tasks/{id}/approve
func (h *GovernanceHandler) SubmitApproval(w http.ResponseWriter, r *http.Request) {
	task, err := gctx.GetTask(h.db, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	var req struct {
		ApproverID string `json:"approver_id"`
		Vote       string `json:"vote"`
		Reason     string `json:"reason"`
		ActorID    string `json:"actor_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.ApproverID == "" {
		writeError(w, http.StatusBadRequest, "approver_id is required")
		return
	}
	if req.Vote == "" {
		writeError(w, http.StatusBadRequest, "vote is required (approved|rejected|abstain)")
		return
	}
	if req.ActorID == "" {
		req.ActorID = req.ApproverID
	}

	av, err := governance.SubmitApprovalVote(h.db, task.ID, req.ApproverID, req.Vote, req.Reason, req.ActorID)
	if errors.Is(err, governance.ErrNotAssigned) {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if h.hub != nil {
		h.hub.Publish("approval_vote", map[string]string{"task_id": task.ID, "vote": req.Vote})
	}
	writeJSON(w, av)
}

// Transition handles POST /api/tasks/{id}/transition
func (h *GovernanceHandler) Transition(w http.ResponseWriter, r *http.Request) {
	task, err := gctx.GetTask(h.db, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	var req struct {
		To      string `json:"to"`
		ActorID string `json:"actor_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.To == "" {
		writeError(w, http.StatusBadRequest, "to (target stage) is required")
		return
	}
	if req.ActorID == "" {
		req.ActorID = "system"
	}

	from := task.ExecutionStage
	// Agent-created tasks leave a parked stage only through the Board's
	// stage endpoint (POST /api/tasks/{id}/stage), which runs the Board gate.
	if gctx.RequiresBoardToLeave(task, strings.ToLower(strings.TrimSpace(req.To))) {
		writeBoardError(w, "board_session_required", "forbidden: "+task.ID+" was created by an agent; only the Board can move it out of "+from)
		return
	}
	if err := governance.ExecuteTransition(h.db, task.ID, from, req.To, req.ActorID); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	if h.hub != nil {
		h.hub.Publish("task_transition", map[string]string{"task_id": task.ID, "from": from, "to": req.To})
	}

	updated, _ := gctx.GetTask(h.db, task.ID)
	writeJSON(w, updated)
}

// GetAuditLog handles GET /api/tasks/{id}/audit
func (h *GovernanceHandler) GetAuditLog(w http.ResponseWriter, r *http.Request) {
	task, err := gctx.GetTask(h.db, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	entries, err := governance.GetAuditLog(h.db, task.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"task_id": task.ID, "audit_log": entries})
}

// writeJSON is a convenience helper to write a JSON response.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
