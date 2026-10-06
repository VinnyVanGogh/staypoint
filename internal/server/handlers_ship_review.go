package server

import (
	gocontext "context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/checkpoint"
	"github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/migration"
	"github.com/VinnyVanGogh/staypoint/internal/security"
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
		ctx, cancel := gitRequestContext(r)
		unverified, gitErr := unverifiedMigrations(ctx, h.db, task)
		cancel()
		cardBytes, _ := json.Marshal(card)
		var merged map[string]any
		if jsonErr := json.Unmarshal(cardBytes, &merged); jsonErr == nil {
			merged["unverified_migrations"] = unverified
			merged["repo_name"] = filepath.Base(task.RepoPath)
			// STA-727: the card shows the LIVE banner and confirm from these.
			// dev_configured: start-dev has something to run (a saved
			// dev_command, or a Supabase project it will auto-configure).
			if cfg, cfgErr := shipreview.GetProjectDevConfig(h.db, task.RepoPath); cfgErr == nil {
				isWork := shipreview.IsWorkRepo(task.RepoPath)
				merged["is_work_repo"] = isWork
				merged["effective_merge_mode"] = shipreview.EffectiveMergeMode(cfg, isWork)
				merged["live_credentials"] = cfg.LiveCredentials
				merged["dev_configured"] = cfg.DevCommand != "" || shipreview.HasSupabaseConfig(task.RepoPath)
			}
			if gitErr != nil {
				// The card itself is DB-only; return it and say why the
				// migration check is missing rather than hanging or erroring.
				if errors.Is(gitErr, errRepoUnreadable) {
					merged["repo_error"] = gitErr.Error()
				} else {
					merged["migration_check_error"] = gitErr.Error()
				}
			}
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

// liveDevWarning is the warning the Board confirms before starting a dev
// server for a live_credentials project (STA-727). The UI shows the same text.
const liveDevWarning = "LIVE PRODUCTION DATA. Actions in this preview are real."

type liveBoardGateKey struct{}

// StartDevGated routes POST start-dev. Non-live projects go straight to
// StartDev, agent-callable as before. A live_credentials project must first
// pass boardGate (WrapBoardAction: Board session + passkey assertion); StartDev
// then also requires {"confirm_live": true} and audits the confirmation.
func (h *ShipReviewHandler) StartDevGated(boardGate func(http.Handler) http.Handler) http.Handler {
	live := boardGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.StartDev(w, r.WithContext(gocontext.WithValue(r.Context(), liveBoardGateKey{}, true)))
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, task, ok := h.requireCard(w, r.PathValue("id"))
		if !ok {
			return
		}
		cfg, err := shipreview.GetProjectDevConfig(h.db, task.RepoPath)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "load project config: "+err.Error())
			return
		}
		if cfg.LiveCredentials {
			live.ServeHTTP(w, r)
			return
		}
		h.StartDev(w, r)
	})
}

