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

// IsArchived reports whether t is a finished imported task: origin
// paperclip_import and stage done or cancelled (the "Paperclip archive"
// parents and their children).
func IsArchived(t Task) bool {
	return t.Origin == OriginPaperclipImport &&
		(t.ExecutionStage == "done" || t.ExecutionStage == "cancelled")
}

// IsHiddenByDefault reports whether list surfaces hide t unless asked:
// legacy tasks and archived imports.
func IsHiddenByDefault(t Task) bool {
	return t.Origin == OriginLegacy || IsArchived(t)
}

// FilterLegacy drops legacy tasks and archived imports unless includeHidden
// is set. Every list surface (API, CLI, MCP, board) hides them by default
// behind one "archive & legacy" switch.
func FilterLegacy(tasks []Task, includeHidden bool) []Task {
	if includeHidden {
		return tasks
	}
	out := tasks[:0:0]
	for _, t := range tasks {
		if !IsHiddenByDefault(t) {
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
