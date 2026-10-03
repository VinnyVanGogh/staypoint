package server

import (
	"bytes"
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
	"github.com/VinnyVanGogh/staypoint/internal/testgate"
	"github.com/VinnyVanGogh/staypoint/internal/workspace"
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
			// live_gate/live_gate_reason (STA-799): start-dev needs the Board
			// gate, either because the project is live_credentials or because
			// the repo path could not be verified against the live projects.
			// dev_configured: start-dev has something to run (a saved
			// dev_command, or a Supabase project it will auto-configure).
			if cfg, reason, cfgErr := shipreview.LiveGate(h.db, task.RepoPath); cfgErr == nil {
				isWork := shipreview.IsWorkRepo(task.RepoPath)
				merged["is_work_repo"] = isWork
				merged["effective_merge_mode"] = shipreview.EffectiveMergeMode(cfg, isWork)
				if card.TargetBranch == "" {
					tctx, tcancel := gitRequestContext(r)
					if target, tErr := shipreview.TaskTargetBranch(tctx, h.db, task.RepoPath, task.ID); tErr == nil {
						merged["target_branch"] = target
					}
					tcancel()
				}
				merged["live_credentials"] = cfg.LiveCredentials
				merged["live_gate"] = reason != ""
				merged["live_gate_reason"] = reason
				merged["dev_configured"] = cfg.DevCommand != "" || shipreview.HasSupabaseConfig(task.RepoPath)
			}
			// STA-562: push policy and whether the branch is on origin, so the
			// Board UI can show "not pushed" and offer the Push action.
			merged["push_policy"] = string(shipreview.GetProjectPushPolicy(h.db, task.RepoPath))
			rctx, rcancel := gitRequestContext(r)
			merged["remote"] = shipreview.GetBranchRemoteInfo(rctx, task.RepoPath, card.Branch)
			rcancel()
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
		if writeTaskBaseError(w, err) {
			return
		}
		code := http.StatusInternalServerError
		if errors.Is(err, shipreview.ErrTestStepsRequired) || errors.Is(err, shipreview.ErrInvalidBranch) || errors.Is(err, shipreview.ErrInvalidDevURL) ||
			errors.Is(err, shipreview.ErrProtectedBranch) || errors.Is(err, shipreview.ErrInvalidTargetBranch) {
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

// writeTaskBaseError answers a card or Approve request whose task base could
// not be verified (STA-774) and reports whether err was such an error. The
// caller refuses the request either way when err is non-nil.
func writeTaskBaseError(w http.ResponseWriter, err error) bool {
	var code string
	switch {
	case errors.Is(err, shipreview.ErrNoChanges):
		code = "no_changes"
	case errors.Is(err, workspace.ErrTaskBaseTampered):
		code = "base_tampered"
	case errors.Is(err, workspace.ErrNoTaskBase):
		code = "base_unverified"
	default:
		return false
	}
	writeErrorJSON(w, http.StatusConflict, map[string]any{
		"error":   code,
		"message": err.Error(),
	})
	return true
}

// verifyCardBase runs shipreview.VerifyCardChanges for Approve and writes
// the refusal when it fails. Any failure, including a git or DB error, blocks.
func (h *ShipReviewHandler) verifyCardBase(w http.ResponseWriter, r *http.Request, card *shipreview.Card, task *context.Task) bool {
	ctx, cancel := gitRequestContext(r)
	defer cancel()
	if _, err := shipreview.VerifyCardChanges(ctx, h.db, card, task.RepoPath); err != nil {
		if !writeTaskBaseError(w, err) {
			writeErrorJSON(w, gitErrorStatus(err), map[string]any{
				"error":   "base_unverified",
				"message": "could not verify the task base: " + err.Error(),
			})
		}
		return false
	}
	return true
}

// liveDevWarning is the warning the Board confirms before starting a dev
// server for a live_credentials project (STA-727). The UI shows the same text.
const liveDevWarning = "LIVE PRODUCTION DATA. Actions in this preview are real."

// unverifiedDevWarning is the warning the Board confirms when start-dev is
// gated because the repo path could not be verified against the
// live_credentials projects (STA-799). The UI shows the same text.
const unverifiedDevWarning = "UNVERIFIED REPO PATH. This repo could not be verified as separate from a live_credentials project, so treat this preview as live."

// liveGateWarning returns the warning for a shipreview.LiveGate reason.
func liveGateWarning(reason string) string {
	if reason == shipreview.LiveGateUnverifiedPath {
		return unverifiedDevWarning
	}
	return liveDevWarning
}

// liveGateRefusal returns the 403 message for a gated start-dev that did not
// pass the Board gate.
func liveGateRefusal(reason string) string {
	if reason == shipreview.LiveGateUnverifiedPath {
		return "forbidden: this repo path could not be verified as separate from a live_credentials project, so starting its dev server requires a Board session and passkey"
	}
	return "forbidden: starting a live_credentials dev server requires a Board session and passkey"
}

type liveBoardGateKey struct{}

// StartDevGated routes POST start-dev. Ungated projects go straight to
// StartDev, agent-callable as before. A gated project (shipreview.LiveGate)
// must first pass boardGate (WrapBoardAction: Board session + passkey
// assertion); StartDev then also requires {"confirm_live": true} and audits
// the confirmation. A refusal from boardGate keeps its error code and gains
// live_gate_reason and a message saying why the start is gated.
func (h *ShipReviewHandler) StartDevGated(boardGate func(http.Handler) http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, task, ok := h.requireCard(w, r.PathValue("id"))
		if !ok {
			return
		}
		// STA-767: LiveGate matches the live row by directory, so a task
		// repo_path aliasing a live repo is gated too.
		_, reason, err := shipreview.LiveGate(h.db, task.RepoPath)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "load project config: "+err.Error())
			return
		}
		if reason == "" {
			h.StartDev(w, r)
			return
		}
		var passed *http.Request
		refusal := &boardGateRefusal{header: http.Header{}, status: http.StatusOK}
		boardGate(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			passed = r
		})).ServeHTTP(refusal, r)
		if passed == nil {
			refusal.writeTo(w, reason)
			return
		}
		h.StartDev(w, passed.WithContext(gocontext.WithValue(passed.Context(), liveBoardGateKey{}, true)))
	})
}

