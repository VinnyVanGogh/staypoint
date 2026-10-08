package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/bridge"
	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/VinnyVanGogh/staypoint/internal/security"
	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
	"github.com/VinnyVanGogh/staypoint/internal/telemetry"
	"github.com/VinnyVanGogh/staypoint/internal/trackgate"
	"github.com/VinnyVanGogh/staypoint/internal/wire"
	"github.com/VinnyVanGogh/staypoint/internal/workspace"
	"github.com/spf13/cobra"
)

var hookCmd = &cobra.Command{
	Use:   "hook",
	Short: "Staypoint lifecycle and prompt hooks for Claude Code and Antigravity",
}

var hookPromptCmd = &cobra.Command{
	Use:   "prompt",
	Short: "Claude Code UserPromptSubmit hook: monitors 5h limit & stages handoff",
	Run: func(cmd *cobra.Command, args []string) {
		handleHookPrompt()
	},
}

var hookPromptFormat string

func handleHookPrompt() {
	var rawInput []byte
	stat, err := os.Stdin.Stat()
	if err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
		rawInput, _ = io.ReadAll(os.Stdin)
	}

	var promptText string
	var sessionID string
	var payload map[string]interface{}
	isAntigravity := false

	if len(rawInput) > 0 {
		if err := json.Unmarshal(rawInput, &payload); err == nil {
			if p, ok := payload["prompt"].(string); ok {
				promptText = p
			}
			if s, ok := payload["sessionId"].(string); ok && s != "" {
				sessionID = s
			} else if s, ok := payload["session_id"].(string); ok && s != "" {
				sessionID = s
			} else if c, ok := payload["conversationId"].(string); ok && c != "" {
				sessionID = c
			}
			if _, ok := payload["conversationId"]; ok {
				isAntigravity = true
			}
			if _, ok := payload["invocationNum"]; ok {
				isAntigravity = true
			}
			if _, ok := payload["workspacePaths"]; ok {
				isAntigravity = true
			}
		} else {
			promptText = string(rawInput)
		}
	}

	if hookPromptFormat == "gemini" {
		isAntigravity = true
	} else if hookPromptFormat == "claude" {
		isAntigravity = false
	}

	if sessionID == "" {
		if s := os.Getenv("CLAUDE_SESSION_ID"); s != "" {
			sessionID = s
		} else if s := os.Getenv("GEMINI_SESSION_ID"); s != "" {
			sessionID = s
		} else if s := os.Getenv("STAYPOINT_SESSION_ID"); s != "" {
			sessionID = s
		} else if s := os.Getenv("MESH_SESSION_ID"); s != "" {
			sessionID = s
		} else {
			sessionID = fmt.Sprintf("session-pid-%d", os.Getppid())
		}
	}

	cwd, _ := os.Getwd()
	if payload != nil {
		if wsPaths, ok := payload["workspacePaths"].([]interface{}); ok && len(wsPaths) > 0 {
			if firstPath, ok := wsPaths[0].(string); ok && firstPath != "" {
				cwd = firstPath
			}
		}
	}
	var notices []string

	// Open staypoint database
	var dbConn *sql.DB
	if cfg != nil && cfg.DBPath != "" {
		if store, err := db.Open(cfg.DBPath); err == nil {
			dbConn = store.DB()
			defer store.Close()
			// Alerts raised by this hook reach the Board through board_alerts.
			telemetry.SetAlertSink(telemetry.DBAlertSink(dbConn))
			defer telemetry.SetAlertSink(nil)
		}
	}

	if dbConn != nil {
		// A. Register / Heartbeat Session
		branch := meshContext.GetCurrentGitBranch(cwd)
		agentType := "claude"
		if isAntigravity {
			agentType = "gemini"
		}
		_ = telemetry.HeartbeatSession(dbConn, telemetry.AgentSession{
			ID:        sessionID,
			AgentType: agentType,
			RepoPath:  cwd,
			GitBranch: branch,
			PID:       os.Getppid(),
		})

		// Asynchronously update base handoff as turn begins
		go func() {
			maxKeep := 3
			if cfg != nil && cfg.MaxHandoffsPerRepo > 0 {
				maxKeep = cfg.MaxHandoffsPerRepo
			}
			dataDir := ""
			if cfg != nil {
				dataDir = cfg.DataDir
			}
			_, _ = meshContext.AutoGenerateHandoffForSession(sessionID, cwd, "active_session", dbConn, dataDir, maxKeep)
		}()

		// B. Inspect working tree for multi-agent collision detection
		gitCtx := meshContext.GatherGitContext(cwd)
		var dirtyFiles []string
		for _, f := range gitCtx.ModifiedFiles {
			cleanF := cleanGitStatusFile(f)
			if cleanF != "" {
				dirtyFiles = append(dirtyFiles, cleanF)
				_ = telemetry.RecordWorkingFile(dbConn, sessionID, cwd, cleanF, "write", 15*time.Minute)
			}
		}

		if len(dirtyFiles) > 0 {
			collisions, _ := telemetry.CheckCollisions(dbConn, sessionID, cwd, dirtyFiles)
			if len(collisions) > 0 {
				var collLines []string
				for _, c := range collisions {
					ago := time.Since(c.LastTouchedAt).Round(time.Second)
					collLines = append(collLines, fmt.Sprintf("  • %s (touched %s ago by agent %s [session: %s, PID: %d])", c.FilePath, ago, c.OtherAgent, c.OtherSessionID, c.OtherPID))
				}
				notices = append(notices, fmt.Sprintf("⚠️ [STAYPOINT MULTI-AGENT COLLISION WARNING]: Another agent session is actively editing overlapping files in this repository:\n%s\nCoordinate with the user or wait for peer completion before editing or committing these files to prevent conflicts.", strings.Join(collLines, "\n")))
			}
		}

		// C. Agent Circuit Breaker check. Only this session's own breaker
		// pauses it: borrowing another session's tripped breaker for the same
		// repo told unrelated agents (e.g. a reviewer) to stop working.
		if cb, _ := telemetry.GetCircuitBreaker(dbConn, sessionID); cb != nil && cb.IsTripped {
			notices = append(notices, circuitBreakerNotice(cb))
		}

		// D. Task Budget Evaluation. Only the task this session runs for
		// counts: a daemon run's own task, or the task this interactive
		// session is attached to. Picking "the newest task in this repo"
		// blocked unrelated sessions on another task's spent budget.
		if activeTask := budgetTaskForSession(dbConn, sessionID, os.Getenv); activeTask != nil {
			eval := meshContext.EvaluateTaskBudget(activeTask)
			if eval.IsBlocked {
				fmt.Fprintf(os.Stderr, "❌ [STAYPOINT TASK BUDGET EXCEEDED]\nTask %q budget limit reached: %s\nSpent: $%.2f / $%.2f (%d / %d turns)\nHalting execution to prevent runaway costs.\nTo increase budget, run: staypoint task budget %s --usd <limit>\n", activeTask.Name, eval.Reason, activeTask.SpentUSD, activeTask.MaxBudgetUSD, activeTask.SpentTurns, activeTask.MaxTurns, activeTask.ID)
				os.Exit(2)
			} else if eval.IsWarning {
				notices = append(notices, fmt.Sprintf("⚠️ [STAYPOINT TASK BUDGET WARNING]: Task %q is at %.1f%% of budget ($%.2f / $%.2f max, %d / %d turns). %s", activeTask.Name, eval.PctBudget, activeTask.SpentUSD, activeTask.MaxBudgetUSD, activeTask.SpentTurns, activeTask.MaxTurns, eval.Reason))
			}
		}

		// E. Check unread Mesh Wire broadcasts
		unreadMsgs, _ := wire.GetUnread(dbConn, sessionID, cwd)
		if len(unreadMsgs) > 0 {
			var wireLines []string
			for _, m := range unreadMsgs {
				wireLines = append(wireLines, fmt.Sprintf("  • [%s] <%s>: %s", m.Channel, m.Author, m.Content))
			}
			notices = append(notices, fmt.Sprintf("📡 [STAYPOINT WIRE :: PEER AGENT BROADCASTS]:\n%s", strings.Join(wireLines, "\n")))
		}

		// F. Check for previous session handoffs in repo to present proactive pickup banner
		baseHandoffsDir := meshContext.GetHandoffsDir(cfg.DataDir)
		if latestMan, err := meshContext.GetLatestManifest(baseHandoffsDir, cwd); err == nil && latestMan != nil {
			if latestMan.SessionID != sessionID && time.Since(latestMan.CreatedAt) < 4*time.Hour {
				cleanSess := strings.ReplaceAll(sessionID, "/", "_")
				pickupDebounce := filepath.Join(os.TempDir(), fmt.Sprintf("staypoint-pickup-warned-%s.ts", cleanSess))
				if _, err := os.Stat(pickupDebounce); os.IsNotExist(err) {
					_ = os.WriteFile(pickupDebounce, []byte(fmt.Sprintf("%d", time.Now().Unix())), 0644)
					age := time.Since(latestMan.CreatedAt).Round(time.Minute)
					branchInfo := latestMan.GitBranch
					if branchInfo != "" {
						branchInfo = fmt.Sprintf(" on branch [%s]", branchInfo)
					}
					goalInfo := latestMan.Goal
					if goalInfo == "" {
						goalInfo = latestMan.Title
					}
					notices = append(notices, fmt.Sprintf("📋 [STAYPOINT PREVIOUS CONTEXT AVAILABLE]: Recent handoff from session %s (%s ago%s) found.\nGoal: %s\nTo inspect or adopt this context, view: %s or run: staypoint handoff show %s", latestMan.SessionID, age, branchInfo, goalInfo, latestMan.HandoffFile, latestMan.SessionID))
				}
			}
		}
	}

	// 1. Programmatically inspect prompt for client machine paths and auto-fetch them
	if promptText != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		guidance, _, _ := bridge.ProcessPromptForClientPaths(ctx, promptText)
		cancel()
		if guidance != "" {
			notices = append(notices, guidance)
		}
	}

	// 2. Quota notice check
	pacerState, err := router.LoadPacerState()
	if err == nil {
		poolPersonal := pacerState.Pools[router.PoolPersonalClaude]
		pool3P := pacerState.Pools[router.Pool3PClaude]
		poolWork := pacerState.Pools[router.PoolWorkClaude]

		var triggeredPool *router.QuotaPool
		var warningReason string

		checkPool := func(pool *router.QuotaPool) bool {
			if pool == nil {
				return false
			}
			if pool.FiveHour.UsedPct >= 85.0 || (pool.FiveHour.RemainingPct > 0 && pool.FiveHour.RemainingPct <= 15.0) {
				resetStr := "soon"
				if !pool.FiveHour.ResetsAt.IsZero() {
					resetStr = pool.FiveHour.ResetsAt.Format("3:04pm")
				} else if !pool.LockoutUntil.IsZero() {
					resetStr = pool.LockoutUntil.Format("3:04pm")
				}
				warningReason = fmt.Sprintf("5-hour session quota is at %.0f%% (~%.0f%% left, resets @%s)", pool.FiveHour.UsedPct, pool.FiveHour.RemainingPct, resetStr)
				return true
			}
			if pool.Weekly.UsedPct >= 85.0 || (pool.Weekly.RemainingPct > 0 && pool.Weekly.RemainingPct <= 15.0) {
				resetStr := "soon"
				if !pool.Weekly.ResetsAt.IsZero() {
					resetStr = pool.Weekly.ResetsAt.Format("Mon 3:04pm")
				}
				warningReason = fmt.Sprintf("weekly quota is at %.0f%% (only %.0f%% remaining, resets @%s)", pool.Weekly.UsedPct, pool.Weekly.RemainingPct, resetStr)
				return true
			}
			if pool.IsLocked {
				warningReason = fmt.Sprintf("quota is currently locked (%s)", pool.LockoutReason)
				return true
			}
			return false
		}

		if isAntigravity {
			// When running in Antigravity (Gemini), only check Gemini Native quota.
			poolGemini := pacerState.Pools[router.PoolGeminiNative]
			if checkPool(poolGemini) {
				triggeredPool = poolGemini
			}
		} else {
			// When running in Claude Code, check Claude pools.
			if checkPool(poolPersonal) {
				triggeredPool = poolPersonal
			} else if checkPool(pool3P) {
				triggeredPool = pool3P
			} else if checkPool(poolWork) {
				triggeredPool = poolWork
			}
		}

		if triggeredPool != nil {
			debounceFile := filepath.Join(os.TempDir(), fmt.Sprintf("staypoint-prelock-warned-u%d.ts", os.Getuid()))
			shouldNotify := true
			if stat, err := os.Stat(debounceFile); err == nil {
				if time.Since(stat.ModTime()) < 15*time.Minute {
					shouldNotify = false
				}
			}

			targetModel := "gemini"
			targetDisplay := "Gemini (/model gemini-3.8-flash-high or open Antigravity 'agy' and paste)"
			if isAntigravity {
				targetModel = "claude"
				targetDisplay = "Claude Code (/model claude-sonnet-4-6)"
			}

			if shouldNotify {
				_ = os.WriteFile(debounceFile, []byte(fmt.Sprintf("%d", time.Now().Unix())), 0644)
				_, _ = meshContext.GenerateHandoff(meshContext.HandoffOptions{
					TargetModel:       targetModel,
					ImmediateNextStep: fmt.Sprintf("Approaching quota limit: %s. Resume session seamlessly in %s.", warningReason, targetModel),
					Directory:         cwd,
					DB:                dbConn,
				})

				telemetry.SendQuotaAlert(triggeredPool.ID, telemetry.QuotaWarning,
					"[Staypoint] Quota Limit Warning (15% left)",
					fmt.Sprintf("%s %s. Handoff staged in clipboard. Switch to %s.", triggeredPool.Name, warningReason, targetDisplay),
				)
			}

			notices = append(notices, fmt.Sprintf("⚠️ [STAYPOINT QUOTA NOTICE]: %s %s. Staypoint has pre-staged a zero-token context handoff snapshot in your system clipboard and /tmp/ai-handoff.md. Remind the user to prepare to switch to %s before running out of turns.", triggeredPool.Name, warningReason, targetDisplay))
		}
	}

	// 3. Drain pending code reviews from Claude Code for Gemini
	homeDir, _ := os.UserHomeDir()
	if homeDir != "" {
		pendingGeminiDir := filepath.Join(homeDir, ".claude", "reviews", "pending-gemini")
		if entries, err := os.ReadDir(pendingGeminiDir); err == nil {
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".json") {
					filePath := filepath.Join(pendingGeminiDir, e.Name())
					if data, err := os.ReadFile(filePath); err == nil {
						var rev struct {
							SHA      string `json:"sha"`
							Repo     string `json:"repo"`
							Verdict  string `json:"verdict"`
							Review   string `json:"review"`
							Reviewer string `json:"reviewer"`
						}
						if json.Unmarshal(data, &rev) == nil {
							if rev.Repo == "" || strings.EqualFold(rev.Repo, filepath.Base(cwd)) {
								notices = append(notices, fmt.Sprintf("⚖️ [CODE REVIEW FROM CLAUDE CODE on commit %s : VERDICT %s]:\n%s", rev.SHA, rev.Verdict, rev.Review))
								_ = os.Remove(filePath)
							}
						}
					}
				}
			}
		}
	}

	fmt.Println(promptHookOutput(isAntigravity, notices))
}