// StartDev handles POST /api/tasks/{id}/ship-review/start-dev
// Returns 202 immediately; setup runs async and streams progress via SSE.
// Mount it through StartDevGated: on a live_credentials project it refuses any
// request that did not pass the Board gate.
func (h *ShipReviewHandler) StartDev(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	card, task, ok := h.requireCard(w, taskID)
	if !ok {
		return
	}
	// STA-654: only an open review gets a dev server. Approve/Reject stop it and
	// cleanup removes .worktrees/devserver-<id>; starting here would recreate
	// both. A sent_back card is also closed: the agent's resubmit replaces it
	// with a fresh pending card for the new head.
	if card.Status != shipreview.StatusPending {
		writeError(w, http.StatusConflict, "card is not pending")
		return
	}

	cfg, err := shipreview.GetProjectDevConfig(h.db, task.RepoPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load project config: "+err.Error())
		return
	}

	// STA-727: every gate check happens before any side effect (config
	// proposal, worktree, process), so a refused live start leaves no trace.
	if cfg.LiveCredentials {
		if passed, _ := r.Context().Value(liveBoardGateKey{}).(bool); !passed {
			writeBoardError(w, "board_session_required", "forbidden: starting a live_credentials dev server requires a Board session and passkey")
			return
		}
		var body struct {
			ConfirmLive bool `json:"confirm_live"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.ConfirmLive {
			writeErrorJSON(w, http.StatusConflict, map[string]any{
				"error":   "live_confirmation_required",
				"message": liveDevWarning + " Confirm to start the dev server.",
				"warning": liveDevWarning,
			})
			return
		}
		if err := governance.LogBoardEvent(h.db, "board", governance.AuditBoardAction, map[string]any{
			"action":     "live_dev_start_confirmed",
			"task_id":    taskID,
			"repo_path":  task.RepoPath,
			"warning":    liveDevWarning,
			"ip":         r.RemoteAddr,
			"user_agent": r.UserAgent(),
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "audit log write failed: "+err.Error())
			return
		}
	}

	// Auto-detect Supabase project if no config exists yet.
	if cfg.DevCommand == "" && shipreview.HasSupabaseConfig(task.RepoPath) {
		proposed := shipreview.ProposeSupabaseDevConfig(task.RepoPath)
		// The proposal replaces the whole row; keep the Board's settings.
		proposed.LiveCredentials = cfg.LiveCredentials
		proposed.MergeMode = cfg.MergeMode
		proposed.GHConfigDir = cfg.GHConfigDir
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

// errRepoUnreadable marks a migration check that failed because git could not
// read the task repo, as opposed to a database error.
var errRepoUnreadable = errors.New("cannot read task repo")

// unverifiedMigrations returns migration file paths that appear in the task diff
// but have not been marked applied (with successful verification) in the activity log.
//
// It fails closed: a diff error is returned (wrapping errRepoUnreadable) unless
// nothingToDiff confirms there was nothing to compare, so a repo git cannot
// read is never taken to have no migrations.
func unverifiedMigrations(ctx gocontext.Context, db *sql.DB, task *context.Task) ([]string, error) {
	// Detect migration files in the task diff.
	workDir, hasWorktree := taskCheckpointWorkDir(task)
	cpID := ""
	branch := "staypoint/" + task.ID
	refs := []string{checkpoint.LatestRef, branch}
	fileStats, diffErr := checkpoint.DiffCheckpointFilesAgainstRef(ctx, task.RepoPath, cpID, branch)
	if diffErr != nil {
		if gitexec.IsTimeout(diffErr) || !nothingToDiff(ctx, task.RepoPath, refs...) {
			return nil, fmt.Errorf("%w: %w", errRepoUnreadable, diffErr)
		}
		if !hasWorktree {
			return nil, nil
		}
	}
	if hasWorktree {
		wtStats, wtErr := checkpoint.DiffCheckpointFiles(ctx, workDir, cpID)
		if wtErr != nil {
			if gitexec.IsTimeout(wtErr) || !nothingToDiff(ctx, workDir, checkpoint.LatestRef) {
				return nil, fmt.Errorf("%w: %w", errRepoUnreadable, wtErr)
			}
		} else {
			fileStats = append(fileStats, wtStats...)
		}
	}

	seen := make(map[string]bool, len(fileStats))
	var filePaths []string
	for _, s := range fileStats {
		if !seen[s.Path] {
			seen[s.Path] = true
			filePaths = append(filePaths, s.Path)
		}
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
		if appliedPaths[p] {
			continue
		}
		exists, existsErr := migrationFileExistsAtTask(ctx, task, workDir, hasWorktree, p)
		if existsErr != nil {
			return nil, fmt.Errorf("%w: %w", errRepoUnreadable, existsErr)
		}
		if !exists {
			// Deleted migration files do not count as unverified migrations.
			continue
		}
		unverified = append(unverified, p)
	}
	return unverified, nil
}

// migrationFileExistsAtTask checks whether the migration file exists at the task branch
// tip or in the task's current working tree (if present). Deleted migrations
// return false so they do not count as unverified migrations. If git or filesystem
// inspection encounters an error (e.g. timeout, EPERM), it returns the error so the
// caller fails closed.
func migrationFileExistsAtTask(ctx gocontext.Context, task *context.Task, workDir string, hasWorktree bool, relPath string) (bool, error) {
	// First check the task branch tip, which is what ApproveAndMerge ships.
	// An uncommitted deletion in the worktree must not hide a migration that
	// the branch still ships.
	branch := "staypoint/" + task.ID
	cmd := gitexec.Command(ctx, "ls-tree", "--full-tree", "-z", "--name-only", branch, "--", filepath.ToSlash(relPath))
	cmd.Dir = task.RepoPath
	cmd.Env = security.ChildEnv()
	out, err := cmd.Output()
	if err == nil {
		if len(strings.TrimRight(string(out), "\x00")) > 0 {
			return true, nil
		}
	} else {
		// If the branch doesn't exist at all, and there is a worktree, we fall through
		// only if we can verify the branch ref simply does not exist (rev-parse exit 1).
		// Any timeout or git read error must fail closed.
		if hasWorktree && isRefMissing(ctx, task.RepoPath, branch) {
			// branch ref is missing; fall through to worktree check
		} else {
			return false, err
		}
	}

	// If absent from the branch tip, check the worktree (if present) for uncommitted files.
	if hasWorktree {
		_, statErr := os.Lstat(filepath.Join(workDir, relPath))
		if statErr == nil {
			return true, nil
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			return false, statErr
		}
	}

	return false, nil
}

func isRefMissing(ctx gocontext.Context, repoDir, ref string) bool {
	cmd := gitexec.Command(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	cmd.Dir = repoDir
	cmd.Env = security.ChildEnv()
	err := cmd.Run()
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 1
}

// nothingToDiff reports whether a failed migration diff failed only because
// there was nothing to compare: dir is not a git repo, or one of refs (the
// checkpoint baseline, the task branch) does not exist yet. It decides from the
// filesystem and git's exit status, never from the text of git's error, so a
// repo git was refused (EPERM) or timed out on is never mistaken for one with
// nothing in it.
func nothingToDiff(ctx gocontext.Context, dir string, refs ...string) bool {
	if inRepo, err := insideGitRepo(dir); err != nil {
		return false
	} else if !inRepo {
		return true
	}
	for _, ref := range refs {
		// --verify --quiet exits 1 when the ref does not exist and 128 when git
		// cannot read the repo.
		cmd := gitexec.Command(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
		cmd.Dir = dir
		cmd.Env = security.ChildEnv()
		err := cmd.Run()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return true
		}
		if err != nil {
			return false
		}
	}
	return false
}

// insideGitRepo reports whether dir or any of its ancestors holds a .git, the
// way git itself discovers a repo, since a task's repo_path can be a
// subdirectory of the repo. Any stat error other than "does not exist" (EPERM
// from a refused folder) is returned so the caller fails closed.
func insideGitRepo(dir string) (bool, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return false, err
	}
	for {
		_, err := os.Stat(filepath.Join(dir, ".git"))
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false, nil
		}
		dir = parent
	}
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
		// HeadSHA, when sent, is the head the Board was shown (STA-717).
		HeadSHA string `json:"head_sha"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.HeadSHA != "" && req.HeadSHA != card.HeadSHA {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":        "head_moved",
			"message":      "the card was re-pinned to a new head since you opened it",
			"new_head_sha": card.HeadSHA,
		})
		return
	}

	// Block if any migration has not been verified, unless override supplied.
	// Fail-closed: if we can't determine unverified migrations, block approval.
	if req.MigrationOverrideReason == "" {
		ctx, cancel := gitRequestContext(r)
		unverified, unverifiedErr := unverifiedMigrations(ctx, h.db, task)
		cancel()
		if unverifiedErr != nil {
			writeError(w, gitErrorStatus(unverifiedErr), "could not check migration verification status: "+unverifiedErr.Error())
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

	// STA-717: projects in a PR mode land through GitHub instead.
	cfg, err := shipreview.GetProjectDevConfig(h.db, task.RepoPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load project config: "+err.Error())
		return
	}
	if mode := shipreview.EffectiveMergeMode(cfg, shipreview.IsWorkRepo(task.RepoPath)); mode != shipreview.MergeModeDirect {
		h.approvePR(w, r, card, task, cfg, mode)
		return
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

	// STA-637: the task branch is kept while the review is open and deleted
	// once merged. A failed delete never undoes the merge; the Board retries
	// from the final card via POST .../ship-review/delete-branch.
	deleteErr := h.cleanupMergedBranch(r.Context(), card, task, mainSHA)

	_ = governance.LogEvent(h.db, taskID, "board", governance.AuditBoardAction, nil, nil,
		map[string]any{"action": "approve", "ip": r.RemoteAddr, "user_agent": r.UserAgent(),
			"branch": card.Branch, "branch_deleted": deleteErr == "", "branch_delete_error": deleteErr})
	_ = governance.LogBoardEvent(h.db, "board", governance.AuditBoardAction,
		branchAuditPayload(r, "approve", taskID, card.Branch, mainSHA, deleteErr))

	h.hub.Publish("ship_review_approved", map[string]any{
		"task_id":        taskID,
		"approved_sha":   card.HeadSHA,
		"main_sha":       mainSHA,
		"branch_deleted": deleteErr == "",
	})

	refreshed, _ := shipreview.GetCard(h.db, taskID)
	resp := map[string]any{"card": refreshed, "main_sha": mainSHA, "branch_deleted": deleteErr == ""}
	if deleteErr != "" {
		resp["branch_delete_error"] = deleteErr
		resp["warning"] = "merged; branch delete failed: " + deleteErr
	}
	writeJSON(w, resp)
}

// DeleteMergedBranch handles POST /api/tasks/{id}/ship-review/delete-branch
// (Board action): retries the post-merge branch cleanup when Approve could
// merge but not delete the branch. A no-op once the branch is gone.
func (h *ShipReviewHandler) DeleteMergedBranch(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	card, task, ok := h.requireCard(w, taskID)
	if !ok {
		return
	}
	if card.Status != shipreview.StatusApproved || card.MainSHA == "" {
		writeError(w, http.StatusConflict, "card is not approved and merged")
		return
	}
	if card.BranchDeleted {
		writeJSON(w, map[string]any{"card": card, "main_sha": card.MainSHA, "branch_deleted": true})
		return
	}

	ctx := r.Context()
	if card.PRNumber > 0 {
		// Merged through GitHub: delete with the repo's own gh identity.
		auth, ok := h.requireGHAuth(w, task)
		if !ok {
			return
		}
		ctx = auth.WithAuth(ctx)
	}
	deleteErr := h.cleanupMergedBranch(ctx, card, task, card.MainSHA)
	_ = governance.LogBoardEvent(h.db, "board", governance.AuditBoardAction,
		branchAuditPayload(r, "delete_branch_retry", taskID, card.Branch, card.MainSHA, deleteErr))
	h.hub.Publish("ship_review_branch_cleanup", map[string]any{
		"task_id":        taskID,
		"branch_deleted": deleteErr == "",
	})

	refreshed, _ := shipreview.GetCard(h.db, taskID)
	resp := map[string]any{"card": refreshed, "main_sha": card.MainSHA, "branch_deleted": deleteErr == ""}
	if deleteErr != "" {
		resp["error"] = "branch_delete_failed"
		resp["branch_delete_error"] = deleteErr
		resp["warning"] = "merged; branch delete failed: " + deleteErr
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	writeJSON(w, resp)
}

// cleanupMergedBranch deletes the merged task branch and records the outcome
// on the card. Returns the failure message, or "" when the branch is gone.
func (h *ShipReviewHandler) cleanupMergedBranch(reqCtx gocontext.Context, card *shipreview.Card, task *context.Task, mainSHA string) string {
	// Detach from the request so a closed browser tab can't abort the git
	// commands halfway through.
	ctx := gocontext.WithoutCancel(reqCtx)
	errMsg := ""
	if err := shipreview.CleanupMergedBranch(ctx, task.RepoPath, card, mainSHA); err != nil {
		errMsg = err.Error()
	}
	_ = shipreview.SetBranchCleanup(h.db, card.ID, errMsg == "", errMsg)
	return errMsg
}

func branchAuditPayload(r *http.Request, action, taskID, branch, mainSHA, deleteErr string) map[string]any {
	return map[string]any{
		"action":              action,
		"task_id":             taskID,
		"branch":              branch,
		"main_sha":            mainSHA,
		"branch_deleted":      deleteErr == "",
		"branch_delete_error": deleteErr,
		"ip":                  r.RemoteAddr,
		"user_agent":          r.UserAgent(),
	}
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
		// CIFailures marks a "Send failures to agent": the agent's resubmitted
		// card is pushed to the PR and its checks re-run (STA-717).
		CIFailures bool `json:"ci_failures"`
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
	ciFix := req.CIFailures && card.MergeMode == shipreview.MergeModePRMerge && card.PRNumber > 0
	if ciFix {
		if err := shipreview.SetCIFixRequested(h.db, card.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	shipreview.StopDevServer(h.db, card)
	_ = context.AddTaskComment(h.db, taskID, "board", req.Comment)
	_ = governance.LogEvent(h.db, taskID, "board", governance.AuditBoardAction, nil, nil,
		map[string]any{"action": "send_back", "ci_failures": ciFix, "ip": r.RemoteAddr, "user_agent": r.UserAgent()})

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
		ctx := r.Context()
		if card.PRNumber > 0 {
			auth, ok := h.requireGHAuth(w, task)
			if !ok {
				return
			}
			ctx = auth.WithAuth(ctx)
		}
		if err := shipreview.DeleteBranch(ctx, task.RepoPath, card.Branch); err != nil {
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
	type configView struct {
		*shipreview.ProjectDevConfig
		IsWorkRepo         bool   `json:"is_work_repo"`
		EffectiveMergeMode string `json:"effective_merge_mode"`
	}
	views := make([]configView, 0, len(cfgs))
	for _, c := range cfgs {
		isWork := shipreview.IsWorkRepo(c.RepoPath)
		views = append(views, configView{c, isWork, shipreview.EffectiveMergeMode(c, isWork)})
	}
	writeJSON(w, map[string]any{"configs": views})
}

// devConfigUpdateReq is the partial-update request body for PUT /api/project-dev-configs.
// Pointer fields: nil means absent (keep existing value); non-nil applies the value,
// including "" or [] to clear a field.
type devConfigUpdateReq struct {
	RepoPath        string    `json:"repo_path"`
	DevCommand      *string   `json:"dev_command"`
	DevURL          *string   `json:"dev_url"`
	SetupSteps      *[]string `json:"setup_steps"`
	MigrationGlobs  *[]string `json:"migration_globs"`
	SQLEditorURL    *string   `json:"sql_editor_url"`
	SupabaseEnabled *bool     `json:"supabase_enabled"`
	SupabaseKeepUp  *bool     `json:"supabase_keep_up"`
	MergeMode       *string   `json:"merge_mode"`
	GHConfigDir     *string   `json:"gh_config_dir"`
	LiveCredentials *bool     `json:"live_credentials"`
}

// UpsertProjectDevConfig handles PUT /api/project-dev-configs.
// Board-action gate is enforced by WrapBoardAction in server.go (STA-520).
// The request body is a partial update: only supplied fields overwrite the existing config.
func (h *ShipReviewHandler) UpsertProjectDevConfig(w http.ResponseWriter, r *http.Request) {
	var req devConfigUpdateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.RepoPath == "" {
		writeError(w, http.StatusBadRequest, "repo_path required")
		return
	}
	if req.DevURL != nil && *req.DevURL != "" {
		if err := shipreview.ValidateDevURL(*req.DevURL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.SQLEditorURL != nil && *req.SQLEditorURL != "" {
		if err := shipreview.ValidateSQLEditorURL(*req.SQLEditorURL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if req.MergeMode != nil && !shipreview.ValidMergeMode(*req.MergeMode) {
		writeError(w, http.StatusBadRequest, shipreview.ErrInvalidMergeMode.Error())
		return
	}
	if req.GHConfigDir != nil && *req.GHConfigDir != "" && !filepath.IsAbs(*req.GHConfigDir) {
		writeError(w, http.StatusBadRequest, "gh_config_dir must be an absolute path")
		return
	}

	old, err := shipreview.GetProjectDevConfig(h.db, req.RepoPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load existing config: "+err.Error())
		return
	}

	cfg := *old
	if req.DevCommand != nil {
		cfg.DevCommand = *req.DevCommand
	}
	if req.DevURL != nil {
		cfg.DevURL = *req.DevURL
	}
	if req.SQLEditorURL != nil {
		cfg.SQLEditorURL = *req.SQLEditorURL
	}
	if req.SetupSteps != nil {
		cfg.SetupSteps = *req.SetupSteps
	}
	if req.MigrationGlobs != nil {
		cfg.MigrationGlobs = *req.MigrationGlobs
	}
	if req.SupabaseEnabled != nil {
		cfg.SupabaseEnabled = *req.SupabaseEnabled
	}
	if req.SupabaseKeepUp != nil {
		cfg.SupabaseKeepUp = *req.SupabaseKeepUp
	}
	if req.MergeMode != nil {
		cfg.MergeMode = *req.MergeMode
	}
	if req.GHConfigDir != nil {
		cfg.GHConfigDir = *req.GHConfigDir
	}
	if req.LiveCredentials != nil {
		cfg.LiveCredentials = *req.LiveCredentials
	}

	// Audit failure must roll back the config write; both go in one transaction.
	tx, txErr := h.db.Begin()
	if txErr != nil {
		writeError(w, http.StatusInternalServerError, "tx begin failed: "+txErr.Error())
		return
	}
	defer tx.Rollback() //nolint:errcheck

	if err := shipreview.UpsertProjectDevConfigTx(tx, &cfg); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := governance.LogBoardEventTx(tx, "board", "dev_config_change", map[string]any{
		"repo_path":       cfg.RepoPath,
		"old_dev_command": old.DevCommand,
		"new_dev_command": cfg.DevCommand,
		"old_setup_steps": old.SetupSteps,
		"new_setup_steps": cfg.SetupSteps,

		"old_merge_mode":    old.MergeMode,
		"new_merge_mode":    cfg.MergeMode,
		"old_gh_config_dir": old.GHConfigDir,
		"new_gh_config_dir": cfg.GHConfigDir,
		// STA-727: the live flag is only ever changed here, so this row is
		// its full history.
		"old_live_credentials": old.LiveCredentials,
		"new_live_credentials": cfg.LiveCredentials,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "audit log write failed: "+err.Error())
		return
	}

	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "commit failed: "+err.Error())
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

