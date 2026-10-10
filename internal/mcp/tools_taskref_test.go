package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
)

// Every tool takes a reference (STA-1) where it takes a task id.
func TestMCP_TaskGetAcceptsReference(t *testing.T) {
	_, database := setupTestDB(t)
	task, err := meshContext.CreateTaskWithOptions(database, meshContext.TaskCreateOptions{
		Name: "Playwright UI specs", RepoPath: "/tmp/r", GitBranch: "main", AccountRole: "personal",
		Organization: "StayPoint", ExecutionStage: "backlog",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(WithDB(database))

	// Unknown reference: not found, never another task.
	res := s.handleCallTool(t.Context(), CallToolParams{Name: "staypoint_task_get", Arguments: json.RawMessage(`{"task_id":"STA-99"}`)})
	if !res.IsError {
		t.Fatalf("STA-99 resolved: %+v", res)
	}

	res = s.handleCallTool(t.Context(), CallToolParams{Name: "staypoint_task_get", Arguments: json.RawMessage(`{"task_id":"sta-1"}`)})
	if res.IsError || len(res.Content) == 0 {
		t.Fatalf("sta-1: %+v", res)
	}
	text := res.Content[0].Text
	for _, want := range []string{`"id": "` + task.ID + `"`, `"identifier": "STA-1"`, `"url": "http://localhost:41421/STA-1/playwright-ui-specs"`} {
		if !strings.Contains(text, want) {
			t.Errorf("task_get output missing %s:\n%s", want, text)
		}
	}

	// Non-reference arguments pass through byte for byte.
	raw := json.RawMessage(`{"task_id":"task-abc","message":"STA-1"}`)
	if got := s.resolveTaskRefArgs(raw); string(got) != string(raw) {
		t.Fatalf("rewrote non-reference args: %s", got)
	}
}
