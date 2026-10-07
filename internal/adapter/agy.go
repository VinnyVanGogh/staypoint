package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/VinnyVanGogh/staypoint/internal/router"
	"strconv"
	"strings"
)

// AgyAdapter drives the Antigravity CLI (`agy`) used for the Gemini pool.
// Verified against agy 1.2.x `--output-format stream-json`.
type AgyAdapter struct{}

func (AgyAdapter) Provider() string          { return "gemini" }
func (AgyAdapter) BinaryName() string        { return "agy" }
func (AgyAdapter) KnownMajorVersions() []int { return []int{1} }

func (AgyAdapter) BuildArgs(opts ParsedOptions) []string { return buildAgyArgs(opts) }

func (a AgyAdapter) Execute(ctx context.Context, req ExecRequest) error {
	return execute(ctx, a, req)
}

type agyUsage struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ThinkingTokens  int64 `json:"thinking_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`
}

func (u *agyUsage) normalize() *Usage {
	if u == nil {
		return nil
	}
	return &Usage{
		InputTokens:     u.InputTokens,
		OutputTokens:    u.OutputTokens,
		CacheReadTokens: u.CacheReadTokens,
		ThinkingTokens:  u.ThinkingTokens,
	}
}

type agyEvent struct {
	Event          string `json:"event"`
	ConversationID string `json:"conversation_id"`
	Init           struct {
		Model string `json:"model"`
	} `json:"init"`
	StepUpdate struct {
		ConversationID string    `json:"conversation_id"`
		StepIndex      int       `json:"step_index"`
		State          string    `json:"state"`
		StepType       string    `json:"step_type"`
		ToolName       string    `json:"tool_name"`
		Usage          *agyUsage `json:"usage"`
	} `json:"step_update"`
	Result struct {
		ConversationID string    `json:"conversation_id"`
		Status         string    `json:"status"`
		Response       string    `json:"response"`
		Error          string    `json:"error"`
		Usage          *agyUsage `json:"usage"`
	} `json:"result"`
}

// ParseStreamDelta normalizes one agy stream-json line. agy reports the final
// response text only in the result event; steps carry tool activity and usage.
func (AgyAdapter) ParseStreamDelta(line []byte) ([]StreamDelta, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, nil
	}
	var ev agyEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, fmt.Errorf("agy stream: %w", err)
	}

	switch ev.Event {
	case "init":
		return []StreamDelta{{Kind: DeltaInit, SessionID: ev.ConversationID, Model: ev.Init.Model}}, nil

	case "step_update":
		su := ev.StepUpdate
		d := StreamDelta{SessionID: su.ConversationID, Status: su.State}
		switch su.StepType {
		case "agent_response":
			if su.Usage == nil {
				d.Kind, d.Raw = DeltaOther, "step_update:agent_response"
				break
			}
			d.Kind, d.Usage = DeltaUsage, su.Usage.normalize()
		case "tool":
			d.ToolName, d.ToolID = su.ToolName, strconv.Itoa(su.StepIndex)
			if su.State == "DONE" {
				d.Kind = DeltaToolResult
			} else {
				d.Kind = DeltaToolUse
			}
		case "error_message":
			d.Kind, d.IsError = DeltaError, true
		default:
			d.Kind, d.Raw = DeltaOther, "step_update:"+su.StepType
		}
		return []StreamDelta{d}, nil

	case "result":
		r := ev.Result
		return []StreamDelta{{
			Kind:      DeltaResult,
			SessionID: r.ConversationID,
			Status:    r.Status,
			Text:      r.Response,
			Error:     r.Error,
			IsError:   r.Status != "SUCCESS",
			Usage:     r.Usage.normalize(),
		}}, nil
	}

	return []StreamDelta{{Kind: DeltaOther, Raw: ev.Event}}, nil
}

func buildAgyArgs(opts ParsedOptions) []string {
	args := []string{
		"--output-format", opts.OutputFormat,
		"--dangerously-skip-permissions",
	}
	if opts.Prompt != "" {
		args = append(args, "--prompt", opts.Prompt)
	}

	model := opts.Model
	effort := opts.Effort

	// Translate Claude models to Gemini equivalents via FallbackPairingMatrix.
	// Opus -> Gemini 3.1 Pro (high effort), Sonnet -> Gemini 3.8 Flash (medium effort).
	isClaudeModel := strings.Contains(model, "claude") || strings.Contains(model, "sonnet") || strings.Contains(model, "opus")
	if isClaudeModel || model == "" {
		geminiModel, geminiEffort, _ := router.FallbackPairingMatrix(model, effort)
		model = geminiModel
		effort = geminiEffort
	}

	if model != "" {
		args = append(args, "--model", model)
	}
	if effort != "" && model != "" {
		args = append(args, "--effort", effort)
	}
	// Do NOT pass ConversationID for cross-provider fallback (Claude session IDs are invalid for agy).
	// When running native Gemini sessions, pass --conversation.
	if opts.ConversationID != "" && !isClaudeModel {
		args = append(args, "--conversation", opts.ConversationID)
	}
	for _, dir := range opts.AddDirs {
		args = append(args, "--add-dir", dir)
	}
	return args
}

// ParseChainStreamDelta parses one line from a failover-chain run (gemini >
// claude …), where only the output shows which provider answered: agy lines
// carry a top-level "event" key, Claude lines a "type" key. Parsing a Gemini
// run as Claude drops every event, leaving an empty timeline (STA-775).
func ParseChainStreamDelta(line []byte) ([]StreamDelta, error) {
	var probe struct {
		Event string `json:"event"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &probe) == nil && probe.Event != "" {
		return AgyAdapter{}.ParseStreamDelta(line)
	}
	return ClaudeAdapter{}.ParseStreamDelta(line)
}
