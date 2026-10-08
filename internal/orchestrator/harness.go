package orchestrator

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/checkpoint"
	"github.com/VinnyVanGogh/staypoint/internal/geminiguard"
	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
	"github.com/VinnyVanGogh/staypoint/internal/gitgate"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/logging"
	"github.com/VinnyVanGogh/staypoint/internal/security"
	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
	"github.com/VinnyVanGogh/staypoint/internal/workspace"
	"github.com/google/uuid"
)

// Sentinel errors.
var (
	ErrAlreadyClaimed = errors.New("Can't start run: this task is already checked out by another run in this session. Wait for it to finish or clear the stale checkout.")
	ErrTaskNotFound   = errors.New("task not found")
	// ErrNotRunnable: the task is parked (backlog) or closed (done,
	// cancelled, rejected). Run Now moves a backlog task to todo first.
	ErrNotRunnable    = errors.New("Can't start run: this task is not runnable in its current stage (backlog, stopped, done or cancelled). Move it to todo or press Run Now first.")
	ErrConcurrencyCap = errors.New("Can't start run: the maximum number of parallel runs (max_concurrent_runs) is already active.")
	// ErrOrgHeld: the task's organization is on a Board hold; nothing in it
	// is claimed until the Board lifts the hold.
	ErrOrgHeld = errors.New("Can't start run: this task's organization is on hold. The Board lifts the hold in Settings (Touch ID).")
)

// taskCompleteMarker is the canonical signal an adapter emits on completion.
const taskCompleteMarker = "[[TASK_COMPLETE]]"

// markerOnOwnLine returns true if text contains the task-complete marker standing
// alone on a line (after trimming), outside any fenced code block. This prevents
// an agent mentioning the marker in prose (e.g. "I omitted `[[TASK_COMPLETE]]`")
// from being mistaken for a genuine completion signal.
func markerOnOwnLine(text string) bool {
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if trimmed == taskCompleteMarker {
			return true
		}
	}
	return false
}

// defaultProviderEnvKeys are provider credential variables that must pass through
// the sanitized env.
var defaultProviderEnvKeys = []string{
	"ANTHROPIC_API_KEY",
	"OPENAI_API_KEY",
	"GEMINI_API_KEY",
	"GOOGLE_API_KEY",
	"AGY_API_KEY",
}

// AdapterRunFunc is the injectable adapter turn runner.
// The harness calls it once per turn. Signature matches adapter.RunAdapter
// with extraEnv pre-built by the harness so the adapter package isn't imported here
// (it would create an import cycle via context → orchestrator → adapter → router → context).
type AdapterRunFunc func(ctx context.Context, cwd, provider string, rawArgs, extraEnv []string, stdout, stderr io.Writer) error

// RunConfig holds per-task execution parameters.
type RunConfig struct {
	// MaxTurns is the per-run turn budget; 0 defaults to 50.
	MaxTurns int
	// MaxBudgetUSD caps cumulative spend; 0 = unlimited.
	MaxBudgetUSD float64
	// MaxWallclock caps the whole run's wall-clock duration; 0 = no limit.
	// The daemon never sets it (Board: no fixed time cap); `staypoint run
	// --max-wallclock` can.
	MaxWallclock time.Duration
	// TurnTimeout caps one adapter turn's wall-clock time; 0 = no limit
	// (config.toml turn_timeout).
	TurnTimeout time.Duration
	// StallTimeout stops a turn after this long with no agent output or tool
	// activity (config.toml stall_timeout). 0 = DefaultStallTimeout (20m);
	// negative turns the check off.
	StallTimeout time.Duration
	// Clock, when set, replaces time.Now for the turn watch (tests).
	Clock func() time.Time
	// watchPoll is how often the turn watch checks; 0 = defaultWatchPoll.
	watchPoll time.Duration
	// SkipPermissions forwards --dangerously-skip-permissions to the adapter.
	// Opt-in only; never set by default.
	SkipPermissions bool
	// SkipGitPreflight disables the git fetch/dirty/fast-forward pre-flight check.
	// For tests that run in a non-git directory; never set in production.
	SkipGitPreflight bool
	// AgentID tags the checkout for audit.
	AgentID string
	// Provider selects the adapter chain entry point. Empty = auto-resolve.
	Provider string
	// RunAdapter is the injected turn runner. Nil = skip adapter (dry-run).
	RunAdapter AdapterRunFunc
	// StepRecorder, if set, receives parsed stream deltas for live timeline emission.
	// Nil disables step recording (tests, dry-runs).
	StepRecorder *StepRecorder
	// ParseDelta converts one raw stream line into StepDeltas. Required when
	// StepRecorder is set. Injected to avoid import cycle (adapter → router → context → orchestrator).
	ParseDelta func(line []byte) ([]StepDelta, error)
	// WakeReason is forwarded to StepRecorder.EmitWake when recording is enabled.
	WakeReason string
	// EmitRoute, if set, is called once after EmitWake and before the adapter starts.
	// Use it to write a provider-routing step without importing router from this package.
	// Only called when StepRecorder is non-nil.
	EmitRoute func(sr *StepRecorder)
	// RunControl, when set, enables pause/stop/message-inject controls for this run.
	RunControl *RunControl
	// HookBin, when set, is forwarded to the adapter as STAYPOINT_HOOK_BIN so the
	// Claude adapter can register staypoint hook pre-tool as a PreToolUse hook in a
	// per-run --settings file (STA-525).  Should point to the staypoint CLI binary
	// built from the same commit as the daemon.
	HookBin string
	// GeminiDocsOnly applies the Board rule router.GeminiCodeForbidden to this
	// run (every repo, STA-856 revised 2026-10-06): after any turn that
	// spawned Gemini, changes outside the non-code allowlist are reverted to
	// the daemon's pre-turn checkpoint and the run fails.
	GeminiDocsOnly bool
	// GeminiCodeApproved relaxes the guard for this one run after a Board
	// Touch ID approval (personal repo only, router.GeminiCodeApprovalAllowed):
	// code changes are allowed, .git tampering and paths outside the repo are
	// still reverted. Requires GeminiDocsOnly (the guard still runs).
	GeminiCodeApproved bool
	// TurnUsedGemini reports whether a Gemini CLI was spawned since its last
	// call, and resets. Nil falls back to the static Provider.
	TurnUsedGemini func() bool
}

// turnUsedGemini reports whether the turn that just ran spawned Gemini.
func (cfg RunConfig) turnUsedGemini() bool {
	if cfg.TurnUsedGemini != nil {
		return cfg.TurnUsedGemini()
	}
	return geminiguard.IsGeminiProvider(cfg.Provider)
}

// RunResult summarises a completed autonomous run.
type RunResult struct {
	TaskID        string
	RunID         string
	Turns         int
	SpentUSD      float64
	Disposition   string // "in_review" | "done" | "capped" | "in_progress"
	DiffStat      string
	DiagnosticMsg string // non-empty when interceptor blocked the transition
}

// WorktreeManagerIface abstracts worktree operations for testability.
type WorktreeManagerIface interface {
	CreateContext(ctx context.Context, taskID, sessionID string) (string, error)
	PruneContext(ctx context.Context, taskID string) error
	// PruneWorktreeDirContext removes the worktree directory but keeps the branch
	// so committed work remains reachable after the run ends.
	PruneWorktreeDirContext(ctx context.Context, taskID string) error
}

