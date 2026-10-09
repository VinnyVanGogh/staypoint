package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/security"
	"github.com/VinnyVanGogh/staypoint/internal/trackgate"
)

// agy kills a PreToolUse hook after its registered timeout (30s,
// hook_install_pretool.go), and what it does with the tool call then is
// unverified. Claude Code runs it. So the Gemini gate always answers well
// inside that limit: the daemon skips the request after geminiGateMaxWait,
// and the hook stops waiting at geminiGateBudget whatever happens.
const geminiGateMaxWait = 15 * time.Second

var geminiGateBudget = 22 * time.Second

// geminiGateDaemon resolves the daemon for the Gemini gate; tests replace it.
var geminiGateDaemon = gateDaemonConn

// gateGeminiPreTool runs agy's shell commands through the same Red-tier
// Board gate as Claude's Bash (task-21e96721) and returns agy's JSON answer.
// Anything that is not a shell command is allowed here: the tracking gate
// and the Gemini code guard have already run.
func gateGeminiPreTool(raw []byte) string {
	allow := preToolAllowJSON(trackgate.ClientGemini)
	deny := func(reason string) string { return preToolDeny(trackgate.ClientGemini, reason) }

	req, ok := parsePreToolRequest(raw, "gemini", os.Getenv)
	if !ok || strings.TrimSpace(req.Command) == "" {
		return allow
	}

	cwd := req.CWD
	cwdTrusted := cwd != ""
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	taskID := os.Getenv("STAYPOINT_TASK_ID")
	snap := security.NewSnapshotter()
	c := &security.Classifier{CWD: cwd, CWDTrusted: cwdTrusted, Snap: snap,
		ScratchDirs:   hookScratchDirs(taskID),
		PushPolicyFor: hookPushPolicy}
	verdict := c.Classify(req.Command)
	if taskID != "" {
		raiseForBoardRules(req.Command, cwd, snap, &verdict)
	}
	// Claude runs a script judged by its contents pinned to those bytes;
	// agy cannot run a rewritten command, so such a command goes to the Board.
	if verdict.Tier < security.Red && len(verdict.Scripts) > 0 {
		verdict.Tier = security.Red
		verdict.Reasons = append(verdict.Reasons, "script judged by its contents, but agy cannot be pinned to those bytes")
	}
	if verdict.Tier < security.Red {
		return allow
	}
	if cfg != nil && !cfg.Gates.MainMergeApprovalEnabled() {
		return allow
	}

	reasons := strings.Join(verdict.Reasons, "; ")
	daemonURL, token := geminiGateDaemon()
	if daemonURL == "" {
		return deny(fmt.Sprintf("Board gate unreachable; command blocked (%s)", reasons))
	}
	scripts, _ := snapshotScripts(req.Command, cwd, snap)
	gr := createGateRequest(daemonURL, token, gateRequestBody{
		Cmdline: req.Command, Reasons: verdict.Reasons, RunID: req.SessionID,
		TaskID: taskID, CWD: cwd, Scripts: scripts,
		MaxWaitSeconds: int(geminiGateMaxWait / time.Second),
	})
	if gr == nil {
		return deny(fmt.Sprintf("could not register gate request; command blocked (%s)", reasons))
	}
	if gr.Status == "approved" {
		return allow
	}
	status, _ := waitGateDecisionWithin(daemonURL, token, gr.ID, geminiGateBudget)
	switch status {
	case "approved":
		return allow
	case "denied":
		return deny(fmt.Sprintf("Board denied: %s", reasons))
	case "deferred":
		return deny(deferredMessage(gr.ID))
	case "expired":
		return deny(heldPastWaitMessage(gr.ID))
	default:
		return deny(fmt.Sprintf("Board gate unreachable during poll; command blocked (%s)", reasons))
	}
}
