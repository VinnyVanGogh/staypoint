package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/VinnyVanGogh/staypoint/internal/config"
	"github.com/VinnyVanGogh/staypoint/internal/taskopen"
)

// task-63a9779d: an agent puts a task page in front of the Board (Run Now,
// approve a card) by opening it in the Board's configured browser profile.
// Effect: read. It changes no data; its only side effect is browser tabs on
// the Board's Mac, so it is capped per call and per MCP session.

const (
	// maxTaskOpenURLs caps the task pages one call may open.
	maxTaskOpenURLs = 10
	// maxTaskOpenCalls caps the calls that open tabs in one MCP session (one
	// agent run).
	maxTaskOpenCalls = 3
)

func taskOpenTool() Tool {
	return Tool{
		Name: "staypoint_task_open",
		Description: fmt.Sprintf("Open StayPoint task pages in the Board's browser (configured profile) and return their URLs. "+
			"Use it whenever you need the Board to act on a task: Run Now, approve a Ship Review or interaction card. "+
			"Effect: read (no data change). Only existing tasks' localhost:41421 pages open; every id must exist or nothing opens. "+
			"Max %d tasks per call, %d calls per run.", maxTaskOpenURLs, maxTaskOpenCalls),
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"task_ids": {
					Type:        "array",
					Description: "Task ids (task-1234abcd), references recorded on tasks (STA-775), or exact task names",
					Items:       &Property{Type: "string"},
				},
			},
			Required: []string{"task_ids"},
		},
	}
}

func (s *Server) handleTaskOpen(_ context.Context, rawArgs json.RawMessage) *ToolCallResult {
	var args struct {
		TaskIDs []string `json:"task_ids"`
	}
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return toolError(fmt.Sprintf("invalid arguments: %v", err))
		}
	}
	if len(args.TaskIDs) == 0 {
		return toolError("task_ids is required")
	}
	if len(args.TaskIDs) > maxTaskOpenURLs {
		return toolError(fmt.Sprintf("at most %d tasks per call (got %d)", maxTaskOpenURLs, len(args.TaskIDs)))
	}

	dbConn, err := s.getDB()
	if err != nil {
		return toolError(fmt.Sprintf("database error: %v", err))
	}
	targets, err := taskopen.Resolve(dbConn, args.TaskIDs)
	if err != nil {
		return toolError(err.Error())
	}

	s.mu.Lock()
	if s.taskOpenCalls >= maxTaskOpenCalls {
		s.mu.Unlock()
		return toolError(fmt.Sprintf("staypoint_task_open already opened pages %d times this run; "+
			"put the URLs in your message instead", maxTaskOpenCalls))
	}
	s.taskOpenCalls++
	browser := config.BrowserConfig{}
	if s.cfg != nil {
		browser = s.cfg.Browser
	}
	launch := s.launch
	s.mu.Unlock()
	if launch == nil {
		launch = taskopen.Launch
	}

	home, _ := os.UserHomeDir()
	if err := taskopen.Open(browser, targets, home, launch); err != nil {
		s.mu.Lock()
		s.taskOpenCalls--
		s.mu.Unlock()
		return toolError(err.Error())
	}
	data, err := json.MarshalIndent(map[string]any{"opened": targets}, "", "  ")
	if err != nil {
		return toolError(fmt.Sprintf("json marshal error: %v", err))
	}
	return toolSuccess(string(data))
}
