package server

import (
	gocontext "context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"

	"github.com/VinnyVanGogh/staypoint/internal/checkpoint"
	"github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/migration"
	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
	"github.com/google/uuid"
)

// ShipReviewHandler handles Ship Review card lifecycle endpoints.
type ShipReviewHandler struct {
	db  *sql.DB
	hub *EventHub
}

func NewShipReviewHandler(db *sql.DB, hub *EventHub) *ShipReviewHandler {
	return &ShipReviewHandler{db: db, hub: hub}
}

// shipReviewEnabled reads the live toggle from settings_kv (default on).
func (h *ShipReviewHandler) shipReviewEnabled() bool {
	val, err := getSettingKV(h.db, "gates.ship_review")
	if err != nil {
		return true // default on
	}
	return val != "false"
}

// GetCard handles GET /api/tasks/{id}/ship-review
func (h *ShipReviewHandler) GetCard(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "task id required")
		return
	}
	card, err := shipreview.GetCard(h.db, taskID)
	if errors.Is(err, shipreview.ErrNoCard) {
		writeError(w, http.StatusNotFound, "no ship review card")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Augment the card with unverified migration info so the UI can block Approve.
	// Merge into a flat map so existing clients that read card fields directly continue to work.
	task, taskErr := context.GetTask(h.db, taskID)
	if taskErr == nil {
		unverified, _ := unverifiedMigrations(r.Context(), h.db, task)
		cardBytes, _ := json.Marshal(card)
		var merged map[string]any
		if jsonErr := json.Unmarshal(cardBytes, &merged); jsonErr == nil {
			merged["unverified_migrations"] = unverified
			merged["repo_name"] = filepath.Base(task.RepoPath)
			writeJSON(w, merged)
			return
		}
	}
	writeJSON(w, card)
}

