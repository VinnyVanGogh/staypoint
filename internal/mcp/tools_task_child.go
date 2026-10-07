package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
)

// STA-820: a running planning/architecture agent spawns follow-up tasks with
// their own work_kind; the daemon stores the handoff on the child.

func taskCreateChildTool() Tool {
	kinds := strings.Join(meshContext.ValidWorkKinds(), ", ")
	return Tool{
		Name: "staypoint_task_create_child",
		Description: "Create a follow-up child task of the current task. The child inherits repo, branch, org and project, " +
			"starts parked in backlog (only the Board can start it), and is routed by its own work_kind (" + kinds + "). Put your plan in `handoff`: the daemon " +
			"stores it with a link to this task and your final message, and shows it in the child's first prompt.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"title": {
					Type:        "string",
					Description: "Child task title",
				},
				"work_kind": {
					Type:        "string",
					Description: "Routing kind for the child: " + kinds,
				},
				"handoff": {
					Type:        "string",
					Description: "Plan text handed to the child (defaults to this task's latest 'plan' document)",
				},
				"description": {
					Type:        "string",
					Description: "Optional child task description",
				},
				"parent_id": {
					Type:        "string",
					Description: "Parent task ID (defaults to STAYPOINT_TASK_ID)",
				},
			},
			Required: []string{"title", "work_kind"},
		},
	}
}

func (s *Server) handleTaskCreateChild(ctx context.Context, rawArgs json.RawMessage) *ToolCallResult {
	var args struct {
		Title       string `json:"title"`
		WorkKind    string `json:"work_kind"`
		Handoff     string `json:"handoff"`
		Description string `json:"description"`
		ParentID    string `json:"parent_id"`
	}
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return toolError(fmt.Sprintf("invalid arguments: %v", err))
		}
	}
	parentID := strings.TrimSpace(args.ParentID)
	if parentID == "" {
		parentID = strings.TrimSpace(os.Getenv("STAYPOINT_TASK_ID"))
	}
	if parentID == "" {
		return toolError("parent_id is required (or set STAYPOINT_TASK_ID)")
	}

	dbConn, err := s.getDB()
	if err != nil {
		return toolError(fmt.Sprintf("database error: %v", err))
	}
	// Agents never get the Board's depth override, and an agent-created
	// child always starts in backlog: only the Board can start it.
	child, err := meshContext.CreateChildTask(dbConn, meshContext.ChildTaskOptions{
		ParentID:    parentID,
		Name:        args.Title,
		WorkKind:    strings.TrimSpace(args.WorkKind),
		Handoff:     args.Handoff,
		Description: args.Description,
		Origin:      meshContext.OriginAgent,
	})
	if err != nil {
		return toolError(fmt.Sprintf("create child task: %v", err))
	}
	data, err := json.MarshalIndent(child, "", "  ")
	if err != nil {
		return toolError(fmt.Sprintf("json marshal error: %v", err))
	}
	return toolSuccess(string(data))
}
