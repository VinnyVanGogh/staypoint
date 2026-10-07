package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/VinnyVanGogh/staypoint/internal/trackgate"
)

// STA-854: tracking gate in the PreToolUse hook. Claude Code and agy (Gemini /
// Antigravity CLI) both call `staypoint hook pre-tool`; the payload shape
// tells them apart (agy sends camelCase toolCall/conversationId).

// preToolPayload is the union of the Claude Code and agy PreToolUse payloads.
type preToolPayload struct {
	// Claude Code
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	SessionID string          `json:"session_id"`
	CWD       string          `json:"cwd"`
	// agy
	ToolCall *struct {
		Name string                     `json:"name"`
		Args map[string]json.RawMessage `json:"args"`
	} `json:"toolCall"`
	ConversationID string   `json:"conversationId"`
	WorkspacePaths []string `json:"workspacePaths"`
}

// parsePreToolRequest turns a hook payload into a tracking-gate request.
// format is the --format flag: "auto", "claude" or "gemini".
func parsePreToolRequest(raw []byte, format string, getenv func(string) string) (trackgate.Request, bool) {
	var p preToolPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return trackgate.Request{}, false
	}
	client := trackgate.ClientClaude
	if format == "gemini" || (format != "claude" && p.ToolCall != nil) {
		client = trackgate.ClientGemini
	}
	req := trackgate.Request{Client: client, TaskID: getenv("STAYPOINT_TASK_ID")}

	if client == trackgate.ClientGemini {
		if p.ToolCall == nil || p.ToolCall.Name == "" {
			return trackgate.Request{}, false
		}
		req.ToolName = p.ToolCall.Name
		req.SessionID = p.ConversationID
		args := map[string]string{}
		for k, v := range p.ToolCall.Args {
			var s string
			if json.Unmarshal(v, &s) == nil {
				args[k] = s
			}
		}
		req.Command = firstNonEmpty(args["CommandLine"], args["Command"])
		req.CWD = args["Cwd"]
		if req.CWD == "" && len(p.WorkspacePaths) > 0 {
			req.CWD = p.WorkspacePaths[0]
		}
		keys := make([]string, 0, len(args))
		for k := range args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			lk := strings.ToLower(k)
			if lk == "cwd" || lk == "commandline" || lk == "command" {
				continue
			}
			if strings.Contains(lk, "file") || strings.Contains(lk, "path") || strings.Contains(lk, "target") || strings.Contains(lk, "destination") {
				if v := args[k]; strings.HasPrefix(v, "/") || strings.HasPrefix(v, ".") || strings.HasPrefix(v, "~") {
					req.FilePaths = append(req.FilePaths, v)
				}
			}
		}
	} else {
		if p.ToolName == "" {
			return trackgate.Request{}, false
		}
		req.ToolName = p.ToolName
		req.SessionID = p.SessionID
		var in struct {
			Command      string `json:"command"`
			CWD          string `json:"cwd"`
			FilePath     string `json:"file_path"`
			NotebookPath string `json:"notebook_path"`
		}
		_ = json.Unmarshal(p.ToolInput, &in)
		req.Command = in.Command
		req.CWD = firstNonEmpty(p.CWD, in.CWD)
		for _, f := range []string{in.FilePath, in.NotebookPath} {
			if f != "" {
				req.FilePaths = append(req.FilePaths, f)
			}
		}
	}
	if req.CWD == "" {
		req.CWD, _ = os.Getwd()
	}
	return req, true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// productionTrackingGate wires the gate to the configured StayPoint DB and
// router.IsWorkRepo.
func productionTrackingGate() *trackgate.Gate {
	return &trackgate.Gate{
		OpenDB: func() (*sql.DB, func(), error) {
			if cfg == nil || cfg.DBPath == "" {
				return nil, nil, errors.New("StayPoint config not loaded")
			}
			if _, err := os.Stat(cfg.DBPath); err != nil {
				return nil, nil, err
			}
			store, err := db.Open(cfg.DBPath)
			if err != nil {
				return nil, nil, err
			}
			return store.DB(), func() { store.Close() }, nil
		},
		IsWorkRepo: func(p string) bool {
			ok, _, err := router.IsWorkRepo(p)
			return err == nil && ok
		},
	}
}

// preToolDeny renders a block decision in the calling client's format.
func preToolDeny(client trackgate.Client, reason string) string {
	if client == trackgate.ClientGemini {
		out, _ := json.Marshal(map[string]string{"decision": "deny", "reason": reason})
		return string(out)
	}
	return claudeBlockJSON(reason)
}

// claudeBlockJSON carries both the current hookSpecificOutput form and the
// legacy top-level decision so old and new Claude Code builds both deny.
func claudeBlockJSON(reason string) string {
	out, _ := json.Marshal(map[string]any{
		"decision": "block",
		"reason":   reason,
		"hookSpecificOutput": map[string]string{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       "deny",
			"permissionDecisionReason": reason,
		},
	})
	return string(out)
}

// runTrackingGate evaluates raw against gate. It returns the client and, when
// the call is blocked, the JSON to print.
func runTrackingGate(raw []byte, format string, getenv func(string) string, gate *trackgate.Gate) (trackgate.Client, string, bool) {
	req, ok := parsePreToolRequest(raw, format, getenv)
	if !ok {
		if format == "gemini" {
			return trackgate.ClientGemini, "", false
		}
		return trackgate.ClientClaude, "", false
	}
	d := gate.Evaluate(req)
	if !d.Block {
		return req.Client, "", false
	}
	fmt.Fprintln(os.Stderr, d.Reason)
	return req.Client, preToolDeny(req.Client, d.Reason), true
}
