package server

import (
	gocontext "context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/checkpoint"
	"github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/migration"
	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
	"github.com/VinnyVanGogh/staypoint/internal/workspace"
	"github.com/google/uuid"
)

// isNotFound returns true when err signals a missing entity.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found")
}

type TasksHandler struct {
	db  *sql.DB
	hub *EventHub
}

func NewTasksHandler(db *sql.DB, hub *EventHub) *TasksHandler {
	return &TasksHandler{db: db, hub: hub}
}

// ListTasks handles GET /api/tasks
//
// Query: status (active|done|soft_deleted|all), stage (an execution stage),
// origin (native|paperclip_import|legacy), include_legacy (1/true; legacy
// tasks are hidden unless set or origin=legacy), limit, offset.
func (h *TasksHandler) ListTasks(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	stageFilter := strings.TrimSpace(r.URL.Query().Get("stage"))
	originFilter := strings.TrimSpace(r.URL.Query().Get("origin"))
	if originFilter != "" && !context.IsValidOrigin(originFilter) {
		writeError(w, http.StatusBadRequest, "invalid origin: must be native, paperclip_import, or legacy")
		return
	}
	includeLegacy := parseBoolParam(r.URL.Query().Get("include_legacy")) || originFilter == context.OriginLegacy
	limitStr := r.URL.Query().Get("limit")
	offsetStr := r.URL.Query().Get("offset")

	limit := 100
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			if l > 1000 {
				l = 1000
			}
			limit = l
		}
	}
	offset := 0
	if offsetStr != "" {
		if o, err := strconv.Atoi(offsetStr); err == nil && o >= 0 {
			offset = o
		}
	}

	tasks, err := context.ListTasks(h.db, status == "all" || status == "done" || status == "soft_deleted")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	tasks = context.FilterLegacy(tasks, includeLegacy)

	// Filter by specific status if requested and not "all"
	var filtered []context.Task
	for _, t := range tasks {
		if !(status == "" || status == "all" || strings.EqualFold(t.Status, status)) {
			continue
		}
		if stageFilter != "" && !strings.EqualFold(t.ExecutionStage, stageFilter) {
			continue
		}
		if originFilter != "" && t.Origin != originFilter {
			continue
		}
		filtered = append(filtered, t)
	}

	total := len(filtered)
	// Apply offset and limit
	start := offset
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	slice := filtered[start:end]
	if slice == nil {
		slice = []context.Task{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"tasks":    slice,
		"total":    total,
		"limit":    limit,
		"offset":   offset,
		"has_more": end < total,
	})
}

func parseBoolParam(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// GetTask handles GET /api/tasks/{id}
func (h *TasksHandler) GetTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}

	task, err := context.GetTask(h.db, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	comments, _ := context.GetTaskComments(h.db, task.ID)
	depGraph, _ := context.GetTaskDependencyGraph(h.db, task.ID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"task":         task,
		"comments":     comments,
		"dependencies": depGraph,
		"work_kind":    task.WorkKind,
		// Run queue position (STA-773): {queued, ahead, wait}.
		"queue": orchestrator.GlobalRunSlots.Position(task.ID),
	})
}

// validWorkKinds is the set of accepted work_kind values.
var validWorkKinds = map[string]bool{
	"coding":       true,
	"review":       true,
	"architecture": true,
	"planning":     true,
	"qa":           true,
}

