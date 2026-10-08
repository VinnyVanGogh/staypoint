package context

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// STA-820: a planning/architecture task can spawn follow-up child tasks, each
// with its own work_kind so routing picks the model, and the parent's plan is
// handed off through the daemon's DB rather than a file in the parent's
// worktree that an agent could edit.

var (
	ErrInvalidWorkKind = errors.New("invalid work_kind")
	ErrParentNotFound  = errors.New("parent task not found")
	ErrChildLimit      = errors.New("parent already has the maximum number of child tasks")
	ErrDepthLimit      = errors.New("child task would exceed the maximum nesting depth")
	ErrOpenChildren    = errors.New("task has open child tasks")
	ErrNoWorkProduct   = errors.New("cannot mark task as done without a registered work product")
)

// HandoffDocKey is the task_documents key holding a child's handoff context.
const HandoffDocKey = "handoff"

// Settings keys (settings_kv) for the child-task guardrails. The Board raises
// them; the defaults apply when unset or unparsable.
const (
	SettingMaxChildren   = "tasks.max_children"
	SettingMaxChildDepth = "tasks.max_child_depth"

	DefaultMaxChildren   = 10
	DefaultMaxChildDepth = 2
)

// validWorkKinds mirrors router.ValidWorkKinds (internal/router/kinds.go).
// The router package imports this one, so the list cannot be imported here;
// TestWorkKindsMatchRouter in internal/mcp keeps the two in step.
// "review" (Claude Opus first) lands in the router with STA-772 / PR #213.
var validWorkKinds = []string{"coding", "review", "architecture", "planning", "qa", "docs"}

// ValidWorkKinds returns the accepted work_kind values.
func ValidWorkKinds() []string {
	return append([]string(nil), validWorkKinds...)
}

