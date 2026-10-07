package context

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/bridge"
	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
	"github.com/google/uuid"
)

// Task represents an engineering task tracked within SQLite mesh.db.
type Task struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	RepoPath        string  `json:"repo_path"`
	GitBranch       string  `json:"git_branch"`
	Status          string  `json:"status"`       // active, done, soft_deleted
	AccountRole     string  `json:"account_role"` // work, personal, other
	MaxBudgetUSD    float64 `json:"max_budget_usd"`
	MaxTurns        int     `json:"max_turns"`
	SpentTokens     int64   `json:"spent_tokens"`
	SpentUSD        float64 `json:"spent_usd"`
	SpentTurns      int     `json:"spent_turns"`
	Organization    string  `json:"organization,omitempty"`
	Project         string  `json:"project,omitempty"`
	ParentID        string  `json:"parent_id,omitempty"`
	ExecutionStage  string  `json:"execution_stage"`
	CheckoutRunID   string  `json:"checkout_run_id,omitempty"`
	CheckoutAgentID string  `json:"checkout_agent_id,omitempty"`
	AssigneeAgentID string  `json:"assignee_agent_id,omitempty"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
	DeletedAt       *string `json:"deleted_at,omitempty"`
	IsBlocked       bool              `json:"is_blocked"`
	BlockReason     string            `json:"block_reason"`
	BlockedBy       []TaskBlockerInfo `json:"blocked_by,omitempty"`
	Blocks          []TaskBlockerInfo `json:"blocks,omitempty"`
	Description     string            `json:"description,omitempty"`
	Comments        []TaskComment     `json:"comments,omitempty"`
	// WorkKind is the routing category for this task.
	// Valid values: "coding" (default), "review", "architecture", "planning", "qa", "docs".
	WorkKind        string            `json:"work_kind"`
	// Origin is where the task came from: native (created in StayPoint),
	// paperclip_import (staypoint import paperclip), legacy (existed before
	// origins were tracked) or agent (created by an agent; starts in backlog
	// and only the Board moves it out). Boards and lists hide legacy by default.
	Origin string `json:"origin"`
	// Priority is low, medium (default), high or critical.
	Priority string `json:"priority,omitempty"`
	// SourceRef / SourceID identify an imported task's source record
	// (Paperclip identifier such as STA-772, and its uuid). Empty for
	// tasks created in StayPoint.
	SourceRef string `json:"source_ref,omitempty"`
	SourceID  string `json:"source_id,omitempty"`
	// Provider is the Board's explicit provider choice: "" (default: Claude
	// Opus on the repo's seat), "claude" or "gemini". Gemini runs only when
	// this is "gemini" (router.GeminiRequiresExplicitChoice).
	Provider string `json:"provider"`
	// ModelOverride pins the model for Provider ("opus", "sonnet",
	// "gemini-3.1-pro-high", "gemini-3.8-flash-high"); "" = the default.
	ModelOverride string `json:"model_override"`
}

// TaskBlockerInfo contains summarized info about an upstream or downstream related task.
type TaskBlockerInfo struct {
	ID             string `json:"id"`
	Identifier     string `json:"identifier,omitempty"`
	Name           string `json:"name"`
	Title          string `json:"title,omitempty"` // Alias for Name for WebUI compatibility
	Status         string `json:"status"`
	ExecutionStage string `json:"execution_stage"`
	IsBlocked      bool   `json:"is_blocked"`
	BlockReason    string `json:"block_reason,omitempty"`
	Rationale      string `json:"rationale,omitempty"`
}

// BlockerInput represents an upstream task to block on with an explicit rationale.
type BlockerInput struct {
	ID        string `json:"id"`
	Rationale string `json:"rationale,omitempty"`
}

// TaskDependencyNode represents a node in a dependency tree.
type TaskDependencyNode struct {
	TaskBlockerInfo
	Children []*TaskDependencyNode `json:"children,omitempty"`
}

// TaskDependencyGraph provides structured dependency graph information for a task.
type TaskDependencyGraph struct {
	TaskID       string              `json:"task_id"`
	Task         *TaskBlockerInfo    `json:"task"`
	Parent       *TaskBlockerInfo    `json:"parent,omitempty"`
	Subtasks     []TaskBlockerInfo   `json:"subtasks,omitempty"`
	BlockedBy    []TaskBlockerInfo   `json:"blocked_by"`              // direct upstream blockers
	Blocks       []TaskBlockerInfo   `json:"blocks"`                  // direct downstream blocked tasks
	RootBlockers []TaskBlockerInfo   `json:"root_blockers,omitempty"` // transitive blockers at root of tree
	UpstreamTree *TaskDependencyNode `json:"upstream_tree,omitempty"`
}

// TaskCreateOptions holds configuration for creating a task with budgets.
type TaskCreateOptions struct {
	Name            string
	RepoPath        string
	GitBranch       string
	AccountRole     string
	MaxBudgetUSD    float64
	MaxTurns        int
	Organization    string
	Project         string
	ParentID        string
	AssigneeAgentID string
	// WorkKind is the routing category. Defaults to "coding" when empty.
	// TODO(STA-316): validate and persist.
	WorkKind    string
	Description string
	// ExecutionStage overrides the initial stage (default "todo"). A
	// "backlog" task is parked: it is created without waking an agent.
	ExecutionStage string
	// Origin defaults to OriginNative.
	Origin string
	// Priority defaults to medium.
	Priority string
	// SourceRef / SourceID record an imported task's source (see Task).
	SourceRef string
	SourceID  string
	// Provider / ModelOverride are the explicit provider choice (see Task).
	// Callers normalise them with router.NormalizeRouteChoice (this package
	// cannot import router); CreateTaskWithOptions rejects unknown providers.
	Provider      string
	ModelOverride string
	// NoRepo leaves repo_path and git_branch empty instead of defaulting to
	// the working directory. The task cannot leave backlog until the Board
	// sets a repo (SetTaskRepo).
	NoRepo bool
	// ClosedAt (RFC 3339) is when a task created already done or cancelled
	// was closed at its source (Paperclip completedAt / cancelledAt). It
	// becomes updated_at (and deleted_at for cancelled). Empty means now.
	ClosedAt string
}

// GetCurrentGitBranch returns the current active git branch for a directory.
func GetCurrentGitBranch(dir string) string {
	cmd := gitexec.Command(context.Background(), "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "main"
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return "main"
	}
	return branch
}

// CreateTask inserts a new active task into the database.
func CreateTask(db *sql.DB, name, repoPath, gitBranch, role string) (*Task, error) {
	return CreateTaskWithOptions(db, TaskCreateOptions{
		Name:        name,
		RepoPath:    repoPath,
		GitBranch:   gitBranch,
		AccountRole: role,
	})
}

// CreateTaskWithOptions inserts a new task with budget and turn limit configurations.
func CreateTaskWithOptions(db *sql.DB, opts TaskCreateOptions) (*Task, error) {
	name := strings.TrimSpace(opts.Name)
	if name == "" {
		return nil, fmt.Errorf("task name cannot be empty")
	}

	repoPath := opts.RepoPath
	gitBranch := opts.GitBranch
	if opts.NoRepo {
		repoPath, gitBranch = "", ""
	} else {
		if repoPath == "" {
			var err error
			repoPath, err = os.Getwd()
			if err != nil {
				return nil, fmt.Errorf("failed to get current working directory: %w", err)
			}
		}
		if absRepoPath, err := filepath.Abs(repoPath); err == nil {
			repoPath = absRepoPath
		}
		if gitBranch == "" {
			gitBranch = GetCurrentGitBranch(repoPath)
		}
	}

	role := opts.AccountRole
	if role == "" {
		if bridge.IsWorkRepo(repoPath) {
			role = "work"
		} else {
			role = "personal"
		}
	}

	taskID := fmt.Sprintf("task-%s", uuid.New().String()[:8])

	workKind := opts.WorkKind
	if workKind == "" {
		workKind = "coding"
	}

	stage := strings.ToLower(strings.TrimSpace(opts.ExecutionStage))
	if stage == "" {
		stage = governance.StageTodo
	}
	if !governance.IsBoardSettableStage(stage) && stage != governance.StageBlocked {
		return nil, fmt.Errorf("%w %q", ErrInvalidStage, stage)
	}
	origin := strings.TrimSpace(opts.Origin)
	if origin == "" {
		origin = OriginNative
	}
	if !IsValidOrigin(origin) {
		return nil, fmt.Errorf("%w %q", ErrInvalidOrigin, origin)
	}
	// An agent-created task always starts parked in backlog, whatever stage
	// the agent asked for; only the Board moves it out (see
	// RequiresBoardToLeave). A runnable stage here would wake it at once.
	if origin == OriginAgent {
		stage = governance.StageBacklog
	}
	priority := NormalizeTaskPriority(opts.Priority)
	if !IsValidTaskProvider(opts.Provider) {
		return nil, fmt.Errorf("%w %q", ErrInvalidProvider, opts.Provider)
	}

	query := `
		INSERT INTO tasks (
			id, name, repo_path, git_branch, status, account_role,
			max_budget_usd, max_turns, spent_tokens, spent_usd, spent_turns,
			organization, project, parent_id, assignee_agent_id, work_kind,
			execution_stage, origin, priority, source_ref, source_id, provider, model_override, created_at, updated_at, deleted_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, 0.0, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
		        COALESCE(?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now')), ?)
	`
	// status follows the stage, as in writeStage: a task created closed
	// (an imported done/cancelled issue) is never active.
	status := "active"
	var closedAt, deletedAt interface{}
	if c := strings.TrimSpace(opts.ClosedAt); c != "" && (stage == governance.StageDone || stage == governance.StageCancelled) {
		closedAt = c
	}
	switch stage {
	case governance.StageDone:
		status = "done"
	case governance.StageCancelled:
		status = "soft_deleted"
		if closedAt != nil {
			deletedAt = closedAt
		} else {
			deletedAt = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
		}
	}

	var parentID interface{}
	if opts.ParentID != "" {
		parentID = opts.ParentID
	}

	var assigneeAgentID interface{}
	if opts.AssigneeAgentID != "" {
		assigneeAgentID = opts.AssigneeAgentID
	}

	if _, err := db.Exec(query, taskID, name, repoPath, gitBranch, status, role, opts.MaxBudgetUSD, opts.MaxTurns, opts.Organization, opts.Project, parentID, assigneeAgentID, workKind, stage, origin, priority, opts.SourceRef, opts.SourceID, opts.Provider, opts.ModelOverride, closedAt, deletedAt); err != nil {
		return nil, fmt.Errorf("failed to insert task: %w", err)
	}

	if opts.Description != "" {
		if err := UpsertTaskDescription(db, taskID, opts.Description); err != nil {
			return nil, fmt.Errorf("store description: %w", err)
		}
	}

	// Waking the agent as the task is ready for assignment/pickup. Backlog
	// tasks are parked, so nothing picks them up yet.
	if governance.IsRunnableStage(stage) {
		_ = orchestrator.NotifyDaemon(taskID, "assignment", "")
	}

	return GetTask(db, taskID)
}

// UpsertTaskDescription inserts a new description version for taskID.
func UpsertTaskDescription(db *sql.DB, taskID, content string) error {
	var maxVer int
	_ = db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM task_documents WHERE task_id = ? AND doc_key = 'description'`, taskID).Scan(&maxVer)
	_, err := db.Exec(
		`INSERT INTO task_documents (task_id, doc_key, version, content) VALUES (?, 'description', ?, ?)`,
		taskID, maxVer+1, content,
	)
	return err
}