// CreateTask handles POST /api/tasks
func (h *TasksHandler) CreateTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name            string  `json:"name"`
		RepoPath        string  `json:"repo_path"`
		GitBranch       string  `json:"git_branch"`
		AccountRole     string  `json:"account_role"`
		MaxBudgetUSD    float64 `json:"max_budget_usd"`
		MaxTurns        int     `json:"max_turns"`
		Organization    string  `json:"organization"`
		Project         string  `json:"project"`
		AssigneeAgentID string  `json:"assignee_agent_id"`
		WorkKind        string  `json:"work_kind"`
		Description     string  `json:"description"`
		// STA-820: child task fields. Repo/org/project are inherited from the parent.
		ParentID string `json:"parent_id"`
		Handoff  string `json:"handoff"`
		// AllowDeep is the Board override for the child depth cap.
		AllowDeep bool `json:"allow_deep"`
		// ExecutionStage is the initial stage: todo (default) or backlog
		// (parked: created without waking an agent).
		ExecutionStage string `json:"execution_stage"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "task name is required")
		return
	}

	if req.WorkKind != "" && !validWorkKinds[req.WorkKind] {
		writeError(w, http.StatusBadRequest, "invalid work_kind: must be one of coding, review, architecture, planning, qa")
		return
	}

	req.ExecutionStage = strings.ToLower(strings.TrimSpace(req.ExecutionStage))
	switch req.ExecutionStage {
	case "", governance.StageTodo, governance.StageBacklog:
	default:
		writeError(w, http.StatusBadRequest, "invalid execution_stage: a new task starts in todo or backlog")
		return
	}

	if strings.TrimSpace(req.ParentID) != "" {
		h.createChildTask(w, req.ParentID, req.Name, req.WorkKind, req.Handoff, req.Description, req.MaxBudgetUSD, req.MaxTurns, req.AllowDeep)
		return
	}

	opts := context.TaskCreateOptions{
		Name:            req.Name,
		RepoPath:        req.RepoPath,
		GitBranch:       req.GitBranch,
		AccountRole:     req.AccountRole,
		MaxBudgetUSD:    req.MaxBudgetUSD,
		MaxTurns:        req.MaxTurns,
		Organization:    req.Organization,
		Project:         req.Project,
		AssigneeAgentID: req.AssigneeAgentID,
		WorkKind:        req.WorkKind,
		Description:     req.Description,
		ExecutionStage:  req.ExecutionStage,
	}

	task, err := context.CreateTaskWithOptions(h.db, opts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create task: "+err.Error())
		return
	}

	if h.hub != nil {
		h.hub.Publish("task_created", task)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(task)
}

// UpdateTaskDescription handles PUT /api/tasks/{id}/description
func (h *TasksHandler) UpdateTaskDescription(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	var req struct {
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	task, err := context.GetTask(h.db, taskID)
	if err != nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if err := context.UpsertTaskDescription(h.db, task.ID, req.Description); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update description: "+err.Error())
		return
	}
	updated, err := context.GetTask(h.db, task.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to fetch updated task: "+err.Error())
		return
	}
	if h.hub != nil {
		h.hub.Publish("task_updated", updated)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(updated)
}

// GetComments handles GET /api/tasks/{id}/comments
func (h *TasksHandler) GetComments(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}

	comments, err := context.GetTaskComments(h.db, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"comments": comments,
	})
}

// AddComment handles POST /api/tasks/{id}/comments
func (h *TasksHandler) AddComment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}

	var req struct {
		Author  string `json:"author"`
		Message string `json:"message"`
		Body    string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Message == "" && req.Body != "" {
		req.Message = req.Body
	}

	if strings.TrimSpace(req.Message) == "" {
		writeError(w, http.StatusBadRequest, "comment message is required")
		return
	}
	if req.Author == "" {
		req.Author = "user"
	}

	if err := context.AddTaskComment(h.db, id, req.Author, req.Message); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "failed to add comment: "+err.Error())
		}
		return
	}

	if h.hub != nil {
		h.hub.Publish("task_comment_added", map[string]string{
			"task_id": id,
			"author":  req.Author,
			"message": req.Message,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// MarkDone handles POST /api/tasks/{id}/done. A parent with open child tasks
// is refused with 409 unless the Board passes {"override": true}.
func (h *TasksHandler) MarkDone(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}
	var req struct {
		Override bool `json:"override"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // body is optional

	if err := context.MarkTaskDoneWithOptions(h.db, id, context.DoneOptions{BoardOverride: req.Override}); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
		} else if errors.Is(err, context.ErrOpenChildren) {
			writeError(w, http.StatusConflict, err.Error())
		} else if strings.Contains(err.Error(), "without a registered work product") {
			writeError(w, http.StatusConflict, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "failed to mark task done: "+err.Error())
		}
		return
	}

	if h.hub != nil {
		h.hub.Publish("task_done", map[string]string{"task_id": id})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// BlockTask handles POST /api/tasks/{id}/block
func (h *TasksHandler) BlockTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}

	var req struct {
		Reason       string   `json:"reason"`
		BlockedByIDs []string `json:"blocked_by_ids"`
		Blockers     []struct {
			ID        string `json:"id"`
			Rationale string `json:"rationale"`
		} `json:"blockers"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	var blockers []context.BlockerInput
	for _, b := range req.Blockers {
		if b.ID != "" {
			blockers = append(blockers, context.BlockerInput{
				ID:        b.ID,
				Rationale: b.Rationale,
			})
		}
	}
	for _, bid := range req.BlockedByIDs {
		if bid != "" {
			blockers = append(blockers, context.BlockerInput{
				ID:        bid,
				Rationale: req.Reason,
			})
		}
	}

	var err error
	if len(blockers) > 0 {
		err = context.BlockTaskWithBlockers(h.db, id, req.Reason, blockers)
	} else {
		err = context.BlockTask(h.db, id, req.Reason)
	}

	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "failed to block task: "+err.Error())
		}
		return
	}

	if h.hub != nil {
		h.hub.Publish("task_blocked", map[string]any{"task_id": id, "reason": req.Reason, "blockers": blockers})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// GetTaskDependencies handles GET /api/tasks/{id}/dependencies
func (h *TasksHandler) GetTaskDependencies(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}

	depGraph, err := context.GetTaskDependencyGraph(h.db, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(depGraph)
}

// AddBlocker handles POST /api/tasks/{id}/blockers
func (h *TasksHandler) AddBlocker(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}

	var req struct {
		BlockerID string `json:"blocker_id"`
		Rationale string `json:"rationale"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.BlockerID == "" {
		writeError(w, http.StatusBadRequest, "blocker_id is required")
		return
	}

	if req.BlockerID == id {
		writeError(w, http.StatusBadRequest, "task cannot block itself")
		return
	}

	if err := context.AddTaskBlocker(h.db, id, req.BlockerID, req.Rationale); err != nil {
		switch {
		case err == context.ErrBlockerSelfReference:
			writeError(w, http.StatusBadRequest, err.Error())
		case err == context.ErrBlockerCycle:
			writeError(w, http.StatusConflict, err.Error())
		case isNotFound(err):
			writeError(w, http.StatusNotFound, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "failed to add blocker: "+err.Error())
		}
		return
	}

	if h.hub != nil {
		h.hub.Publish("task_blocked", map[string]any{"task_id": id, "blocker_id": req.BlockerID, "rationale": req.Rationale})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// RemoveBlocker handles DELETE /api/tasks/{id}/blockers/{bid}
func (h *TasksHandler) RemoveBlocker(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	bid := r.PathValue("bid")
	if id == "" || bid == "" {
		writeError(w, http.StatusBadRequest, "task id and blocker id are required")
		return
	}

	if err := context.RemoveTaskBlocker(h.db, id, bid); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "failed to remove blocker: "+err.Error())
		}
		return
	}

	if h.hub != nil {
		h.hub.Publish("task_unblocked", map[string]any{"task_id": id, "unblocked_from": bid})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// UnblockTask handles POST /api/tasks/{id}/unblock
func (h *TasksHandler) UnblockTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}

	if err := context.UnblockTask(h.db, id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "failed to unblock task: "+err.Error())
		}
		return
	}

	if h.hub != nil {
		h.hub.Publish("task_unblocked", map[string]string{"task_id": id})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// ListInteractions handles GET /api/tasks/{id}/interactions
func (h *TasksHandler) ListInteractions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}

	interactions, err := context.ListInteractions(h.db, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if interactions == nil {
		interactions = []context.TaskInteraction{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"interactions": interactions,
	})
}

