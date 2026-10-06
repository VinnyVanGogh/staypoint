package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/checkpoint"
	"github.com/VinnyVanGogh/staypoint/internal/condenser"
	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/decision"
	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
	"github.com/VinnyVanGogh/staypoint/internal/wire"
)

func toolSuccess(text string) *ToolCallResult {
	return &ToolCallResult{
		Content: []ToolContent{
			{
				Type: "text",
				Text: text,
			},
		},
		IsError: false,
	}
}

func toolError(msg string) *ToolCallResult {
	return &ToolCallResult{
		Content: []ToolContent{
			{
				Type: "text",
				Text: msg,
			},
		},
		IsError: true,
	}
}

func (s *Server) getToolsList() []Tool {
	return []Tool{
		{
			Name:        "staypoint_checkpoint",
			Description: "take ephemeral git micro-checkpoint",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"message": {
						Type:        "string",
						Description: "Optional label or description for the checkpoint",
					},
					"session_id": {
						Type:        "string",
						Description: "Optional agent session identifier",
					},
				},
			},
		},
		{
			Name:        "staypoint_undo",
			Description: "restore tree to latest or specific checkpoint",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"checkpoint_id": {
						Type:        "string",
						Description: "Target checkpoint ID, commit SHA, or empty for latest",
					},
					"dry_run": {
						Type:        "boolean",
						Description: "Preview changes without modifying the working tree",
					},
					"keep_untracked": {
						Type:        "boolean",
						Description: "Do not delete untracked files created after checkpoint",
					},
					"clean_ignored": {
						Type:        "boolean",
						Description: "Remove ignored files during restoration",
					},
				},
			},
		},
		{
			Name:        "staypoint_wire_post",
			Description: "broadcast a message on Mesh Wire",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"content": {
						Type:        "string",
						Description: "Message content to broadcast",
					},
					"channel": {
						Type:        "string",
						Description: "Channel name, defaults to global",
					},
					"ttl_seconds": {
						Type:        "integer",
						Description: "Time to live in seconds, defaults to 86400",
					},
				},
				Required: []string{"content"},
			},
		},
		{
			Name:        "staypoint_wire_list",
			Description: "list recent wire messages",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"channel": {
						Type:        "string",
						Description: "Filter messages by channel name",
					},
					"limit": {
						Type:        "integer",
						Description: "Maximum number of messages to return",
					},
				},
			},
		},
		{
			Name:        "staypoint_task_list",
			Description: "list active tasks and budget spend meters",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"all": {
						Type:        "boolean",
						Description: "Include completed and soft-deleted tasks",
					},
				},
			},
		},
		{
			Name:        "staypoint_condense",
			Description: "condense compiler errors / stack traces",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"raw_text": {
						Type:        "string",
						Description: "Raw compiler output, stack trace, or log dump",
					},
					"format": {
						Type:        "string",
						Description: "Target format: auto, typescript, go, python, generic",
					},
					"max_lines": {
						Type:        "integer",
						Description: "Maximum lines allowed in condensed output",
					},
				},
				Required: []string{"raw_text"},
			},
		},
		{
			Name:        "staypoint_status",
			Description: "return rate limit quotas, active models, and pacer status",
			InputSchema: InputSchema{
				Type:       "object",
				Properties: map[string]Property{},
			},
		},
		{
			Name:        "staypoint_wake",
			Description: "trigger an event-driven wake for an agent task",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"task_id": {
						Type:        "string",
						Description: "ID of the task to wake",
					},
					"reason": {
						Type:        "string",
						Description: "Reason for the wake (e.g. assignment, comment, blocker_cleared)",
					},
					"idempotency_key": {
						Type:        "string",
						Description: "Unique key to prevent duplicate wakes for the same event",
					},
				},
				Required: []string{"task_id", "reason"},
			},
		},
		{
			Name:        "staypoint_create_interaction",
			Description: "raise an interaction card on a task (ask_user_questions, request_confirmation, suggest_tasks)",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"task_id": {
						Type:        "string",
						Description: "ID of the task to attach the interaction to",
					},
					"kind": {
						Type:        "string",
						Description: "Interaction kind: ask_user_questions, request_confirmation, or suggest_tasks",
					},
					"payload": {
						Type:        "string",
						Description: "JSON payload for the interaction card",
					},
					"idempotency_key": {
						Type:        "string",
						Description: "Optional unique key to prevent duplicate cards",
					},
				},
				Required: []string{"task_id", "kind"},
			},
		},
		{
			Name:        "staypoint_task_get",
			Description: "get the current task brief: name, org/project, repo, branch, description, and all board/user comments",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"task_id": {
						Type:        "string",
						Description: "Task ID to fetch (defaults to STAYPOINT_TASK_ID env var)",
					},
				},
			},
		},
		{
			Name:        "staypoint_ship_review",
			Description: "Create or refresh a Ship Review card so the Board can approve and merge your branch. Call this when your branch is ready for review. Requires a numbered test list. Files changed are populated automatically from git diff. Use check_runs to attach CI/verification evidence.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"task_id": {
						Type:        "string",
						Description: "ID of the current task (leave empty to use STAYPOINT_TASK_ID env var)",
					},
					"test_steps": {
						Type:        "string",
						Description: `JSON array of numbered test instructions, e.g. ["1. Run npm test","2. Open /dashboard and verify X"]`,
					},
					"dev_url": {
						Type:        "string",
						Description: "Optional loopback URL for the running dev server, e.g. http://localhost:3000",
					},
					"check_runs": {
						Type:        "string",
						Description: `Optional JSON array of check run results: [{"command":"go test ./...","exit_code":0,"output_tail":"ok  ..."}, ...]`,
					},
				},
				Required: []string{"test_steps"},
			},
		},
	}
}