// Harness orchestrates an autonomous single-task agent run.
type Harness struct {
	DB          *sql.DB
	RepoRoot    string
	WM          WorktreeManagerIface
	Interceptor *Interceptor
	// Slots enforces the parallel-run caps. Nil uses GlobalRunSlots.
	Slots *RunSlots
}

func (h *Harness) slots() *RunSlots {
	if h.Slots != nil {
		return h.Slots
	}
	return GlobalRunSlots
}

// SlotKeyForTask returns the caps a task's run counts against (STA-867): its
// plain non-git folder (a repo_path that is not in a repo, or the per-task
// scratch dir when repo_path is empty), else its git repo, keyed by the
// repo's main checkout so every path inside one repo shares its cap; and its
// organization.
func (h *Harness) SlotKeyForTask(ctx context.Context, taskID string) SlotKey {
	var repoPath, org string
	_ = h.DB.QueryRowContext(ctx, "SELECT COALESCE(repo_path,''), COALESCE(organization,'') FROM tasks WHERE id=?", taskID).Scan(&repoPath, &org)
	key := SlotKey{Org: OrgBucket(org)}
	if td, err := workspace.DescribeTaskDir(repoPath, taskID); err == nil && !td.Git {
		key.Dir, key.Plain = RepoKey(td.Dir), true
		return key
	}
	if repoPath == "" {
		repoPath = h.RepoRoot
	}
	key.Dir = RepoKey(repoPath)
	if common := gitexec.CommonDir(key.Dir); filepath.Base(common) == ".git" {
		key.Dir = RepoKey(filepath.Dir(common))
	}
	return key
}

// newWorktreeManager makes a WorktreeManager that cuts new task branches
// from the project's target branch (dev-server for work repos), the branch
// Approve merges into.
func newWorktreeManager(repoRoot string, db *sql.DB) *workspace.WorktreeManager {
	wm := workspace.NewWorktreeManager(repoRoot, db)
	wm.TargetBranch = func(ctx context.Context, repo string) (string, error) {
		return shipreview.ProjectTargetBranch(ctx, db, repo)
	}
	return wm
}

// NewHarness creates a Harness backed by the given SQLite DB and repo root.
func NewHarness(db *sql.DB, repoRoot string) *Harness {
	return &Harness{
		DB:          db,
		RepoRoot:    repoRoot,
		WM:          newWorktreeManager(repoRoot, db),
		Interceptor: NewInterceptor(db),
	}
}

// closedStageGuard keeps a run's stage writes off a task the Board closed
// while it ran (STA-861: Mark done / Cancel stop the run, then close the task;
// the stopping run must not write stopped or in_progress over done).
const closedStageGuard = " AND execution_stage NOT IN ('done', 'cancelled')"

