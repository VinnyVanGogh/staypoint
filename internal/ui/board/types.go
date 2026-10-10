package board

import (
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/context"
)

// ColumnType identifies the Kanban columns.
type ColumnType string

const (
	ColBacklog    ColumnType = "backlog"
	ColTodo       ColumnType = "todo"
	ColInProgress ColumnType = "in_progress"
	ColInReview   ColumnType = "in_review"
	ColDone       ColumnType = "done"
)

// AllColumns defines the fixed order of columns in the Kanban board.
var AllColumns = [...]ColumnType{ColBacklog, ColTodo, ColInProgress, ColInReview, ColDone}

// ColumnTitle returns a human-friendly uppercase label for a column.
func ColumnTitle(col ColumnType) string {
	switch col {
	case ColBacklog:
		return "BACKLOG"
	case ColTodo:
		return "TODO"
	case ColInProgress:
		return "IN PROGRESS"
	case ColInReview:
		return "IN REVIEW"
	case ColDone:
		return "DONE"
	default:
		return strings.ToUpper(string(col))
	}
}

// MapTaskToColumn maps a task's execution_stage and status to one of the 4 Kanban columns.
func MapTaskToColumn(t context.Task) ColumnType {
	if strings.EqualFold(t.Status, "done") || strings.EqualFold(t.ExecutionStage, "done") {
		return ColDone
	}
	switch strings.ToLower(t.ExecutionStage) {
	case "in_review":
		return ColInReview
	case "in_progress":
		return ColInProgress
	case "todo":
		return ColTodo
	case "backlog":
		return ColBacklog
	default:
		// Default unrecognized active stages (e.g. capped, soft_deleted check)
		if strings.EqualFold(t.Status, "active") {
			return ColTodo
		}
		return ColTodo
	}
}

// ViewMode defines the active UI view.
type ViewMode int

const (
	ViewBoard ViewMode = iota
	ViewThread
	ViewCommentInput
	ViewMove
	ViewHelp
)

// Internal message types for Bubble Tea

type tasksLoadedMsg struct {
	tasks []context.Task
	err   error
}

type threadLoadedMsg struct {
	taskID       string
	comments     []context.TaskComment
	workProducts []context.TaskWorkProduct
	activity     []context.ActivityLog
	err          error
}

type DaemonEvent struct {
	ID        int64          `json:"id"`
	Type      string         `json:"type"`
	Data      map[string]any `json:"data"`
	RawData   string         `json:"raw_data,omitempty"`
}

type daemonEventMsg struct {
	event DaemonEvent
}

type daemonStatusMsg struct {
	connected bool
	err       error
}

type commentAddedMsg struct {
	taskID string
	err    error
}

type stageChangedMsg struct {
	taskID string
	stage  string
	err    error
}

type blockToggledMsg struct {
	taskID  string
	blocked bool
	err     error
}

type statusMessageMsg struct {
	message string
	isError bool
}
