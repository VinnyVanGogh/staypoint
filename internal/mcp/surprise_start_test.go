package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
)

// 2026-10-07: task-a1a02cee created task-1a925aed (a prod port) and
// task-88e4350a through this tool and both started at once. An agent-created
// child now always lands in backlog.
func TestToolCallTaskCreateChild_LandsInBacklog(t *testing.T) {
	_, database := setupTestDB(t)
	s := NewServer(WithDB(database))
	defer s.Close()
	parent, err := meshContext.CreateTaskWithOptions(database, meshContext.TaskCreateOptions{Name: "p", RepoPath: "/tmp/repo", GitBranch: "main", AccountRole: "work", Organization: "Managed Solution"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("STAYPOINT_TASK_ID", parent.ID)

	res := callStageTool(t, s, "staypoint_task_create_child", map[string]any{"title": "port it to prod", "work_kind": "coding"})
	if res.IsError {
		t.Fatalf("create child: %s", res.Content[0].Text)
	}
	var child meshContext.Task
	if err := json.Unmarshal([]byte(res.Content[0].Text), &child); err != nil {
		t.Fatal(err)
	}
	if child.ExecutionStage != governance.StageBacklog || child.Origin != meshContext.OriginAgent {
		t.Errorf("agent child: stage %s origin %s, want backlog/agent", child.ExecutionStage, child.Origin)
	}
	// And the agent cannot wake it.
	res = callStageTool(t, s, "staypoint_wake", map[string]any{"task_id": child.ID, "reason": "assignment"})
	if !res.IsError {
		t.Error("wake on the agent's backlog child must be refused")
	}
}

func TestToolCallWake_RefusesHeldOrg(t *testing.T) {
	_, database := setupTestDB(t)
	s := NewServer(WithDB(database))
	defer s.Close()
	task, err := meshContext.CreateTaskWithOptions(database, meshContext.TaskCreateOptions{Name: "t", RepoPath: "/tmp/repo", GitBranch: "main", AccountRole: "work", Organization: "Managed Solution"})
	if err != nil {
		t.Fatal(err)
	}
	if err := governance.SetOrgHold(database, "Managed Solution", true); err != nil {
		t.Fatal(err)
	}
	res := callStageTool(t, s, "staypoint_wake", map[string]any{"task_id": task.ID, "reason": "assignment"})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "held") {
		t.Fatalf("wake in held org must be refused as held: %+v", res)
	}
}