// promptHookOutput renders the prompt hook's notices for the calling client:
// agy PreInvocation takes injectSteps (camelCase, protojson); Claude Code
// UserPromptSubmit takes additionalContext inside hookSpecificOutput. A
// top-level additionalContext is not in Claude's schema, so notices sent that
// way never reached the model.
func promptHookOutput(isAntigravity bool, notices []string) string {
	if len(notices) == 0 {
		return "{}"
	}
	text := strings.Join(notices, "\n\n")
	var resp any
	if isAntigravity {
		resp = map[string]any{"injectSteps": []map[string]string{{"ephemeralMessage": text}}}
	} else {
		resp = map[string]any{"hookSpecificOutput": map[string]string{
			"hookEventName": "UserPromptSubmit", "additionalContext": text,
		}}
	}
	out, _ := json.Marshal(resp)
	return string(out)
}

var hookInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install Antigravity and Claude Code lifecycle hooks for bidirectional review",
	Run: func(cmd *cobra.Command, args []string) {
		installHooks()
	},
}

// hookPreToolCmd is wired as a Claude Code PreToolUse hook.
// It reads the tool-call JSON from stdin, classifies Bash commands via the
// security classifier, and blocks Red-tier main-push/merge commands until the
// Board approves or the run is stopped — with no auto-deny timeout.
var hookPreToolCmd = &cobra.Command{
	Use:   "pre-tool",
	Short: "PreToolUse hook (Claude Code and agy): tracking gate + Red-tier Board approval gate",
	Run: func(cmd *cobra.Command, args []string) {
		handleHookPreTool()
	},
}

