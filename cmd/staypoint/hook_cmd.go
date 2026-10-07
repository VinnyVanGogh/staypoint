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
	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/VinnyVanGogh/staypoint/internal/security"
	"github.com/VinnyVanGogh/staypoint/internal/telemetry"
	"github.com/VinnyVanGogh/staypoint/internal/trackgate"
	"github.com/VinnyVanGogh/staypoint/internal/wire"
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

		// D. Task Budget Evaluation
		activeTask, _ := meshContext.GetActiveTaskForRepo(dbConn, cwd)
		if activeTask != nil {
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

				telemetry.SendNotification(
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

	if isAntigravity {
		if len(notices) > 0 {
			type InjectedStep struct {
				EphemeralMessage string `json:"ephemeralMessage,omitempty"`
			}
			type HookResp struct {
				InjectSteps []InjectedStep `json:"injectSteps"`
			}
			resp := HookResp{
				InjectSteps: []InjectedStep{
					{
						EphemeralMessage: strings.Join(notices, "\n\n"),
					},
				},
			}
			out, _ := json.Marshal(resp)
			fmt.Println(string(out))
			return
		}
		fmt.Println("{}")
		return
	}

	if len(notices) > 0 {
		resp := map[string]string{
			"additionalContext": strings.Join(notices, "\n\n"),
		}
		out, _ := json.Marshal(resp)
		fmt.Println(string(out))
		return
	}

	fmt.Println("{}")
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
func preToolAllow() { fmt.Println("{}") }

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

	// Pause gate: block before ANY tool call when the run is paused.
	// This implements step-boundary pause (STA-505): at most the step in
	// flight when Pause was clicked finishes; subsequent steps are held here
	// until Resume is clicked (or Stop cancels the turn).
	if taskID := os.Getenv("STAYPOINT_TASK_ID"); taskID != "" {
		daemonURL, token := resolveDaemonConn()
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
		preToolAllow()
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

	// Only intercept Bash tool calls for security classification.
	if !strings.EqualFold(payload.ToolName, "bash") {
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

	// Resolve effective working directory for bare-push detection.
	cwd := payload.CWD
	if cwd == "" {
		cwd = bashInput.CWD
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	// CWD enables bare-push branch resolution inside the classifier so the
	// same parsed argv handles `git -C /dir push` correctly.
	c := &security.Classifier{CWD: cwd}
	verdict := c.Classify(bashInput.Command)

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
	daemonURL, token := resolveDaemonConn()
	if daemonURL == "" {
		// Fail-closed: daemon unreachable, cannot get Board decision.
		preToolBlock(fmt.Sprintf("Board gate unreachable; command blocked (%s)", strings.Join(verdict.Reasons, "; ")))
		return
	}

	// Create a pending gate request in the daemon.
	gr := createGateRequest(daemonURL, token, bashInput.Command, verdict.Reasons, payload.SessionID)
	if gr == nil {
		preToolBlock(fmt.Sprintf("could not register gate request; command blocked (%s)", strings.Join(verdict.Reasons, "; ")))
		return
	}

	// Block indefinitely until the Board decides (no timeout).
	// The hook process sits here; Claude Code cannot call the tool while we hold.
	for {
		status := pollGateRequest(daemonURL, token, gr.ID)
		switch status {
		case "approved":
			preToolAllow()
			return
		case "denied":
			preToolBlock(fmt.Sprintf("Board denied: %s", strings.Join(verdict.Reasons, "; ")))
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
	ID string `json:"id"`
}

func createGateRequest(daemonURL, token, cmdline string, reasons []string, runID string) *gateRequestRef {
	body, _ := json.Marshal(map[string]any{
		"cmdline": cmdline,
		"reasons": reasons,
		"run_id":  runID,
	})
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

// pollGateRequest long-polls the gate request and returns "approved", "denied",
// "pending" (still waiting), or "" (connection error).
func pollGateRequest(daemonURL, token, id string) string {
	url := fmt.Sprintf("%s/api/security/gate-requests/%s?wait=true", daemonURL, id)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 35 * time.Second} // slightly longer than server 29s
	resp, err := client.Do(req)
	if err != nil {
		// Daemon may be restarting; wait briefly then retry (still no timeout).
		time.Sleep(2 * time.Second)
		return "pending"
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ""
	}
	var gr struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return ""
	}
	return gr.Status
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
