package governance

import (
	"database/sql"
	"fmt"
	"strings"
)

// Execution stages (tasks.execution_stage). The set mirrors Paperclip's issue
// statuses (backlog, todo, in_progress, in_review, blocked, done, cancelled);
// "running" is in_progress: Claim sets it when a run checks the task out.
// paused, capped and stopped are StayPoint-only run sub-states of
// in_progress written by the harness, and rejected is the governance
// review outcome. See docs/task-stages.md for the full table.
const (
	StageBacklog    = "backlog"
	StageTodo       = "todo"
	StageInProgress = "in_progress"
	StageInReview   = "in_review"
	StageDone       = "done"
	StageBlocked    = "blocked"
	StageCancelled  = "cancelled"
	StageRejected   = "rejected"

	// Run sub-states (harness-owned, never set from the API).
	StagePaused  = "paused"
	StageCapped  = "capped"
	StageStopped = "stopped"
)

// validTransitions defines which transitions are structurally allowed.
// backlog is parked: nothing claims or wakes it, and the only way to run it
// is through todo (Run Now does backlog -> todo -> in_progress).
var validTransitions = map[string][]string{
	StageBacklog:    {StageTodo, StageCancelled},
	StageTodo:       {StageInProgress, StageBlocked, StageBacklog, StageCancelled},
	StageInProgress: {StageInReview, StageBlocked, StageTodo, StageCancelled},
	StageInReview:   {StageDone, StageInProgress, StageBlocked, StageRejected, StageCancelled},
	StageBlocked:    {StageTodo, StageInProgress, StageInReview, StageBacklog, StageCancelled},
	StageCancelled:  {StageBacklog, StageTodo},
	StageDone:       {},
	StageRejected:   {},
}

// BoardSettableStages are the stages POST /api/tasks/{id}/stage, the CLI and
// the board accept. blocked is set through the block endpoint (it needs a
// reason); the run sub-states belong to the harness.
var BoardSettableStages = []string{StageBacklog, StageTodo, StageInProgress, StageInReview, StageDone, StageCancelled}

// IsBoardSettableStage reports whether stage is in BoardSettableStages.
func IsBoardSettableStage(stage string) bool {
	for _, s := range BoardSettableStages {
		if s == stage {
			return true
		}
	}
	return false
}

// nonRunnableStages are never claimed by a run or woken by the dispatcher.
var nonRunnableStages = []string{StageBacklog, StageDone, StageCancelled, StageRejected}

// IsRunnableStage reports whether a task in stage may be claimed by a run.
func IsRunnableStage(stage string) bool {
	for _, s := range nonRunnableStages {
		if s == stage {
			return false
		}
	}
	return true
}

// NonRunnableStagesSQL is nonRunnableStages as a quoted SQL list, for
// "execution_stage NOT IN (...)" guards.
func NonRunnableStagesSQL() string {
	q := make([]string, len(nonRunnableStages))
	for i, s := range nonRunnableStages {
		q[i] = "'" + s + "'"
	}
	return strings.Join(q, ", ")
}

// TransitionError is returned when a gate blocks a transition.
type TransitionError struct {
	From   string
	To     string
	Reason string
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("governance: cannot transition %s→%s: %s", e.From, e.To, e.Reason)
}

// IsValidTransition returns true if the structural edge exists.
func IsValidTransition(from, to string) bool {
	targets, ok := validTransitions[from]
	if !ok {
		return false
	}
	for _, t := range targets {
		if t == to {
			return true
		}
	}
	return false
}

// CheckGates validates governance gates for a transition and returns an error if blocked.
// actorID is the agent or user requesting the transition.
func CheckGates(db *sql.DB, taskID, from, to, actorID string) error {
	if !IsValidTransition(from, to) {
		return &TransitionError{From: from, To: to, Reason: "transition not in allowed set"}
	}

	// Gate: in_review → done requires review approval + sufficient approval votes.
	if from == StageInReview && to == StageDone {
		if err := checkReviewGate(db, taskID); err != nil {
			return err
		}
		if err := checkApprovalGate(db, taskID); err != nil {
			return err
		}
	}

	// Gate: in_progress → in_review requires at least one reviewer or approver assigned
	// when governance is configured (soft gate — just a best-effort hint, not a hard block).
	return nil
}

// ExecuteTransition applies the state change, enforces gates, and writes the audit log.
func ExecuteTransition(db *sql.DB, taskID, from, to, actorID string) error {
	if err := CheckGates(db, taskID, from, to, actorID); err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}

	// cancelled closes the task (status soft_deleted, as DeleteTask does);
	// leaving cancelled reopens it.
	query := `UPDATE tasks SET execution_stage = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`
	switch {
	case to == StageCancelled:
		query = `UPDATE tasks SET execution_stage = ?, status = 'soft_deleted', deleted_at = COALESCE(deleted_at, strftime('%Y-%m-%dT%H:%M:%fZ', 'now')), updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`
	case from == StageCancelled:
		query = `UPDATE tasks SET execution_stage = ?, status = 'active', deleted_at = NULL, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`
	}
	res, err := tx.Exec(query, to, taskID)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("governance: update execution_stage: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		tx.Rollback()
		return fmt.Errorf("governance: task not found: %s", taskID)
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	fromCopy, toCopy := from, to
	_ = LogEvent(db, taskID, actorID, AuditStateTransition, &fromCopy, &toCopy, nil)
	shadowVote(taskID, from, to)
	return nil
}

// checkReviewGate ensures at least one reviewer has approved when require_review is set.
func checkReviewGate(db *sql.DB, taskID string) error {
	var requireReview int
	err := db.QueryRow(`SELECT require_review FROM task_governance WHERE task_id = ?`, taskID).Scan(&requireReview)
	if err == sql.ErrNoRows || requireReview == 0 {
		return nil
	}
	if err != nil {
		return fmt.Errorf("governance: read governance config: %w", err)
	}

	// Only decisions from currently assigned reviewers count.
	var approved int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM task_review_decisions d
		 JOIN task_reviewers r ON r.task_id = d.task_id AND r.reviewer_id = d.reviewer_id
		 WHERE d.task_id = ? AND d.decision = 'approved'`,
		taskID,
	).Scan(&approved); err != nil {
		return fmt.Errorf("governance: count review approvals: %w", err)
	}

	if approved == 0 {
		return &TransitionError{From: StageInReview, To: StageDone, Reason: "require_review=true but no reviewer has approved"}
	}
	return nil
}

// checkApprovalGate ensures enough approval votes have been cast.
func checkApprovalGate(db *sql.DB, taskID string) error {
	var threshold int
	err := db.QueryRow(`SELECT approval_threshold FROM task_governance WHERE task_id = ?`, taskID).Scan(&threshold)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("governance: read approval threshold: %w", err)
	}
	if threshold <= 0 {
		return nil
	}

	// Only votes from currently assigned approvers count.
	var approvedCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM task_approval_votes v
		 JOIN task_approvers a ON a.task_id = v.task_id AND a.approver_id = v.approver_id
		 WHERE v.task_id = ? AND v.vote = 'approved'`,
		taskID,
	).Scan(&approvedCount); err != nil {
		return fmt.Errorf("governance: count approval votes: %w", err)
	}

	if approvedCount < threshold {
		return &TransitionError{
			From:   StageInReview,
			To:     StageDone,
			Reason: fmt.Sprintf("requires %d approval votes, only %d received", threshold, approvedCount),
		}
	}
	return nil
}