// CreateInteraction handles POST /api/tasks/{id}/interactions
func (h *TasksHandler) CreateInteraction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}

	var req struct {
		Kind           string `json:"kind"`
		Payload        string `json:"payload"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Kind) == "" {
		writeError(w, http.StatusBadRequest, "interaction kind is required")
		return
	}

	in := &context.TaskInteraction{
		TaskID:          id,
		InteractionKind: req.Kind,
		Payload:         req.Payload,
		IdempotencyKey:  req.IdempotencyKey,
	}

	created, err := context.CreateInteraction(h.db, in)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
		} else {
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}

	if h.hub != nil {
		h.hub.Publish("task_interaction_created", map[string]any{
			"task_id":        id,
			"interaction_id": created.ID,
			"kind":           created.InteractionKind,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(created)
}

// ResolveInteraction handles POST /api/tasks/{id}/interactions/{iid}/resolve
func (h *TasksHandler) ResolveInteraction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	iidStr := r.PathValue("iid")
	if id == "" || iidStr == "" {
		writeError(w, http.StatusBadRequest, "task id and interaction id are required")
		return
	}

	iid, err := strconv.Atoi(iidStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "interaction id must be an integer")
		return
	}

	var req struct {
		Status   string `json:"status"`
		Response any    `json:"response"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Status) == "" {
		writeError(w, http.StatusBadRequest, "status is required")
		return
	}

	updated, err := context.ResolveTaskInteraction(h.db, id, iid, req.Status, req.Response)
	if errors.Is(err, context.ErrInteractionNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		if errors.Is(err, context.ErrInteractionNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
		} else {
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}

	if h.hub != nil {
		h.hub.Publish("task_interaction_resolved", map[string]any{
			"task_id":        id,
			"interaction_id": iid,
			"status":         req.Status,
		})
	}

	orchestrator.GlobalDispatcher.Wake(id, "interaction_resolved",
		fmt.Sprintf("interaction_resolved:%s:%d", id, iid))

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(updated)
}