func (s *Server) handleCallTool(ctx context.Context, params CallToolParams) *ToolCallResult {
	switch params.Name {
	case "staypoint_checkpoint":
		return s.handleCheckpoint(ctx, params.Arguments)
	case "staypoint_undo":
		return s.handleUndo(ctx, params.Arguments)
	case "staypoint_wire_post":
		return s.handleWirePost(ctx, params.Arguments)
	case "staypoint_wire_list":
		return s.handleWireList(ctx, params.Arguments)
	case "staypoint_task_list":
		return s.handleTaskList(ctx, params.Arguments)
	case "staypoint_condense":
		return s.handleCondense(ctx, params.Arguments)
	case "staypoint_status":
		return s.handleStatus(ctx, params.Arguments)
	case "staypoint_wake":
		return s.handleWake(ctx, params.Arguments)
	case "staypoint_create_interaction":
		return s.handleCreateInteraction(ctx, params.Arguments)
	case "staypoint_task_get":
		return s.handleTaskGet(ctx, params.Arguments)
	case "staypoint_ship_review":
		return s.handleShipReview(ctx, params.Arguments)
	default:
		return toolError(fmt.Sprintf("unknown tool: %s", params.Name))
	}
}

func (s *Server) handleCheckpoint(ctx context.Context, rawArgs json.RawMessage) *ToolCallResult {
	var args struct {
		Message   string `json:"message"`
		SessionID string `json:"session_id"`
	}
	if len(rawArgs) > 0 {
		_ = json.Unmarshal(rawArgs, &args)
	}

	cp, err := checkpoint.CreateCheckpoint(ctx, checkpoint.CreateOptions{
		WorkDir:   s.getWorkDir(),
		SessionID: args.SessionID,
		Message:   args.Message,
	})
	if err != nil {
		return toolError(fmt.Sprintf("checkpoint error: %v", err))
	}

	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return toolError(fmt.Sprintf("json marshal error: %v", err))
	}
	return toolSuccess(string(data))
}

// undoCleanTimeout bounds staypoint_undo's removal of ignored files.
const undoCleanTimeout = 5 * time.Minute

