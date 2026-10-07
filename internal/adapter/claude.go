package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// ClaudeAdapter drives the Claude Code CLI in print mode
// (`claude --print --output-format stream-json --verbose`). Verified against 2.1.x.
type ClaudeAdapter struct{}

func (ClaudeAdapter) Provider() string          { return "claude" }
func (ClaudeAdapter) BinaryName() string        { return "claude" }
func (ClaudeAdapter) KnownMajorVersions() []int { return []int{2} }

func (ClaudeAdapter) BuildArgs(opts ParsedOptions) []string { return buildClaudeArgs(opts) }

// Execute overrides the generic helper to inject a per-run --settings file that
// registers staypoint hook pre-tool as a PreToolUse hook (STA-525).
// The hook path comes from STAYPOINT_HOOK_BIN in extraEnv (injected by the harness)
// or from the OS environment as a fallback. If no binary is found the invocation
// proceeds without the extra settings file (fail-open).
// Gemini/agy adapter parity is intentionally deferred — that adapter does not
// support a --settings equivalent; a separate issue tracks it (STA-525 out-of-scope).
func (a ClaudeAdapter) Execute(ctx context.Context, req ExecRequest) error {
	args := a.BuildArgs(req.Opts)
	if settingsPath := writePreToolHookSettings(req.ExtraEnv); settingsPath != "" {
		defer os.Remove(settingsPath)
		args = append([]string{"--settings", settingsPath}, args...)
	}
	return runCommandWithEnv(ctx, req.Dir, req.Bin, args, req.ExtraEnv, req.Stdin, req.Stdout, req.Stderr)
}

// writePreToolHookSettings writes a per-run temp settings JSON that registers
// staypoint hook pre-tool as a Claude Code PreToolUse hook, then returns its
// path.  Returns "" (fail-open) if no hook binary can be found or the file
// cannot be written.
func writePreToolHookSettings(extraEnv []string) string {
	hookBin := extraEnvValue(extraEnv, "STAYPOINT_HOOK_BIN")
	if hookBin == "" {
		hookBin = os.Getenv("STAYPOINT_HOOK_BIN")
	}
	if hookBin == "" {
		return ""
	}

	type hookEntry struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type hookGroup struct {
		Hooks []hookEntry `json:"hooks"`
	}
	settings := map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []hookGroup{
				{Hooks: []hookEntry{{Type: "command", Command: hookBin + " hook pre-tool"}}},
			},
		},
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return ""
	}
	f, err := os.CreateTemp("", "staypoint-claude-settings-*.json")
	if err != nil {
		return ""
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		os.Remove(f.Name())
		return ""
	}
	return f.Name()
}

// extraEnvValue returns the value for key in an env slice ("KEY=val" format).
// Returns "" if not found.
func extraEnvValue(env []string, key string) string {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return e[len(prefix):]
		}
	}
	return ""
}

type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	OutputTokensDetails      struct {
		ThinkingTokens int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

type claudeContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	// Content holds tool_result output: either a JSON string or array of text blocks.
	Content json.RawMessage `json:"content"`
}

type claudeEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
	Message   struct {
		Model string `json:"model"`
		// Content is an array of blocks, or a plain string for replayed user prompts.
		Content json.RawMessage `json:"content"`
		// Usage is populated on assistant events; Claude CLI puts usage here, not at top level.
		Usage *claudeUsage `json:"usage"`
	} `json:"message"`
	IsError bool         `json:"is_error"`
	Result  string       `json:"result"`
	Usage   *claudeUsage `json:"usage"`
}