// SetStage handles POST /api/tasks/{id}/stage
func (h *TasksHandler) SetStage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}

	var req struct {
		Stage    string `json:"stage"`
		Override bool   `json:"override"` // Board override: done with open children
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	req.Stage = strings.TrimSpace(req.Stage)
	if req.Stage == "" {
		writeError(w, http.StatusBadRequest, "stage is required")
		return
	}

	req.Stage = strings.ToLower(req.Stage)
	if !governance.IsBoardSettableStage(req.Stage) {
		writeError(w, http.StatusBadRequest, "invalid stage: must be one of "+strings.Join(governance.BoardSettableStages, ", "))
		return
	}

	if err := context.SetTaskExecutionStageWithOptions(h.db, id, req.Stage, context.DoneOptions{BoardOverride: req.Override}); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, err.Error())
		} else if errors.Is(err, context.ErrOpenChildren) {
			writeError(w, http.StatusConflict, err.Error())
		} else if errors.Is(err, context.ErrInvalidStage) {
			writeError(w, http.StatusBadRequest, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "failed to update task stage: "+err.Error())
		}
		return
	}

	if h.hub != nil {
		h.hub.Publish("task_stage_changed", map[string]string{
			"task_id": id,
			"stage":   req.Stage,
		})
	}

	if req.Stage == governance.StageInProgress {
		// Run Now. A backlog task was moved to todo first by
		// SetTaskExecutionStageWithOptions, so the claim accepts it.
		// Per-click key so each Run Now press can start a new run even within 24 h.
		orchestrator.GlobalDispatcher.Wake(id, "run_now", "run_now:"+id+":"+uuid.New().String()[:8])
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"task_id": id,
		"stage":   req.Stage,
	})
}

// GetRunSteps handles GET /api/tasks/{id}/run-steps
// GetTaskCheckpoints handles GET /api/tasks/{id}/checkpoints
func (h *TasksHandler) GetTaskCheckpoints(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}
	task, err := context.GetTask(h.db, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	limitStr := r.URL.Query().Get("limit")
	limit := 20
	if limitStr != "" {
		if v, err2 := strconv.Atoi(limitStr); err2 == nil && v > 0 {
			limit = v
		}
	}
	ctx, cancel := gitRequestContext(r)
	defer cancel()
	checkpoints, err := checkpoint.ListCheckpoints(ctx, task.RepoPath, limit)
	if gitexec.IsTimeout(err) {
		writeError(w, http.StatusGatewayTimeout, err.Error())
		return
	}
	if err != nil {
		// not a git repo or no checkpoints yet — return empty list
		checkpoints = []checkpoint.Checkpoint{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"checkpoints": checkpoints})
}

// taskCheckpointWorkDir returns the path to use for checkpoint operations.
// If the task's git worktree still exists (active run), it is returned so that
// diffs and restores target the agent's isolated tree. Otherwise the parent
// repo path is returned with hasWorktree=false so the caller can fall back to a
// branch-based diff instead.
func taskCheckpointWorkDir(task *context.Task) (workDir string, hasWorktree bool) {
	// A path-like task ID would point at .worktrees itself or outside it.
	if workspace.ValidateTaskID(task.ID) != nil {
		return task.RepoPath, false
	}
	wt := filepath.Join(task.RepoPath, ".worktrees", task.ID)
	if _, err := os.Stat(wt); err == nil {
		return wt, true
	}
	return task.RepoPath, false
}