func (s *Server) handleUndo(ctx context.Context, rawArgs json.RawMessage) *ToolCallResult {
	var args struct {
		CheckpointID  string `json:"checkpoint_id"`
		DryRun        bool   `json:"dry_run"`
		KeepUntracked bool   `json:"keep_untracked"`
		CleanIgnored  bool   `json:"clean_ignored"`
	}
	if len(rawArgs) > 0 {
		_ = json.Unmarshal(rawArgs, &args)
	}

	res, err := checkpoint.Undo(ctx, checkpoint.UndoOptions{
		WorkDir:       s.getWorkDir(),
		CheckpointID:  args.CheckpointID,
		DryRun:        args.DryRun,
		KeepUntracked: args.KeepUntracked,
	})
	if err != nil {
		return toolError(fmt.Sprintf("undo error: %v", err))
	}

	if args.CleanIgnored {
		if args.DryRun {
			cmd := gitexec.Command(ctx, "clean", "-n", "-X", "-d")
			cmd.Dir = s.getWorkDir()
			if out, cleanErr := cmd.Output(); cleanErr == nil && len(out) > 0 {
				res.DiffStat += "\nIgnored files to clean:\n" + string(out)
			}
		} else {
			// Deleting a large ignored tree (node_modules, build output) can
			// take far longer than gitexec's default, and a clean cut off
			// partway leaves the tree half-removed.
			cleanCtx, cancel := context.WithTimeout(ctx, undoCleanTimeout)
			cmd := gitexec.Command(cleanCtx, "clean", "-f", "-X", "-d")
			cmd.Dir = s.getWorkDir()
			_ = cmd.Run()
			cancel()
		}
	}

	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return toolError(fmt.Sprintf("json marshal error: %v", err))
	}
	return toolSuccess(string(data))
}

func (s *Server) handleWirePost(ctx context.Context, rawArgs json.RawMessage) *ToolCallResult {
	var args struct {
		Content    string `json:"content"`
		Channel    string `json:"channel"`
		TTLSeconds int    `json:"ttl_seconds"`
	}
	if len(rawArgs) > 0 {
		_ = json.Unmarshal(rawArgs, &args)
	}

	if strings.TrimSpace(args.Content) == "" {
		return toolError("content is required")
	}

	channel := args.Channel
	if channel == "" {
		channel = "global"
	}
	ttl := args.TTLSeconds
	if ttl <= 0 {
		ttl = 86400
	}

	dbConn, err := s.getDB()
	if err != nil {
		return toolError(fmt.Sprintf("database error: %v", err))
	}

	author := os.Getenv("USER")
	if author == "" {
		author = "agent"
	}

	msg, err := wire.Post(dbConn, channel, author, s.getWorkDir(), args.Content, ttl)
	if err != nil {
		return toolError(fmt.Sprintf("wire post error: %v", err))
	}

	data, err := json.MarshalIndent(msg, "", "  ")
	if err != nil {
		return toolError(fmt.Sprintf("json marshal error: %v", err))
	}
	return toolSuccess(string(data))
}

func (s *Server) handleWireList(ctx context.Context, rawArgs json.RawMessage) *ToolCallResult {
	var args struct {
		Channel string `json:"channel"`
		Limit   int    `json:"limit"`
	}
	if len(rawArgs) > 0 {
		_ = json.Unmarshal(rawArgs, &args)
	}

	limit := args.Limit
	if limit <= 0 {
		limit = 20
	}

	dbConn, err := s.getDB()
	if err != nil {
		return toolError(fmt.Sprintf("database error: %v", err))
	}

	msgs, err := wire.List(dbConn, args.Channel, limit)
	if err != nil {
		return toolError(fmt.Sprintf("wire list error: %v", err))
	}
	if msgs == nil {
		msgs = []wire.Message{}
	}

	data, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		return toolError(fmt.Sprintf("json marshal error: %v", err))
	}
	return toolSuccess(string(data))
}

func (s *Server) handleTaskList(ctx context.Context, rawArgs json.RawMessage) *ToolCallResult {
	var args struct {
		All bool `json:"all"`
	}
	if len(rawArgs) > 0 {
		_ = json.Unmarshal(rawArgs, &args)
	}

	dbConn, err := s.getDB()
	if err != nil {
		return toolError(fmt.Sprintf("database error: %v", err))
	}

	tasks, err := meshContext.ListTasks(dbConn, args.All)
	if err != nil {
		return toolError(fmt.Sprintf("task list error: %v", err))
	}
	if tasks == nil {
		tasks = []meshContext.Task{}
	}

	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return toolError(fmt.Sprintf("json marshal error: %v", err))
	}
	return toolSuccess(string(data))
}

