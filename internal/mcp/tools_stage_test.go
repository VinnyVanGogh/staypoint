package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
)

func callStageTool(t *testing.T, s *Server, name string, args map[string]any) ToolCallResult {
	t.Helper()
	raw, _ := json.Marshal(args)
	params, _ := json.Marshal(CallToolParams{Name: name, Arguments: raw})
	resp := sendRequest(t, s, Request{JSONRPC: "2.0", ID: makeRawID(name), Method: "tools/call", Params: params})
	return parseToolCallResult(t, resp)
}

func TestToolCallTaskList_HidesLegacyAndFiltersStage(t *testing.T) {
	_, database := setupTestDB(t)
	s := NewServer(WithDB(database))
	defer s.Close()

	native, err := meshContext.CreateTaskWithOptions(database, meshContext.TaskCreateOptions{Name: "Native backlog task", RepoPath: "/tmp/repo", GitBranch: "main", ExecutionStage: "backlog"})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := meshContext.CreateTaskWithOptions(database, meshContext.TaskCreateOptions{Name: "Old legacy task", RepoPath: "/tmp/repo", GitBranch: "main", ExecutionStage: "backlog"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE tasks SET origin = 'legacy' WHERE id = ?`, legacy.ID); err != nil {
		t.Fatal(err)
	}

	res := callStageTool(t, s, "staypoint_task_list", map[string]any{})
	if res.IsError || !strings.Contains(res.Content[0].Text, native.Name) || strings.Contains(res.Content[0].Text, legacy.Name) {
		t.Fatalf("default list must hide legacy: %s", res.Content[0].Text)
	}
	res = callStageTool(t, s, "staypoint_task_list", map[string]any{"include_legacy": true, "stage": "backlog"})
	if res.IsError || !strings.Contains(res.Content[0].Text, legacy.Name) {
		t.Fatalf("include_legacy: %s", res.Content[0].Text)
	}
	res = callStageTool(t, s, "staypoint_task_list", map[string]any{"stage": "todo"})
	if res.IsError || strings.Contains(res.Content[0].Text, native.Name) {
		t.Fatalf("stage=todo must exclude backlog tasks: %s", res.Content[0].Text)
	}
	if res := callStageTool(t, s, "staypoint_task_list", map[string]any{"origin": "nope"}); !res.IsError {
		t.Fatal("bad origin must error")
	}
}

func TestToolCallWake_RefusesBacklog(t *testing.T) {
	_, database := setupTestDB(t)
	s := NewServer(WithDB(database))
	defer s.Close()

	task, err := meshContext.CreateTaskWithOptions(database, meshContext.TaskCreateOptions{Name: "Parked", RepoPath: "/tmp/repo", GitBranch: "main", ExecutionStage: "backlog"})
	if err != nil {
		t.Fatal(err)
	}
	res := callStageTool(t, s, "staypoint_wake", map[string]any{"task_id": task.ID, "reason": "assignment"})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "backlog") {
		t.Fatalf("wake on backlog task must be refused: %+v", res)
	}
}