// RecordTaskSpend updates the cumulative token, dollar, and turn spend on a task.
func RecordTaskSpend(db *sql.DB, taskID string, tokens int64, costUSD float64, turns int) error {
	task, err := GetTask(db, taskID)
	if err != nil {
		return err
	}

	query := `
		UPDATE tasks
		SET spent_tokens = spent_tokens + ?,
		    spent_usd = spent_usd + ?,
		    spent_turns = spent_turns + ?,
		    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?
	`
	res, err := db.Exec(query, tokens, costUSD, turns, task.ID)
	if err != nil {
		return fmt.Errorf("failed to record task spend: %w", err)
	}

	affected, _ := res.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("task not found: %s", taskID)
	}
	return nil
}

// UpdateTaskBudget updates the budget limits for an existing task.
func UpdateTaskBudget(db *sql.DB, taskID string, maxUSD float64, maxTurns int) error {
	task, err := GetTask(db, taskID)
	if err != nil {
		return err
	}

	query := `
		UPDATE tasks
		SET max_budget_usd = ?, max_turns = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?
	`
	res, err := db.Exec(query, maxUSD, maxTurns, task.ID)
	if err != nil {
		return fmt.Errorf("failed to update task budget: %w", err)
	}

	affected, _ := res.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("task not found: %s", taskID)
	}
	return nil
}

// BudgetEvaluation encapsulates the status of a task's spending limits.
type BudgetEvaluation struct {
	IsBlocked bool
	IsWarning bool
	Reason    string
	PctBudget float64
	PctTurns  float64
}

// EvaluateTaskBudget determines whether a task is approaching or has exceeded its limits.
func EvaluateTaskBudget(task *Task) BudgetEvaluation {
	var eval BudgetEvaluation
	if task == nil {
		return eval
	}

	if task.MaxBudgetUSD > 0 {
		eval.PctBudget = (task.SpentUSD / task.MaxBudgetUSD) * 100.0
		if task.SpentUSD >= task.MaxBudgetUSD {
			eval.IsBlocked = true
			eval.Reason = fmt.Sprintf("Dollar budget exhausted ($%.2f spent / $%.2f limit)", task.SpentUSD, task.MaxBudgetUSD)
			return eval
		} else if eval.PctBudget >= 80.0 {
			eval.IsWarning = true
			eval.Reason = fmt.Sprintf("Dollar budget at %.0f%% ($%.2f spent / $%.2f limit)", eval.PctBudget, task.SpentUSD, task.MaxBudgetUSD)
		}
	}

	if task.MaxTurns > 0 {
		eval.PctTurns = (float64(task.SpentTurns) / float64(task.MaxTurns)) * 100.0
		if task.SpentTurns >= task.MaxTurns {
			eval.IsBlocked = true
			eval.Reason = fmt.Sprintf("Turn limit exhausted (%d turns spent / %d turn limit)", task.SpentTurns, task.MaxTurns)
			return eval
		} else if eval.PctTurns >= 80.0 {
			eval.IsWarning = true
			eval.Reason = fmt.Sprintf("Turn limit at %.0f%% (%d turns spent / %d turn limit)", eval.PctTurns, task.SpentTurns, task.MaxTurns)
		}
	}

	return eval
}

