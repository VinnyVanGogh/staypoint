package context

import (
	"errors"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/governance"
)

// Surprise starts (2026-10-07).

func TestCreateChildTask_HonoursBacklog(t *testing.T) {
	database := setupTestDB(t)
	parent, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "p", RepoPath: "/tmp/r", GitBranch: "main", AccountRole: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := CreateChildTask(database, ChildTaskOptions{ParentID: parent.ID, Name: "c", WorkKind: "coding", ExecutionStage: "backlog"})
	if err != nil {
		t.Fatal(err)
	}
	if child.ExecutionStage != governance.StageBacklog {
		t.Fatalf("child stage = %s, want backlog", child.ExecutionStage)
	}
	if commentWakes(database, child.ID) {
		t.Error("a comment must not wake a backlog child")
	}
}

func TestCreateTask_AgentOriginAlwaysBacklog(t *testing.T) {
	database := setupTestDB(t)
	for _, stage := range []string{"", "todo", "in_progress", "blocked"} {
		task, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "a", RepoPath: "/tmp/r", GitBranch: "main", AccountRole: "personal", ExecutionStage: stage, Origin: OriginAgent})
		if err != nil {
			t.Fatalf("stage %q: %v", stage, err)
		}
		if task.ExecutionStage != governance.StageBacklog || task.Origin != OriginAgent {
			t.Errorf("agent asked %q: got stage %s origin %s, want backlog/agent", stage, task.ExecutionStage, task.Origin)
		}
	}
	parent, _ := CreateTaskWithOptions(database, TaskCreateOptions{Name: "p", RepoPath: "/tmp/r", GitBranch: "main", AccountRole: "personal"})
	child, err := CreateChildTask(database, ChildTaskOptions{ParentID: parent.ID, Name: "c", WorkKind: "coding", ExecutionStage: "todo", Origin: OriginAgent})
	if err != nil {
		t.Fatal(err)
	}
	if child.ExecutionStage != governance.StageBacklog {
		t.Errorf("agent child stage = %s, want backlog", child.ExecutionStage)
	}
}

func TestSetStage_AgentTaskNeedsBoard(t *testing.T) {
	database := setupTestDB(t)
	task, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "a", RepoPath: "/tmp/r", GitBranch: "main", AccountRole: "personal", Origin: OriginAgent})
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"todo", "in_progress", "in_review"} {
		if err := SetTaskExecutionStageWithOptions(database, task.ID, stage, DoneOptions{}); !errors.Is(err, ErrBoardRequired) {
			t.Errorf("no Board -> %s: err = %v, want ErrBoardRequired", stage, err)
		}
	}
	if got, _ := GetTask(database, task.ID); got.ExecutionStage != governance.StageBacklog {
		t.Fatalf("refused move changed the stage to %s", got.ExecutionStage)
	}
	// Closing it needs no Board; reopening from cancelled to todo does.
	if err := SetTaskExecutionStageWithOptions(database, task.ID, "cancelled", DoneOptions{}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := SetTaskExecutionStageWithOptions(database, task.ID, "todo", DoneOptions{}); !errors.Is(err, ErrBoardRequired) {
		t.Errorf("cancelled -> todo without Board: err = %v", err)
	}
	if err := SetTaskExecutionStageWithOptions(database, task.ID, "todo", DoneOptions{BoardStage: true}); err != nil {
		t.Fatalf("Board -> todo: %v", err)
	}
	// Native tasks are not gated.
	native, _ := CreateTaskWithOptions(database, TaskCreateOptions{Name: "n", RepoPath: "/tmp/r", GitBranch: "main", AccountRole: "personal", ExecutionStage: "backlog"})
	if err := SetTaskExecutionStageWithOptions(database, native.ID, "todo", DoneOptions{}); err != nil {
		t.Errorf("native backlog -> todo: %v", err)
	}
}

// #234 re-check: a comment never wakes a task in a held organization, and
// the refusal is logged as held.
func TestCommentWakes_HeldOrgStaysQuiet(t *testing.T) {
	database := setupTestDB(t)
	task, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "c", RepoPath: "/tmp/r", GitBranch: "main", AccountRole: "work", Organization: "Managed Solution"})
	if err != nil {
		t.Fatal(err)
	}
	if !commentWakes(database, task.ID) {
		t.Fatal("todo task, no hold: a comment should wake it")
	}
	if err := governance.SetOrgHold(database, "Managed Solution", true); err != nil {
		t.Fatal(err)
	}
	if commentWakes(database, task.ID) {
		t.Fatal("held org: a comment must not wake the task")
	}
	if err := AddTaskComment(database, task.ID, "board", "please continue"); err != nil {
		t.Fatalf("comment on held task: %v", err)
	}
	var n int
	_ = database.QueryRow(`SELECT COUNT(*) FROM activity_log WHERE task_id = ? AND event_type = 'wake_held'`, task.ID).Scan(&n)
	if n == 0 {
		t.Error("held comment wake was not logged as held")
	}
	if err := governance.SetOrgHold(database, "Managed Solution", false); err != nil {
		t.Fatal(err)
	}
	if !commentWakes(database, task.ID) {
		t.Error("hold lifted: a comment should wake it again")
	}
}

func TestNameTargetsProd(t *testing.T) {
	for name, want := range map[string]bool{
		"port the fix to prod":     true,
		"Production deploy":        true,
		"PROD hotfix":              true,
		"product page copy":        false,
		"reproduce the flaky test": false,
	} {
		if got := NameTargetsProd(name); got != want {
			t.Errorf("NameTargetsProd(%q) = %v, want %v", name, got, want)
		}
	}
}