// preToolAllow is the response that lets the tool call proceed.
func preToolAllow() { fmt.Println(preToolAllowJSON(trackgate.ClientClaude)) }

// preToolAllowPinned lets the tool call proceed with its command replaced by
// pinned (STA-868): scripts judged or approved by content run from the exact
// bytes that were judged, not from a file that may have changed since. No
// permissionDecision: Claude Code's normal permission flow still applies.
func preToolAllowPinned(toolInput json.RawMessage, pinned string) {
	fmt.Println(pinnedHookOutput(toolInput, pinned))
}

func pinnedHookOutput(toolInput json.RawMessage, pinned string) string {
	in := map[string]any{}
	_ = json.Unmarshal(toolInput, &in)
	in["command"] = pinned
	out, _ := json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "updatedInput": in},
	})
	return string(out)
}

// pinJudged returns the command to run for a verdict below Red. A verdict
// that rests on script contents holds only when the command can be pinned to
// those bytes; otherwise it is raised to Red.
func pinJudged(cmd string, v *security.Verdict) string {
	if v.Tier >= security.Red || len(v.Scripts) == 0 {
		return ""
	}
	pinned, err := security.PinCommand(cmd, security.PinsFromVerdict(*v))
	if err != nil {
		v.Tier = security.Red
		v.Reasons = append(v.Reasons, "script judged by its contents, but the command cannot be pinned to those bytes: "+err.Error())
		return ""
	}
	return pinned
}