// IsValidWorkKind reports whether kind is an exact routing work kind.
func IsValidWorkKind(kind string) bool {
	for _, k := range validWorkKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// ChildTaskOptions configures CreateChildTask. Repo, branch, account role,
// organization and project are always inherited from the parent.
type ChildTaskOptions struct {
	ParentID     string
	Name         string
	WorkKind     string
	Handoff      string // plan text; falls back to the parent's latest "plan" document
	Description  string
	MaxBudgetUSD float64
	MaxTurns     int
	// BoardOverride lifts the depth cap. Only Board-facing surfaces set it.
	BoardOverride bool
	// ExecutionStage is the requested initial stage ("" = todo). It is
	// honoured: a backlog child is created parked and never woken.
	ExecutionStage string
	// Origin defaults to OriginNative. Agent surfaces set OriginAgent, which
	// always creates the child in backlog.
	Origin string
}

// ChildTaskLimits returns (maxChildren, maxDepth) from settings_kv, falling
// back to the defaults.
func ChildTaskLimits(db *sql.DB) (int, int) {
	return intSetting(db, SettingMaxChildren, DefaultMaxChildren),
		intSetting(db, SettingMaxChildDepth, DefaultMaxChildDepth)
}

// SetChildTaskLimits persists the child-task guardrails.
func SetChildTaskLimits(db *sql.DB, maxChildren, maxDepth int) error {
	for key, v := range map[string]int{SettingMaxChildren: maxChildren, SettingMaxChildDepth: maxDepth} {
		if _, err := db.Exec(
			`INSERT INTO settings_kv (key, value, updated_at) VALUES (?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			key, strconv.Itoa(v)); err != nil {
			return fmt.Errorf("set %s: %w", key, err)
		}
	}
	return nil
}

func intSetting(db *sql.DB, key string, def int) int {
	var raw string
	if err := db.QueryRow(`SELECT value FROM settings_kv WHERE key = ?`, key).Scan(&raw); err != nil {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		return def
	}
	return n
}

// taskDepth is the number of ancestors above taskID (a root task is 0).
func taskDepth(db *sql.DB, taskID string) (int, error) {
	depth := 0
	seen := map[string]bool{taskID: true}
	cur := taskID
	for {
		var parent sql.NullString
		if err := db.QueryRow(`SELECT parent_id FROM tasks WHERE id = ?`, cur).Scan(&parent); err != nil {
			return 0, err
		}
		if !parent.Valid || parent.String == "" {
			return depth, nil
		}
		if seen[parent.String] {
			return 0, fmt.Errorf("parent cycle at %s", parent.String)
		}
		seen[parent.String] = true
		depth++
		cur = parent.String
	}
}

// openChildrenWhere selects children that still block the parent from done.
const openChildrenWhere = `parent_id = ? AND status NOT IN ('done', 'soft_deleted') AND execution_stage NOT IN ('done', 'cancelled')`

// CountOpenChildren returns the number of open child tasks of taskID.
func CountOpenChildren(db *sql.DB, taskID string) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE `+openChildrenWhere, taskID).Scan(&n)
	return n, err
}

// CreateChildTask creates a child of opts.ParentID (todo unless
// opts.ExecutionStage or an agent origin parks it in backlog) that inherits the
// parent's repo, branch, role, org and project, and stores the handoff
// (parent link, plan, parent's latest final message) as the child's
// "handoff" document.
func CreateChildTask(db *sql.DB, opts ChildTaskOptions) (*Task, error) {
	if !IsValidWorkKind(opts.WorkKind) {
		return nil, fmt.Errorf("%w %q: must be one of %s", ErrInvalidWorkKind, opts.WorkKind, strings.Join(validWorkKinds, ", "))
	}
	if strings.TrimSpace(opts.Name) == "" {
		return nil, fmt.Errorf("task name cannot be empty")
	}
	parent, err := GetTask(db, strings.TrimSpace(opts.ParentID))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrParentNotFound, opts.ParentID)
	}
	if parent.Status == "soft_deleted" {
		return nil, fmt.Errorf("%w: %s is deleted", ErrParentNotFound, parent.ID)
	}

	maxChildren, maxDepth := ChildTaskLimits(db)
	var existing int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE parent_id = ? AND status != 'soft_deleted'`, parent.ID).Scan(&existing); err != nil {
		return nil, fmt.Errorf("count children: %w", err)
	}
	if existing >= maxChildren {
		return nil, fmt.Errorf("%w (%d)", ErrChildLimit, maxChildren)
	}
	parentDepth, err := taskDepth(db, parent.ID)
	if err != nil {
		return nil, fmt.Errorf("resolve depth: %w", err)
	}
	if parentDepth+1 > maxDepth && !opts.BoardOverride {
		return nil, fmt.Errorf("%w (%d)", ErrDepthLimit, maxDepth)
	}

	handoff := buildHandoff(db, parent, opts.Handoff)

	child, err := CreateTaskWithOptions(db, TaskCreateOptions{
		Name:         opts.Name,
		RepoPath:     parent.RepoPath,
		GitBranch:    parent.GitBranch,
		AccountRole:  parent.AccountRole,
		MaxBudgetUSD: opts.MaxBudgetUSD,
		MaxTurns:     opts.MaxTurns,
		Organization: parent.Organization,
		Project:      parent.Project,
		ParentID:     parent.ID,
		WorkKind:     opts.WorkKind,
		Description:  opts.Description,
		// The child is created with its final stage, so a backlog (or agent)
		// child never fires the create-time assignment wake.
		ExecutionStage: opts.ExecutionStage,
		Origin:         opts.Origin,
	})
	if err != nil {
		return nil, err
	}
	if err := AddTaskDocument(db, child.ID, HandoffDocKey, handoff); err != nil {
		return nil, fmt.Errorf("store handoff: %w", err)
	}
	_ = LogActivity(db, parent.ID, "child_created", fmt.Sprintf("%s (%s): %s", child.ID, opts.WorkKind, child.Name))
	_ = LogActivity(db, child.ID, "handoff_stored", "from parent "+parent.ID)
	return child, nil
}

func buildHandoff(db *sql.DB, parent *Task, plan string) string {
	plan = strings.TrimSpace(plan)
	if plan == "" {
		if doc, err := GetLatestTaskDocument(db, parent.ID, "plan"); err == nil {
			plan = strings.TrimSpace(doc.Content)
		}
	}
	var finalMsg string
	_ = db.QueryRow(`SELECT message FROM task_comments WHERE task_id = ? AND author = 'agent-summary' ORDER BY id DESC LIMIT 1`, parent.ID).Scan(&finalMsg)

	kind := parent.WorkKind
	if kind == "" {
		kind = "coding"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Parent task: %s (%s, work_kind: %s)\n", parent.ID, parent.Name, kind)
	if plan != "" {
		b.WriteString("--- Plan ---\n" + plan + "\n")
	}
	if fm := strings.TrimSpace(finalMsg); fm != "" {
		b.WriteString("--- Parent's final message ---\n" + fm + "\n")
	}
	return b.String()
}

// GetTaskHandoff returns the latest handoff document for taskID, or "" when
// the task has none.
func GetTaskHandoff(db *sql.DB, taskID string) (string, error) {
	doc, err := GetLatestTaskDocument(db, taskID, HandoffDocKey)
	if errors.Is(err, ErrDocumentNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return doc.Content, nil
}

// DoneOptions configures MarkTaskDoneWithOptions.
type DoneOptions struct {
	// BoardOverride marks a parent done even while children are open.
	BoardOverride bool
	// BoardDone is the Board closing the task from the task page (STA-861):
	// no registered work product is required and a block (watchdog or
	// blocker) does not stop it. Only set it after the Board gate passed;
	// agents and token-only callers keep the work-product requirement.
	BoardDone bool
	// BoardNote is recorded on the timeline with a BoardDone close as
	// "Marked done by Board: <note>".
	BoardNote string
	// BoardStage is the Board (past its gate) changing the stage. Moving an
	// agent-created task out of a parked stage into a runnable one requires
	// it (RequiresBoardToLeave); without it ErrBoardRequired is returned.
	BoardStage bool
}

func checkOpenChildren(db *sql.DB, taskID string) error {
	n, err := CountOpenChildren(db, taskID)
	if err != nil {
		return fmt.Errorf("count open children: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("%w (%d open); close them or use the Board override", ErrOpenChildren, n)
	}
	return nil
}
