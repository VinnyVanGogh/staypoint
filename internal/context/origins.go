package context

import (
	"errors"
	"regexp"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/governance"
)

// Task origins (tasks.origin). See Task.Origin.
const (
	OriginNative          = "native"
	OriginPaperclipImport = "paperclip_import"
	OriginLegacy          = "legacy"
	// OriginAgent marks a task an agent created (MCP, the agent token on the
	// API, or the CLI inside an agent session). It is created in backlog and
	// only the Board can move it to a runnable stage.
	OriginAgent = "agent"
)

var (
	ErrInvalidOrigin = errors.New("invalid origin")
	ErrInvalidStage  = errors.New("invalid execution stage")
	ErrNoRepo        = errors.New("task has no repo")
	ErrInvalidRepo   = errors.New("invalid repo path")
)

// ErrBoardRequired is returned when a stage change needs the Board.
var ErrBoardRequired = errors.New("board required")

// RequiresBoardToLeave reports whether moving t to stage needs the Board: t
// was created by an agent, sits in a parked stage (backlog, cancelled,
// stopped, ...) and stage is runnable. Moving it to done or cancelled, or
// between runnable stages, does not.
func RequiresBoardToLeave(t *Task, stage string) bool {
	if t == nil || t.Origin != OriginAgent {
		return false
	}
	return !governance.IsRunnableStage(t.ExecutionStage) && governance.IsRunnableStage(stage)
}

// prodWord matches "prod" / "production" as a word.
var prodWord = regexp.MustCompile(`(?i)\bprod(uction)?\b`)

// NameTargetsProd reports whether a task's name says it targets production
// (e.g. "port X to prod"). Board surfaces add the repo's live_credentials
// flag; either one makes leaving backlog a Touch ID action.
func NameTargetsProd(name string) bool {
	return prodWord.MatchString(name)
}

// IsValidOrigin reports whether origin is one of the task origins.
func IsValidOrigin(origin string) bool {
	switch origin {
	case OriginNative, OriginPaperclipImport, OriginLegacy, OriginAgent:
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

// HiddenByDefaultSQL is the SQL form of IsHiddenByDefault for a tasks row
// aliased as alias ("" for an unaliased tasks table). SQL aggregators (fleet
// overview, spend totals) use VisibleTasksSQL so they hide the same rows the
// slice filters do.
func HiddenByDefaultSQL(alias string) string {
	p := ""
	if alias != "" {
		p = alias + "."
	}
	return "(COALESCE(" + p + "origin,'native') = '" + OriginLegacy + "' OR (COALESCE(" + p + "origin,'native') = '" +
		OriginPaperclipImport + "' AND COALESCE(" + p + "execution_stage,'') IN ('done','cancelled')))"
}

// VisibleTasksSQL is a WHERE fragment keeping the rows list surfaces show by
// default; with includeHidden it is the always-true "1=1".
func VisibleTasksSQL(alias string, includeHidden bool) string {
	if includeHidden {
		return "1=1"
	}
	return "NOT " + HiddenByDefaultSQL(alias)
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