// ListTasks queries active tasks, or all non-deleted tasks if includeAll is true.
func ListTasks(db *sql.DB, includeAll bool) ([]Task, error) {
	var query string
	if includeAll {
		query = `
			SELECT id, name, repo_path, git_branch, status, account_role,
			       max_budget_usd, max_turns, spent_tokens, spent_usd, spent_turns,
			       organization, project, parent_id, execution_stage, checkout_run_id, checkout_agent_id,
			       assignee_agent_id, is_blocked, block_reason, created_at, updated_at, deleted_at,
			       COALESCE(origin, 'native'), COALESCE(priority, 'medium'), source_ref, source_id,
			       COALESCE(provider, ''), COALESCE(model_override, '')
			FROM tasks
			WHERE status != 'soft_deleted'
			ORDER BY created_at DESC
		`
	} else {
		query = `
			SELECT id, name, repo_path, git_branch, status, account_role,
			       max_budget_usd, max_turns, spent_tokens, spent_usd, spent_turns,
			       organization, project, parent_id, execution_stage, checkout_run_id, checkout_agent_id,
			       assignee_agent_id, is_blocked, block_reason, created_at, updated_at, deleted_at,
			       COALESCE(origin, 'native'), COALESCE(priority, 'medium'), source_ref, source_id,
			       COALESCE(provider, ''), COALESCE(model_override, '')
			FROM tasks
			WHERE status = 'active'
			ORDER BY created_at DESC
		`
	}

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to query tasks: %w", err)
	}
	defer rows.Close()

	var tasks []Task
	for rows.Next() {
		var t Task
		var deletedAt, org, proj, blockReason, parentID, checkoutRunID, checkoutAgentID, assigneeAgentID sql.NullString
		if err := rows.Scan(
			&t.ID,
			&t.Name,
			&t.RepoPath,
			&t.GitBranch,
			&t.Status,
			&t.AccountRole,
			&t.MaxBudgetUSD,
			&t.MaxTurns,
			&t.SpentTokens,
			&t.SpentUSD,
			&t.SpentTurns,
			&org,
			&proj,
			&parentID,
			&t.ExecutionStage,
			&checkoutRunID,
			&checkoutAgentID,
			&assigneeAgentID,
			&t.IsBlocked,
			&blockReason,
			&t.CreatedAt,
			&t.UpdatedAt,
			&deletedAt,
			&t.Origin,
			&t.Priority,
			&t.SourceRef,
			&t.SourceID,
			&t.Provider,
			&t.ModelOverride,
		); err != nil {
			return nil, fmt.Errorf("failed to scan task row: %w", err)
		}
		if blockReason.Valid {
			t.BlockReason = blockReason.String
		}
		if parentID.Valid {
			t.ParentID = parentID.String
		}
		if checkoutRunID.Valid {
			t.CheckoutRunID = checkoutRunID.String
		}
		if checkoutAgentID.Valid {
			t.CheckoutAgentID = checkoutAgentID.String
		}
		if assigneeAgentID.Valid {
			t.AssigneeAgentID = assigneeAgentID.String
		}
		if deletedAt.Valid {
			t.DeletedAt = &deletedAt.String
		}
		if org.Valid {
			t.Organization = org.String
		}
		if proj.Valid {
			t.Project = proj.String
		}
		tasks = append(tasks, t)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(tasks) > 0 {
		cRows, err := db.Query(`SELECT task_id, id, author, message, created_at FROM task_comments ORDER BY created_at ASC`)
		if err == nil {
			defer cRows.Close()
			commentsMap := make(map[string][]TaskComment)
			for cRows.Next() {
				var c TaskComment
				if err := cRows.Scan(&c.TaskID, &c.ID, &c.Author, &c.Message, &c.CreatedAt); err == nil {
					commentsMap[c.TaskID] = append(commentsMap[c.TaskID], c)
				}
			}
			for i := range tasks {
				tasks[i].Comments = commentsMap[tasks[i].ID]
			}
		}

		dRows, err := db.Query(`SELECT task_id, content FROM task_documents WHERE doc_key = 'description' ORDER BY version DESC`)
		if err == nil {
			defer dRows.Close()
			for dRows.Next() {
				var tid, content string
				if err := dRows.Scan(&tid, &content); err == nil && content != "" {
					for i := range tasks {
						if tasks[i].ID == tid {
							tasks[i].Description = content
							break
						}
					}
				}
			}
		}
	}

	return tasks, nil
}

// GetTask fetches a single task by exact ID or ID prefix.
func GetTask(db *sql.DB, id string) (*Task, error) {
	id = strings.TrimSpace(id)
	query := `
		SELECT id, name, repo_path, git_branch, status, account_role,
		       max_budget_usd, max_turns, spent_tokens, spent_usd, spent_turns,
		       organization, project, parent_id, execution_stage, checkout_run_id, checkout_agent_id,
		       assignee_agent_id, is_blocked, block_reason, created_at, updated_at, deleted_at,
		       COALESCE(work_kind, 'coding'), COALESCE(origin, 'native'), COALESCE(priority, 'medium'), source_ref, source_id,
			       COALESCE(provider, ''), COALESCE(model_override, '')
		FROM tasks
		WHERE id = ? OR id = ? OR id LIKE ?
		ORDER BY created_at DESC
		LIMIT 1
	`
	prefixMatch := id + "%"
	fullID := id
	if !strings.HasPrefix(fullID, "task-") {
		fullID = "task-" + id
	}

	row := db.QueryRow(query, id, fullID, prefixMatch)
	var t Task
	var deletedAt, org, proj, blockReason, parentID, checkoutRunID, checkoutAgentID, assigneeAgentID sql.NullString
	if err := row.Scan(
		&t.ID,
		&t.Name,
		&t.RepoPath,
		&t.GitBranch,
		&t.Status,
		&t.AccountRole,
		&t.MaxBudgetUSD,
		&t.MaxTurns,
		&t.SpentTokens,
		&t.SpentUSD,
		&t.SpentTurns,
		&org,
		&proj,
		&parentID,
		&t.ExecutionStage,
		&checkoutRunID,
		&checkoutAgentID,
		&assigneeAgentID,
		&t.IsBlocked,
		&blockReason,
		&t.CreatedAt,
		&t.UpdatedAt,
		&deletedAt,
		&t.WorkKind,
		&t.Origin,
		&t.Priority,
		&t.SourceRef,
		&t.SourceID,
		&t.Provider,
		&t.ModelOverride,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("task not found: %s", id)
		}
		return nil, fmt.Errorf("failed to get task: %w", err)
	}
	if blockReason.Valid {
		t.BlockReason = blockReason.String
	}
	if parentID.Valid {
		t.ParentID = parentID.String
	}
	if checkoutRunID.Valid {
		t.CheckoutRunID = checkoutRunID.String
	}
	if checkoutAgentID.Valid {
		t.CheckoutAgentID = checkoutAgentID.String
	}
	if assigneeAgentID.Valid {
		t.AssigneeAgentID = assigneeAgentID.String
	}
	if deletedAt.Valid {
		t.DeletedAt = &deletedAt.String
	}
	if org.Valid {
		t.Organization = org.String
	}
	if proj.Valid {
		t.Project = proj.String
	}
	t.BlockedBy, _ = GetTaskBlockedBy(db, t.ID)
	t.Blocks, _ = GetTaskBlocks(db, t.ID)
	if cRows, err := db.Query(`SELECT id, task_id, author, message, created_at FROM task_comments WHERE task_id = ? ORDER BY created_at ASC`, t.ID); err == nil {
		defer cRows.Close()
		for cRows.Next() {
			var c TaskComment
			if err := cRows.Scan(&c.ID, &c.TaskID, &c.Author, &c.Message, &c.CreatedAt); err == nil {
				t.Comments = append(t.Comments, c)
			}
		}
	}
	var content string
	if err := db.QueryRow(`SELECT content FROM task_documents WHERE task_id = ? AND doc_key = 'description' ORDER BY version DESC LIMIT 1`, t.ID).Scan(&content); err == nil {
		t.Description = content
	}
	return &t, nil
}