// Claim atomically checks out a task for the given runID.
//
// Parallelism is bounded by RunSlots (STA-773, STA-867): a global cap
// (max_concurrent_runs, default 9), a per-repo cap (max_runs_per_repo,
// default 3; one run per plain non-git folder) and a per-organization cap
// (max_runs_per_org, default 3). A refusal returns an
// error matching errors.Is(err, ErrConcurrencyCap); callers queue the run and
// RunSlots re-dispatches it when a slot frees. Across restarts, RecoveryScan clears stale checkout_run_id values so
// the DB guard (checkout_run_id IS NULL) unblocks on the next wake.
// Parked (backlog) and closed (done, cancelled, rejected) tasks are never
// claimed; see governance.IsRunnableStage. Neither is a task whose
// organization is on a Board hold (ErrOrgHeld). A child whose parent already
// has tasks.max_running_children children running is refused with
// ErrParentBusy, a capacity refusal: it queues and starts when a sibling ends.
func (h *Harness) Claim(ctx context.Context, taskID, runID, agentID string) error {
	slots := h.slots()
	if err := slots.Acquire(taskID, h.SlotKeyForTask(ctx, taskID)); err != nil {
		return err
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	childLimit := governance.RunningChildLimit(h.DB)
	res, err := h.DB.ExecContext(ctx,
		`UPDATE tasks
		    SET execution_stage='in_progress', checkout_run_id=?, checkout_agent_id=?, updated_at=?
		  WHERE id=? AND checkout_run_id IS NULL AND execution_stage NOT IN (`+governance.NonRunnableStagesSQL()+`)
		    AND `+governance.OrgNotHeldSQL("tasks")+`
		    AND `+governance.SiblingsUnderRunLimitSQL("tasks", childLimit),
		runID, agentID, now, taskID,
	)
	if err != nil {
		slots.Release(taskID)
		return fmt.Errorf("claim db update: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		slots.Release(taskID)
		var stage string
		_ = h.DB.QueryRowContext(ctx, "SELECT execution_stage FROM tasks WHERE id=?", taskID).Scan(&stage)
		if stage == "" {
			return ErrTaskNotFound
		}
		if !governance.IsRunnableStage(stage) {
			return ErrNotRunnable
		}
		if held, _ := governance.TaskOrgHeld(h.DB, taskID); held {
			return ErrOrgHeld
		}
		if governance.ParentAtRunLimit(h.DB, taskID, childLimit) {
			return ErrParentBusy
		}
		return ErrAlreadyClaimed
	}

	slog.Info("task claimed", slog.String("task", taskID), slog.String("run", runID))
	return nil
}

// Release clears the task checkout, then frees the run slot (which
// re-dispatches queued runs). Always called via defer; uses a fresh context to
// survive parent cancellation.
func (h *Harness) Release(taskID, runID string) {
	defer h.slots().Release(taskID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = h.DB.ExecContext(ctx,
		`UPDATE tasks SET checkout_run_id=NULL, checkout_agent_id=NULL, updated_at=?
		  WHERE id=? AND checkout_run_id=?`,
		time.Now().UTC().Format(time.RFC3339Nano), taskID, runID,
	)
}

// Run claims and executes a task to completion.
//
// Lifecycle:
//  1. Claim task atomically (cap enforced).
//  2. Create isolated git worktree.
//  3. Pre-run checkpoint.
//  4. Drive adapter turns; checkpoint before each; scan stdout for [[TASK_COMPLETE]].
//  5. Run Mechanical Completion Interceptor on completion signal.
//  6. Set disposition, record work product and cost.
//  7. Prune worktree on exit (deferred).
func (h *Harness) Run(ctx context.Context, taskID string, cfg RunConfig) (*RunResult, error) {
	runID := buildRunID(cfg.AgentID)
	if err := h.Claim(ctx, taskID, runID, cfg.AgentID); err != nil {
		return nil, err
	}
	defer h.Release(taskID, runID)

	runLog := logging.WithRunContext(
		logging.WithComponent(slog.Default(), "harness"),
		taskID, runID, cfg.AgentID,
	)

	maxTurns := cfg.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 50
	}
	// No fixed run-time cap by default: a stuck turn is caught by the turn
	// watch's stall timeout instead (see turn_watch.go).
	cancel := context.CancelFunc(func() {})
	if cfg.MaxWallclock > 0 {
		ctx, cancel = context.WithTimeout(ctx, cfg.MaxWallclock)
	}
	defer cancel()

	var repoPath string
	if err := h.DB.QueryRowContext(ctx, "SELECT COALESCE(repo_path,'') FROM tasks WHERE id=?", taskID).Scan(&repoPath); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("fetch repo path: %w", err)
	}
	// Non-git tasks (STA-864): an existing directory that is not a git repo,
	// or no repo_path at all (per-task scratch dir), runs in place with a
	// warning: no worktree, checkpoints or ship review. See plaindir.go.
	taskDir, err := workspace.ResolveTaskDir(repoPath, taskID)
	if err != nil {
		return nil, fmt.Errorf("task directory: %w", err)
	}
	nonGit := !taskDir.Git
	if nonGit {
		repoPath = taskDir.Dir
		ctx = withPlainDir(ctx)
	} else if repoPath == "" {
		repoPath = h.RepoRoot
	}

	var wtPath string
	var preCP *checkpoint.Checkpoint
	if nonGit {
		wtPath = taskDir.Dir
	} else {
		// When the task names a specific repo that differs from the harness default,
		// build a fresh WorktreeManager for that repo so worktrees land in the right
		// place and never touch h.RepoRoot.
		wm := h.WM
		if repoPath != h.RepoRoot {
			wm = newWorktreeManager(repoPath, h.DB)
		}

		wtPath, err = wm.CreateContext(ctx, taskID, runID)
		if err != nil {
			return nil, fmt.Errorf("create worktree: %w", err)
		}
		defer func() {
			if pruneErr := wm.PruneWorktreeDirContext(context.Background(), taskID); pruneErr != nil {
				runLog.Warn("worktree prune failed", slog.Any("error", pruneErr))
			}
		}()

		preCP, _ = checkpoint.CreateCheckpoint(ctx, checkpoint.CreateOptions{
			WorkDir:   wtPath,
			SessionID: runID,
			Message:   "pre-run " + taskID,
		})
	}

	providerEnv := security.ChildEnv(defaultProviderEnvKeys...)
	if cfg.SkipPermissions {
		providerEnv = append(providerEnv, "STAYPOINT_SKIP_PERMISSIONS=1")
	}
	// Expose task ID so the PreToolUse hook can check the pause flag before
	// each tool call, enabling step-boundary pause rather than turn-boundary.
	providerEnv = append(providerEnv, "STAYPOINT_TASK_ID="+taskID)
	// Expose the staypoint CLI path so the Claude adapter can register it as a
	// PreToolUse hook in a per-run --settings file (STA-525).
	if cfg.HookBin != "" {
		providerEnv = append(providerEnv, "STAYPOINT_HOOK_BIN="+cfg.HookBin)
	}

	// Emit wake + route steps now that the claim succeeded.
	// These are intentionally emitted after Claim so that refused runs
	// (ErrConcurrencyCap before this point) never write any timeline steps.
	sr := cfg.StepRecorder
	if sr != nil {
		sr.SetWorktreeRoot(wtPath)
		wakeReason := cfg.WakeReason
		if wakeReason == "" {
			wakeReason = "run started"
		}
		sr.EmitWake(wakeReason)
		if cfg.EmitRoute != nil {
			cfg.EmitRoute(sr)
		}
	}

	result := &RunResult{TaskID: taskID, RunID: runID}

	// Pre-flight cumulative budget check: if the task has already exhausted its
	// max_turns or max_budget_usd across prior runs, skip the adapter entirely.
	// Run this before the git preflight so an already-exhausted task is capped
	// immediately without requiring a valid git repo.
	{
		var spentTurns, dbMaxTurns int
		var spentUSD, dbMaxBudget float64
		if qErr := h.DB.QueryRowContext(ctx,
			`SELECT spent_turns, max_turns, spent_usd, max_budget_usd FROM tasks WHERE id=?`, taskID,
		).Scan(&spentTurns, &dbMaxTurns, &spentUSD, &dbMaxBudget); qErr == nil {
			var capMsg string
			switch {
			case dbMaxTurns > 0 && spentTurns >= dbMaxTurns:
				capMsg = fmt.Sprintf(
					"Turn budget exhausted (%d turns spent / %d turn limit). Task capped; no further adapter runs.",
					spentTurns, dbMaxTurns,
				)
			case dbMaxBudget > 0 && spentUSD >= dbMaxBudget:
				capMsg = fmt.Sprintf(
					"USD budget exhausted ($%.4f spent / $%.4f limit). Task capped; no further adapter runs.",
					spentUSD, dbMaxBudget,
				)
			}
			if capMsg != "" {
				result.Disposition = "capped"
				if sr != nil {
					sr.EmitState("capped")
					sr.Close()
				}
				capCtx, capCancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer capCancel()
				capNow := time.Now().UTC().Format(time.RFC3339Nano)
				_, _ = h.DB.ExecContext(capCtx,
					`UPDATE tasks SET execution_stage='capped', updated_at=? WHERE id=?`+closedStageGuard,
					capNow, taskID,
				)
				_, _ = h.DB.ExecContext(capCtx,
					`INSERT INTO task_comments (task_id, author, message) VALUES (?, 'harness', ?)`,
					taskID, capMsg,
				)
				_, _ = h.DB.ExecContext(capCtx,
					`INSERT INTO activity_log (task_id, event_type, details) VALUES (?, 'run_complete', ?)`,
					taskID, "disposition=capped turns=0 reason=budget_exhausted",
				)
				return result, nil
			}
		}
	}

	if nonGit {
		h.announceNonGit(ctx, taskID, taskDir, sr)
	}

	// Git pre-flight: fetch, dirty check, fast-forward.
	// A failure is logged as a timeline comment and blocks the run.
	// Skipped when cfg.SkipGitPreflight is true (tests running in a non-git dir).
	if !cfg.SkipGitPreflight && !nonGit {
		gfCtx, gfCancel := context.WithTimeout(ctx, 60*time.Second)
		gfResult, gfErr := gitgate.PreFlight(gfCtx, wtPath, "main")
		gfCancel()
		gfSummary := "git-preflight: "
		if gfErr != nil {
			gfSummary += "error: " + gfErr.Error()
		} else if !gfResult.OK {
			gfSummary += "FAILED — " + strings.Join(gfResult.Errors, "; ")
		} else {
			gfSummary += "ok (" + strings.Join(gfResult.Details, " | ") + ")"
		}
		runLog.Info("git preflight", slog.String("result", gfSummary))
		_, _ = h.DB.ExecContext(ctx,
			`INSERT INTO task_comments (task_id, author, message) VALUES (?, 'harness', ?)`,
			taskID, gfSummary,
		)
		if gfErr != nil || (gfResult != nil && !gfResult.OK) {
			result.Disposition = "in_progress"
			result.DiagnosticMsg = gfSummary
			if sr != nil {
				sr.EmitState("in_progress")
				sr.Close()
			}
			cleanCtx2, cleanCancel2 := context.WithTimeout(context.Background(), 10*time.Second)
			defer cleanCancel2()
			now2 := time.Now().UTC().Format(time.RFC3339Nano)
			_, _ = h.DB.ExecContext(cleanCtx2,
				`UPDATE tasks SET execution_stage='in_progress', updated_at=? WHERE id=?`+closedStageGuard,
				now2, taskID,
			)
			_, _ = h.DB.ExecContext(cleanCtx2,
				`INSERT INTO activity_log (task_id, event_type, details) VALUES (?, 'run_complete', ?)`,
				taskID, "disposition=in_progress turns=0 reason=git_preflight_failed",
			)
			return result, nil
		}
		// STA-774: the recorded task base is never moved from git state
		// here. The preflight's fetch and fast-forward follow the worktree's
		// remote config and refs, which the agent can rewrite (e.g. a fetch
		// refspec that maps its own branch onto origin/main); commits a
		// fast-forward brings in are listed as task changes instead.
	} // end git preflight block

	// Clear stale run control flags from previous runs.
	if cfg.RunControl != nil {
		cfg.RunControl.ClearForRun(taskID)
	}

	// Fetch the task brief once; pass it to each turn.
	brief := fetchTaskBrief(ctx, h.DB, taskID)
	if nonGit {
		brief.RepoPath = wtPath
		brief.PlainDir = true
		brief.ShipReviewGate = false // nothing to merge, no card
	}

	// Track the highest comment id seen so far so each turn only injects new comments.
	var lastSeenCommentID int64

	// consecutiveAdapterErrors counts back-to-back adapter failures with no
	// successful output between them. lastTurnWasAdapterError suppresses the
	// checkpoint that would otherwise be written at the start of the next turn
	// when the adapter failed and produced no filesystem changes.
	var consecutiveAdapterErrors int
	var lastTurnWasAdapterError bool

	// lastTurnOutput preserves the raw stream-json bytes from the final adapter
	// turn so extractFinalResponse can find the agent's last assistant message.
	var lastTurnOutput []byte

	// workProductRegistered tracks whether the branch work product was inserted
	// during an inline interceptor call so the post-loop path doesn't duplicate it.
	var workProductRegistered bool

	// completionRejected is set when the agent emitted [[TASK_COMPLETE]] and the
	// interceptor rejected it at least once. Used post-loop to distinguish
	// "marker-sent-then-turns-exhausted" (→ capped) from "no marker at all"
	// (→ post-loop interceptor run, old behaviour).
	var completionRejected bool
	// completionRejections counts rejections in this run; at
	// maxCompletionRejections the run ends and the task is parked.
	var completionRejections int

	// Silent-run detection (STA-775): whether any adapter turn ran, whether
	// the agent produced any output, and the last turn's exit code and stderr
	// for the "Run ended with no output" reason.
	var adapterRan, sawOutput bool
	var lastExitCode int
	var lastStderr string

	// turnCP is the daemon-created checkpoint the next turn starts from. The
	// Gemini guard (STA-856) restores from its in-memory commit SHA, never from
	// refs the agent could move.
	turnCP := preCP

	for turn := 0; turn < maxTurns; turn++ {
		if ctx.Err() != nil {
			result.Disposition = "capped"
			if sr != nil && cfg.MaxWallclock > 0 {
				sr.EmitMessage("Stopped: run hit max wall-clock "+fmtWatchDuration(cfg.MaxWallclock), "", "error")
			}
			break
		}

		if turn > 0 && !lastTurnWasAdapterError && !nonGit {
			cp, _ := checkpoint.CreateCheckpoint(ctx, checkpoint.CreateOptions{
				WorkDir:   wtPath,
				SessionID: runID,
				Message:   fmt.Sprintf("turn %d %s", turn, taskID),
			})
			if sr != nil && cp != nil {
				sr.EmitCheckpoint(cp.ID, fmt.Sprintf("turn %d", turn))
			}
			if cp != nil {
				turnCP = cp
			}
		}
		lastTurnWasAdapterError = false

		// Gemini guard (STA-856): record the worktree before the turn so a
		// Gemini turn's changes can be checked and reverted afterwards.
		var guardSnap *geminiguard.Snapshot
		var guardSnapErr error
		if nonGit && cfg.RunAdapter != nil {
			// No checkpoint to restore from: snapshot only, never run git.
			guardSnap, guardSnapErr = geminiguard.TakeNoGit(wtPath)
			if cfg.TurnUsedGemini != nil {
				_ = cfg.TurnUsedGemini()
			}
		} else if cfg.GeminiDocsOnly && cfg.RunAdapter != nil {
			cpSHA := ""
			if turnCP != nil {
				cpSHA = turnCP.CommitSHA
			}
			guardSnap, guardSnapErr = geminiguard.Take(ctx, wtPath, cpSHA)
			if guardSnapErr == nil && cfg.GeminiCodeApproved {
				guardSnap.AllowCode()
			}
			if cfg.TurnUsedGemini != nil {
				_ = cfg.TurnUsedGemini() // clear spawns from before this turn
			}
		}

		// Drive one adapter turn. Tee stdout through StepRecorder line scanner if enabled.
		var outBuf bytes.Buffer
		tw := &completionWriter{dst: &outBuf}
		newComments := fetchUserComments(ctx, h.DB, taskID, lastSeenCommentID)
		if len(newComments) > 0 {
			lastSeenCommentID = newComments[len(newComments)-1].ID
		}
		rawArgs := buildRawArgs(taskID, turn, cfg, brief, newComments)

		var stdout io.Writer = tw
		if sr != nil && cfg.ParseDelta != nil {
			stdout = &stepTeeWriter{dst: tw, rec: sr, parse: cfg.ParseDelta}
		}

		if cfg.RunAdapter != nil {
			var stderrBuf limitedWriter
			turnStart := time.Now()

			// Watch for stop signal during this turn: if stop is requested,
			// cancel the adapter context so the subprocess receives SIGTERM.
			turnCtx, turnCancel := context.WithCancel(ctx)
			if cfg.RunControl != nil {
				stopCh := cfg.RunControl.StopChan(taskID)
				go func() {
					select {
					case <-turnCtx.Done():
					case <-stopCh:
						turnCancel()
					}
				}()
			}

			// Turn watch: stop the turn if the agent goes quiet for the stall
			// timeout (or passes turn_timeout, if set). Not a fixed cap.
			watch := startTurnWatch(cfg, boardWaiting(h.DB, cfg.RunControl, taskID), turnCancel)
			beginTurn(cfg, taskID, turn, sr)
			var turnOut, turnErrOut io.Writer = stdout, &stderrBuf
			if watch != nil {
				turnOut = &activityWriter{dst: stdout, w: watch}
				turnErrOut = &activityWriter{dst: &stderrBuf, w: watch}
			}
			// A panic inside the turn (stream parser, writers) must not unwind
			// past the Gemini guard below: it is turned into a turn error, the
			// guard runs, and the run then stops (see turnPanic).
			turnErr, turnPanic := runAdapterTurn(func() error {
				return cfg.RunAdapter(turnCtx, wtPath, cfg.Provider, rawArgs, providerEnv, turnOut, turnErrOut)
			})
			watchStop := watch.Stop()
			endTurn(taskID, sr)
			turnCancel()
			adapterRan = true
			lastExitCode = exitCodeFrom(turnErr)
			lastStderr = stderrBuf.String()

			// Non-git dir (STA-864): any Gemini turn that changed a non-doc
			// file fails the run; it cannot be reverted. Fails closed.
			if nonGit && cfg.turnUsedGemini() {
				nr := geminiguard.NoGitResult{Dir: wtPath, Err: guardSnapErr}
				if guardSnapErr == nil {
					nr = geminiguard.CheckNoGit(ctx, guardSnap)
				}
				if nr.Violated() {
					h.blockGeminiNoGit(result, nr, taskID, turn, stdout, sr, runLog)
					sawOutput = true
					break
				}
			}
			// Board rule (STA-856): Gemini never writes code in a work repo.
			// Checked before error handling so a failed Gemini turn is checked too.
			if cfg.GeminiDocsOnly && !nonGit && cfg.turnUsedGemini() {
				gr := geminiguard.Result{Err: guardSnapErr}
				if guardSnapErr == nil {
					gCtx, gCancel := context.WithTimeout(context.Background(), 2*time.Minute)
					gr = geminiguard.Enforce(gCtx, guardSnap)
					gCancel()
				}
				if gr.Violated() {
					if stw, ok := stdout.(*stepTeeWriter); ok {
						_ = stw.Close()
					}
					result.Turns++
					sawOutput = true // the block row explains the run; not a silent run
					result.Disposition = "error"
					result.DiagnosticMsg = gr.Title()
					runLog.Warn("gemini code guard blocked turn",
						slog.Int("turn", turn),
						slog.Any("blocked", gr.Blocked),
						slog.Any("unresolved", gr.Unresolved),
						slog.Any("error", gr.Err),
					)
					if sr != nil {
						sr.EmitMessage(gr.Title(), gr.Body(), "error")
					}
					_, _ = h.DB.ExecContext(context.Background(),
						`INSERT INTO activity_log (task_id, event_type, details) VALUES (?, 'gemini_code_blocked', ?)`,
						taskID, gr.Title(),
					)
					break
				}
			}

			if turnPanic != nil {
				h.stopTurnForPanic(result, turnPanic, taskID, turn, stdout, sr, runLog)
				sawOutput = true // the stop row explains the run
				break
			}

			if watchStop != "" {
				if h.stopTurnForWatch(result, watch, watchStop, taskID, stdout, sr) {
					sawOutput = true // the stop row explains the run
					break
				}
			}

			turnDuration := time.Since(turnStart)
			if turnErr != nil {
				lastTurnWasAdapterError = true
				consecutiveAdapterErrors++
				stderrTail := stderrBuf.String()
				exitCode := exitCodeFrom(turnErr)
				runLog.Warn("adapter turn error",
					slog.Int("turn", turn),
					slog.Int("exit_code", exitCode),
					slog.Int64("duration_ms", turnDuration.Milliseconds()),
					slog.String("stderr_tail", truncate(stderrTail, 500)),
					slog.Any("error", turnErr),
				)
				_, _ = h.DB.ExecContext(ctx,
					`INSERT INTO run_errors (id, run_id, task_id, turn, exit_code, stderr_tail, duration_ms, model, adapter)
					 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					uuid.NewString(), runID, taskID, turn, exitCode,
					truncate(stderrTail, 4096),
					turnDuration.Milliseconds(),
					cfg.Provider, cfg.Provider,
				)
				_, _ = h.DB.ExecContext(ctx,
					`INSERT INTO activity_log (task_id, event_type, details) VALUES (?, 'adapter_failure', ?)`,
					taskID, plainAdapterError(turnErr, exitCode, stderrTail),
				)
				// Stop the run after 2 consecutive adapter failures to avoid burning
				// turns against a persistently failing provider (e.g. quota-exceeded).
				if consecutiveAdapterErrors >= 2 {
					result.Turns++ // count this turn; the normal counter below won't run
					result.Disposition = "error"
					result.DiagnosticMsg = fmt.Sprintf(
						"Run stopped: adapter failed %d turns in a row. Last error: %s",
						consecutiveAdapterErrors,
						plainAdapterError(turnErr, exitCode, stderrTail),
					)
					break
				}
			} else {
				consecutiveAdapterErrors = 0
			}
		}
		if stw, ok := stdout.(*stepTeeWriter); ok {
			_ = stw.Close()
		}

		// One harness turn = one adapter invocation. spent_turns counts these,
		// not provider-internal tool-use rounds (STA-466).
		result.Turns++

		// Preserve this turn's raw output for the run-summary comment posted at end of run.
		if n := outBuf.Len(); n > 0 {
			lastTurnOutput = make([]byte, n)
			copy(lastTurnOutput, outBuf.Bytes())
		}
		if extractFinalResponse(outBuf.Bytes()) != "" {
			sawOutput = true
		} else if _, parsed := stdout.(*stepTeeWriter); !parsed && len(bytes.TrimSpace(outBuf.Bytes())) > 0 {
			// No stream parser wired: any printed output counts.
			sawOutput = true
		}

		// Prefer text-only detection when the stream parser is active; fall back
		// to raw-byte scan only when no parser is wired (dry-run/test mode).
		var markerSeen bool
		if stw, ok := stdout.(*stepTeeWriter); ok {
			markerSeen = stw.textDetected
		} else {
			markerSeen = tw.detected || strings.Contains(outBuf.String(), taskCompleteMarker)
		}
		if markerSeen {
			// STA-391 ordering: register the branch work product BEFORE the interceptor
			// so checkWorkProducts finds it. Eagerly compute the diff; calling
			// DiffCheckpoint here is idempotent with the post-loop call.
			if !workProductRegistered && nonGit {
				workProductRegistered = h.registerPlainDirProduct(ctx, taskID, wtPath)
			}
			if !workProductRegistered && preCP != nil {
				if ds, dsErr := checkpoint.DiffCheckpoint(ctx, wtPath, preCP.ID); dsErr == nil && ds != "" {
					result.DiffStat = ds
					if _, wpErr := h.DB.ExecContext(ctx,
						`INSERT INTO task_work_products (task_id, product_type, reference) VALUES (?, 'branch', ?)`,
						taskID, "staypoint/"+taskID,
					); wpErr == nil {
						workProductRegistered = true
					} else {
						runLog.Warn("register work product (inline) failed", slog.Any("error", wpErr))
					}
				}
			}

			// Run the Mechanical Completion Interceptor immediately so the agent
			// receives self-correcting feedback within the same run rather than
			// discovering the rejection only after the heartbeat ends.
			icCtx, icCancel := context.WithTimeout(ctx, 30*time.Second)
			approved, diag, _ := h.Interceptor.InterceptCompletion(icCtx, taskID, wtPath, repoPath)
			icCancel()
			if approved {
				result.Disposition = "in_review"
				break
			}
			// Rejected: persist feedback with author 'interceptor' so fetchUserComments
			// picks it up on the next turn without re-injecting on subsequent runs
			// (the lastSeenCommentID cursor advances past it).
			rejMsg := "Completion check failed. Fix the issues and emit [[TASK_COMPLETE]] again."
			if diag != nil && diag.Message != "" {
				rejMsg = diag.Message
			}
			result.DiagnosticMsg = rejMsg
			_, _ = h.DB.ExecContext(ctx,
				`INSERT INTO task_comments (task_id, author, message) VALUES (?, 'interceptor', ?)`,
				taskID, rejMsg,
			)
			completionRejected = true
			completionRejections++
			if completionRejections >= maxCompletionRejections {
				// The agent cannot clear the checks on its own (2026-10-07: five
				// planning runs looped ~9h on the same rejection). Park the task
				// on the Board instead of re-prompting forever.
				result.Disposition = "backlog"
				result.DiagnosticMsg = fmt.Sprintf(
					"Completion was rejected %d times in this run, so the run was stopped and the task parked in backlog for the Board.\n\nLast rejection:\n%s",
					completionRejections, rejMsg)
				_, _ = h.DB.ExecContext(ctx,
					`UPDATE tasks SET is_blocked=1, block_reason=? WHERE id=?`,
					fmt.Sprintf("completion rejected %d times; needs the Board", completionRejections), taskID)
				runLog.Warn("interceptor rejected completion repeatedly; parking task", slog.Int("rejections", completionRejections))
				break
			}
			runLog.Info("interceptor rejected completion; continuing run", slog.Int("turn", turn))
			// Don't break — let the agent self-correct, up to maxCompletionRejections.
		}

		if cfg.MaxBudgetUSD > 0 && result.SpentUSD >= cfg.MaxBudgetUSD {
			result.Disposition = "capped"
			break
		}

		// Check run control signals after the turn completes.
		if rc := cfg.RunControl; rc != nil {
			if rc.IsStopRequested(taskID) {
				result.Disposition = "stopped"
				break
			}
			if rc.IsPaused(taskID) {
				// Emit paused state via SSE and update DB.
				if sr != nil {
					sr.EmitRunState("paused")
				}
				now := time.Now().UTC().Format(time.RFC3339Nano)
				_, _ = h.DB.ExecContext(ctx,
					`UPDATE tasks SET execution_stage='paused', updated_at=? WHERE id=?`+closedStageGuard,
					now, taskID,
				)
				runLog.Info("run paused after step", slog.Int("turn", turn))
				stopped := rc.WaitForResume(ctx, taskID)
				if stopped {
					result.Disposition = "stopped"
					break
				}
				// Resumed — update execution_stage back to in_progress.
				now = time.Now().UTC().Format(time.RFC3339Nano)
				_, _ = h.DB.ExecContext(ctx,
					`UPDATE tasks SET execution_stage='in_progress', updated_at=? WHERE id=?`+closedStageGuard,
					now, taskID,
				)
				if sr != nil {
					sr.EmitRunState("in_progress")
				}
				runLog.Info("run resumed", slog.Int("turn", turn))
			}
		}
	}

	if sr != nil {
		sr.Close()
	}

	// Git post-flight: dirty check, unpushed commits, merged-to-main report.
	// A failure is recorded but does not override the disposition — it annotates
	// the timeline and blocks `Mark done` at the UI/interceptor layer.
	if !nonGit {
		pfCtx, pfCancel := context.WithTimeout(context.Background(), 60*time.Second)
		pfResult, pfErr := gitgate.PostFlight(pfCtx, wtPath, "main")
		pfCancel()
		pfSummary := "git-postflight: "
		if pfErr != nil {
			pfSummary += "error: " + pfErr.Error()
		} else if !pfResult.OK {
			pfSummary += "FAILED — " + strings.Join(pfResult.Errors, "; ")
		} else {
			pfSummary += "ok (" + strings.Join(pfResult.Details, " | ") + ")"
		}
		_, _ = h.DB.ExecContext(context.Background(),
			`INSERT INTO task_comments (task_id, author, message) VALUES (?, 'harness', ?)`,
			taskID, pfSummary,
		)
		_, _ = h.DB.ExecContext(context.Background(),
			`INSERT INTO activity_log (task_id, event_type, details) VALUES (?, 'git_postflight', ?)`,
			taskID, pfSummary,
		)
		runLog.Info("git postflight", slog.String("result", pfSummary))
	}

	// Diff against pre-run checkpoint.
	if preCP != nil {
		if ds, err := checkpoint.DiffCheckpoint(ctx, wtPath, preCP.ID); err == nil {
			result.DiffStat = ds
		}
	}

	// Cleanup writes use a fresh context: the run context may be expired (wallclock cap).
	cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cleanCancel()

	// Register the work product before any post-loop disposition check (STA-391).
	// Skip if already registered by the inline interceptor path inside the turn loop.
	if result.DiffStat != "" && result.Disposition == "" && !workProductRegistered {
		if _, err := h.DB.ExecContext(cleanCtx,
			`INSERT INTO task_work_products (task_id, product_type, reference) VALUES (?, 'branch', ?)`,
			taskID, "staypoint/"+taskID,
		); err != nil {
			runLog.Warn("register work product failed", slog.Any("error", err))
		}
	}

	// Post-loop disposition: two cases.
	//
	// (a) completionRejected: the agent emitted [[TASK_COMPLETE]], the interceptor
	//     rejected it (and injected feedback), but turns were exhausted before the
	//     agent could self-correct. Set "capped" so the task never sits in_progress
	//     with no live run and no waiting path (STA-572).
	//
	// (b) No explicit completion signal (agent never emitted the marker): run the
	//     interceptor once as the classic post-loop gate — approved → in_review,
	//     rejected → in_progress.  This preserves the existing behaviour for runs
	//     where the agent does meaningful work across multiple turns but does not
	//     emit the marker explicitly.
	if result.Disposition == "" {
		if completionRejected {
			result.Disposition = "capped"
			capMsg := fmt.Sprintf(
				"Turn budget exhausted after completion rejection (%d turn(s)). Fix the listed issues and start a new run.",
				result.Turns,
			)
			result.DiagnosticMsg = capMsg
		} else {
			approved, diag, _ := h.Interceptor.InterceptCompletion(ctx, taskID, wtPath, repoPath)
			if approved {
				result.Disposition = "in_review"
			} else {
				result.Disposition = "in_progress"
				if diag != nil {
					result.DiagnosticMsg = diag.Message
				}
			}
		}
	}

	// Silent run (STA-775): the adapter ran but the agent printed nothing and
	// changed nothing. Say so with the exit code and stderr tail, and mark the
	// run failed instead of leaving it in_progress looking idle. A run the
	// Board stopped or a cap ended keeps that disposition; only the reason
	// is added to the timeline.
	var noOutputMsg string
	if sr != nil && sr.SawContent() {
		sawOutput = true
	}
	// A run the interceptor approved (in_review, e.g. from earlier work
	// products) is left alone: the task is ready regardless of this run.
	if adapterRan && !sawOutput && result.DiffStat == "" && result.Disposition != "in_review" {
		noOutputMsg = noOutputMessage(lastExitCode, lastStderr)
		switch result.Disposition {
		case "stopped", "capped":
		case "error":
			if result.DiagnosticMsg == "" {
				result.DiagnosticMsg = noOutputMsg
			}
		default: // in_progress: ended without completing and said nothing
			result.Disposition = "error"
			result.DiagnosticMsg = noOutputMsg
		}
		if sr != nil {
			sr.EmitMessage("Run ended with no output", noOutputMsg, "error")
		}
	}

	// Inject stop comment before updating execution_stage.
	if result.Disposition == "stopped" {
		result.DiagnosticMsg = "Run stopped by user request."
	}

	if sr != nil {
		sr.EmitState(result.Disposition)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)

	if _, err := h.DB.ExecContext(cleanCtx,
		`UPDATE tasks SET execution_stage=?, updated_at=? WHERE id=?`+closedStageGuard,
		result.Disposition, now, taskID,
	); err != nil {
		runLog.Error("persist disposition failed", slog.String("disposition", result.Disposition), slog.Any("error", err))
	}

	if result.DiagnosticMsg != "" {
		if _, err := h.DB.ExecContext(cleanCtx,
			`INSERT INTO task_comments (task_id, author, message) VALUES (?, 'harness', ?)`,
			taskID, result.DiagnosticMsg,
		); err != nil {
			runLog.Warn("inject diagnostic comment failed", slog.Any("error", err))
		}
	}

	// Post the agent's final response + run-summary footer so it appears in the
	// task chat thread — visible to the board without digging into think rows.
	// Author 'agent-summary' is excluded from fetchUserComments so this comment
	// is never re-injected into the agent's prompt as user input.
	{
		agentText := extractFinalResponse(lastTurnOutput)
		footer := buildRunFooter(result, wtPath)
		var summaryBody string
		if agentText != "" {
			summaryBody = agentText + "\n\n" + footer
		} else if noOutputMsg != "" {
			summaryBody = noOutputMsg + "\n\n" + footer
		} else {
			summaryBody = footer
		}
		if _, err := h.DB.ExecContext(cleanCtx,
			`INSERT INTO task_comments (task_id, author, message) VALUES (?, 'agent-summary', ?)`,
			taskID, summaryBody,
		); err != nil {
			runLog.Warn("post run summary comment failed", slog.Any("error", err))
		}
	}

	if _, err := h.DB.ExecContext(cleanCtx,
		`UPDATE tasks SET spent_turns=spent_turns+?, spent_usd=spent_usd+?, updated_at=? WHERE id=?`,
		result.Turns, result.SpentUSD, now, taskID,
	); err != nil {
		runLog.Warn("persist cost failed", slog.Any("error", err))
	}

	if _, err := h.DB.ExecContext(cleanCtx,
		`INSERT INTO activity_log (task_id, event_type, details) VALUES (?, 'run_complete', ?)`,
		taskID, fmt.Sprintf("disposition=%s turns=%d", result.Disposition, result.Turns),
	); err != nil {
		runLog.Warn("activity log failed", slog.Any("error", err))
	}

	return result, nil
}

// briefMaxBytes is the size cap for the injected task brief block.
// Content beyond this limit is truncated with a note so the agent knows it was cut.
const briefMaxBytes = 8 * 1024 // 8 KB

// taskBrief holds the immutable task metadata fetched once before the run loop.
type taskBrief struct {
	Name           string
	Org            string
	Project        string
	RepoPath       string
	GitBranch      string
	Description    string
	ShipReviewGate bool // true when gates.ship_review is enabled
	// PlainDir is set when the task runs in a non-git directory (STA-864).
	PlainDir bool
	// Handoff is the daemon-stored handoff from the parent task (STA-820).
	Handoff string
}

// harnessComment is a non-harness comment visible to the agent.
type harnessComment struct {
	ID      int64
	Author  string
	Message string
}

// fetchTaskBrief reads the task brief (name, org, project, repo, branch, description) from db.
// Returns a zero-value brief on any error so the run continues without brief injection.
func fetchTaskBrief(ctx context.Context, db *sql.DB, taskID string) taskBrief {
	var b taskBrief
	_ = db.QueryRowContext(ctx,
		`SELECT name, COALESCE(organization,''), COALESCE(project,''), repo_path, COALESCE(git_branch,'')
		 FROM tasks WHERE id = ?`, taskID,
	).Scan(&b.Name, &b.Org, &b.Project, &b.RepoPath, &b.GitBranch)
	_ = db.QueryRowContext(ctx,
		`SELECT content FROM task_documents WHERE task_id = ? AND doc_key = 'description' ORDER BY version DESC LIMIT 1`,
		taskID,
	).Scan(&b.Description)
	b.Handoff = fetchHandoff(ctx, db, taskID)
	var gateVal string
	_ = db.QueryRowContext(ctx, `SELECT value FROM settings_kv WHERE key='gates.ship_review'`).Scan(&gateVal)
	b.ShipReviewGate = gateVal != "false"
	return b
}

// handoffMaxBytes caps the parent handoff block separately from the brief so a
// long description cannot push the plan out of the first prompt.
const handoffMaxBytes = 16 * 1024

// fetchHandoff returns a child task's stored handoff document (STA-820). When
// the parent posted a final message after the child was created (the child
// was spawned mid-run), that message is appended so the child sees it too.
func fetchHandoff(ctx context.Context, db *sql.DB, taskID string) string {
	var handoff string
	_ = db.QueryRowContext(ctx,
		`SELECT content FROM task_documents WHERE task_id = ? AND doc_key = 'handoff' ORDER BY version DESC LIMIT 1`,
		taskID,
	).Scan(&handoff)
	if handoff == "" {
		return ""
	}
	var parentID string
	_ = db.QueryRowContext(ctx, `SELECT COALESCE(parent_id,'') FROM tasks WHERE id = ?`, taskID).Scan(&parentID)
	if parentID == "" {
		return handoff
	}
	var finalMsg string
	_ = db.QueryRowContext(ctx,
		`SELECT message FROM task_comments WHERE task_id = ? AND author = 'agent-summary' ORDER BY id DESC LIMIT 1`,
		parentID,
	).Scan(&finalMsg)
	finalMsg = strings.TrimSpace(finalMsg)
	if finalMsg != "" && !strings.Contains(handoff, finalMsg) {
		handoff = strings.TrimRight(handoff, "\n") + "\n--- Parent's latest final message ---\n" + finalMsg + "\n"
	}
	return handoff
}

// buildHandoffBlock wraps the parent handoff for the first-turn prompt.
func buildHandoffBlock(handoff string) string {
	if handoff == "" {
		return ""
	}
	body := safeField(handoff)
	if len(body) > handoffMaxBytes {
		body = body[:handoffMaxBytes] + "\n[...handoff truncated at 16 KB...]"
	}
	return "<<<PARENT_HANDOFF_BEGIN>>>\n" + body + "\n<<<PARENT_HANDOFF_END>>>\n"
}

// fetchUserComments returns board/user comments for taskID with id > afterID, ordered ascending.
// Comments authored by 'harness' or 'agent-summary' are excluded: they are harness-internal
// messages that must not be re-injected into the agent's prompt as user input.
func fetchUserComments(ctx context.Context, db *sql.DB, taskID string, afterID int64) []harnessComment {
	rows, err := db.QueryContext(ctx,
		`SELECT id, author, message FROM task_comments
		 WHERE task_id = ? AND author NOT IN ('harness', 'agent-summary') AND id > ?
		 ORDER BY id ASC`, taskID, afterID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []harnessComment
	for rows.Next() {
		var c harnessComment
		if err := rows.Scan(&c.ID, &c.Author, &c.Message); err == nil {
			out = append(out, c)
		}
	}
	return out
}

// buildBriefBlock constructs the task brief block for the agent prompt.
// It is treated as user-provided data: wrapped in clear delimiters.
// Content is truncated at briefMaxBytes with a note.
func buildBriefBlock(brief taskBrief, comments []harnessComment, isFirstTurn bool) string {
	var b strings.Builder
	if isFirstTurn {
		b.WriteString("<<<TASK_BRIEF_BEGIN>>>\n")
		b.WriteString("Name: " + safeField(brief.Name) + "\n")
		if brief.Org != "" || brief.Project != "" {
			b.WriteString("Project: " + safeField(brief.Org+"/"+brief.Project) + "\n")
		}
		b.WriteString("Repo: " + safeField(brief.RepoPath) + "\n")
		if brief.PlainDir {
			b.WriteString("Workspace: this folder is not a git repository. Work in it directly: there is no branch, no commit and no Ship Review card, and StayPoint cannot undo your edits, so change only what the task needs. Do not run git init.\n")
		}
		if brief.GitBranch != "" {
			b.WriteString("Branch: " + safeField(brief.GitBranch) + "\n")
		}
		if brief.Description != "" {
			b.WriteString("---\nDescription:\n" + safeField(brief.Description) + "\n")
		}
		if brief.ShipReviewGate {
			b.WriteString("---\nCompletion gate: Ship Review is enabled. Before emitting " + taskCompleteMarker + ", use the `staypoint_ship_review` MCP tool (or run `staypoint ship-review create`) to create a Ship Review card with a numbered test list and check runs. The harness will reject completion without a pending card.\n")
		}
	}
	if len(comments) > 0 {
		if isFirstTurn {
			b.WriteString("---\nComments:\n")
		} else {
			b.WriteString("<<<NEW_COMMENTS_BEGIN>>>\n")
		}
		for _, c := range comments {
			b.WriteString("[" + safeField(c.Author) + "]: " + safeField(c.Message) + "\n")
		}
	}
	if isFirstTurn {
		b.WriteString("<<<TASK_BRIEF_END>>>\n")
	} else if len(comments) > 0 {
		b.WriteString("<<<NEW_COMMENTS_END>>>\n")
	}

	result := b.String()
	if len(result) > briefMaxBytes {
		result = result[:briefMaxBytes] + "\n[...task brief truncated at 8 KB...]\n"
	}
	return result
}

// safeField strips control markers from user-supplied strings so field content
// cannot forge task completion or break out of the brief delimiters.
// Replacements use parentheses so the original bracket-delimited form can never
// be reconstructed from the replacement text (e.g. "[[[TASK_COMPLETE]]]" would
// produce "[[TASK_COMPLETE]]" if we replaced with "[TASK_COMPLETE]").
func safeField(s string) string {
	s = strings.ReplaceAll(s, taskCompleteMarker, "(TASK_COMPLETE)")
	s = strings.ReplaceAll(s, "<<<TASK_BRIEF_BEGIN>>>", "(TASK_BRIEF_BEGIN)")
	s = strings.ReplaceAll(s, "<<<TASK_BRIEF_END>>>", "(TASK_BRIEF_END)")
	s = strings.ReplaceAll(s, "<<<NEW_COMMENTS_BEGIN>>>", "(NEW_COMMENTS_BEGIN)")
	s = strings.ReplaceAll(s, "<<<NEW_COMMENTS_END>>>", "(NEW_COMMENTS_END)")
	s = strings.ReplaceAll(s, "<<<PARENT_HANDOFF_BEGIN>>>", "(PARENT_HANDOFF_BEGIN)")
	s = strings.ReplaceAll(s, "<<<PARENT_HANDOFF_END>>>", "(PARENT_HANDOFF_END)")
	return s
}

// buildRawArgs constructs CLI arguments for the adapter on the given turn.
// These are parsed by adapter.parseRawArgs into ParsedOptions.
func buildRawArgs(taskID string, turn int, cfg RunConfig, brief taskBrief, newComments []harnessComment) []string {
	var prompt string
	briefBlock := buildBriefBlock(brief, newComments, turn == 0)

	if briefBlock != "" {
		prompt = briefBlock + "\n"
	}
	if turn == 0 {
		if hb := buildHandoffBlock(brief.Handoff); hb != "" {
			prompt += hb + "\n"
		}
	}
	prompt += fmt.Sprintf(
		"Continue work on task %s (turn %d). When you are finished, emit %s on its own line.",
		taskID, turn+1, taskCompleteMarker,
	)
	if cfg.SkipPermissions {
		prompt += " Pre-approved permissions: proceed without confirmation prompts."
	}
	// Inject any pending user messages queued via the run-control endpoint.
	if cfg.RunControl != nil {
		if msgs := cfg.RunControl.DequeuePendingMessages(taskID); len(msgs) > 0 {
			prompt += "\n\n[Injected user message(s)]:\n"
			for _, m := range msgs {
				prompt += m + "\n"
			}
		}
	}
	args := []string{"--print", prompt, "--output-format", "stream-json"}
	return args
}

func buildRunID(agentID string) string {
	id := agentID
	if id == "" {
		id = "harness"
	}
	return id + "-" + uuid.New().String()[:8]
}

// stepTeeWriter tees all writes to dst AND feeds each newline-delimited line to StepRecorder.
// textDetected is set only when [[TASK_COMPLETE]] appears on its own line in the
// agent's own answer text, never in thinking/tool/user blocks or echoed prompts.
// The provider's parser decides what counts as answer text, so every output
// format works: Claude text blocks, the agy/Gemini final response (event=result),
// token-streamed text, and plain non-JSON output.
type stepTeeWriter struct {
	dst          io.Writer
	rec          *StepRecorder
	parse        func([]byte) ([]StepDelta, error)
	buf          []byte
	textDetected bool
	// agentText accumulates this turn's answer text so a marker split across
	// token-streamed deltas (or plain-text lines) is still seen on its own line.
	agentText []byte
}

// agentTextMax bounds agentText; past it only the most recent tail is kept.
const agentTextMax = 1 << 20

func (w *stepTeeWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	if n > 0 {
		w.buf = append(w.buf, p[:n]...)
		for {
			idx := bytes.IndexByte(w.buf, '\n')
			if idx < 0 {
				break
			}
			line := w.buf[:idx]
			w.buf = w.buf[idx+1:]
			if w.rec != nil {
				w.rec.FeedRawLine(line, w.parse)
			}
			w.checkTextMarker(line)
		}
	}
	return n, err
}

// checkTextMarker scans the agent's answer text for the completion marker.
// Text deltas from input events (FromUser: the continuation prompt itself
// contains the literal marker), thinking, tool input/output and failed results
// are ignored.
func (w *stepTeeWriter) checkTextMarker(line []byte) {
	if w.textDetected {
		return
	}
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	deltas, err := w.parse(line)
	if err != nil {
		// Not a stream event. Plain-text output is the agent's answer; a
		// malformed JSON event is not.
		if line[0] != '{' {
			w.addAgentText(line)
			w.addAgentText([]byte{'\n'})
			w.textDetected = markerOnOwnLine(string(w.agentText))
		}
		return
	}
	for _, d := range deltas {
		switch {
		case d.Kind == StepDeltaText && !d.FromUser:
			w.addAgentText([]byte(d.Text))
		case d.Kind == StepDeltaResult && !d.IsError:
			// The final answer (agy result.response, Claude result.result).
		default:
			continue
		}
		if markerOnOwnLine(d.Text) {
			w.textDetected = true
			return
		}
	}
	w.textDetected = markerOnOwnLine(string(w.agentText))
}

func (w *stepTeeWriter) addAgentText(p []byte) {
	w.agentText = append(w.agentText, p...)
	if over := len(w.agentText) - agentTextMax; over > 0 {
		w.agentText = append(w.agentText[:0], w.agentText[over:]...)
	}
}

// Close flushes any buffered partial line that lacked a trailing newline.
func (w *stepTeeWriter) Close() error {
	if len(w.buf) > 0 {
		if w.rec != nil {
			w.rec.FeedRawLine(w.buf, w.parse)
		}
		w.checkTextMarker(w.buf)
		w.buf = nil
	}
	return nil
}

// completionWriter wraps a Writer and sets detected=true on [[TASK_COMPLETE]].
type completionWriter struct {
	dst      io.Writer
	detected bool
	tail     []byte // sliding window to catch markers split across writes
}

func (w *completionWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	// Maintain a small sliding window across write boundaries.
	window := append(w.tail, p[:n]...)
	if strings.Contains(string(window), taskCompleteMarker) {
		w.detected = true
	}
	// Keep only the last len(marker)-1 bytes for the next boundary check.
	keep := len(taskCompleteMarker) - 1
	if len(window) > keep {
		w.tail = window[len(window)-keep:]
	} else {
		w.tail = window
	}
	return n, err
}