// ParseStreamDelta normalizes one Claude stream-json line.
func (ClaudeAdapter) ParseStreamDelta(line []byte) ([]StreamDelta, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, nil
	}
	var ev claudeEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("claude stream: %w", err)
	}

	switch ev.Type {
	case "system":
		if ev.Subtype == "init" {
			return []StreamDelta{{Kind: DeltaInit, SessionID: ev.SessionID, Model: ev.Model}}, nil
		}
		return []StreamDelta{{Kind: DeltaOther, SessionID: ev.SessionID, Raw: "system:" + ev.Subtype}}, nil

	case "assistant", "user":
		var blocks []claudeContent
		if len(ev.Message.Content) == 0 || json.Unmarshal(ev.Message.Content, &blocks) != nil {
			return []StreamDelta{{Kind: DeltaOther, SessionID: ev.SessionID, Raw: ev.Type}}, nil
		}
		var out []StreamDelta
		for _, b := range blocks {
			d := StreamDelta{SessionID: ev.SessionID, Model: ev.Message.Model}
			switch b.Type {
			case "text":
				d.Kind, d.Text = DeltaText, b.Text
				d.FromUser = ev.Type == "user"
			case "thinking":
				d.Kind, d.Text = DeltaThinking, b.Thinking
			case "tool_use":
				d.Kind, d.ToolName, d.ToolID = DeltaToolUse, b.Name, b.ID
				if len(b.Input) > 0 {
					d.ToolInput = string(b.Input)
				}
			case "tool_result":
				d.Kind, d.ToolID, d.IsError = DeltaToolResult, b.ToolUseID, b.IsError
				d.Text = extractToolResultText(b.Content)
			default:
				d.Kind, d.Raw = DeltaOther, ev.Type+":"+b.Type
			}
			out = append(out, d)
		}
		// Emit usage delta from assistant events so stats bar updates during the turn.
		// Claude CLI puts usage inside message.usage (not at the event top level).
		if ev.Type == "assistant" {
			usage := ev.Usage
			if usage == nil {
				usage = ev.Message.Usage
			}
			if usage != nil {
				model := ev.Message.Model
				if model == "" {
					model = ev.Model
				}
				out = append(out, StreamDelta{
					Kind:      DeltaUsage,
					SessionID: ev.SessionID,
					Model:     model,
					Usage: &Usage{
						InputTokens:         usage.InputTokens,
						OutputTokens:        usage.OutputTokens,
						CacheReadTokens:     usage.CacheReadInputTokens,
						CacheCreationTokens: usage.CacheCreationInputTokens,
						ThinkingTokens:      usage.OutputTokensDetails.ThinkingTokens,
					},
				})
			}
		}
		return out, nil

	case "result":
		d := StreamDelta{
			Kind:      DeltaResult,
			SessionID: ev.SessionID,
			Status:    ev.Subtype,
			Text:      ev.Result,
			IsError:   ev.IsError,
		}
		var out []StreamDelta
		if ev.Usage != nil {
			u := &Usage{
				InputTokens:         ev.Usage.InputTokens,
				OutputTokens:        ev.Usage.OutputTokens,
				CacheReadTokens:     ev.Usage.CacheReadInputTokens,
				CacheCreationTokens: ev.Usage.CacheCreationInputTokens,
				ThinkingTokens:      ev.Usage.OutputTokensDetails.ThinkingTokens,
			}
			d.Usage = u
			// Emit a separate DeltaUsage so the stats bar gets final token counts.
			out = append(out, StreamDelta{
				Kind:      DeltaUsage,
				SessionID: ev.SessionID,
				Model:     ev.Model,
				Usage:     u,
			})
		}
		out = append(out, d)
		return out, nil
	}

	return []StreamDelta{{Kind: DeltaOther, SessionID: ev.SessionID, Raw: ev.Type}}, nil
}

// extractToolResultText extracts the plain text from a tool_result content field,
// which may be a JSON string or an array of {"type":"text","text":"..."} blocks.
func extractToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// Try array of content blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

func buildClaudeArgs(opts ParsedOptions) []string {
	args := []string{
		"--print",
		"--output-format", opts.OutputFormat,
		"--verbose",
		"--dangerously-skip-permissions",
	}
	if strings.Contains(opts.Model, "claude") || strings.Contains(opts.Model, "sonnet") || strings.Contains(opts.Model, "opus") {
		args = append(args, "--model", opts.Model)
	}
	if opts.ConversationID != "" {
		args = append(args, "--resume", opts.ConversationID)
	}
	for _, dir := range opts.AddDirs {
		args = append(args, "--add-dir", dir)
	}
	if opts.Prompt != "" {
		args = append(args, opts.Prompt)
	}
	return args
}
