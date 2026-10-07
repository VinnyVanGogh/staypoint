package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/router"
)

func callChildTool(t *testing.T, s *Server, name string, args map[string]any) *ToolCallResult {
	t.Helper()
	raw, _ := json.Marshal(args)
	params, _ := json.Marshal(CallToolParams{Name: name, Arguments: raw})
	resp := sendRequest(t, s, Request{JSONRPC: "2.0", ID: makeRawID(name), Method: "tools/call", Params: params})
	res := parseToolCallResult(t, resp)
	return &res
}

// STA-820: the running agent creates a child of its own task (STAYPOINT_TASK_ID).
func TestToolCallTaskCreateChild_InheritsAndStoresHandoff(t *testing.T) {
	_, database := setupTestDB(t)
	s := NewServer(WithDB(database))
	defer s.Close()

	parent, err := meshContext.CreateTaskWithOptions(database, meshContext.TaskCreateOptions{
		Name: "Plan routing", RepoPath: "/tmp/repo", GitBranch: "main", AccountRole: "personal",
		Organization: "acme", Project: "core", WorkKind: "planning",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("STAYPOINT_TASK_ID", parent.ID)

	res := callChildTool(t, s, "staypoint_task_create_child", map[string]any{
		"title": "Implement router chain", "work_kind": "coding", "handoff": "Step 1: wire kinds",
	})
	if res.IsError {
		t.Fatalf("create child error: %s", res.Content[0].Text)
	}
	var child meshContext.Task
	if err := json.Unmarshal([]byte(res.Content[0].Text), &child); err != nil {
		t.Fatalf("decode child: %v\n%s", err, res.Content[0].Text)
	}
	if child.ParentID != parent.ID || child.RepoPath != "/tmp/repo" || child.Organization != "acme" || child.Project != "core" {
		t.Errorf("child not inherited: %+v", child)
	}
	// Until STA-772 wires routing on work_kind, assert the kind is stored.
	if child.WorkKind != "coding" {
		t.Errorf("work_kind: want coding, got %q", child.WorkKind)
	}
	h, _ := meshContext.GetTaskHandoff(database, child.ID)
	if !strings.Contains(h, "Step 1: wire kinds") || !strings.Contains(h, parent.ID) {
		t.Errorf("handoff: %q", h)
	}
}

func TestToolCallTaskCreateChild_RejectsInvalidKindAndMissingParent(t *testing.T) {
	_, database := setupTestDB(t)
	s := NewServer(WithDB(database))
	defer s.Close()
	parent, _ := meshContext.CreateTask(database, "p", "/tmp/repo", "main", "personal")
	t.Setenv("STAYPOINT_TASK_ID", parent.ID)

	if res := callChildTool(t, s, "staypoint_task_create_child", map[string]any{"title": "x", "work_kind": "review"}); !res.IsError {
		t.Error("work_kind review should be rejected")
	}
	if res := callChildTool(t, s, "staypoint_task_create_child", map[string]any{"title": "x"}); !res.IsError {
		t.Error("missing work_kind should be rejected")
	}
	t.Setenv("STAYPOINT_TASK_ID", "")
	if res := callChildTool(t, s, "staypoint_task_create_child", map[string]any{"title": "x", "work_kind": "qa"}); !res.IsError {
		t.Error("no parent should be rejected")
	}
}

func TestToolsListIncludesTaskCreateChild(t *testing.T) {
	s := NewServer()
	defer s.Close()
	for _, tool := range s.getToolsList() {
		if tool.Name == "staypoint_task_create_child" {
			return
		}
	}
	t.Fatal("staypoint_task_create_child not listed")
}

// The context package cannot import router (router imports context), so it
// keeps its own copy of the kinds; this keeps them in step.
func TestWorkKindsMatchRouter(t *testing.T) {
	var want []string
	for _, k := range router.ValidWorkKinds {
		want = append(want, string(k))
	}
	got := meshContext.ValidWorkKinds()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("context kinds %v != router kinds %v", got, want)
	}
}