// UpsertCard handles PUT /api/tasks/{id}/ship-review
// Agent calls this to create or refresh the card.
func (h *ShipReviewHandler) UpsertCard(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "task id required")
		return
	}

	if !h.shipReviewEnabled() {
		writeError(w, http.StatusConflict, "ship_review gate is disabled")
		return
	}

	var req struct {
		TestSteps []string               `json:"test_steps"`
		DevURL    string                 `json:"dev_url"`
		CheckRuns []shipreview.CheckRun  `json:"check_runs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if len(req.TestSteps) == 0 {
		writeError(w, http.StatusBadRequest, shipreview.ErrTestStepsRequired.Error())
		return
	}
	if req.DevURL != "" {
		if err := shipreview.ValidateDevURL(req.DevURL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	task, err := context.GetTask(h.db, taskID)
	if err != nil {
		writeError(w, http.StatusNotFound, "task not found: "+err.Error())
		return
	}

	card, err := shipreview.BuildAndStartCard(r.Context(), h.db, task.ID, task.RepoPath, req.TestSteps, req.DevURL, req.CheckRuns)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, shipreview.ErrTestStepsRequired) || errors.Is(err, shipreview.ErrInvalidBranch) || errors.Is(err, shipreview.ErrInvalidDevURL) {
			code = http.StatusBadRequest
		}
		writeError(w, code, err.Error())
		return
	}

	h.hub.Publish("ship_review_created", map[string]any{
		"task_id":    taskID,
		"card_id":    card.ID,
		"branch":     card.Branch,
		"head_sha":   card.HeadSHA,
		"test_steps": card.TestSteps,
		"dev_url":    card.DevURL,
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(card)
}

// StartDev handles POST /api/tasks/{id}/ship-review/start-dev
// Returns 202 immediately; setup runs async and streams progress via SSE.
func (h *ShipReviewHandler) StartDev(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	card, task, ok := h.requireCard(w, taskID)
	if !ok {
		return
	}

	cfg, err := shipreview.GetProjectDevConfig(h.db, task.RepoPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load project config: "+err.Error())
		return
	}

	// Auto-detect Supabase project if no config exists yet.
	if cfg.DevCommand == "" && shipreview.HasSupabaseConfig(task.RepoPath) {
		proposed := shipreview.ProposeSupabaseDevConfig(task.RepoPath)
		if uErr := shipreview.UpsertProjectDevConfig(h.db, proposed); uErr == nil {
			cfg = proposed
			h.hub.Publish("ship_review_dev_config_proposed", map[string]any{
				"task_id":          taskID,
				"supabase_enabled": true,
				"dev_url":          proposed.DevURL,
			})
		}
	}

	if cfg.DevCommand == "" {
		writeError(w, http.StatusConflict, "no dev_command configured for this project")
		return
	}

	// Stream progress to board via SSE.
	progress := func(p shipreview.DevProgress) {
		h.hub.Publish("ship_review_dev_progress", map[string]any{
			"task_id": p.TaskID,
			"step":    p.Step,
			"message": p.Message,
			"ok":      p.OK,
		})
	}

	url, err := shipreview.StartDevServerAsync(h.db, card, cfg, task.RepoPath, progress)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "start dev server: "+err.Error())
		return
	}

	if card.DevURL == "" {
		_ = shipreview.SetDevURL(h.db, card.ID, url)
	}

	h.hub.Publish("ship_review_dev_started", map[string]any{"task_id": taskID, "dev_url": url})
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]any{"dev_url": url, "status": "starting"})
}

// StopDev handles POST /api/tasks/{id}/ship-review/stop-dev
func (h *ShipReviewHandler) StopDev(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	card, _, ok := h.requireCard(w, taskID)
	if !ok {
		return
	}
	shipreview.StopDevServer(h.db, card)
	h.hub.Publish("ship_review_dev_stopped", map[string]any{"task_id": taskID})
	writeJSON(w, map[string]string{"status": "ok"})
}

// unverifiedMigrations returns migration file paths that appear in the task diff
// but have not been marked applied (with successful verification) in the activity log.
func unverifiedMigrations(ctx gocontext.Context, db *sql.DB, task *context.Task) ([]string, error) {
	// Detect migration files in the task diff.
	workDir, hasWorktree := taskCheckpointWorkDir(task)
	cpID := ""
	var fileStats []checkpoint.FileDiffStat
	if hasWorktree {
		fileStats, _ = checkpoint.DiffCheckpointFiles(ctx, workDir, cpID)
	} else {
		branch := "staypoint/" + task.ID
		fileStats, _ = checkpoint.DiffCheckpointFilesAgainstRef(ctx, task.RepoPath, cpID, branch)
	}
	filePaths := make([]string, len(fileStats))
	for i, s := range fileStats {
		filePaths[i] = s.Path
	}
	migPaths := migration.Detect(filePaths, migration.DefaultGlobs)
	if len(migPaths) == 0 {
		return nil, nil
	}

	// Build set of applied migration paths from activity log.
	logs, err := context.GetTaskActivityLog(db, task.ID)
	if err != nil {
		return nil, err
	}
	appliedPaths := map[string]bool{}
	for _, l := range logs {
		if l.EventType != "migration_applied" {
			continue
		}
		var detail struct {
			Path string `json:"path"`
		}
		if jsonErr := json.Unmarshal([]byte(l.Details), &detail); jsonErr == nil && detail.Path != "" {
			appliedPaths[detail.Path] = true
		}
	}

	var unverified []string
	for _, p := range migPaths {
		if !appliedPaths[p] {
			unverified = append(unverified, p)
		}
	}
	return unverified, nil
}

// Approve handles POST /api/tasks/{id}/ship-review/approve (Board action)
func (h *ShipReviewHandler) Approve(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	card, task, ok := h.requireCard(w, taskID)
	if !ok {
		return
	}
	if card.Status != shipreview.StatusPending {
		writeError(w, http.StatusConflict, "card is not pending")
		return
	}

	// Parse optional override reason from request body.
	var req struct {
		MigrationOverrideReason string `json:"migration_override_reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	// Block if any migration has not been verified, unless override supplied.
	// Fail-closed: if we can't determine unverified migrations, block approval.
	if req.MigrationOverrideReason == "" {
		unverified, unverifiedErr := unverifiedMigrations(r.Context(), h.db, task)
		if unverifiedErr != nil {
			writeError(w, http.StatusInternalServerError, "could not check migration verification status: "+unverifiedErr.Error())
			return
		}
		if len(unverified) > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":                 "unverified_migrations",
				"message":               "one or more migration files have not been verified; mark them applied or supply migration_override_reason",
				"unverified_migrations": unverified,
			})
			return
		}
	} else {
		// Log the override reason before merging.
		logPayload, _ := json.Marshal(map[string]string{
			"override_reason": req.MigrationOverrideReason,
		})
		_ = context.LogActivity(h.db, taskID, "migration_override", string(logPayload))
	}

	// Always merge from the repo root (on main), never from a task worktree which
	// may be gone or have main checked out elsewhere (causing checkout conflicts).
	mainSHA, err := shipreview.ApproveAndMerge(r.Context(), h.db, card, task.RepoPath, "main")
	if errors.Is(err, shipreview.ErrHeadMoved) {
		newHead, _ := shipreview.CurrentBranchHEAD(r.Context(), task.RepoPath, card.Branch)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":        "head_moved",
			"message":      err.Error(),
			"new_head_sha": newHead,
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "merge failed: "+err.Error())
		return
	}

	shipreview.StopDevServer(h.db, card)
	// Register the merged commit so MarkTaskDone's work-product guard succeeds.
	_ = context.AddWorkProduct(h.db, taskID, "commit", mainSHA)
	_ = context.MarkTaskDone(h.db, taskID)

	_ = governance.LogEvent(h.db, taskID, "board", governance.AuditBoardAction, nil, nil,
		map[string]string{"action": "approve", "ip": r.RemoteAddr, "user_agent": r.UserAgent()})

	h.hub.Publish("ship_review_approved", map[string]any{
		"task_id":      taskID,
		"approved_sha": card.HeadSHA,
		"main_sha":     mainSHA,
	})

	refreshed, _ := shipreview.GetCard(h.db, taskID)
	writeJSON(w, map[string]any{"card": refreshed, "main_sha": mainSHA})
}