// GetTaskBlockedBy returns all upstream tasks that block taskID.
func GetTaskBlockedBy(db *sql.DB, taskID string) ([]TaskBlockerInfo, error) {
	query := `
		SELECT tr.task_id, COALESCE(t.name, tr.task_id), COALESCE(t.status, 'active'), COALESCE(t.execution_stage, 'todo'),
		       COALESCE(t.is_blocked, 0), COALESCE(t.block_reason, ''), COALESCE(tr.rationale, '')
		FROM task_relations tr
		LEFT JOIN tasks t ON tr.task_id = t.id
		WHERE tr.blocks_id = ?
		ORDER BY tr.created_at ASC
	`
	rows, err := db.Query(query, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []TaskBlockerInfo
	for rows.Next() {
		var info TaskBlockerInfo
		var isBlockedInt int
		if err := rows.Scan(&info.ID, &info.Name, &info.Status, &info.ExecutionStage, &isBlockedInt, &info.BlockReason, &info.Rationale); err != nil {
			return nil, err
		}
		info.Identifier = info.ID
		info.Title = info.Name
		info.IsBlocked = isBlockedInt == 1
		if info.Rationale == "" && info.BlockReason != "" {
			info.Rationale = info.BlockReason
		}
		list = append(list, info)
	}
	return list, rows.Err()
}

// GetTaskBlocks returns all downstream tasks blocked by taskID.
func GetTaskBlocks(db *sql.DB, taskID string) ([]TaskBlockerInfo, error) {
	query := `
		SELECT tr.blocks_id, COALESCE(t.name, tr.blocks_id), COALESCE(t.status, 'active'), COALESCE(t.execution_stage, 'todo'),
		       COALESCE(t.is_blocked, 0), COALESCE(t.block_reason, ''), COALESCE(tr.rationale, '')
		FROM task_relations tr
		LEFT JOIN tasks t ON tr.blocks_id = t.id
		WHERE tr.task_id = ?
		ORDER BY tr.created_at ASC
	`
	rows, err := db.Query(query, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []TaskBlockerInfo
	for rows.Next() {
		var info TaskBlockerInfo
		var isBlockedInt int
		if err := rows.Scan(&info.ID, &info.Name, &info.Status, &info.ExecutionStage, &isBlockedInt, &info.BlockReason, &info.Rationale); err != nil {
			return nil, err
		}
		info.Identifier = info.ID
		info.Title = info.Name
		info.IsBlocked = isBlockedInt == 1
		if info.Rationale == "" && info.BlockReason != "" {
			info.Rationale = info.BlockReason
		}
		list = append(list, info)
	}
	return list, rows.Err()
}

// GetTaskDependencyGraph builds a comprehensive dependency graph and hierarchy around taskID.
func GetTaskDependencyGraph(db *sql.DB, taskID string) (*TaskDependencyGraph, error) {
	task, err := GetTask(db, taskID)
	if err != nil {
		return nil, err
	}

	taskInfo := &TaskBlockerInfo{
		ID:             task.ID,
		Identifier:     task.ID,
		Name:           task.Name,
		Title:          task.Name,
		Status:         task.Status,
		ExecutionStage: task.ExecutionStage,
		IsBlocked:      task.IsBlocked,
		BlockReason:    task.BlockReason,
	}

	graph := &TaskDependencyGraph{
		TaskID:    task.ID,
		Task:      taskInfo,
		BlockedBy: task.BlockedBy,
		Blocks:    task.Blocks,
	}

	// Fetch parent if configured
	if task.ParentID != "" {
		if parentTask, err := GetTask(db, task.ParentID); err == nil {
			graph.Parent = &TaskBlockerInfo{
				ID:             parentTask.ID,
				Identifier:     parentTask.ID,
				Name:           parentTask.Name,
				Title:          parentTask.Name,
				Status:         parentTask.Status,
				ExecutionStage: parentTask.ExecutionStage,
				IsBlocked:      parentTask.IsBlocked,
				BlockReason:    parentTask.BlockReason,
			}
		}
	}

	// Fetch subtasks (where parent_id = task.ID)
	subRows, err := db.Query(`
		SELECT id, name, status, execution_stage, is_blocked, COALESCE(block_reason, '')
		FROM tasks
		WHERE parent_id = ? AND status != 'soft_deleted'
		ORDER BY created_at ASC
	`, task.ID)
	if err == nil {
		defer subRows.Close()
		for subRows.Next() {
			var st TaskBlockerInfo
			var isBlockedInt int
			if err := subRows.Scan(&st.ID, &st.Name, &st.Status, &st.ExecutionStage, &isBlockedInt, &st.BlockReason); err == nil {
				st.Identifier = st.ID
				st.Title = st.Name
				st.IsBlocked = isBlockedInt == 1
				graph.Subtasks = append(graph.Subtasks, st)
			}
		}
	}

	// Build upstream recursive dependency tree with cycle prevention
	visited := map[string]bool{task.ID: true}
	var buildUpstreamTree func(currID string, depth int) *TaskDependencyNode
	buildUpstreamTree = func(currID string, depth int) *TaskDependencyNode {
		if depth > 8 {
			return nil
		}
		blockers, _ := GetTaskBlockedBy(db, currID)
		node := &TaskDependencyNode{}
		for _, b := range blockers {
			childNode := &TaskDependencyNode{TaskBlockerInfo: b}
			if !visited[b.ID] {
				visited[b.ID] = true
				subTree := buildUpstreamTree(b.ID, depth+1)
				if subTree != nil && len(subTree.Children) > 0 {
					childNode.Children = subTree.Children
				}
			}
			node.Children = append(node.Children, childNode)
		}
		return node
	}

	upstreamTree := buildUpstreamTree(task.ID, 0)
	if upstreamTree != nil {
		graph.UpstreamTree = upstreamTree
	}

	// Determine root blockers: tasks in the chain that have no upstream blockers of their own and are not done
	rootBlockersMap := make(map[string]TaskBlockerInfo)
	var collectRoots func(nodes []*TaskDependencyNode)
	collectRoots = func(nodes []*TaskDependencyNode) {
		for _, n := range nodes {
			if len(n.Children) == 0 {
				if n.Status != "done" && n.ExecutionStage != "done" {
					rootBlockersMap[n.ID] = n.TaskBlockerInfo
				}
			} else {
				collectRoots(n.Children)
			}
		}
	}
	if upstreamTree != nil {
		collectRoots(upstreamTree.Children)
	}
	for _, rb := range rootBlockersMap {
		graph.RootBlockers = append(graph.RootBlockers, rb)
	}

	return graph, nil
}

// GetActiveTaskForRepo finds the most recently updated active task for a repo (or globally).
// Parked (backlog) tasks and tasks with no repo are never "the active task":
// a bulk import or a backlog edit must not take over hooks, context and the
// statusline.
func GetActiveTaskForRepo(db *sql.DB, repoPath string) (*Task, error) {
	cleanPath := filepath.Clean(repoPath)

	// First try exact or prefix match on repo_path
	query := `
		SELECT id, name, repo_path, git_branch, status, account_role,
		       max_budget_usd, max_turns, spent_tokens, spent_usd, spent_turns,
		       organization, project, parent_id, execution_stage, checkout_run_id, checkout_agent_id,
		       assignee_agent_id, is_blocked, block_reason, created_at, updated_at, deleted_at,
		       COALESCE(origin, 'native')
		FROM tasks
		WHERE status = 'active' AND execution_stage != 'backlog' AND repo_path != ''
		  AND (repo_path = ? OR repo_path LIKE ?)
		ORDER BY updated_at DESC
		LIMIT 1
	`
	row := db.QueryRow(query, cleanPath, cleanPath+"/%")
	var t Task
	var deletedAt, org, proj, blockReason, parentID, checkoutRunID, checkoutAgentID, assigneeAgentID sql.NullString
	err := row.Scan(
		&t.ID,
		&t.Name,
		&t.RepoPath,
		&t.GitBranch,
		&t.Status,
		&t.AccountRole,
		&t.MaxBudgetUSD,
		&t.MaxTurns,
		&t.SpentTokens,
		&t.SpentUSD,
		&t.SpentTurns,
		&org,
		&proj,
		&parentID,
		&t.ExecutionStage,
		&checkoutRunID,
		&checkoutAgentID,
		&assigneeAgentID,
		&t.IsBlocked,
		&blockReason,
		&t.CreatedAt,
		&t.UpdatedAt,
		&deletedAt,
		&t.Origin,
	)
	if err == nil {
		if blockReason.Valid {
			t.BlockReason = blockReason.String
		}
		if assigneeAgentID.Valid {
			t.AssigneeAgentID = assigneeAgentID.String
		}
		if deletedAt.Valid {
			t.DeletedAt = &deletedAt.String
		}
		if org.Valid {
			t.Organization = org.String
		}
		if proj.Valid {
			t.Project = proj.String
		}
		return &t, nil
	}

	// Fallback to most recent active task in any repo
	fallbackQuery := `
		SELECT id, name, repo_path, git_branch, status, account_role,
		       max_budget_usd, max_turns, spent_tokens, spent_usd, spent_turns,
		       organization, project, parent_id, execution_stage, checkout_run_id, checkout_agent_id,
		       assignee_agent_id, is_blocked, block_reason, created_at, updated_at, deleted_at,
		       COALESCE(origin, 'native')
		FROM tasks
		WHERE status = 'active' AND execution_stage != 'backlog' AND repo_path != ''
		ORDER BY updated_at DESC
		LIMIT 1
	`
	fbRow := db.QueryRow(fallbackQuery)
	err = fbRow.Scan(
		&t.ID,
		&t.Name,
		&t.RepoPath,
		&t.GitBranch,
		&t.Status,
		&t.AccountRole,
		&t.MaxBudgetUSD,
		&t.MaxTurns,
		&t.SpentTokens,
		&t.SpentUSD,
		&t.SpentTurns,
		&org,
		&proj,
		&parentID,
		&t.ExecutionStage,
		&checkoutRunID,
		&checkoutAgentID,
		&assigneeAgentID,
		&t.IsBlocked,
		&blockReason,
		&t.CreatedAt,
		&t.UpdatedAt,
		&deletedAt,
		&t.Origin,
	)
	if err == nil {
		if blockReason.Valid {
			t.BlockReason = blockReason.String
		}
		if assigneeAgentID.Valid {
			t.AssigneeAgentID = assigneeAgentID.String
		}
		if deletedAt.Valid {
			t.DeletedAt = &deletedAt.String
		}
		if org.Valid {
			t.Organization = org.String
		}
		if proj.Valid {
			t.Project = proj.String
		}
		return &t, nil
	}

	return nil, fmt.Errorf("no active tasks found")
}

// MarkTaskDone updates the task's status to 'done'. A task with open child
// tasks is refused (ErrOpenChildren); see MarkTaskDoneWithOptions.
func MarkTaskDone(db *sql.DB, id string) error {
	return MarkTaskDoneWithOptions(db, id, DoneOptions{})
}

// MarkTaskDoneWithOptions is MarkTaskDone with the Board override.
func MarkTaskDoneWithOptions(db *sql.DB, id string, opts DoneOptions) error {
	task, err := GetTask(db, id)
	if err != nil {
		return err
	}
	if !opts.BoardOverride {
		if err := checkOpenChildren(db, task.ID); err != nil {
			return err
		}
	}

	if !opts.BoardDone {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM task_work_products WHERE task_id = ?`, task.ID).Scan(&count); err != nil {
			return fmt.Errorf("failed to check work products: %w", err)
		}
		if count == 0 {
			return ErrNoWorkProduct
		}

		// Watchdog: evaluate criteria on deliverable-status change before allowing done.
		_ = governance.TriggerWatchdogEval(db, task.ID, "deliverable_update")
		// Re-read block status — watchdog may have just set is_blocked = 1.
		var isBlocked int
		var blockReason string
		_ = db.QueryRow(`SELECT is_blocked, COALESCE(block_reason,'') FROM tasks WHERE id = ?`, task.ID).Scan(&isBlocked, &blockReason)
		if isBlocked == 1 {
			return fmt.Errorf("task is blocked by watchdog: %s", blockReason)
		}
	}

	// A Board close also clears any block: the task is finished.
	query := `
		UPDATE tasks
		SET status = 'done', execution_stage = 'done', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?
	`
	if opts.BoardDone {
		query = `
		UPDATE tasks
		SET status = 'done', execution_stage = 'done', is_blocked = 0, block_reason = NULL,
		    deleted_at = NULL, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?
	`
	}
	res, err := db.Exec(query, task.ID)
	if err != nil {
		return fmt.Errorf("failed to mark task as done: %w", err)
	}

	affected, _ := res.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("task not found: %s", id)
	}

	if opts.BoardDone {
		msg := "Marked done by Board"
		if note := strings.TrimSpace(opts.BoardNote); note != "" {
			msg += ": " + note
		}
		// Written directly, not through AddTaskComment: the note must not
		// notify the daemon.
		_, _ = db.Exec(`INSERT INTO task_comments (task_id, author, message) VALUES (?, 'board', ?)`, task.ID, msg)
		_ = LogActivity(db, task.ID, "board_done", msg)
	}

	// Unblock any tasks that were waiting on this one
	rows, err := db.Query(`SELECT blocks_id FROM task_relations WHERE task_id = ?`, task.ID)
	if err == nil {
		var blockedIDs []string
		for rows.Next() {
			var bid string
			if err := rows.Scan(&bid); err == nil {
				blockedIDs = append(blockedIDs, bid)
			}
		}
		rows.Close()

		for _, bid := range blockedIDs {
			_ = UnblockTask(db, bid, task.ID)
		}
	}

	return nil
}

// DeleteTask marks a task as soft_deleted.
func DeleteTask(db *sql.DB, id string) error {
	task, err := GetTask(db, id)
	if err != nil {
		return err
	}

	// Reset execution_stage so the mutex-lease check in the interceptor does not
	// see this task as holding an in_progress lock after it is cancelled.
	query := `
		UPDATE tasks
		SET status = 'soft_deleted',
		    execution_stage = 'cancelled',
		    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
		    deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?
	`
	res, err := db.Exec(query, task.ID)
	if err != nil {
		return fmt.Errorf("failed to delete task: %w", err)
	}

	affected, _ := res.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("task not found: %s", id)
	}
	return nil
}

type TaskComment struct {
	ID        int    `json:"id"`
	TaskID    string `json:"task_id"`
	Author    string `json:"author"`
	Message   string `json:"message"`
	CreatedAt string `json:"created_at"`
}

func AddTaskComment(db *sql.DB, taskID, author, message string) error {
	task, err := GetTask(db, taskID)
	if err != nil {
		return err
	}
	query := `INSERT INTO task_comments (task_id, author, message) VALUES (?, ?, ?)`
	if _, err := db.Exec(query, task.ID, author, message); err != nil {
		return err
	}

	_ = LogActivity(db, task.ID, "comment_added", fmt.Sprintf("author=%s", author))
	// Any pending interactions configured to supersede on comment are superseded
	_, _ = SupersedeInteractionsOnComment(db, task.ID)
	// Watchdog: re-evaluate criteria on every comment (update-triggered, no polling).
	_ = governance.TriggerWatchdogEval(db, task.ID, "comment")
	if commentWakes(db, task.ID) {
		_ = orchestrator.NotifyDaemon(task.ID, "comment", "")
	}
	return nil
}

// commentWakes reports whether a new comment should wake the task's agent
// (STA-861). It does not when the task is not runnable (backlog, including
// interactive tasks; stopped; closed) or when a Board stop is pending for the
// run that is still winding down: only Run Now or a stage change resumes a
// stopped task.
func commentWakes(db *sql.DB, taskID string) bool {
	var stage string
	if err := db.QueryRow(`SELECT COALESCE(execution_stage, '') FROM tasks WHERE id = ?`, taskID).Scan(&stage); err != nil {
		return false
	}
	if !governance.IsRunnableStage(stage) {
		return false
	}
	// A held organization's tasks never wake; the refusal is logged as held.
	if orchestrator.WakeHeld(db, taskID, "comment") {
		return false
	}
	// A stop is pending while the stopped run still holds the checkout; once
	// it exits the stage is stopped (not runnable).
	var stop int
	if err := db.QueryRow(
		`SELECT rc.stop_requested FROM run_control rc JOIN tasks t ON t.id = rc.task_id
		  WHERE rc.task_id = ? AND t.checkout_run_id IS NOT NULL`, taskID,
	).Scan(&stop); err == nil && stop != 0 {
		return false
	}
	return true
}

func GetTaskComments(db *sql.DB, taskID string) ([]TaskComment, error) {
	task, err := GetTask(db, taskID)
	if err != nil {
		return nil, err
	}
	query := `SELECT id, task_id, author, message, created_at FROM task_comments WHERE task_id = ? ORDER BY created_at ASC`
	rows, err := db.Query(query, task.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var comments []TaskComment
	for rows.Next() {
		var c TaskComment
		if err := rows.Scan(&c.ID, &c.TaskID, &c.Author, &c.Message, &c.CreatedAt); err != nil {
			return nil, err
		}
		comments = append(comments, c)
	}
	return comments, rows.Err()
}

func BlockTask(db *sql.DB, taskID, reason string, blockedByIDs ...string) error {
	var blockers []BlockerInput
	for _, id := range blockedByIDs {
		blockers = append(blockers, BlockerInput{ID: id, Rationale: reason})
	}
	return BlockTaskWithBlockers(db, taskID, reason, blockers)
}

func BlockTaskWithBlockers(db *sql.DB, taskID, reason string, blockers []BlockerInput) error {
	task, err := GetTask(db, taskID)
	if err != nil {
		return err
	}

	type resolvedBlocker struct {
		id        string
		rationale string
	}
	var resolved []resolvedBlocker
	cleanReason := reason
	for _, b := range blockers {
		blockedByTask, err := GetTask(db, b.ID)
		if err != nil {
			return err
		}
		rationale := b.Rationale
		if rationale == "" {
			rationale = reason
		}
		if cleanReason == "" && rationale != "" {
			cleanReason = rationale
		}
		resolved = append(resolved, resolvedBlocker{
			id:        blockedByTask.ID,
			rationale: rationale,
		})
	}

	if cleanReason == "" && len(resolved) > 0 {
		cleanReason = fmt.Sprintf("Blocked by %s", resolved[0].id)
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}

	query := `UPDATE tasks SET is_blocked = 1, block_reason = ?, execution_stage = `+blockedStageSQL()+`, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`
	if _, err := tx.Exec(query, cleanReason, task.ID); err != nil {
		tx.Rollback()
		return err
	}

	for _, b := range resolved {
		if _, err := tx.Exec(`
			INSERT INTO task_relations (task_id, blocks_id, rationale)
			VALUES (?, ?, ?)
			ON CONFLICT(task_id, blocks_id) DO UPDATE SET rationale = excluded.rationale
		`, b.id, task.ID, b.rationale); err != nil {
			tx.Rollback()
			return err
		}
	}

	return tx.Commit()
}

// ErrBlockerSelfReference is returned when a task tries to block itself.
var ErrBlockerSelfReference = fmt.Errorf("task cannot block itself")

// ErrBlockerCycle is returned when adding a blocker would create a dependency cycle.
var ErrBlockerCycle = fmt.Errorf("blocker cycle detected")

// hasBlockerCycle reports true if taskID can reach targetID through "blocks" edges.
// Used to detect cycles before inserting a new blocker relation.
func hasBlockerCycle(db *sql.DB, taskID, targetID string) (bool, error) {
	visited := map[string]bool{}
	queue := []string{taskID}
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		if curr == targetID {
			return true, nil
		}
		if visited[curr] {
			continue
		}
		visited[curr] = true
		rows, err := db.Query(`SELECT blocks_id FROM task_relations WHERE task_id = ?`, curr)
		if err != nil {
			return false, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return false, err
			}
			if !visited[id] {
				queue = append(queue, id)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return false, err
		}
	}
	return false, nil
}

func AddTaskBlocker(db *sql.DB, taskID, blockerID, rationale string) error {
	task, err := GetTask(db, taskID)
	if err != nil {
		return err
	}
	blockerTask, err := GetTask(db, blockerID)
	if err != nil {
		return err
	}

	if task.ID == blockerTask.ID {
		return ErrBlockerSelfReference
	}

	// Cycle check: would adding "blockerTask blocks task" create a cycle?
	// A cycle exists iff task can already reach blockerTask through "blocks" edges.
	cycle, err := hasBlockerCycle(db, task.ID, blockerTask.ID)
	if err != nil {
		return fmt.Errorf("cycle check failed: %w", err)
	}
	if cycle {
		return ErrBlockerCycle
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}

	if _, err := tx.Exec(`
		INSERT INTO task_relations (task_id, blocks_id, rationale)
		VALUES (?, ?, ?)
		ON CONFLICT(task_id, blocks_id) DO UPDATE SET rationale = excluded.rationale
	`, blockerTask.ID, task.ID, rationale); err != nil {
		tx.Rollback()
		return err
	}

	reason := task.BlockReason
	if reason == "" || strings.EqualFold(reason, "blocked via tui") {
		if rationale != "" {
			reason = rationale
		} else {
			reason = fmt.Sprintf("Blocked by %s", blockerTask.ID)
		}
	}
	if _, err := tx.Exec(`
		UPDATE tasks
		SET is_blocked = 1, block_reason = ?, execution_stage = `+blockedStageSQL()+`, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?
	`, reason, task.ID); err != nil {
		tx.Rollback()
		return err
	}

	return tx.Commit()
}

func RemoveTaskBlocker(db *sql.DB, taskID, blockerID string) error {
	return UnblockTask(db, taskID, blockerID)
}

func UnblockTask(db *sql.DB, taskID string, unblockFromIDs ...string) error {
	task, err := GetTask(db, taskID)
	if err != nil {
		return err
	}

	var resolvedUnblockIDs []string
	for _, unblockFromID := range unblockFromIDs {
		unblockFromTask, err := GetTask(db, unblockFromID)
		if err != nil {
			return err
		}
		resolvedUnblockIDs = append(resolvedUnblockIDs, unblockFromTask.ID)
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}

	var actuallyUnblocked bool
	if len(resolvedUnblockIDs) > 0 {
		for _, unblockID := range resolvedUnblockIDs {
			if _, err := tx.Exec(`DELETE FROM task_relations WHERE task_id = ? AND blocks_id = ?`, unblockID, task.ID); err != nil {
				tx.Rollback()
				return err
			}
		}

		var count int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM task_relations WHERE blocks_id = ?`, task.ID).Scan(&count); err != nil {
			tx.Rollback()
			return err
		}

		if count == 0 {
			if _, err := tx.Exec(`UPDATE tasks SET is_blocked = 0, block_reason = '', execution_stage = CASE WHEN execution_stage = 'blocked' THEN 'todo' ELSE execution_stage END, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, task.ID); err != nil {
				tx.Rollback()
				return err
			}
			actuallyUnblocked = true
		}
	} else {
		if _, err := tx.Exec(`DELETE FROM task_relations WHERE blocks_id = ?`, task.ID); err != nil {
			tx.Rollback()
			return err
		}
		if _, err := tx.Exec(`UPDATE tasks SET is_blocked = 0, block_reason = '', execution_stage = CASE WHEN execution_stage = 'blocked' THEN 'todo' ELSE execution_stage END, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, task.ID); err != nil {
			tx.Rollback()
			return err
		}
		actuallyUnblocked = true
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	if actuallyUnblocked {
		_ = orchestrator.NotifyDaemon(task.ID, "blocker_cleared", "")
	}

	return nil
}

func ArchiveTask(db *sql.DB, taskID string) error {
	task, err := GetTask(db, taskID)
	if err != nil {
		return err
	}
	// "cancel" / "archive" transitions to soft_deleted according to scope? Or maybe "cancelled" / "archived"?
	// scope says: "Transitions task to cancelled/archived state cleanly"
	// In the DB constraint: status IN ('active', 'done', 'soft_deleted').
	// Let's just use 'soft_deleted' and soft-delete it or change DB schema to allow 'cancelled' and 'archived'?
	// The CLI code says: if iss.Status == "done" || iss.Status == "cancelled"
	// Wait, the DB check constraint is: status IN ('active', 'done', 'soft_deleted'). So cancelled could just be 'soft_deleted'.
	query := `UPDATE tasks SET status = 'soft_deleted', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`
	_, err = db.Exec(query, task.ID)
	return err
}

func TouchTask(db *sql.DB, taskID string) error {
	task, err := GetTask(db, taskID)
	if err != nil {
		return err
	}
	query := `UPDATE tasks SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`
	_, err = db.Exec(query, task.ID)
	return err
}

type TaskDocument struct {
	ID        int    `json:"id"`
	TaskID    string `json:"task_id"`
	DocKey    string `json:"doc_key"`
	Version   int    `json:"version"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

type TaskWorkProduct struct {
	ID          int    `json:"id"`
	TaskID      string `json:"task_id"`
	ProductType string `json:"product_type"`
	Reference   string `json:"reference"`
	CreatedAt   string `json:"created_at"`
}

type ActivityLog struct {
	ID        int    `json:"id"`
	TaskID    string `json:"task_id"`
	EventType string `json:"event_type"`
	Details   string `json:"details"`
	CreatedAt string `json:"created_at"`
}

func AddTaskDocument(db *sql.DB, taskID, docKey, content string) error {
	var maxVer sql.NullInt32
	err := db.QueryRow(`SELECT MAX(version) FROM task_documents WHERE task_id = ? AND doc_key = ?`, taskID, docKey).Scan(&maxVer)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	v := 1
	if maxVer.Valid {
		v = int(maxVer.Int32) + 1
	}
	_, err = db.Exec(`INSERT INTO task_documents (task_id, doc_key, version, content) VALUES (?, ?, ?, ?)`, taskID, docKey, v, content)
	return err
}

// ErrInvalidWorkProduct is returned for an unknown work product type or an
// empty reference.
var ErrInvalidWorkProduct = errors.New("invalid work product")

// workProductTypeAliases maps the CLI/API names to task_work_products types.
var workProductTypeAliases = map[string]string{
	"pr":             "pull_request",
	"pull_request":   "pull_request",
	"pull-request":   "pull_request",
	"commit":         "commit",
	"branch":         "branch",
	"doc":            "doc",
	"workspace_file": "workspace_file",
	"workspace-file": "workspace_file",
	"file":           "workspace_file",
}

// WorkProductTypeNames are the type names RegisterWorkProduct accepts, for
// help and error text.
const WorkProductTypeNames = "pr, commit, branch, doc, workspace_file"

// NormalizeWorkProductType maps a CLI/API type name to the stored type.
func NormalizeWorkProductType(t string) (string, bool) {
	v, ok := workProductTypeAliases[strings.ToLower(strings.TrimSpace(t))]
	return v, ok
}

// RegisterWorkProduct records a work product (PR, commit, branch, doc or
// workspace file) for a task from the CLI or API (STA-861), so an interactive
// task can be marked done. taskID may be an id or name, as for GetTask.
func RegisterWorkProduct(db *sql.DB, taskID, productType, reference string) (*TaskWorkProduct, error) {
	task, err := GetTask(db, taskID)
	if err != nil {
		return nil, err
	}
	typ, ok := NormalizeWorkProductType(productType)
	if !ok {
		return nil, fmt.Errorf("%w: type %q must be one of %s", ErrInvalidWorkProduct, productType, WorkProductTypeNames)
	}
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return nil, fmt.Errorf("%w: a reference (--ref) is required", ErrInvalidWorkProduct)
	}
	res, err := db.Exec(`INSERT INTO task_work_products (task_id, product_type, reference) VALUES (?, ?, ?)`, task.ID, typ, reference)
	if err != nil {
		return nil, fmt.Errorf("register work product: %w", err)
	}
	id, _ := res.LastInsertId()
	_ = LogActivity(db, task.ID, "work_product_added", fmt.Sprintf("%s %s", typ, reference))
	var p TaskWorkProduct
	if err := db.QueryRow(`SELECT id, task_id, product_type, reference, created_at FROM task_work_products WHERE id = ?`, id).
		Scan(&p.ID, &p.TaskID, &p.ProductType, &p.Reference, &p.CreatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

func AddWorkProduct(db *sql.DB, taskID, productType, reference string) error {
	_, err := db.Exec(`INSERT INTO task_work_products (task_id, product_type, reference) VALUES (?, ?, ?)`, taskID, productType, reference)
	return err
}

func LogActivity(db *sql.DB, taskID, eventType, details string) error {
	_, err := db.Exec(`INSERT INTO activity_log (task_id, event_type, details) VALUES (?, ?, ?)`, taskID, eventType, details)
	return err
}

func GetLatestTaskDocument(db *sql.DB, taskID, docKey string) (*TaskDocument, error) {
	task, err := GetTask(db, taskID)
	if err != nil {
		return nil, err
	}
	var doc TaskDocument
	query := `SELECT id, task_id, doc_key, version, content, created_at FROM task_documents WHERE task_id = ? AND doc_key = ? ORDER BY version DESC LIMIT 1`
	err = db.QueryRow(query, task.ID, docKey).Scan(&doc.ID, &doc.TaskID, &doc.DocKey, &doc.Version, &doc.Content, &doc.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrDocumentNotFound
	}
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

func GetTaskDocumentRevision(db *sql.DB, taskID, docKey string, version int) (*TaskDocument, error) {
	task, err := GetTask(db, taskID)
	if err != nil {
		return nil, err
	}
	var doc TaskDocument
	query := `SELECT id, task_id, doc_key, version, content, created_at FROM task_documents WHERE task_id = ? AND doc_key = ? AND version = ?`
	err = db.QueryRow(query, task.ID, docKey, version).Scan(&doc.ID, &doc.TaskID, &doc.DocKey, &doc.Version, &doc.Content, &doc.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrDocumentNotFound
	}
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

func ListTaskDocuments(db *sql.DB, taskID string) ([]TaskDocument, error) {
	task, err := GetTask(db, taskID)
	if err != nil {
		return nil, err
	}
	query := `SELECT id, task_id, doc_key, version, content, created_at FROM task_documents WHERE task_id = ? ORDER BY doc_key ASC, version DESC`
	rows, err := db.Query(query, task.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var docs []TaskDocument
	for rows.Next() {
		var doc TaskDocument
		if err := rows.Scan(&doc.ID, &doc.TaskID, &doc.DocKey, &doc.Version, &doc.Content, &doc.CreatedAt); err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return docs, rows.Err()
}

// SetTaskExecutionStage updates the execution stage of a task and sets status accordingly.
// Moving a task with open child tasks to "done" is refused (ErrOpenChildren).
func SetTaskExecutionStage(db *sql.DB, taskID, stage string) error {
	return SetTaskExecutionStageWithOptions(db, taskID, stage, DoneOptions{})
}

// SetTaskExecutionStageWithOptions is SetTaskExecutionStage with the Board override.
//
// Only governance.BoardSettableStages are accepted. status follows the stage:
// done -> done, cancelled -> soft_deleted (as DeleteTask), anything else ->
// active, which also reopens a cancelled task. A backlog task moved straight
// to in_progress (Run Now) passes through todo first, and both steps are
// logged, so a parked task is never run without leaving backlog.
func SetTaskExecutionStageWithOptions(db *sql.DB, taskID, stage string, opts DoneOptions) error {
	task, err := GetTask(db, taskID)
	if err != nil {
		return err
	}
	stage = strings.ToLower(strings.TrimSpace(stage))
	if !governance.IsBoardSettableStage(stage) {
		return fmt.Errorf("%w %q: must be one of %s", ErrInvalidStage, stage, strings.Join(governance.BoardSettableStages, ", "))
	}
	if stage == governance.StageDone && !opts.BoardOverride {
		if err := checkOpenChildren(db, task.ID); err != nil {
			return err
		}
	}
	if RequiresBoardToLeave(task, stage) && !opts.BoardStage {
		return fmt.Errorf("%w: %s was created by an agent and is %s; only the Board can move it to %s", ErrBoardRequired, task.ID, task.ExecutionStage, stage)
	}
	if strings.TrimSpace(task.RepoPath) == "" && governance.IsRunnableStage(stage) {
		return fmt.Errorf("%w: set one with 'staypoint task set-repo %s <path>' first", ErrNoRepo, task.ID)
	}
	if task.ExecutionStage == governance.StageBacklog && stage == governance.StageInProgress {
		if err := writeStage(db, task.ID, governance.StageTodo); err != nil {
			return err
		}
		_ = LogActivity(db, task.ID, "stage_change", "execution stage set to todo (left backlog for Run Now)")
	}
	if err := writeStage(db, task.ID, stage); err != nil {
		return err
	}
	_ = LogActivity(db, task.ID, "stage_change", fmt.Sprintf("execution stage set to %s", stage))
	return nil
}

// SetTaskRepo sets a task's repo_path (absolute, cleaned) and git_branch
// (empty: the repo's current branch). Imported tasks start without a repo
// and cannot leave backlog until one is set.
func SetTaskRepo(db *sql.DB, taskID, repoPath, gitBranch string) (*Task, error) {
	task, err := GetTask(db, taskID)
	if err != nil {
		return nil, err
	}
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" || !filepath.IsAbs(repoPath) {
		return nil, fmt.Errorf("%w: repo path must be absolute, got %q", ErrInvalidRepo, repoPath)
	}
	repoPath = filepath.Clean(repoPath)
	gitBranch = strings.TrimSpace(gitBranch)
	if gitBranch == "" {
		gitBranch = GetCurrentGitBranch(repoPath)
	}
	if _, err := db.Exec(`UPDATE tasks SET repo_path = ?, git_branch = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, repoPath, gitBranch, task.ID); err != nil {
		return nil, fmt.Errorf("set task repo: %w", err)
	}
	_ = LogActivity(db, task.ID, "repo_set", fmt.Sprintf("repo set to %s (%s)", repoPath, gitBranch))
	return GetTask(db, task.ID)
}

func writeStage(db *sql.DB, taskID, stage string) error {
	var query string
	switch stage {
	case governance.StageDone:
		query = `UPDATE tasks SET execution_stage = ?, status = 'done', deleted_at = NULL, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`
	case governance.StageCancelled:
		query = `UPDATE tasks SET execution_stage = ?, status = 'soft_deleted', deleted_at = COALESCE(deleted_at, strftime('%Y-%m-%dT%H:%M:%fZ', 'now')), updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`
	default:
		query = `UPDATE tasks SET execution_stage = ?, status = 'active', deleted_at = NULL, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`
	}
	if _, err := db.Exec(query, stage, taskID); err != nil {
		return fmt.Errorf("failed to set task execution stage: %w", err)
	}
	return nil
}

// GetTaskWorkProducts retrieves all work products associated with a task.
func GetTaskWorkProducts(db *sql.DB, taskID string) ([]TaskWorkProduct, error) {
	query := `SELECT id, task_id, product_type, reference, created_at FROM task_work_products WHERE task_id = ? ORDER BY created_at ASC`
	rows, err := db.Query(query, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var products []TaskWorkProduct
	for rows.Next() {
		var p TaskWorkProduct
		if err := rows.Scan(&p.ID, &p.TaskID, &p.ProductType, &p.Reference, &p.CreatedAt); err != nil {
			return nil, err
		}
		products = append(products, p)
	}
	return products, rows.Err()
}

// GetTaskActivityLog retrieves all activity log entries for a task.
func GetTaskActivityLog(db *sql.DB, taskID string) ([]ActivityLog, error) {
	query := `SELECT id, task_id, event_type, details, created_at FROM activity_log WHERE task_id = ? ORDER BY created_at ASC`
	rows, err := db.Query(query, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []ActivityLog
	for rows.Next() {
		var l ActivityLog
		if err := rows.Scan(&l.ID, &l.TaskID, &l.EventType, &l.Details, &l.CreatedAt); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

// RunStep represents a single step emitted during a task run.
type RunStep struct {
	ID        string  `json:"id"`
	RunID     string  `json:"run_id"`
	TaskID    string  `json:"task_id,omitempty"`
	Seq       int     `json:"seq"`
	ParentSeq *int    `json:"parent_seq,omitempty"`
	Kind      string  `json:"kind"`
	Title     string  `json:"title"`
	Body      *string `json:"body,omitempty"`
	// Command holds the verbatim shell command for run/Bash steps. Title may be a
	// plain-language description; Command is what the expanded body shows.
	Command   string  `json:"command,omitempty"`
	Status    string  `json:"status"`
	StartedAt *string `json:"started_at,omitempty"`
	EndedAt   *string `json:"ended_at,omitempty"`
	CreatedAt string  `json:"created_at"`
}

// ListRunStepsByTask returns all run_steps for a given task, ordered by run start time then seq.
// seq restarts at 1 per run, so ordering by seq alone interleaves steps from different runs.
// We group runs by their earliest step timestamp and order within each run by seq.
func ListRunStepsByTask(db *sql.DB, taskID string) ([]RunStep, error) {
	query := `SELECT id, run_id, COALESCE(task_id,''), seq, parent_seq, kind, title, body, COALESCE(status,''), started_at, ended_at, created_at, COALESCE(command,'')
	          FROM run_steps WHERE task_id = ?
	          ORDER BY MIN(COALESCE(started_at, created_at)) OVER (PARTITION BY run_id) ASC, seq ASC`
	rows, err := db.Query(query, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var steps []RunStep
	for rows.Next() {
		var s RunStep
		if err := rows.Scan(&s.ID, &s.RunID, &s.TaskID, &s.Seq, &s.ParentSeq, &s.Kind, &s.Title, &s.Body, &s.Status, &s.StartedAt, &s.EndedAt, &s.CreatedAt, &s.Command); err != nil {
			return nil, err
		}
		steps = append(steps, s)
	}
	return steps, rows.Err()
}

// RunError represents one failed adapter turn recorded in run_errors.
type RunError struct {
	ID         string `json:"id"`
	RunID      string `json:"run_id"`
	TaskID     string `json:"task_id,omitempty"`
	Turn       int    `json:"turn"`
	ExitCode   int    `json:"exit_code"`
	StderrTail string `json:"stderr_tail"`
	DurationMs int64  `json:"duration_ms"`
	Model      string `json:"model"`
	Adapter    string `json:"adapter"`
	CreatedAt  string `json:"created_at"`
}

func scanRunErrors(rows *sql.Rows) ([]RunError, error) {
	defer rows.Close()
	var errs []RunError
	for rows.Next() {
		var e RunError
		if err := rows.Scan(&e.ID, &e.RunID, &e.TaskID, &e.Turn, &e.ExitCode, &e.StderrTail, &e.DurationMs, &e.Model, &e.Adapter, &e.CreatedAt); err != nil {
			return nil, err
		}
		errs = append(errs, e)
	}
	return errs, rows.Err()
}

// ListRunErrorsByTask returns run_errors for the given task, newest first.
func ListRunErrorsByTask(db *sql.DB, taskID string, limit int) ([]RunError, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.Query(
		`SELECT id, run_id, COALESCE(task_id,''), turn, exit_code, stderr_tail, duration_ms, model, adapter, created_at
		 FROM run_errors WHERE task_id = ? ORDER BY created_at DESC LIMIT ?`,
		taskID, limit,
	)
	if err != nil {
		return nil, err
	}
	return scanRunErrors(rows)
}

// ListAllRunErrors returns run_errors across all tasks, newest first.
// taskID and runID are optional filters; zero string means no filter.
func ListAllRunErrors(db *sql.DB, taskID, runID string, limit int) ([]RunError, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT id, run_id, COALESCE(task_id,''), turn, exit_code, stderr_tail, duration_ms, model, adapter, created_at
	          FROM run_errors`
	var args []any
	var clauses []string
	if taskID != "" {
		clauses = append(clauses, "task_id = ?")
		args = append(args, taskID)
	}
	if runID != "" {
		clauses = append(clauses, "run_id = ?")
		args = append(args, runID)
	}
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	return scanRunErrors(rows)
}

// ListRecentRunErrors returns the most recent run_errors across all tasks.
func ListRecentRunErrors(db *sql.DB, limit int) ([]RunError, error) {
	return ListAllRunErrors(db, "", "", limit)
}

