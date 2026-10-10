package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
)

type launchCall struct {
	name string
	args []string
}

func taskOpenServer(t *testing.T) (*Server, *[]launchCall, []string) {
	t.Helper()
	_, database := setupTestDB(t)
	s := NewServer(WithDB(database))
	t.Cleanup(func() { _ = s.Close() })
	var calls []launchCall
	s.launch = func(n string, a []string) error {
		calls = append(calls, launchCall{n, a})
		return nil
	}
	var ids []string
	for i := 0; i < maxTaskOpenURLs+1; i++ {
		task, err := meshContext.CreateTaskWithOptions(database, meshContext.TaskCreateOptions{
			Name: fmt.Sprintf("Task %d", i), RepoPath: "/tmp/repo", GitBranch: "main", AccountRole: "personal",
			Organization: "Managed Solution",
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, task.ID)
	}
	return s, &calls, ids
}

func TestToolCallTaskOpen_UnknownIDOpensNothing(t *testing.T) {
	s, calls, ids := taskOpenServer(t)
	res := callChildTool(t, s, "staypoint_task_open", map[string]any{"task_ids": []string{ids[0], "task-00000000"}})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "task-00000000") {
		t.Fatalf("want error naming the unknown id, got %+v", res)
	}
	if len(*calls) != 0 {
		t.Fatalf("browser launched: %+v", *calls)
	}
}

func TestToolCallTaskOpen_TooManyURLsRefused(t *testing.T) {
	s, calls, ids := taskOpenServer(t)
	res := callChildTool(t, s, "staypoint_task_open", map[string]any{"task_ids": ids})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "at most") {
		t.Fatalf("want per-call cap error, got %+v", res)
	}
	if len(*calls) != 0 {
		t.Fatalf("browser launched: %+v", *calls)
	}
}

func TestToolCallTaskOpen_CallsPerRunCapped(t *testing.T) {
	s, calls, ids := taskOpenServer(t)
	for i := 0; i < maxTaskOpenCalls; i++ {
		res := callChildTool(t, s, "staypoint_task_open", map[string]any{"task_ids": []string{ids[i]}})
		if res.IsError {
			t.Fatalf("call %d: %s", i, res.Content[0].Text)
		}
	}
	res := callChildTool(t, s, "staypoint_task_open", map[string]any{"task_ids": []string{ids[0]}})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "this run") {
		t.Fatalf("want per-run cap error, got %+v", res)
	}
	if len(*calls) != maxTaskOpenCalls {
		t.Fatalf("launches = %d, want %d", len(*calls), maxTaskOpenCalls)
	}
}

// Refused calls (unknown ids) do not use up the per-run budget.
func TestToolCallTaskOpen_FailedCallsDoNotCount(t *testing.T) {
	s, _, ids := taskOpenServer(t)
	for i := 0; i < maxTaskOpenCalls+2; i++ {
		callChildTool(t, s, "staypoint_task_open", map[string]any{"task_ids": []string{"nope"}})
	}
	res := callChildTool(t, s, "staypoint_task_open", map[string]any{"task_ids": []string{ids[0]}})
	if res.IsError {
		t.Fatalf("valid call refused after failed ones: %s", res.Content[0].Text)
	}
}

func TestToolCallTaskOpen_OpensAndReturnsURLs(t *testing.T) {
	s, calls, ids := taskOpenServer(t)
	res := callChildTool(t, s, "staypoint_task_open", map[string]any{"task_ids": ids[:2]})
	if res.IsError {
		t.Fatal(res.Content[0].Text)
	}
	var out struct {
		Opened []struct{ ID, URL string }
	}
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Opened) != 2 || out.Opened[0].URL != "http://localhost:41421/tasks/MAN/default/"+ids[0] {
		t.Fatalf("opened = %+v", out.Opened)
	}
	// No [browser] config: the default browser via `open`, one call, ids as
	// separate argv elements.
	if len(*calls) != 1 || (*calls)[0].name != "open" || len((*calls)[0].args) != 2 {
		t.Fatalf("launch = %+v", *calls)
	}
}

func TestToolsList_IncludesTaskOpenWithArraySchema(t *testing.T) {
	for _, tool := range NewServer().getToolsList() {
		if tool.Name == "staypoint_task_open" {
			p := tool.InputSchema.Properties["task_ids"]
			if p.Type != "array" || p.Items == nil || p.Items.Type != "string" {
				t.Fatalf("task_ids schema = %+v", p)
			}
			return
		}
	}
	t.Fatal("staypoint_task_open not listed")
}
