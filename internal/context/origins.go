package context

import (
	"errors"
	"strings"
)

// Task origins (tasks.origin). See Task.Origin.
const (
	OriginNative          = "native"
	OriginPaperclipImport = "paperclip_import"
	OriginLegacy          = "legacy"
)

var (
	ErrInvalidOrigin = errors.New("invalid origin")
	ErrInvalidStage  = errors.New("invalid execution stage")
	ErrNoRepo        = errors.New("task has no repo")
	ErrInvalidRepo   = errors.New("invalid repo path")
)

// IsValidOrigin reports whether origin is one of the task origins.
func IsValidOrigin(origin string) bool {
	switch origin {
	case OriginNative, OriginPaperclipImport, OriginLegacy:
		return true
	}
	return false
}

// FilterLegacy drops legacy tasks unless includeLegacy is set. Every list
// surface (API, CLI, MCP, board) hides legacy tasks by default.
func FilterLegacy(tasks []Task, includeLegacy bool) []Task {
	if includeLegacy {
		return tasks
	}
	out := tasks[:0:0]
	for _, t := range tasks {
		if t.Origin != OriginLegacy {
			out = append(out, t)
		}
	}
	return out
}

// NormalizeTaskPriority maps a priority to low, medium, high or critical
// ("urgent" is critical); anything else is medium.
func NormalizeTaskPriority(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "critical", "urgent":
		return "critical"
	case "high":
		return "high"
	case "low":
		return "low"
	}
	return "medium"
}
