package chat

import (
	"database/sql"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/adapter"
	"github.com/VinnyVanGogh/staypoint/internal/conversation"
)

// Role defines who sent the message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleError     Role = "error"
)

// ChatMessage represents a single displayed turn in the chat TUI.
type ChatMessage struct {
	ID         string
	Role       Role
	Model      string
	Content    string
	Rendered   string
	Timestamp  time.Time
	TokenCount int
	IsError    bool
}

// Config holds initial parameters for launching the Chat TUI.
type Config struct {
	SessionID          string
	RepoPath           string
	Model              string
	Provider           string
	TaskID             string
	SocketPath         string
	InProcess          bool
	DBPath             string
	DB                 *sql.DB
	Store              conversation.Store
	CustomAdapter      adapter.ProviderAdapter
	CustomResolver     *adapter.Resolver
	CustomDaemonClient DaemonClientInterface
}

// DefaultModel is the chat model when none is specified. A chat session can
// write code, so it is Claude (router.GeminiCodeForbidden); Gemini only runs
// when picked explicitly (--model gemini-..., /model gemini-...).
const (
	DefaultModel    = "opus"
	DefaultProvider = "claude"
)

// ResolveProvider returns the provider for a model name.
func ResolveProvider(model string) string {
	m := model
	switch {
	case m == "codex" || m == "o1" || m == "o3" || m == "gpt-4o":
		return "codex"
	case m == "ollama":
		return "ollama"
	case m == "local" || m == "openai-compat":
		return "local"
	case containsAny(m, "claude", "sonnet", "opus", "haiku"):
		return "claude"
	case containsAny(m, "gemini", "flash", "pro", "agy"):
		return "gemini"
	default:
		// Unknown models are never routed to Gemini implicitly.
		return DefaultProvider
	}
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if len(s) >= len(sub) && (s == sub || stringContains(s, sub)) {
			return true
		}
	}
	return false
}

func stringContains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || (len(substr) > 0 && searchSubstr(s, substr)))
}

func searchSubstr(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
