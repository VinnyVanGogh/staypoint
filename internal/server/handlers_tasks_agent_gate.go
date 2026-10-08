package server

import (
	"net/http"

	"github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

// Surprise starts (2026-10-07): tasks an agent creates through the API land in
// backlog with origin "agent", and only the Board can move them to a runnable
// stage. A request is the Board's when it carries the Board session cookie;
// the agent token alone is an agent. Prod-targeting tasks also need the
// passkey (Touch ID) to leave backlog.

// SetBoardSession wires the check that recognises a Board session. Without
// one, every request is treated as an agent request (fail closed).
func (h *TasksHandler) SetBoardSession(fn func(*http.Request) bool) {
	h.boardSession = fn
}

// isBoardRequest reports whether r carries a Board session.
func (h *TasksHandler) isBoardRequest(r *http.Request) bool {
	return h.boardSession != nil && h.boardSession(r)
}

// createOrigin is the origin of a task created by r: agent unless the Board.
func (h *TasksHandler) createOrigin(r *http.Request) string {
	if h.isBoardRequest(r) {
		return context.OriginNative
	}
	return context.OriginAgent
}

// taskTargetsProd reports whether task targets production: its name says so,
// or its repo is a live_credentials project. A config lookup error counts as
// prod (fail closed).
func (h *TasksHandler) taskTargetsProd(task *context.Task) bool {
	if context.NameTargetsProd(task.Name) {
		return true
	}
	if task.RepoPath == "" {
		return false
	}
	_, gated, err := shipreview.LiveGateConfig(h.db, task.RepoPath)
	return err != nil || gated
}

// requireBoardToLeave gates moving task to stage. It returns true when the
// move may go ahead; otherwise it has written the 403 and the caller stops.
// Only agent-created tasks leaving a parked stage for a runnable one are
// gated: by the Board session, plus the passkey when the task targets prod.
func (h *TasksHandler) requireBoardToLeave(w http.ResponseWriter, r *http.Request, task *context.Task, stage string) bool {
	if !context.RequiresBoardToLeave(task, stage) {
		return true
	}
	if h.taskTargetsProd(task) {
		return h.requireBoardForOverride(w, r, "moving a prod-targeting agent task out of "+task.ExecutionStage)
	}
	if h.isBoardRequest(r) {
		return true
	}
	writeBoardError(w, "board_session_required", "forbidden: "+task.ID+" was created by an agent; only the Board can move it out of "+task.ExecutionStage)
	return false
}
