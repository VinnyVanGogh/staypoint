package context

import "errors"

// Task origins (tasks.origin). See Task.Origin.
const (
	OriginNative          = "native"
	OriginPaperclipImport = "paperclip_import"
	OriginLegacy          = "legacy"
)

var (
	ErrInvalidOrigin = errors.New("invalid origin")
	ErrInvalidStage  = errors.New("invalid execution stage")
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