func (s *Server) handleCondense(ctx context.Context, rawArgs json.RawMessage) *ToolCallResult {
	var args struct {
		RawText  string `json:"raw_text"`
		Format   string `json:"format"`
		MaxLines int    `json:"max_lines"`
	}
	if len(rawArgs) > 0 {
		_ = json.Unmarshal(rawArgs, &args)
	}

	if strings.TrimSpace(args.RawText) == "" {
		return toolError("raw_text is required")
	}

	maxLines := args.MaxLines
	if maxLines <= 0 {
		maxLines = 100
	}
	formatStr := args.Format
	if formatStr == "" {
		formatStr = "auto"
	}

	res, err := condenser.Condense(args.RawText, condenser.CondenseOptions{
		Format:      condenser.Format(formatStr),
		MaxLines:    maxLines,
		ShowSavings: true,
	})
	if err != nil {
		return toolError(fmt.Sprintf("condense error: %v", err))
	}

	return toolSuccess(res.Condensed)
}

func (s *Server) handleStatus(ctx context.Context, rawArgs json.RawMessage) *ToolCallResult {
	pacerState, _ := router.LoadPacerState()
	workDir := s.getWorkDir()

	routeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	remoteHost := "company-mbp"
	if s.cfg != nil && s.cfg.RemoteHost != "" {
		remoteHost = s.cfg.RemoteHost
	}

	decision, _ := router.Route(routeCtx, workDir, pacerState, router.RouteOptions{
		CheckSSH:   false,
		RemoteHost: remoteHost,
		UIOLI:      router.UIOLIFromConfig(s.cfg),
	})

	statusData := map[string]any{
		"time":              time.Now().Format("03:04 PM MST"),
		"pacer_state":       pacerState,
		"recommended_route": decision,
		"working_directory": workDir,
	}
	if s.cfg != nil {
		statusData["db_path"] = s.cfg.DBPath
	}

	data, err := json.MarshalIndent(statusData, "", "  ")
	if err != nil {
		return toolError(fmt.Sprintf("status marshal error: %v", err))
	}
	return toolSuccess(string(data))
}

func (s *Server) handleWake(ctx context.Context, rawArgs json.RawMessage) *ToolCallResult {
	var args struct {
		TaskID         string `json:"task_id"`
		Reason         string `json:"reason"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if len(rawArgs) > 0 {
		_ = json.Unmarshal(rawArgs, &args)
	}

	if args.TaskID == "" || args.Reason == "" {
		return toolError("task_id and reason are required")
	}

	orchestrator.GlobalDispatcher.Wake(args.TaskID, args.Reason, args.IdempotencyKey)

	return toolSuccess("wake dispatched")
}

func (s *Server) handleCreateInteraction(ctx context.Context, rawArgs json.RawMessage) *ToolCallResult {
	var args struct {
		TaskID         string `json:"task_id"`
		Kind           string `json:"kind"`
		Payload        string `json:"payload"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if len(rawArgs) > 0 {
		_ = json.Unmarshal(rawArgs, &args)
	}

	if strings.TrimSpace(args.TaskID) == "" || strings.TrimSpace(args.Kind) == "" {
		return toolError("task_id and kind are required")
	}

	dbConn, err := s.getDB()
	if err != nil {
		return toolError(fmt.Sprintf("database error: %v", err))
	}

	in := &meshContext.TaskInteraction{
		TaskID:          args.TaskID,
		InteractionKind: args.Kind,
		Payload:         args.Payload,
		IdempotencyKey:  args.IdempotencyKey,
	}

	created, err := meshContext.CreateInteraction(dbConn, in)
	if err != nil {
		return toolError(fmt.Sprintf("create interaction error: %v", err))
	}

	t, _ := meshContext.GetTask(dbConn, created.TaskID)
	taskCtx := ""
	if t != nil {
		taskCtx = "Task Goal: " + t.Name + "\n" + t.Description
	}
	decision.GenerateInteractionSuggestion(dbConn, created.ID, created.TaskID, created.InteractionKind, created.Payload, taskCtx)

	data, err := json.MarshalIndent(created, "", "  ")
	if err != nil {
		return toolError(fmt.Sprintf("json marshal error: %v", err))
	}
	return toolSuccess(string(data))
}

func (s *Server) handleTaskGet(ctx context.Context, rawArgs json.RawMessage) *ToolCallResult {
	var args struct {
		TaskID string `json:"task_id"`
	}
	if len(rawArgs) > 0 {
		_ = json.Unmarshal(rawArgs, &args)
	}
	if args.TaskID == "" {
		args.TaskID = os.Getenv("STAYPOINT_TASK_ID")
	}
	if args.TaskID == "" {
		return toolError("task_id is required (or set STAYPOINT_TASK_ID)")
	}

	dbConn, err := s.getDB()
	if err != nil {
		return toolError(fmt.Sprintf("database error: %v", err))
	}

	task, err := meshContext.GetTask(dbConn, args.TaskID)
	if err != nil {
		return toolError(fmt.Sprintf("task not found: %v", err))
	}

	// Filter comments to non-harness only.
	var userComments []meshContext.TaskComment
	for _, c := range task.Comments {
		if c.Author != "harness" {
			userComments = append(userComments, c)
		}
	}

	result := map[string]any{
		"id":          task.ID,
		"name":        task.Name,
		"org":         task.Organization,
		"project":     task.Project,
		"repo_path":   task.RepoPath,
		"git_branch":  task.GitBranch,
		"description": task.Description,
		"comments":    userComments,
	}

	out, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return toolError(fmt.Sprintf("marshal error: %v", err))
	}
	return toolSuccess(string(out))
}

