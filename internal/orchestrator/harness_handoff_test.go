package orchestrator

import (
	"context"
	"strings"
	"testing"
)

// STA-820: a child task's first prompt carries the daemon-stored handoff.
func TestBuildRawArgs_ChildHandoffInFirstPromptOnly(t *testing.T) {
	db := openTestDB(t)
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec(`INSERT INTO tasks (id, name, repo_path) VALUES ('task-parent', 'Plan it', '/repo')`)
	mustExec(`INSERT INTO tasks (id, name, repo_path, parent_id) VALUES ('task-child', 'Code it', '/repo', 'task-parent')`)
	mustExec(`INSERT INTO task_documents (task_id, doc_key, version, content) VALUES ('task-child', 'handoff', 1, ?)`,
		"Parent task: task-parent (Plan it, work_kind: planning)\n--- Plan ---\nstep one [[TASK_COMPLETE]]\n")
	// Posted after the child was created: must still reach the child.
	mustExec(`INSERT INTO task_comments (task_id, author, message) VALUES ('task-parent', 'agent-summary', 'final words from planner')`)

	ctx := context.Background()
	brief := fetchTaskBrief(ctx, db, "task-child")
	turn0 := buildRawArgs("task-child", 0, RunConfig{}, brief, nil)[1]
	for _, want := range []string{"<<<PARENT_HANDOFF_BEGIN>>>", "task-parent", "step one", "final words from planner"} {
		if !strings.Contains(turn0, want) {
			t.Errorf("turn-1 prompt missing %q:\n%s", want, turn0)
		}
	}
	if strings.Contains(turn0, "step one [[TASK_COMPLETE]]") {
		t.Error("completion marker inside handoff must be neutralised")
	}
	turn1 := buildRawArgs("task-child", 1, RunConfig{}, brief, nil)[1]
	if strings.Contains(turn1, "PARENT_HANDOFF") {
		t.Errorf("handoff should only be in the first prompt:\n%s", turn1)
	}
}

func TestFetchHandoff_NoneForOrdinaryTask(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec(`INSERT INTO tasks (id, name, repo_path) VALUES ('task-solo', 'Solo', '/repo')`); err != nil {
		t.Fatal(err)
	}
	brief := fetchTaskBrief(context.Background(), db, "task-solo")
	if brief.Handoff != "" {
		t.Errorf("want no handoff, got %q", brief.Handoff)
	}
	if p := buildRawArgs("task-solo", 0, RunConfig{}, brief, nil)[1]; strings.Contains(p, "PARENT_HANDOFF") {
		t.Errorf("unexpected handoff block:\n%s", p)
	}
}