// boardGateRefusal records the response boardGate writes when it refuses a
// request, so StartDevGated can add why the start is gated.
type boardGateRefusal struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *boardGateRefusal) Header() http.Header         { return b.header }
func (b *boardGateRefusal) WriteHeader(status int)      { b.status = status }
func (b *boardGateRefusal) Write(p []byte) (int, error) { return b.body.Write(p) }

func (b *boardGateRefusal) writeTo(w http.ResponseWriter, reason string) {
	var payload map[string]any
	if err := json.Unmarshal(b.body.Bytes(), &payload); err != nil {
		for k, v := range b.header {
			w.Header()[k] = v
		}
		w.WriteHeader(b.status)
		_, _ = w.Write(b.body.Bytes())
		return
	}
	msg := liveGateRefusal(reason)
	if gateMsg, _ := payload["message"].(string); gateMsg != "" {
		msg += " (" + strings.TrimPrefix(gateMsg, "forbidden: ") + ")"
	}
	payload["message"] = msg
	payload["live_gate_reason"] = reason
	writeErrorJSON(w, b.status, payload)
}

// StartDev handles POST /api/tasks/{id}/ship-review/start-dev
// Returns 202 immediately; setup runs async and streams progress via SSE.
// Mount it through StartDevGated: on a gated project (shipreview.LiveGate) it refuses any
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

	cfg, reason, err := shipreview.LiveGate(h.db, task.RepoPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load project config: "+err.Error())
		return
	}

	// STA-727: every gate check happens before any side effect (config
	// proposal, worktree, process), so a refused live start leaves no trace.
	if reason != "" {
		if passed, _ := r.Context().Value(liveBoardGateKey{}).(bool); !passed {
			writeErrorJSON(w, http.StatusForbidden, map[string]any{
				"error":            "board_session_required",
				"message":          liveGateRefusal(reason),
				"live_gate_reason": reason,
			})
			return
		}
		var body struct {
			ConfirmLive bool `json:"confirm_live"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		warning := liveGateWarning(reason)
		if !body.ConfirmLive {
			writeErrorJSON(w, http.StatusConflict, map[string]any{
				"error":            "live_confirmation_required",
				"message":          warning + " Confirm to start the dev server.",
				"warning":          warning,
				"live_gate_reason": reason,
			})
			return
		}
		if err := governance.LogBoardEvent(h.db, "board", governance.AuditBoardAction, map[string]any{
			"action":           "live_dev_start_confirmed",
			"task_id":          taskID,
			"repo_path":        task.RepoPath,
			"warning":          warning,
			"live_gate_reason": reason,
			"ip":               r.RemoteAddr,
			"user_agent":       r.UserAgent(),
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "audit log write failed: "+err.Error())
			return
		}
	}

	// Auto-detect Supabase project if no config exists yet.
	if cfg.DevCommand == "" && shipreview.HasSupabaseConfig(task.RepoPath) {
		proposed := shipreview.ProposeSupabaseDevConfig(task.RepoPath)
		// The proposal replaces the whole row; keep the Board's settings, and
		// write the row already matched for this repo rather than an alias
		// row keyed by task.RepoPath (STA-767).
		proposed.RepoPath = cfg.RepoPath
		proposed.LiveCredentials = cfg.LiveCredentials
		proposed.MergeMode = cfg.MergeMode
		proposed.GHConfigDir = cfg.GHConfigDir
		proposed.TargetBranch = cfg.TargetBranch
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
// The diff compares against the latest checkpoint or, when the repo has none,
// the commit where the task left main (see migrationBaseline).
//
// It fails closed: a diff error is returned (wrapping errRepoUnreadable) unless
// nothingToDiff confirms there was nothing to compare, so a repo git cannot
// read is never taken to have no migrations.
func unverifiedMigrations(ctx gocontext.Context, db *sql.DB, task *context.Task) ([]string, error) {
	// Detect migration files in the task diff.
	workDir, hasWorktree := taskCheckpointWorkDir(task)
	branch := "staypoint/" + task.ID
	var fileStats []checkpoint.FileDiffStat
	base, err := migrationBaseline(ctx, task.RepoPath, branch)
	if err != nil {
		return nil, err
	}
	if base != "" {
		stats, diffErr := checkpoint.DiffCheckpointFilesAgainstRef(ctx, task.RepoPath, base, branch)
		if diffErr != nil {
			// base is not among the refs checked: it either exists or could not
			// be looked up, and neither means there is nothing to compare.
			if gitexec.IsTimeout(diffErr) || !nothingToDiff(ctx, task.RepoPath, branch) {
				return nil, fmt.Errorf("%w: %w", errRepoUnreadable, diffErr)
			}
		}
		fileStats = stats
	}
	if hasWorktree {
		// The worktree's HEAD can sit on a different commit than the branch,
		// so it gets its own baseline.
		wtBase, err := migrationBaseline(ctx, workDir, "HEAD")
		if err != nil {
			return nil, err
		}
		if wtBase != "" {
			wtStats, wtErr := checkpoint.DiffCheckpointFiles(ctx, workDir, wtBase)
			if wtErr != nil {
				if gitexec.IsTimeout(wtErr) || !nothingToDiff(ctx, workDir) {
					return nil, fmt.Errorf("%w: %w", errRepoUnreadable, wtErr)
				}
			} else {
				fileStats = append(fileStats, wtStats...)
			}
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

// migrationBaseline returns the commit the migration diff compares head
// against: the latest checkpoint or, when the repo has none, the merge-base of
// main and head, so migrations committed on a task that never checkpointed are
// still found (STA-718). It returns "" when there is nothing to compare
// because main or head does not exist. Any other merge-base failure is
// returned wrapping errRepoUnreadable.
func migrationBaseline(ctx gocontext.Context, dir, head string) (string, error) {
	found, err := refExists(ctx, dir, checkpoint.LatestRef)
	if err != nil || found {
		// When git cannot look the ref up, the diff fails the same way and
		// reports git's error, so leave the verdict to it.
		return checkpoint.LatestRef, nil
	}
	cmd := gitexec.Command(ctx, "merge-base", "main", head)
	cmd.Dir = dir
	cmd.Env = security.ChildEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	if !gitexec.IsTimeout(err) && nothingToDiff(ctx, dir, "main", head) {
		return "", nil
	}
	return "", fmt.Errorf("%w: git merge-base main %s failed: %w (stderr: %s)", errRepoUnreadable, head, err, strings.TrimSpace(stderr.String()))
}

// nothingToDiff reports whether a failed migration diff failed only because
// there was nothing to compare: dir is not a git repo, or one of refs (the
// task branch, main) does not exist yet. It decides from the filesystem and
// git's exit status, never from the text of git's error, so a repo git was
// refused (EPERM) or timed out on is never mistaken for one with nothing in it.
func nothingToDiff(ctx gocontext.Context, dir string, refs ...string) bool {
	if inRepo, err := insideGitRepo(dir); err != nil {
		return false
	} else if !inRepo {
		return true
	}
	for _, ref := range refs {
		found, err := refExists(ctx, dir, ref)
		if err != nil {
			return false
		}
		if !found {
			return true
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

// refExists reports whether ref names a commit in dir. A ref git cannot look
// up, as opposed to one that does not exist, is an error.
func refExists(ctx gocontext.Context, dir, ref string) (bool, error) {
	// --verify --quiet exits 1 when the ref does not exist and 128 when git
	// cannot read the repo.
	cmd := gitexec.Command(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	cmd.Dir = dir
	cmd.Env = security.ChildEnv()
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return err == nil, err
}

// PushBranch handles POST /api/tasks/{id}/ship-review/push-branch (Board action)
// Pushes the reviewed head of the task branch to origin so the Board (or an
// external CI/CD) can open a PR or trigger a preview without merging. Only
// callable from the Board; agents are denied git push by the per-project
// push_policy gate (STA-562). It never pushes main or the merge target.
func (h *ShipReviewHandler) PushBranch(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	card, task, ok := h.requireCard(w, taskID)
	if !ok {
		return
	}
	if card.Status != shipreview.StatusPending && card.Status != shipreview.StatusSentBack {
		writeError(w, http.StatusConflict, "card is not in a pushable state")
		return
	}
	cfg, err := shipreview.GetProjectDevConfig(h.db, task.RepoPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	auth, err := shipreview.ResolveGHAuth(cfg, task.RepoPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := auth.WithAuth(r.Context())
	if err := shipreview.PushBranch(ctx, task.RepoPath, card.Branch, card.TargetBranch, card.HeadSHA); err != nil {
		switch {
		case errors.Is(err, shipreview.ErrHeadMoved):
			writeError(w, http.StatusConflict, "branch HEAD moved since card was created; re-submit the card")
		case errors.Is(err, shipreview.ErrProtectedBranch):
			writeError(w, http.StatusForbidden, err.Error())
		default:
			writeError(w, gitErrorStatus(err), "push failed: "+err.Error())
		}
		return
	}
	remoteInfo := shipreview.GetBranchRemoteInfo(ctx, task.RepoPath, card.Branch)
	h.hub.Publish("ship_review_branch_pushed", map[string]any{
		"task_id":    taskID,
		"branch":     card.Branch,
		"head_sha":   card.HeadSHA,
		"remote_sha": remoteInfo.RemoteSHA,
	})
	writeJSON(w, map[string]any{
		"branch":     card.Branch,
		"head_sha":   card.HeadSHA,
		"remote_sha": remoteInfo.RemoteSHA,
	})
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
		testGateBypass
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

	// STA-774: re-verify the pinned head against the task's recorded base,
	// before any merge mode, test gate or merge runs. Fail closed: a base
	// that is missing or was moved by the agent, a git error, or a head with
	// nothing to merge is never approved.
	if !h.verifyCardBase(w, r, card, task) {
		return
	}

	// STA-717: projects in a PR mode land through GitHub instead.
	cfg, err := shipreview.GetProjectDevConfig(h.db, task.RepoPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load project config: "+err.Error())
		return
	}
	mode := shipreview.EffectiveMergeMode(cfg, shipreview.IsWorkRepo(task.RepoPath))

	// STA-734: direct and open_pr Approve hand the change to main, so the
	// test gate runs here; pr_merge only opens the PR and is gated at Merge.
	var gate *gateOutcome
	if mode != shipreview.MergeModePRMerge {
		if gate, ok = h.enforceTestGate(w, r, card, task, "approve", req.HeadSHA, req.testGateBypass); !ok {
			return
		}
	}
	if mode != shipreview.MergeModeDirect {
		h.approvePR(w, r, card, task, cfg, mode, gate)
		return
	}

	// Merge into the card's target (dev-server for work repos), never into
	// main implicitly. Cards made before targets were recorded use the
	// task's target.
	target := card.TargetBranch
	if target == "" {
		if target, err = shipreview.TaskTargetBranch(r.Context(), h.db, task.RepoPath, task.ID); err != nil {
			writeError(w, gitErrorStatus(err), "resolve target branch: "+err.Error())
			return
		}
		card.TargetBranch = target
	}

	// Always merge from the repo root, never from a task worktree which may
	// be gone or have the target checked out elsewhere (checkout conflicts).
	mainSHA, err := shipreview.ApproveAndMerge(r.Context(), h.db, card, task.RepoPath, target)
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
	h.addGateResult(resp, card, task, gate, 0, "", mainSHA)
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
	// #245-2: only a branch the task owns is deleted; anything else stays,
	// with the reason shown on the card.
	if err := shipreview.CleanupTaskBranch(ctx, h.db, task.RepoPath, card, mainSHA); err != nil {
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
		// ConfirmDeleteUnmerged must name card.Branch exactly to delete a
		// branch whose work is not in the target (a separate Board override).
		ConfirmDeleteUnmerged string `json:"confirm_delete_unmerged"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	// #249 F2: the delete goes through the same owned + guarded checks as
	// post-merge cleanup, and is checked before the review is rejected so a
	// refused delete leaves the card as it was.
	var plan *shipreview.RejectBranchDelete
	ctx := r.Context()
	if req.DeleteBranch {
		if card.PRNumber > 0 {
			auth, ok := h.requireGHAuth(w, task)
			if !ok {
				return
			}
			ctx = auth.WithAuth(ctx)
		}
		var err error
		if plan, err = shipreview.PlanRejectBranchDelete(ctx, h.db, task.RepoPath, card, req.ConfirmDeleteUnmerged); err != nil {
			code := "branch_delete_refused"
			switch {
			case errors.Is(err, shipreview.ErrBranchNotOwned):
				code = "branch_not_owned"
			case errors.Is(err, shipreview.ErrUnmergedBranch):
				code = "unmerged_branch"
			}
			writeJSONStatus(w, http.StatusConflict, map[string]any{"error": code, "message": "delete branch refused: " + err.Error(), "branch": card.Branch})
			return
		}
	}

	if err := shipreview.Reject(h.db, card, req.Comment); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	shipreview.StopDevServer(h.db, card)

	if plan != nil {
		if err := shipreview.DeleteRejectedBranch(ctx, task.RepoPath, plan); err != nil {
			writeError(w, http.StatusConflict, "rejected; delete branch failed: "+err.Error())
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
		// TestExemptGlobs are the project's own test-gate exempt paths;
		// DefaultTestExemptGlobs always apply on top (STA-734).
		TestExemptGlobs        []string `json:"test_exempt_globs"`
		DefaultTestExemptGlobs []string `json:"default_test_exempt_globs"`
	}
	views := make([]configView, 0, len(cfgs))
	for _, c := range cfgs {
		isWork := shipreview.IsWorkRepo(c.RepoPath)
		exempt, _ := shipreview.GetTestExemptGlobs(h.db, c.RepoPath)
		views = append(views, configView{c, isWork, shipreview.EffectiveMergeMode(c, isWork), exempt, testgate.DefaultExemptGlobs})
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
	TargetBranch    *string   `json:"target_branch"`
	GHConfigDir     *string   `json:"gh_config_dir"`
	LiveCredentials *bool     `json:"live_credentials"`
	// TestExemptGlobs are the project's own test-gate exempt paths (STA-734).
	TestExemptGlobs *[]string `json:"test_exempt_globs"`
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
	if req.TargetBranch != nil {
		if err := shipreview.ValidTargetBranch(*req.TargetBranch); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
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
	if req.TargetBranch != nil {
		cfg.TargetBranch = *req.TargetBranch
	}
	if req.GHConfigDir != nil {
		cfg.GHConfigDir = *req.GHConfigDir
	}
	if req.LiveCredentials != nil {
		cfg.LiveCredentials = *req.LiveCredentials
	}

	// Read before the transaction: SQLite may have only the one connection.
	oldExempt, err := shipreview.GetTestExemptGlobs(h.db, req.RepoPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load test exempt globs: "+err.Error())
		return
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
	newExempt := oldExempt
	if req.TestExemptGlobs != nil {
		if err := shipreview.SetTestExemptGlobsTx(tx, req.RepoPath, *req.TestExemptGlobs); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		newExempt = *req.TestExemptGlobs
	}

	if err := governance.LogBoardEventTx(tx, "board", "dev_config_change", map[string]any{
		"repo_path":       cfg.RepoPath,
		"old_dev_command": old.DevCommand,
		"new_dev_command": cfg.DevCommand,
		"old_setup_steps": old.SetupSteps,
		"new_setup_steps": cfg.SetupSteps,

		"old_merge_mode":    old.MergeMode,
		"new_merge_mode":    cfg.MergeMode,
		"old_target_branch": old.TargetBranch,
		"new_target_branch": cfg.TargetBranch,
		"old_gh_config_dir": old.GHConfigDir,
		"new_gh_config_dir": cfg.GHConfigDir,
		// STA-727: the live flag is only ever changed here, so this row is
		// its full history.
		"old_live_credentials":  old.LiveCredentials,
		"new_live_credentials":  cfg.LiveCredentials,
		"old_test_exempt_globs": oldExempt,
		"new_test_exempt_globs": newExempt,
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