// GetTaskDiff handles GET /api/tasks/{id}/diff?checkpoint={id}
// Returns diff stat, per-file stats, and file list between working tree and the named checkpoint.
func (h *TasksHandler) GetTaskDiff(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}
	task, err := context.GetTask(h.db, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	cpID := r.URL.Query().Get("checkpoint")
	ctx, cancel := gitRequestContext(r)
	defer cancel()

	// "Whole run" (empty checkpoint) diffs against the task's base: the same
	// verified commit the ship review card and Approve use (STA-774).
	wholeRun := cpID == ""
	baseVerified := false
	if wholeRun {
		var err error
		cpID, baseVerified, err = wholeRunBase(ctx, h.db, task)
		if err != nil {
			writeWholeRunBaseError(w, err)
			return
		}
	}

	workDir, hasWorktree := taskCheckpointWorkDir(task)
	var stat string
	var fileStats []checkpoint.FileDiffStat
	var statErr, filesErr error
	if hasWorktree {
		stat, statErr = checkpoint.DiffCheckpoint(ctx, workDir, cpID)
		fileStats, filesErr = checkpoint.DiffCheckpointFiles(ctx, workDir, cpID)
	} else {
		// Worktree pruned — compare checkpoint against the task branch tip.
		branch := "staypoint/" + task.ID
		stat, statErr = checkpoint.DiffCheckpointAgainstRef(ctx, task.RepoPath, cpID, branch)
		fileStats, filesErr = checkpoint.DiffCheckpointFilesAgainstRef(ctx, task.RepoPath, cpID, branch)
	}
	// A timeout is not "no changes": say so instead of showing an empty diff.
	for _, e := range []error{statErr, filesErr} {
		if gitexec.IsTimeout(e) {
			writeError(w, http.StatusGatewayTimeout, e.Error())
			return
		}
	}
	if statErr != nil {
		stat = ""
	}
	if filesErr != nil {
		fileStats = []checkpoint.FileDiffStat{}
	}

	// Build plain file list for backwards compatibility.
	files := make([]string, len(fileStats))
	for i, s := range fileStats {
		files[i] = s.Path
	}

	resp := map[string]any{
		"diff":          stat,
		"files":         files,
		"file_stats":    fileStats,
		"checkpoint_id": cpID,
	}
	if wholeRun {
		resp["base_verified"] = baseVerified
		// An answer-only run: nothing differs from the verified base, so
		// there is no card and the UI shows the final message instead.
		answerOnly := baseVerified && statErr == nil && filesErr == nil && len(fileStats) == 0
		resp["answer_only"] = answerOnly
		if answerOnly {
			resp["files_read"] = taskFilesRead(ctx, h.db, task.ID)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// wholeRunBase returns the commit the Diff tab's whole-run view compares
// against: the task's recorded base, verified (workspace.VerifiedBase), the
// same one the ship card and Approve use. A task with no recorded base
// (created before STA-774) falls back to its pre-run checkpoint for display
// only, reported as verified=false: such a task can never get a card or an
// Approve. A moved pin or a git/DB error is returned.
func wholeRunBase(ctx gocontext.Context, db *sql.DB, task *context.Task) (cpID string, verified bool, err error) {
	base, err := workspace.VerifiedBase(ctx, db, task.RepoPath, task.ID)
	if err == nil {
		return base, true, nil
	}
	if !errors.Is(err, workspace.ErrNoTaskBase) {
		return "", false, err
	}
	preRunID, _ := checkpoint.FindPreRunCheckpoint(ctx, task.RepoPath, task.ID)
	return preRunID, false, nil
}

// writeWholeRunBaseError answers a whole-run diff whose base failed
// verification: 409 when the pin was moved, the git status otherwise. It
// never returns a diff measured from an unverified commit.
func writeWholeRunBaseError(w http.ResponseWriter, err error) {
	if errors.Is(err, workspace.ErrTaskBaseTampered) {
		writeErrorJSON(w, http.StatusConflict, map[string]any{"error": "base_tampered", "message": err.Error()})
		return
	}
	writeError(w, gitErrorStatus(err), "could not verify the task base: "+err.Error())
}

// taskFilesRead lists the distinct files the task's runs read, from the
// timeline's "Read <path>" steps (STA-774: shown for runs with no edits).
func taskFilesRead(ctx gocontext.Context, db *sql.DB, taskID string) []string {
	files := []string{}
	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT substr(title, 6) FROM run_steps
		 WHERE task_id = ? AND kind = 'read' AND title LIKE 'Read %'
		 ORDER BY 1`, taskID)
	if err != nil {
		return files
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil && p != "" {
			files = append(files, p)
		}
	}
	return files
}

// GetTaskFileDiff handles GET /api/tasks/{id}/diff/file?path={path}&checkpoint={id}
// Returns the full unified diff for a single file.
func (h *TasksHandler) GetTaskFileDiff(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}
	task, err := context.GetTask(h.db, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}

	cpID := r.URL.Query().Get("checkpoint")
	ctx, cancel := gitRequestContext(r)
	defer cancel()
	if cpID == "" {
		base, _, baseErr := wholeRunBase(ctx, h.db, task)
		if baseErr != nil {
			writeWholeRunBaseError(w, baseErr)
			return
		}
		cpID = base
	}

	workDir, hasWorktree := taskCheckpointWorkDir(task)
	var content string
	if hasWorktree {
		content, err = checkpoint.DiffFileContent(ctx, workDir, cpID, filePath)
	} else {
		branch := "staypoint/" + task.ID
		content, err = checkpoint.DiffFileContentAgainstRef(ctx, task.RepoPath, cpID, branch, filePath)
	}
	if gitexec.IsTimeout(err) {
		writeError(w, http.StatusGatewayTimeout, err.Error())
		return
	}
	if err != nil {
		content = ""
	}

	binary := strings.Contains(content, "Binary files")
	status := "modified"
	if strings.Contains(content, "new file mode") {
		status = "added"
	} else if strings.Contains(content, "deleted file mode") {
		status = "deleted"
	} else if strings.Contains(content, "rename from") {
		status = "renamed"
	}

	const maxDiffBytes = 256 * 1024 // 256 KB
	truncated := len(content) > maxDiffBytes
	if truncated {
		// Trim at a safe boundary to avoid multi-byte rune splits.
		content = content[:maxDiffBytes]
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"path":          filePath,
		"content":       content,
		"binary":        binary,
		"truncated":     truncated,
		"status":        status,
		"checkpoint_id": cpID,
	})
}

// RestoreFileHandler handles POST /api/tasks/{id}/checkpoint-restore-file
// Restores a single file from the given checkpoint into the working tree.
func (h *TasksHandler) RestoreFileHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}
	task, err := context.GetTask(h.db, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	var req struct {
		CheckpointID string `json:"checkpoint_id"`
		FilePath     string `json:"file_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.FilePath == "" {
		writeError(w, http.StatusBadRequest, "file_path is required")
		return
	}
	workDir, hasWorktree := taskCheckpointWorkDir(task)
	if !hasWorktree {
		writeError(w, http.StatusConflict, "task worktree is no longer active; restore requires an active run")
		return
	}
	if err := checkpoint.RestoreFile(r.Context(), workDir, req.CheckpointID, req.FilePath); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("restore failed: %s", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "file_path": req.FilePath})
}

// UndoTaskCheckpoint handles POST /api/tasks/{id}/checkpoint-undo
func (h *TasksHandler) UndoTaskCheckpoint(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}
	task, err := context.GetTask(h.db, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	var req struct {
		CheckpointID string `json:"checkpoint_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	workDir, hasWorktree := taskCheckpointWorkDir(task)
	if !hasWorktree {
		writeError(w, http.StatusConflict, "task worktree is no longer active; undo requires an active run")
		return
	}
	result, err := checkpoint.Undo(r.Context(), checkpoint.UndoOptions{
		WorkDir:      workDir,
		CheckpointID: req.CheckpointID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("undo failed: %s", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func (h *TasksHandler) GetRunSteps(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}

	if _, err := context.GetTask(h.db, id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	steps, err := context.ListRunStepsByTask(h.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if steps == nil {
		steps = []context.RunStep{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"steps": steps,
	})
}

// GetRunErrors handles GET /api/tasks/{id}/run-errors
func (h *TasksHandler) GetRunErrors(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id is required")
		return
	}
	if _, err := context.GetTask(h.db, id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	errs, err := context.ListRunErrorsByTask(h.db, id, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if errs == nil {
		errs = []context.RunError{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": errs})
}

// GetAllRunErrors handles GET /api/run-errors
func (h *TasksHandler) GetAllRunErrors(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	taskID := q.Get("task_id")
	runID := q.Get("run_id")
	limit := 100
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	errs, err := context.ListAllRunErrors(h.db, taskID, runID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if errs == nil {
		errs = []context.RunError{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": errs})
}

// GetTaskMigrations handles GET /api/tasks/{id}/migrations
// Returns migration files found in the task's diff, with SQL content, risk analysis,
// and verification checks derived from the DDL.
func (h *TasksHandler) GetTaskMigrations(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id required")
		return
	}
	task, err := context.GetTask(h.db, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	// Load per-project config for globs + SQL editor URL.
	projCfg, _ := shipreview.GetProjectDevConfig(h.db, task.RepoPath)
	globs := migration.DefaultGlobs
	sqlEditorURL := ""
	if projCfg != nil {
		if len(projCfg.MigrationGlobs) > 0 {
			globs = projCfg.MigrationGlobs
		}
		sqlEditorURL = projCfg.SQLEditorURL
	}

	// Get the full diff file list.
	ctx, cancel := gitRequestContext(r)
	defer cancel()
	cpID, _, baseErr := wholeRunBase(ctx, h.db, task)
	if baseErr != nil {
		writeWholeRunBaseError(w, baseErr)
		return
	}
	workDir, hasWorktree := taskCheckpointWorkDir(task)
	var fileStats []checkpoint.FileDiffStat
	var diffErr error
	if hasWorktree {
		fileStats, diffErr = checkpoint.DiffCheckpointFiles(ctx, workDir, cpID)
	} else {
		branch := "staypoint/" + task.ID
		fileStats, diffErr = checkpoint.DiffCheckpointFilesAgainstRef(ctx, task.RepoPath, cpID, branch)
	}
	// Other errors keep the old "no migrations" answer; a timeout means the
	// diff was never read, which must not look like an empty list.
	if gitexec.IsTimeout(diffErr) {
		writeError(w, http.StatusGatewayTimeout, diffErr.Error())
		return
	}

	filePaths := make([]string, len(fileStats))
	for i, s := range fileStats {
		filePaths[i] = s.Path
	}

	migPaths := migration.Detect(filePaths, globs)

	// Determine if a read-only DB connection is configured for auto-verify.
	hasAutoConn := false
	if dsn, _ := migration.GetProjectDSN(task.RepoPath); dsn != "" {
		hasAutoConn = true
	}

	// Read SQL content and check risk for each detected migration file.
	type MigrationFileResponse struct {
		migration.File
		VerificationChecks []migration.Check `json:"verification_checks"`
		VerificationQuery  string            `json:"verification_query"`
	}
	files := make([]MigrationFileResponse, 0, len(migPaths))
	branch := "staypoint/" + task.ID
	for _, p := range migPaths {
		var sqlContent string
		var readErr error
		if hasWorktree {
			sqlContent, readErr = migration.ReadContent(workDir, p)
		}
		if !hasWorktree || readErr != nil {
			// The worktree is gone (or lacks the file): read it from the task
			// branch. The repo checkout is usually on main, where the file
			// doesn't exist yet.
			sqlContent, readErr = migration.ReadContentAtRef(ctx, task.RepoPath, branch, p)
		}
		f := migration.File{Path: p, SQL: sqlContent, RiskStatements: []string{}}
		var checks []migration.Check
		var verQuery string
		if readErr != nil || strings.TrimSpace(sqlContent) == "" {
			// Never verify against unreadable or empty SQL — read_error = ✗.
			f.ReadError = "could not read migration file from the task branch"
			if readErr != nil {
				f.ReadError += ": " + readErr.Error()
			}
			f.AdditiveOnly = false
		} else {
			risks, idempotents, hasRisk := migration.CheckRisk(sqlContent)
			if risks != nil {
				f.RiskStatements = risks
			}
			if idempotents != nil {
				f.IdempotentReCreates = idempotents
			}
			f.AdditiveOnly = !hasRisk
			checks = migration.ParseChecks(sqlContent)
			verQuery = migration.BuildVerificationQuery(checks)
		}
		files = append(files, MigrationFileResponse{
			File:               f,
			VerificationChecks: checks,
			VerificationQuery:  verQuery,
		})
	}

	// Attach the latest "Mark applied" record per path so the state survives a
	// reload (it was only kept in the button before).
	type appliedRecord struct {
		appliedBy string
		appliedAt string
		verified  bool
	}
	if rows, err := h.db.Query(
		`SELECT details, created_at FROM activity_log WHERE task_id = ? AND event_type = 'migration_applied' ORDER BY created_at ASC`, task.ID,
	); err == nil {
		applied := map[string]appliedRecord{}
		for rows.Next() {
			var details, at string
			if rows.Scan(&details, &at) != nil {
				continue
			}
			var d struct {
				Path      string `json:"path"`
				AppliedBy string `json:"applied_by"`
				Mode      string `json:"mode"`
			}
			if json.Unmarshal([]byte(details), &d) == nil && d.Path != "" {
				// verified = true when mode is auto or manual (schema checks ran and passed).
				// unchecked / auto_error = not verified.
				verified := d.Mode == "auto" || d.Mode == "manual"
				applied[d.Path] = appliedRecord{appliedBy: d.AppliedBy, appliedAt: at, verified: verified}
			}
		}
		rows.Close()
		for i := range files {
			if a, ok := applied[files[i].Path]; ok {
				files[i].AppliedBy, files[i].AppliedAt = a.appliedBy, a.appliedAt
				files[i].Verified = a.verified
			}
		}
	}

	writeJSON(w, map[string]any{
		"migrations":      files,
		"sql_editor_url":  sqlEditorURL,
		"has_auto_verify": hasAutoConn,
	})
}

// MarkMigrationApplied handles POST /api/tasks/{id}/migrations/mark-applied.
//
// It always parses the migration file to produce verification checks, then runs
// them in one of two modes:
//
//   - Auto mode: a read-only DSN is stored in the macOS Keychain for this
//     project. StayPoint connects, runs each check inside BEGIN READ ONLY, and
//     records "applied" only when every check passes.
//
//   - Manual fallback: no DSN configured, or the body includes
//     check_results from a previous copy-and-paste round. If check_results are
//     present and all pass, "applied" is recorded. If not, the response returns
//     the verification_query for the caller to run externally.
//
// Override: include override_reason (non-empty string) to bypass failed checks;
// the reason is recorded in the activity log.
func (h *TasksHandler) MarkMigrationApplied(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id required")
		return
	}
	task, err := context.GetTask(h.db, id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	var body struct {
		Path           string `json:"path"`
		AppliedBy      string `json:"applied_by"`
		// ManualResults contains check results pasted back by the user in manual mode.
		ManualResults  []struct {
			Description string `json:"description"`
			Passed      bool   `json:"passed"`
		} `json:"check_results"`
		// OverrideReason bypasses verification failures when non-empty.
		OverrideReason string `json:"override_reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if body.Path == "" {
		writeError(w, http.StatusBadRequest, "path required")
		return
	}
	if body.AppliedBy == "" {
		body.AppliedBy = "board"
	}

	// Read migration SQL: try the task worktree, then the repo root on disk,
	// then the task branch via git-show (handles pruned worktrees).
	// If the file cannot be read at all (e.g. task has no associated repo in CI
	// test environments), fall through with empty SQL — ParseChecks("") returns
	// no checks, which routes to unchecked mode below (ok:true, mode:unchecked).
	workDir, hasWT := taskCheckpointWorkDir(task)
	var sqlContent string
	var readErr error
	if hasWT {
		sqlContent, readErr = migration.ReadContent(workDir, body.Path)
	}
	if !hasWT || readErr != nil {
		// Fall back to repo root on disk (covers dev/test setups without a worktree).
		sqlContent, readErr = migration.ReadContent(task.RepoPath, body.Path)
	}
	if readErr != nil {
		// Repo root doesn't have the file (common after a task branch is pruned and
		// main doesn't include the file yet): read from the task branch via git.
		sqlContent, readErr = migration.ReadContentAtRef(r.Context(), task.RepoPath, "staypoint/"+task.ID, body.Path)
	}
	// readErr means we couldn't find the file anywhere — treat as empty SQL,
	// which results in unchecked mode. Never return 422 here; the caller recorded
	// a human decision (mark applied) and we fall back to unverified rather than
	// blocking the action entirely.
	if readErr != nil {
		sqlContent = ""
	}

	checks := migration.ParseChecks(sqlContent)
	verQuery := migration.BuildVerificationQuery(checks)

	// No detectable objects → nothing to verify; record applied immediately.
	if len(checks) == 0 {
		details, _ := json.Marshal(map[string]any{
			"path":       body.Path,
			"applied_by": body.AppliedBy,
			"mode":       "unchecked",
			"reason":     "no parseable DDL/DML found in migration file",
		})
		if err := context.LogActivity(h.db, id, "migration_applied", string(details)); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]any{
			"ok":         true,
			"path":       body.Path,
			"applied_by": body.AppliedBy,
			"mode":       "unchecked",
			"checks":     []migration.Check{},
		})
		return
	}

	mode := "manual"
	var results []migration.CheckResult
	var verifyErr string

	// Attempt auto mode via Keychain DSN.
	dsn, _ := migration.GetProjectDSN(task.RepoPath)
	if dsn != "" {
		mode = "auto"
		results, err = migration.RunChecks(r.Context(), dsn, checks)
		if err != nil {
			verifyErr = err.Error()
			mode = "auto_error"
		}
	} else if len(body.ManualResults) > 0 {
		// Manual mode: user supplied check results.
		mode = "manual"
		for _, mr := range body.ManualResults {
			results = append(results, migration.CheckResult{
				Check:  migration.Check{Description: mr.Description},
				Passed: mr.Passed,
			})
		}
	} else {
		// No DSN and no manual results — return the query for the user to run.
		writeJSON(w, map[string]any{
			"ok":                 false,
			"mode":               "manual",
			"verification_query": verQuery,
			"checks":             checks,
			"message":            "run the verification_query and submit check_results to confirm",
		})
		return
	}

	allPassed := verifyErr == "" && migration.AllPassed(results)
	overridden := body.OverrideReason != ""

	if !allPassed && !overridden {
		failed := migration.FailedDescriptions(results)
		writeErrorJSON(w, http.StatusConflict, map[string]any{
			"error":   "verification failed: some schema objects are missing",
			"failed":  failed,
			"mode":    mode,
			"checks":  results,
		})
		return
	}

	// Build detailed activity record.
	logPayload := map[string]any{
		"path":       body.Path,
		"applied_by": body.AppliedBy,
		"mode":       mode,
		"checks":     results,
	}
	if verifyErr != "" {
		logPayload["verify_error"] = verifyErr
	}
	if overridden {
		logPayload["override_reason"] = body.OverrideReason
	}
	detailsBytes, _ := json.Marshal(logPayload)
	if err := context.LogActivity(h.db, id, "migration_applied", string(detailsBytes)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, map[string]any{
		"ok":         true,
		"path":       body.Path,
		"applied_by": body.AppliedBy,
		"mode":       mode,
		"checks":     results,
	})
}

// writeErrorJSON writes a JSON error body (for rich error responses with extra fields).
func writeErrorJSON(w http.ResponseWriter, status int, payload map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