// SendBack handles POST /api/tasks/{id}/ship-review/send-back (Board action)
func (h *ShipReviewHandler) SendBack(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	card, _, ok := h.requireCard(w, taskID)
	if !ok {
		return
	}
	var req struct {
		Comment string `json:"comment"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Comment == "" {
		writeError(w, http.StatusBadRequest, "comment is required")
		return
	}

	if err := shipreview.SendBack(h.db, card, req.Comment); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	shipreview.StopDevServer(h.db, card)
	_ = context.AddTaskComment(h.db, taskID, "board", req.Comment)
	_ = governance.LogEvent(h.db, taskID, "board", governance.AuditBoardAction, nil, nil,
		map[string]string{"action": "send_back", "ip": r.RemoteAddr, "user_agent": r.UserAgent()})

	h.hub.Publish("ship_review_sent_back", map[string]any{
		"task_id": taskID,
		"comment": req.Comment,
	})
	writeJSON(w, map[string]string{"status": "ok"})
}

// Reject handles POST /api/tasks/{id}/ship-review/reject (Board action)
func (h *ShipReviewHandler) Reject(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	card, task, ok := h.requireCard(w, taskID)
	if !ok {
		return
	}
	var req struct {
		Comment      string `json:"comment"`
		DeleteBranch bool   `json:"delete_branch"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	if err := shipreview.Reject(h.db, card, req.Comment); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	shipreview.StopDevServer(h.db, card)

	if req.DeleteBranch {
		if err := shipreview.DeleteBranch(r.Context(), task.RepoPath, card.Branch); err != nil {
			writeError(w, http.StatusConflict, "delete branch failed: "+err.Error())
			return
		}
	}

	_ = governance.LogEvent(h.db, taskID, "board", governance.AuditBoardAction, nil, nil,
		map[string]string{"action": "reject", "ip": r.RemoteAddr, "user_agent": r.UserAgent()})

	h.hub.Publish("ship_review_rejected", map[string]any{
		"task_id":        taskID,
		"comment":        req.Comment,
		"branch_deleted": req.DeleteBranch,
	})
	writeJSON(w, map[string]string{"status": "ok"})
}

// GetSettings handles GET /api/settings/ship-review
func (h *ShipReviewHandler) GetSettings(w http.ResponseWriter, r *http.Request) {
	val, _ := getSettingKV(h.db, "gates.ship_review")
	enabled := val != "false"
	writeJSON(w, map[string]any{"ship_review": enabled})
}

// SetSettings handles POST /api/settings/ship-review
func (h *ShipReviewHandler) SetSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ShipReview bool `json:"ship_review"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	val := "true"
	if !req.ShipReview {
		val = "false"
	}
	if err := setSettingKV(h.db, "gates.ship_review", val); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := governance.LogBoardEvent(h.db, "board", governance.AuditBoardAction,
		map[string]string{"action": "set_ship_review_settings", "ip": r.RemoteAddr, "user_agent": r.UserAgent()}); err != nil {
		writeError(w, http.StatusInternalServerError, "audit write failed: "+err.Error())
		return
	}
	h.hub.Publish("ship_review_settings", map[string]any{"ship_review": req.ShipReview})
	writeJSON(w, map[string]any{"ship_review": req.ShipReview})
}

// ListProjectDevConfigs handles GET /api/project-dev-configs
func (h *ShipReviewHandler) ListProjectDevConfigs(w http.ResponseWriter, r *http.Request) {
	cfgs, err := shipreview.ListProjectDevConfigs(h.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if cfgs == nil {
		cfgs = []*shipreview.ProjectDevConfig{}
	}
	writeJSON(w, map[string]any{"configs": cfgs})
}

// UpsertProjectDevConfig handles PUT /api/project-dev-configs.
// Board-action gate is enforced by WrapBoardAction in server.go (STA-520).
func (h *ShipReviewHandler) UpsertProjectDevConfig(w http.ResponseWriter, r *http.Request) {
	var cfg shipreview.ProjectDevConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if cfg.RepoPath == "" {
		writeError(w, http.StatusBadRequest, "repo_path required")
		return
	}
	if cfg.DevURL != "" {
		if err := shipreview.ValidateDevURL(cfg.DevURL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if cfg.SQLEditorURL != "" {
		if err := shipreview.ValidateSQLEditorURL(cfg.SQLEditorURL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// Read old config before the write so we can record old → new in the audit log.
	old, _ := shipreview.GetProjectDevConfig(h.db, cfg.RepoPath)

	if err := shipreview.UpsertProjectDevConfig(h.db, &cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Audit every Board-approved dev-config change via governance.LogBoardEvent.
	// Fail the whole request if the audit write fails (STA-520).
	if err := governance.LogBoardEvent(h.db, "board", "dev_config_change", map[string]any{
		"repo_path":       cfg.RepoPath,
		"old_dev_command": old.DevCommand,
		"new_dev_command": cfg.DevCommand,
		"old_setup_steps": old.SetupSteps,
		"new_setup_steps": cfg.SetupSteps,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "audit log write failed: "+err.Error())
		return
	}

	writeJSON(w, &cfg)
}

// requireCard is a helper that loads card + task for action handlers.
func (h *ShipReviewHandler) requireCard(w http.ResponseWriter, taskID string) (*shipreview.Card, *context.Task, bool) {
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "task id required")
		return nil, nil, false
	}
	task, err := context.GetTask(h.db, taskID)
	if err != nil {
		writeError(w, http.StatusNotFound, "task not found")
		return nil, nil, false
	}
	card, err := shipreview.GetCard(h.db, taskID)
	if errors.Is(err, shipreview.ErrNoCard) {
		writeError(w, http.StatusNotFound, "no ship review card")
		return nil, nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, nil, false
	}
	return card, task, true
}

// SeedCard handles PUT /api/tasks/{id}/ship-review/seed (test-only).
// Inserts a card directly into the DB without requiring a real git branch.
// Only registered when server.Options.TestMode is true.
func (h *ShipReviewHandler) SeedCard(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	if taskID == "" {
		writeError(w, http.StatusBadRequest, "task id required")
		return
	}
	var req struct {
		Status    string   `json:"status"`
		HeadSHA   string   `json:"head_sha"`
		Branch    string   `json:"branch"`
		TestSteps []string `json:"test_steps"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.Status == "" {
		req.Status = "pending"
	}
	if req.HeadSHA == "" {
		req.HeadSHA = "seed-" + uuid.New().String()[:12]
	}
	if req.Branch == "" {
		req.Branch = "staypoint/" + taskID
	}
	if len(req.TestSteps) == 0 {
		req.TestSteps = []string{"Verify the feature works as described."}
	}
	id := uuid.New().String()
	stepsJSON, _ := json.Marshal(req.TestSteps)
	_, err := h.db.Exec(`
		INSERT INTO ship_review_cards (id, task_id, branch, head_sha, test_steps_json, status)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		id, taskID, req.Branch, req.HeadSHA, string(stepsJSON), req.Status,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	card, err := shipreview.GetCard(h.db, taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, card)
}

