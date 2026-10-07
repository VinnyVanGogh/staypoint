package context

import (
	"errors"
	"strings"
	"testing"
)

// STA-861: the Board closes any open task; agents still need a work product;
// work products can be registered from the CLI/API; comments do not wake a
// stopped or parked task; work_kind can be changed when no run holds it.

func TestMarkDone_AgentStillNeedsWorkProduct(t *testing.T) {
	database := setupTestDB(t)
	task, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "research", RepoPath: "/tmp/r", GitBranch: "main", AccountRole: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkTaskDone(database, task.ID); !errors.Is(err, ErrNoWorkProduct) {
		t.Fatalf("agent done without product: err = %v, want ErrNoWorkProduct", err)
	}
	got, _ := GetTask(database, task.ID)
	if got.Status == "done" {
		t.Fatal("task closed without a work product")
	}
}

func TestMarkDone_BoardWithoutWorkProductRecordsNote(t *testing.T) {
	database := setupTestDB(t)
	task, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "review", RepoPath: "/tmp/r", GitBranch: "main", AccountRole: "personal", ExecutionStage: "blocked"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE tasks SET is_blocked = 1, block_reason = 'waiting' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := MarkTaskDoneWithOptions(database, task.ID, DoneOptions{BoardDone: true, BoardNote: "read-only review finished"}); err != nil {
		t.Fatalf("board done: %v", err)
	}
	got, _ := GetTask(database, task.ID)
	if got.Status != "done" || got.ExecutionStage != "done" || got.IsBlocked {
		t.Fatalf("after board done: status=%s stage=%s blocked=%v", got.Status, got.ExecutionStage, got.IsBlocked)
	}
	var author, msg string
	if err := database.QueryRow(`SELECT author, message FROM task_comments WHERE task_id = ? ORDER BY id DESC LIMIT 1`, task.ID).Scan(&author, &msg); err != nil {
		t.Fatal(err)
	}
	if author != "board" || msg != "Marked done by Board: read-only review finished" {
		t.Errorf("timeline note = %s: %q", author, msg)
	}
}

func TestMarkDone_BoardStillRespectsOpenChildren(t *testing.T) {
	database := setupTestDB(t)
	parent, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "p", RepoPath: "/tmp/r", GitBranch: "main", AccountRole: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateChildTask(database, ChildTaskOptions{ParentID: parent.ID, Name: "c", WorkKind: "coding"}); err != nil {
		t.Fatal(err)
	}
	if err := MarkTaskDoneWithOptions(database, parent.ID, DoneOptions{BoardDone: true}); !errors.Is(err, ErrOpenChildren) {
		t.Fatalf("board done with open child: err = %v, want ErrOpenChildren", err)
	}
	if err := MarkTaskDoneWithOptions(database, parent.ID, DoneOptions{BoardDone: true, BoardOverride: true}); err != nil {
		t.Fatalf("board done + override: %v", err)
	}
}

func TestRegisterWorkProduct_TypesAndDone(t *testing.T) {
	database := setupTestDB(t)
	task, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "w", RepoPath: "/tmp/r", GitBranch: "main", AccountRole: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ in, want string }{
		{"pr", "pull_request"}, {"commit", "commit"}, {"doc", "doc"}, {"workspace_file", "workspace_file"}, {"branch", "branch"},
	} {
		p, err := RegisterWorkProduct(database, task.ID, tc.in, "ref-"+tc.in)
		if err != nil {
			t.Fatalf("register %s: %v", tc.in, err)
		}
		if p.ProductType != tc.want || p.TaskID != task.ID {
			t.Errorf("register %s: got %+v", tc.in, p)
		}
	}
	if _, err := RegisterWorkProduct(database, task.ID, "tweet", "x"); !errors.Is(err, ErrInvalidWorkProduct) {
		t.Errorf("bad type: err = %v", err)
	}
	if _, err := RegisterWorkProduct(database, task.ID, "pr", "  "); !errors.Is(err, ErrInvalidWorkProduct) {
		t.Errorf("empty ref: err = %v", err)
	}
	if err := MarkTaskDone(database, task.ID); err != nil {
		t.Fatalf("done after registering products: %v", err)
	}
}

func TestCommentWakes_StoppedAndParkedStayQuiet(t *testing.T) {
	database := setupTestDB(t)
	task, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "c", RepoPath: "/tmp/r", GitBranch: "main", AccountRole: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	set := func(q string, args ...any) {
		t.Helper()
		if _, err := database.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	if !commentWakes(database, task.ID) {
		t.Fatal("todo task: a comment should wake it")
	}
	for _, stage := range []string{"stopped", "backlog", "done", "cancelled"} {
		set(`UPDATE tasks SET execution_stage = ? WHERE id = ?`, stage, task.ID)
		if commentWakes(database, task.ID) {
			t.Errorf("%s task: a comment must not wake it", stage)
		}
	}
	// A stop is pending while the stopped run still holds the checkout.
	set(`UPDATE tasks SET execution_stage = 'in_progress', checkout_run_id = 'run-1' WHERE id = ?`, task.ID)
	set(`INSERT INTO run_control (task_id, pause_after_step, stop_requested) VALUES (?, 0, 1)`, task.ID)
	if commentWakes(database, task.ID) {
		t.Error("stop pending: a comment must not wake the task")
	}
	// The Board moved it back to todo after the run ended: comments wake again.
	set(`UPDATE tasks SET execution_stage = 'todo', checkout_run_id = NULL WHERE id = ?`, task.ID)
	if !commentWakes(database, task.ID) {
		t.Error("todo after stop: a comment should wake it")
	}
	// AddTaskComment still records the comment on a stopped task.
	set(`UPDATE tasks SET execution_stage = 'stopped' WHERE id = ?`, task.ID)
	if err := AddTaskComment(database, task.ID, "board", "note"); err != nil {
		t.Fatal(err)
	}
}

func TestSetTaskWorkKind(t *testing.T) {
	database := setupTestDB(t)
	task, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "k", RepoPath: "/tmp/r", GitBranch: "main", AccountRole: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := SetTaskWorkKind(database, task.ID, "Docs")
	if err != nil || got.WorkKind != "docs" {
		t.Fatalf("set docs: %v %+v", err, got)
	}
	if _, err := SetTaskWorkKind(database, task.ID, "poetry"); !errors.Is(err, ErrInvalidWorkKind) {
		t.Errorf("bad kind: err = %v", err)
	}
	if _, err := database.Exec(`UPDATE tasks SET execution_stage = 'in_progress', checkout_run_id = 'run-1' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := SetTaskWorkKind(database, task.ID, "review"); !errors.Is(err, ErrRunInProgress) || !strings.Contains(err.Error(), "stop it") {
		t.Errorf("kind change mid-run: err = %v, want ErrRunInProgress", err)
	}
	got, _ = GetTask(database, task.ID)
	if got.WorkKind != "docs" {
		t.Errorf("kind changed mid-run: %s", got.WorkKind)
	}
}
