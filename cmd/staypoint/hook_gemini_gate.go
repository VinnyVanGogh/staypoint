package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
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

// geminiShellCall is what the gate judges in an agy shell tool call.
type geminiShellCall struct {
	commands      []string // every distinct command text in the args
	cwd           string
	sessionID     string
	bypassSandbox bool
}

// geminiShellToolNames are agy's tools that run a shell command or feed input
// to a running one (send_command_input), so their text is judged as a command.
var geminiShellToolNames = map[string]bool{"run_command": true, "shell_exec": true, "send_command_input": true}

// geminiShellMeta are shell-tool args that never hold command text.
var geminiShellMeta = map[string]bool{
	"cwd": true, "waitmsbeforeasync": true, "toolaction": true, "toolsummary": true,
	"runpersistent": true, "isdaemon": true, "bypasssandbox": true, "commandid": true,
}

// parseGeminiShellCall reads agy's payload itself rather than trusting the
// tracking-gate parser: it returns nil for a tool that is not a shell tool,
// and an error (deny) whenever a shell call cannot be read completely. Any
// string arg that is not known metadata counts as command text, so a command
// in a field the gate did not expect is judged, not skipped.
func parseGeminiShellCall(raw []byte) (*geminiShellCall, error) {
	var p struct {
		ToolCall *struct {
			Name string                     `json:"name"`
			Args map[string]json.RawMessage `json:"args"`
		} `json:"toolCall"`
		ConversationID string   `json:"conversationId"`
		WorkspacePaths []string `json:"workspacePaths"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.ToolCall == nil || strings.TrimSpace(p.ToolCall.Name) == "" {
		return nil, fmt.Errorf("StayPoint could not read this tool call, so it was blocked")
	}
	name := strings.ToLower(strings.TrimSpace(p.ToolCall.Name))
	name = strings.TrimPrefix(name, "cortex_step_type_")
	if !geminiShellToolNames[name] {
		return nil, nil
	}
	call := &geminiShellCall{sessionID: p.ConversationID}
	seen := map[string]bool{}
	keys := make([]string, 0, len(p.ToolCall.Args))
	for k := range p.ToolCall.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := p.ToolCall.Args[k]
		lk := strings.ToLower(k)
		var s string
		isString := json.Unmarshal(v, &s) == nil
		switch {
		case lk == "cwd":
			if isString {
				call.cwd = s
			}
		case lk == "bypasssandbox":
			var b bool
			if json.Unmarshal(v, &b) != nil || b {
				call.bypassSandbox = true
			}
		case geminiShellMeta[lk]:
		case isString:
			if t := strings.TrimSpace(s); t != "" && !seen[t] {
				seen[t] = true
				call.commands = append(call.commands, t)
			}
		default:
			var scalar any
			if json.Unmarshal(v, &scalar) != nil {
				return nil, fmt.Errorf("StayPoint could not read argument %q of %s, so it was blocked", k, name)
			}
			switch scalar.(type) {
			case bool, float64, nil:
			default:
				// An array or object could be an argv; it cannot be judged.
				return nil, fmt.Errorf("StayPoint cannot judge argument %q of %s, so it was blocked", k, name)
			}
		}
	}
	if len(call.commands) == 0 {
		return nil, fmt.Errorf("StayPoint found no command text in this %s call, so it was blocked", name)
	}
	if call.cwd == "" && len(p.WorkspacePaths) > 0 {
		call.cwd = p.WorkspacePaths[0]
	}
	return call, nil
}

// gateGeminiPreTool runs agy's shell commands through the same Red-tier
// Board gate as Claude's Bash (task-21e96721) and returns agy's JSON answer.
// Anything that is not a shell command is allowed here: the tracking gate
// and the Gemini code guard have already run.
func gateGeminiPreTool(raw []byte) string {
	allow := preToolAllowJSON(trackgate.ClientGemini)
	deny := func(reason string) string { return preToolDeny(trackgate.ClientGemini, reason) }

	call, err := parseGeminiShellCall(raw)
	if err != nil {
		return deny(err.Error())
	}
	if call == nil {
		return allow
	}

	cwd := call.cwd
	cwdTrusted := cwd != ""
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	taskID := os.Getenv("STAYPOINT_TASK_ID")
	snap := security.NewSnapshotter()
	c := &security.Classifier{CWD: cwd, CWDTrusted: cwdTrusted, Snap: snap,
		ScratchDirs:   hookScratchDirs(taskID),
		PushPolicyFor: hookPushPolicy}
	// Every command text in the call is judged; the strictest verdict wins,
	// so a second field agy might run instead cannot slip past.
	var verdict security.Verdict
	for _, text := range call.commands {
		v := c.Classify(text)
		if taskID != "" {
			raiseForBoardRules(text, cwd, snap, &v)
		}
		// Claude runs a script judged by its contents pinned to those bytes;
		// agy cannot run a rewritten command, so such a command goes to the Board.
		if v.Tier < security.Red && len(v.Scripts) > 0 {
			v.Tier = security.Red
			v.Reasons = append(v.Reasons, "script judged by its contents, but agy cannot be pinned to those bytes")
		}
		if v.Tier > verdict.Tier {
			verdict.Tier = v.Tier
		}
		verdict.Reasons = append(verdict.Reasons, v.Reasons...)
	}
	if call.bypassSandbox {
		verdict.Tier = security.Red
		verdict.Reasons = append(verdict.Reasons, "asks to run outside agy's sandbox")
	}
	if verdict.Tier < security.Red {
		return allow
	}
	cmdline := strings.Join(call.commands, "\n")
	if cfg != nil && !cfg.Gates.MainMergeApprovalEnabled() {
		return allow
	}

	reasons := strings.Join(verdict.Reasons, "; ")
	daemonURL, token := geminiGateDaemon()
	if daemonURL == "" {
		return deny(fmt.Sprintf("Board gate unreachable; command blocked (%s)", reasons))
	}
	var scripts []hookScript
	for _, text := range call.commands {
		s, _ := snapshotScripts(text, cwd, snap)
		scripts = append(scripts, s...)
	}
	gr := createGateRequest(daemonURL, token, gateRequestBody{
		Cmdline: cmdline, Reasons: verdict.Reasons, RunID: call.sessionID,
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