func (s *Server) handleShipReview(ctx context.Context, rawArgs json.RawMessage) *ToolCallResult {
	var args struct {
		TaskID     string `json:"task_id"`
		TestSteps  string `json:"test_steps"`
		DevURL     string `json:"dev_url"`
		CheckRuns  string `json:"check_runs"`
	}
	if len(rawArgs) > 0 {
		_ = json.Unmarshal(rawArgs, &args)
	}

	// Resolve task ID: explicit arg > env var.
	taskID := strings.TrimSpace(args.TaskID)
	if taskID == "" {
		taskID = strings.TrimSpace(os.Getenv("STAYPOINT_TASK_ID"))
	}
	if taskID == "" {
		return toolError("task_id is required (or set STAYPOINT_TASK_ID)")
	}

	// Parse test_steps (JSON array string).
	if strings.TrimSpace(args.TestSteps) == "" {
		return toolError("test_steps is required (JSON array of strings)")
	}
	var testSteps []string
	if err := json.Unmarshal([]byte(args.TestSteps), &testSteps); err != nil {
		return toolError(fmt.Sprintf("test_steps must be a JSON array of strings: %v", err))
	}
	if len(testSteps) == 0 {
		return toolError("test_steps must not be empty")
	}

	// Parse check_runs (optional JSON array).
	var checkRuns []shipreview.CheckRun
	if cr := strings.TrimSpace(args.CheckRuns); cr != "" {
		if err := json.Unmarshal([]byte(cr), &checkRuns); err != nil {
			return toolError(fmt.Sprintf("check_runs must be a JSON array: %v", err))
		}
	}

	dbConn, err := s.getDB()
	if err != nil {
		return toolError(fmt.Sprintf("database error: %v", err))
	}

	task, err := meshContext.GetTask(dbConn, taskID)
	if err != nil {
		return toolError(fmt.Sprintf("task not found: %v", err))
	}

	// BuildAndStartCard always pins staypoint/<taskID>, never task.GitBranch
	// (which is the repo's branch at creation time, usually "main").
	card, err := shipreview.BuildAndStartCard(ctx, dbConn, task.ID, task.RepoPath, testSteps, args.DevURL, checkRuns)
	if err != nil {
		return toolError(fmt.Sprintf("create ship review card: %v", err))
	}

	data, err := json.MarshalIndent(card, "", "  ")
	if err != nil {
		return toolError(fmt.Sprintf("json marshal error: %v", err))
	}
	return toolSuccess(fmt.Sprintf("Ship Review card created.\n\n%s\n\nThe Board can now approve, send back, or reject this branch.", string(data)))
}