// snapshotScripts reads the scripts a held command runs, once, and returns
// them for the gate request plus the command pinned to those bytes ("" when
// it cannot be pinned; such a request is never auto-approved by a rule).
//
// snap must be the Snapshotter the classifier used, so a script judged by
// content and snapshotted for the Board is one read with one set of bytes.
func snapshotScripts(cmd, cwd string, snap *security.Snapshotter) ([]hookScript, string) {
	refs := security.ScriptRefs(cmd, cwd, snap, 0)
	if len(refs) == 0 {
		return nil, ""
	}
	scripts := make([]hookScript, 0, len(refs))
	for _, r := range refs {
		scripts = append(scripts, hookScript{Path: r.Path, Content: string(r.Full)})
	}
	pins, err := security.PinsFromRefs(refs)
	if err != nil {
		return scripts, ""
	}
	pinned, err := security.PinCommand(cmd, pins)
	if err != nil {
		return scripts, ""
	}
	return scripts, pinned
}

// preToolBlock writes a block decision back to Claude Code.
func preToolBlock(reason string) {
	fmt.Println(claudeBlockJSON(reason))
}

var (
	hookPreToolFormat       string
	hookPreToolTrackingOnly bool
)

func handleHookPreTool() {
	raw, _ := io.ReadAll(os.Stdin)

	// Nested agent under a daemon run with the task ID dropped: refuse every
	// tool call before any other gate can let it through (task-859a5234).
	if orphanedNestedAgent() {
		client := trackgate.ClientClaude
		if req, ok := parsePreToolRequest(raw, hookPreToolFormat, os.Getenv); ok {
			client = req.Client
		} else if hookPreToolFormat == "gemini" {
			client = trackgate.ClientGemini
		}
		fmt.Println(preToolDeny(client, nestedAgentReason))
		return
	}

	// Pause gate: block before ANY tool call when the run is paused.
	// This implements step-boundary pause (STA-505): at most the step in
	// flight when Pause was clicked finishes; subsequent steps are held here
	// until Resume is clicked (or Stop cancels the turn).
	if taskID := os.Getenv("STAYPOINT_TASK_ID"); taskID != "" {
		daemonURL, token := gateDaemonConn()
		if daemonURL != "" {
			waitForStepResume(daemonURL, token, taskID)
		}
	}

	// Tracking gate (STA-854): no writes in gated work repos unless the
	// session is attached to a StayPoint task. Runs for Claude Code and agy.
	client, blockOut, blocked := runTrackingGate(raw, hookPreToolFormat, os.Getenv, productionTrackingGate())
	if blocked {
		fmt.Println(blockOut)
		return
	}
	if client == trackgate.ClientGemini || hookPreToolTrackingOnly {
		// The Red-tier Board gate below understands Claude payloads only, and
		// interactive registrations (hook install) opt out of it.
		fmt.Println(preToolAllowJSON(client))
		return
	}

	var payload struct {
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
		SessionID string          `json:"session_id"`
		CWD       string          `json:"cwd"` // working directory for bare-push branch resolution
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.ToolName == "" {
		preToolAllow()
		return
	}

	// Only intercept Bash tool calls for security classification, and, for
	// a daemon-run agent, file edits (task-9d94997c).
	taskID := os.Getenv("STAYPOINT_TASK_ID")
	if !strings.EqualFold(payload.ToolName, "bash") {
		if taskID != "" && fileEditTools[strings.ToLower(payload.ToolName)] {
			gateFileEdit(payload.ToolName, payload.ToolInput, payload.SessionID, payload.CWD, taskID)
			return
		}
		preToolAllow()
		return
	}

	var bashInput struct {
		Command string `json:"command"`
		// Claude Code also passes the process cwd in tool_input for some versions
		CWD string `json:"cwd,omitempty"`
	}
	if err := json.Unmarshal(payload.ToolInput, &bashInput); err != nil || bashInput.Command == "" {
		preToolAllow()
		return
	}

	// Resolve effective working directory for bare-push detection. Only a
	// cwd from the payload is the shell's real one; the hook's own Getwd is a
	// guess, so relative script paths are not resolved against it (STA-868).
	cwd := payload.CWD
	if cwd == "" {
		cwd = bashInput.CWD
	}
	cwdTrusted := cwd != ""
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	// CWD enables bare-push branch resolution inside the classifier so the
	// same parsed argv handles `git -C /dir push` correctly.
	// ReadFile + ScratchDirs let a script in /tmp or the run's scratch dir be
	// judged by its contents instead of held as opaque (STA-868).
	// One Snapshotter per hook run: every script is read exactly once, through
	// one O_NOFOLLOW descriptor, and the same bytes are judged, shown to the
	// Board and pinned into the command that runs.
	snap := security.NewSnapshotter()
	c := &security.Classifier{CWD: cwd, CWDTrusted: cwdTrusted, Snap: snap,
		ScratchDirs:   hookScratchDirs(os.Getenv("STAYPOINT_TASK_ID")),
		PushPolicyFor: hookPushPolicy}
	verdict := c.Classify(bashInput.Command)
	// A daemon-run agent (STAYPOINT_TASK_ID is in the hook's own env, which
	// the command cannot change) asks the Board for anything that breaks an
	// unattended-run Board rule, whatever its tier (task-9d94997c).
	if taskID != "" {
		raiseForBoardRules(bashInput.Command, cwd, snap, &verdict)
	}

	if pinned := pinJudged(bashInput.Command, &verdict); pinned != "" {
		preToolAllowPinned(payload.ToolInput, pinned)
		return
	}
	if verdict.Tier < security.Red {
		preToolAllow()
		return
	}

	// Read gate toggle from config.toml (via the cfg global) so the agent
	// cannot disable it at runtime via the API — config.toml is only writable
	// by the user and requires a daemon restart to take effect.
	if cfg != nil && !cfg.Gates.MainMergeApprovalEnabled() {
		preToolAllow()
		return
	}

	// Red-tier command: post to daemon for Board approval.
	daemonURL, token := gateDaemonConn()
	if daemonURL == "" {
		// Fail-closed: daemon unreachable, cannot get Board decision.
		preToolBlock(fmt.Sprintf("Board gate unreachable; command blocked (%s)", strings.Join(verdict.Reasons, "; ")))
		return
	}

	// Snapshot the scripts the command runs: what the Board, advisors and
	// rules see is what runs, when the command can be pinned.
	scripts, pinned := snapshotScripts(bashInput.Command, cwd, snap)
	allow := func() {
		if pinned != "" {
			preToolAllowPinned(payload.ToolInput, pinned)
			return
		}
		preToolAllow()
	}

	// Create a pending gate request in the daemon.
	gr := createGateRequest(daemonURL, token, gateRequestBody{
		Cmdline: bashInput.Command, Reasons: verdict.Reasons, RunID: payload.SessionID,
		TaskID: os.Getenv("STAYPOINT_TASK_ID"), CWD: cwd, Scripts: scripts, Pinned: pinned != "",
	})
	if gr == nil {
		preToolBlock(fmt.Sprintf("could not register gate request; command blocked (%s)", strings.Join(verdict.Reasons, "; ")))
		return
	}
	if gr.Status == "approved" {
		// A Board allow rule matched (STA-868); the daemon logged it.
		allow()
		return
	}

	// Block until the Board decides. Only a trusted task's delete outside its
	// worktree has a deadline, kept by the daemon: then it reports "deferred"
	// and the command is skipped, not run (task-6c1ed91f).
	// The hook process sits here; Claude Code cannot call the tool while we hold.
	for {
		status, decidedBy := pollGateRequest(daemonURL, token, gr.ID)
		switch status {
		case "approved":
			allow()
			return
		case "denied":
			if strings.HasSuffix(decidedBy, ":tev1") {
				preToolBlock(fmt.Sprintf("tev1 denied this command, so it was not run, and this task is parked until the Board reviews it in the morning (gate %s). Stop here; do not retry or work around it.", gr.ID))
				return
			}
			preToolBlock(fmt.Sprintf("Board denied: %s", strings.Join(verdict.Reasons, "; ")))
			return
		case "deferred":
			preToolBlock(deferredMessage(gr.ID))
			return
		case "":
			// daemon unreachable mid-poll — fail-closed
			preToolBlock(fmt.Sprintf("Board gate unreachable during poll; command blocked (%s)", strings.Join(verdict.Reasons, "; ")))
			return
		default:
			// still pending — loop
		}
	}
}

// deferredMessage is what the agent is told when a held request's deadline
// passes: it was skipped, and the Board decides it later.
func deferredMessage(id string) string {
	return fmt.Sprintf("deferred: held for the Board's morning review, skipped and NOT run (gate %s). Continue with other work; do not retry it or work around it. If the Board approves, you will be told on the task thread to perform it.", id)
}

// raiseForBoardRules raises v to Red when cmd, or a script it runs, breaks a
// Board rule. Scripts are read through snap, as on the Red path.
func raiseForBoardRules(cmd, cwd string, snap *security.Snapshotter, v *security.Verdict) {
	var hashes []security.ScriptHash
	for _, r := range security.ScriptRefs(cmd, cwd, snap, 0) {
		hashes = append(hashes, security.ScriptHash{Path: r.Path, Content: string(r.Full)})
	}
	if why := security.AnalyzeBoardRules(cmd, hashes); why != "" {
		v.Tier = security.Red
		v.Reasons = append(v.Reasons, "board rule: "+why)
	}
}

// gateDaemonConn is resolveDaemonConn for the PreToolUse gates (pause, Red
// Bash, file edit); tests point it at a fake or test daemon.
var gateDaemonConn = resolveDaemonConn

// fileEditTools are the Claude Code tools that write files.
var fileEditTools = map[string]bool{"write": true, "edit": true, "multiedit": true, "notebookedit": true}

// fileEditHold reports why a daemon-run agent's edit of path must wait for
// the Board: a self-protected path, or a file outside the task's worktree
// (cwd) and scratch dirs. "" means it may proceed.
func fileEditHold(path, cwd, home string, scratch []string) string {
	if path == "" {
		return "file edit without a path"
	}
	if !filepath.IsAbs(path) && !strings.HasPrefix(path, "~/") && cwd == "" {
		return "relative file path with no working directory"
	}
	path = resolveEditPath(path, cwd, home)
	if why := security.SelfProtectedPath(path); why != "" {
		return why
	}
	inside := func(dir string) bool {
		if dir == "" {
			return false
		}
		dir = resolveBestEffort(filepath.Clean(dir))
		return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
	}
	if inside(cwd) {
		return ""
	}
	for _, d := range scratch {
		if inside(d) {
			return ""
		}
	}
	return "edits a file outside the task worktree: " + path
}

// resolveEditPath makes an edit target absolute (~ and cwd), clean and
// symlink-resolved where it exists.
func resolveEditPath(path, cwd, home string) string {
	if strings.HasPrefix(path, "~/") && home != "" {
		path = filepath.Join(home, path[2:])
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	return resolveBestEffort(filepath.Clean(path))
}

// resolveBestEffort follows symlinks in the longest existing prefix of p.
func resolveBestEffort(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	dir, base := filepath.Split(p)
	dir = filepath.Clean(dir)
	if dir == p || dir == "." || dir == "/" {
		return p
	}
	return filepath.Join(resolveBestEffort(dir), base)
}

// gateFileEdit holds a daemon-run agent's file edit for the Board exactly
// like a held Bash command when fileEditHold says so.
func gateFileEdit(tool string, toolInput json.RawMessage, sessionID, cwd, taskID string) {
	var in struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	}
	_ = json.Unmarshal(toolInput, &in)
	path := in.FilePath
	if path == "" {
		path = in.NotebookPath
	}
	home, _ := os.UserHomeDir()
	why := fileEditHold(path, cwd, home, hookScratchDirs(taskID))
	if why == "" {
		preToolAllow()
		return
	}
	if cfg != nil && !cfg.Gates.MainMergeApprovalEnabled() {
		preToolAllow()
		return
	}
	reasons := []string{why}
	daemonURL, token := gateDaemonConn()
	if daemonURL == "" {
		preToolBlock(fmt.Sprintf("Board gate unreachable; edit blocked (%s)", why))
		return
	}
	gr := createGateRequest(daemonURL, token, gateRequestBody{
		Cmdline: fmt.Sprintf("%s %s", tool, resolveEditPath(path, cwd, home)), Reasons: reasons, RunID: sessionID, TaskID: taskID, CWD: cwd,
	})
	if gr == nil {
		preToolBlock(fmt.Sprintf("could not register gate request; edit blocked (%s)", why))
		return
	}
	if gr.Status == "approved" {
		preToolAllow()
		return
	}
	for {
		status, _ := pollGateRequest(daemonURL, token, gr.ID)
		switch status {
		case "approved":
			preToolAllow()
			return
		case "denied":
			preToolBlock(fmt.Sprintf("Board denied: %s", why))
			return
		case "deferred":
			preToolBlock(deferredMessage(gr.ID))
			return
		case "":
			preToolBlock(fmt.Sprintf("Board gate unreachable during poll; edit blocked (%s)", why))
			return
		}
	}
}

func resolveDaemonConn() (daemonURL, token string) {
	port := 41421
	daemonURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	if cfg != nil {
		tokenPath := filepath.Join(cfg.DataDir, "auth_token")
		if data, err := os.ReadFile(tokenPath); err == nil {
			token = strings.TrimSpace(string(data))
		}
	}
	return daemonURL, token
}

type gateRequestRef struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// gateRequestBody is POST /api/security/gate-requests. TaskID and CWD let the
// daemon match Board allow rules scoped to a task, repo or organization.
type gateRequestBody struct {
	Cmdline string   `json:"cmdline"`
	Reasons []string `json:"reasons"`
	RunID   string   `json:"run_id"`
	TaskID  string   `json:"task_id,omitempty"`
	CWD     string   `json:"cwd,omitempty"`
	// Scripts are snapshots of the scripts the command runs; Pinned says the
	// hook will run exactly those bytes when approved.
	Scripts []hookScript `json:"scripts,omitempty"`
	Pinned  bool         `json:"pinned,omitempty"`
}

type hookScript struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// hookPushPolicy resolves the push_policy (STA-562) of the repo a push runs
// in. A task worktree resolves to its main checkout through the git common
// dir, which is where project_dev_configs rows are keyed. The database is
// opened read-only, so a hook built from another version never migrates the
// daemon's schema. A repo with no row (or no explicit choice) is
// "branch_only", per the Board decision of 2026-10-08. It fails closed: a
// relative dir, no repo, no configured or openable database, a lookup error
// (including a missing column) or an unknown value all return "never".
func hookPushPolicy(dir string) string {
	never := string(shipreview.PushPolicyNever)
	if dir == "" || !filepath.IsAbs(dir) || cfg == nil || cfg.DBPath == "" {
		return never
	}
	root := ""
	if common := gitexec.CommonDir(dir); filepath.Base(common) == ".git" {
		root = filepath.Dir(common)
	}
	if root == "" {
		return never
	}
	conn, err := db.OpenReadOnly(cfg.DBPath)
	if err != nil {
		return never
	}
	defer conn.Close()
	return string(shipreview.GetProjectPushPolicy(conn, root))
}

// hookScratchDirs are the system temp dirs plus the task's scratch dir.
func hookScratchDirs(taskID string) []string {
	dirs := security.DefaultScratchDirs()
	if taskID != "" {
		if d, err := workspace.ScratchDir(taskID); err == nil {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

func createGateRequest(daemonURL, token string, in gateRequestBody) *gateRequestRef {
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		daemonURL+"/api/security/gate-requests", bytes.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusCreated {
		return nil
	}
	defer resp.Body.Close()
	var gr gateRequestRef
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil || gr.ID == "" {
		return nil
	}
	return &gr
}

// pollGateRequest long-polls the gate request and returns its status —
// "approved", "denied", "deferred" (skipped at its deadline), "pending"
// (still waiting), or "" (connection error) — and who decided it.
func pollGateRequest(daemonURL, token, id string) (status, decidedBy string) {
	url := fmt.Sprintf("%s/api/security/gate-requests/%s?wait=true", daemonURL, id)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return "", ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 35 * time.Second} // slightly longer than server 29s
	resp, err := client.Do(req)
	if err != nil {
		// Daemon may be restarting; wait briefly then retry (still no timeout).
		time.Sleep(2 * time.Second)
		return "pending", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", ""
	}
	var gr struct {
		Status    string `json:"status"`
		DecidedBy string `json:"decided_by"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return "", ""
	}
	return gr.Status, gr.DecidedBy
}

// waitForStepResume blocks until the pause flag is cleared or stop is requested.
// It long-polls the daemon's run-control-state endpoint (each call blocks up to
// 29 s server-side) so the PreToolUse hook holds the tool boundary while paused.
// Fail-open: if the daemon is unreachable we allow the tool rather than freezing.
func waitForStepResume(daemonURL, token, taskID string) {
	for {
		url := fmt.Sprintf("%s/api/tasks/%s/run-control-state?wait=true", daemonURL, taskID)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)
		client := &http.Client{Timeout: 35 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return // daemon unreachable — fail-open
		}
		var s struct {
			Paused        bool `json:"paused"`
			StopRequested bool `json:"stop_requested"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&s)
		resp.Body.Close()
		if decodeErr != nil {
			return
		}
		if s.StopRequested || !s.Paused {
			return // stop or unpaused — proceed
		}
		// still paused — loop; long-poll already waited ~29 s server-side
	}
}

func init() {
	hookPromptCmd.Flags().StringVar(&hookPromptFormat, "format", "auto", "Output format: auto, gemini, or claude")
	hookPreToolCmd.Flags().StringVar(&hookPreToolFormat, "format", "auto", "Payload/output format: auto, gemini (agy), or claude")
	hookPreToolCmd.Flags().BoolVar(&hookPreToolTrackingOnly, "tracking-only", false, "Run only the pause and tracking gates, not the Red-tier Board approval gate (interactive sessions)")
	rootCmd.AddCommand(hookCmd)
	hookCmd.AddCommand(hookPromptCmd)
	hookCmd.AddCommand(hookPreToolCmd)
	hookCmd.AddCommand(hookInstallCmd)
}

// circuitBreakerNotice renders a tripped breaker for the prompt hook, leaving
// out the tool and command when the watcher could not attribute the failure.
func circuitBreakerNotice(cb *telemetry.CircuitBreaker) string {
	what := "repeated tool failures"
	switch {
	case cb.FailingTool != "" && cb.FailingTool != "tool" && cb.FailingCommand != "":
		what = fmt.Sprintf("repeated failures of %s on %s", cb.FailingTool, cb.FailingCommand)
	case cb.FailingTool != "" && cb.FailingTool != "tool":
		what = fmt.Sprintf("repeated failures of %s", cb.FailingTool)
	case cb.FailingCommand != "":
		what = fmt.Sprintf("repeated failures of %s", cb.FailingCommand)
	}
	return fmt.Sprintf("🚨 [STAYPOINT CIRCUIT BREAKER ACTIVE]: Execution pause active because an agent loop was detected (%s).\nLast error: %s\nTo reset and proceed, run: staypoint breaker reset %s", what, cb.LastError, cb.SessionID)
}

// budgetTaskForSession returns the task whose budget governs this session:
// STAYPOINT_TASK_ID for a daemon run, else the task the session is attached
// to, else nil (no budget applies).
func budgetTaskForSession(db *sql.DB, sessionID string, getenv func(string) string) *meshContext.Task {
	id := getenv("STAYPOINT_TASK_ID")
	if id == "" && sessionID != "" {
		id, _ = trackgate.SessionTask(db, sessionID)
	}
	if id == "" {
		return nil
	}
	t, err := meshContext.GetTask(db, id)
	if err != nil {
		return nil
	}
	return t
}
