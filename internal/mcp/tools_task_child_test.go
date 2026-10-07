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

	if res := callChildTool(t, s, "staypoint_task_create_child", map[string]any{"title": "x", "work_kind": "deploy"}); !res.IsError {
		t.Error("work_kind deploy should be rejected")
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
	// Kinds context accepts ahead of the router. Delete an entry once its PR
	// merges; the test then requires an exact match for it.
	pendingInRouter := map[string]string{"review": "STA-772 / PR #213"}

	routerKinds := map[string]bool{}
	for _, k := range router.ValidWorkKinds {
		routerKinds[string(k)] = true
	}
	contextKinds := map[string]bool{}
	for _, k := range meshContext.ValidWorkKinds() {
		contextKinds[k] = true
	}
	for k := range routerKinds {
		if !contextKinds[k] {
			t.Errorf("router kind %q missing from context.validWorkKinds (internal/context/child_tasks.go)", k)
		}
	}
	for k := range contextKinds {
		if routerKinds[k] {
			continue
		}
		if pr, ok := pendingInRouter[k]; ok {
			t.Logf("kind %q not in router yet (pending %s)", k, pr)
			continue
		}
		t.Errorf("context kind %q is not a router kind (internal/router/kinds.go)", k)
	}
}

// STA-859: the MCP tool has no depth override. An agent that smuggles
// allow_deep / override / board_override into the arguments still hits the cap.
func TestToolCallTaskCreateChild_CannotDeepCreate(t *testing.T) {
	_, database := setupTestDB(t)
	s := NewServer(WithDB(database))
	defer s.Close()

	if err := meshContext.SetChildTaskLimits(database, 5, 1); err != nil {
		t.Fatal(err)
	}
	root, err := meshContext.CreateTaskWithOptions(database, meshContext.TaskCreateOptions{
		Name: "root", RepoPath: "/tmp/repo", GitBranch: "main", AccountRole: "personal", WorkKind: "planning",
	})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := meshContext.CreateChildTask(database, meshContext.ChildTaskOptions{ParentID: root.ID, Name: "d1", WorkKind: "coding"})
	if err != nil {
		t.Fatal(err)
	}

	for _, prop := range []string{"allow_deep", "override", "board_override"} {
		if _, ok := taskCreateChildTool().InputSchema.Properties[prop]; ok {
			t.Errorf("MCP child tool must not expose %q", prop)
		}
	}

	res := callChildTool(t, s, "staypoint_task_create_child", map[string]any{
		"title": "d2", "work_kind": "coding", "parent_id": leaf.ID,
		"allow_deep": true, "override": true, "board_override": true, "BoardOverride": true,
	})
	if !res.IsError {
		t.Fatalf("MCP deep create past the cap must fail, got: %s", res.Content[0].Text)
	}
	if !strings.Contains(strings.ToLower(res.Content[0].Text), "depth") {
		t.Errorf("want a depth-cap error, got: %s", res.Content[0].Text)
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM tasks WHERE parent_id = ?`, leaf.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("deep child created via MCP: n=%d err=%v", n, err)
	}
}
