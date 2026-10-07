package adapter

import (
	"context"
	"io"
	"strings"
)

// ProviderAdapter isolates everything provider-specific about driving a vendor CLI:
// argument construction, execution, and stream parsing. Upstream flag or stream
// schema drift (risk K-04) should only ever require changes behind this interface.
type ProviderAdapter interface {
	// Provider is the stable provider key used by routing ("claude", "gemini").
	Provider() string
	// BinaryName is the executable name resolved on PATH.
	BinaryName() string
	// BuildArgs converts normalized options into CLI arguments.
	BuildArgs(opts ParsedOptions) []string
	// Execute runs the CLI with keepalive streaming and returns when it exits.
	Execute(ctx context.Context, req ExecRequest) error
	// ParseStreamDelta normalizes one line of the CLI's stream-json output.
	// Blank keepalive lines yield no deltas and no error.
	ParseStreamDelta(line []byte) ([]StreamDelta, error)
	// KnownMajorVersions lists CLI major versions whose flags and stream schema
	// have been verified against golden fixtures.
	KnownMajorVersions() []int
}

// ExecRequest carries a single CLI invocation.
type ExecRequest struct {
	// Bin is the resolved executable path.
	Bin string
	// Dir is the working directory; empty inherits the current process directory.
	Dir      string
	Opts     ParsedOptions
	ExtraEnv []string
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
}

// DeltaKind classifies a normalized stream event.
type DeltaKind string

const (
	DeltaInit       DeltaKind = "init"
	DeltaText       DeltaKind = "text"
	DeltaThinking   DeltaKind = "thinking"
	DeltaToolUse    DeltaKind = "tool_use"
	DeltaToolResult DeltaKind = "tool_result"
	DeltaUsage      DeltaKind = "usage"
	DeltaResult     DeltaKind = "result"
	DeltaError      DeltaKind = "error"
	DeltaOther      DeltaKind = "other"
)

// StreamDelta is the provider-neutral view of one stream event.
type StreamDelta struct {
	Kind      DeltaKind `json:"kind"`
	SessionID string    `json:"session_id,omitempty"`
	Model     string    `json:"model,omitempty"`
	Text      string    `json:"text,omitempty"`
	ToolName  string    `json:"tool_name,omitempty"`
	ToolID    string    `json:"tool_id,omitempty"`
	// ToolInput is the raw JSON input object for tool_use deltas.
	ToolInput string `json:"tool_input,omitempty"`
	Status    string `json:"status,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	Error     string `json:"error,omitempty"`
	Usage     *Usage `json:"usage,omitempty"`
	// Raw is the provider event type, kept for DeltaOther and debugging.
	Raw string `json:"raw,omitempty"`
	// FromUser marks a DeltaText that came from an input (user) event rather
	// than the agent's own output, e.g. a Claude "user" message echoing the
	// prompt. Completion detection ignores it.
	FromUser bool `json:"from_user,omitempty"`
}

// Usage is normalized token accounting.
type Usage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	ThinkingTokens      int64 `json:"thinking_tokens"`
}

// AdapterFor returns the adapter for a provider key. Unknown providers route to
// agy, matching the historical default of the adapter command.
func AdapterFor(provider string) ProviderAdapter {
	switch provider {
	case "claude":
		return ClaudeAdapter{}
	case "cloud_session":
		return CloudSessionAdapter{}
	case "codex":
		return CodexAdapter{}
	case "ollama":
		return OllamaAdapter{}
	case "local", "openai-compat":
		return LocalOpenAIAdapter{}
	default:
		return AgyAdapter{}
	}
}

func adapterFor(provider string) ProviderAdapter {
	return AdapterFor(provider)
}

// Adapters returns every built-in provider adapter.
func Adapters() []ProviderAdapter {
	return []ProviderAdapter{
		ClaudeAdapter{},
		CloudSessionAdapter{},
		AgyAdapter{},
		CodexAdapter{},
		OllamaAdapter{},
		LocalOpenAIAdapter{},
	}
}

func isClaudeModel(model string) bool {
	return strings.Contains(model, "claude") || strings.Contains(model, "sonnet") || strings.Contains(model, "opus")
}

func execute(ctx context.Context, a ProviderAdapter, req ExecRequest) error {
	return runCommandWithEnv(ctx, req.Dir, req.Bin, a.BuildArgs(req.Opts), req.ExtraEnv, req.Stdin, req.Stdout, req.Stderr)
}
