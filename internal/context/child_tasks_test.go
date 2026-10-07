package context

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func createPlanningParent(t *testing.T) (*sql.DB, *Task) {
	t.Helper()
	database := setupTestDB(t)
	parent, err := CreateTaskWithOptions(database, TaskCreateOptions{
		Name:         "Plan the auth rewrite",
		RepoPath:     "/repo/agent-mesh",
		GitBranch:    "main",
		AccountRole:  "personal",
		Organization: "acme",
		Project:      "core",
		WorkKind:     "planning",
	})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	return database, parent
}

func TestCreateChildTask_InheritsParentAndStoresHandoff(t *testing.T) {
	database, parent := createPlanningParent(t)
	if _, err := database.Exec(`INSERT INTO task_comments (task_id, author, message) VALUES (?, 'agent-summary', ?)`,
		parent.ID, "Plan done: split into two coding tasks."); err != nil {
		t.Fatal(err)
	}

	child, err := CreateChildTask(database, ChildTaskOptions{
		ParentID: parent.ID,
		Name:     "Implement token store",
		WorkKind: "coding",
		Handoff:  "1. Add tokens table\n2. Wire refresh",
	})
	if err != nil {
		t.Fatalf("CreateChildTask: %v", err)
	}

	if child.ParentID != parent.ID {
		t.Errorf("parent_id: want %q, got %q", parent.ID, child.ParentID)
	}
	if child.WorkKind != "coding" {
		t.Errorf("work_kind: want coding, got %q", child.WorkKind)
	}
	if child.RepoPath != parent.RepoPath || child.GitBranch != parent.GitBranch {
		t.Errorf("repo/branch not inherited: %q@%q vs %q@%q", child.RepoPath, child.GitBranch, parent.RepoPath, parent.GitBranch)
	}
	if child.Organization != "acme" || child.Project != "core" || child.AccountRole != "personal" {
		t.Errorf("org/project/role not inherited: %q/%q/%q", child.Organization, child.Project, child.AccountRole)
	}
	if child.ExecutionStage != "todo" {
		t.Errorf("child should start in todo, got %q", child.ExecutionStage)
	}

	h, err := GetTaskHandoff(database, child.ID)
	if err != nil {
		t.Fatalf("GetTaskHandoff: %v", err)
	}
	for _, want := range []string{parent.ID, "Plan the auth rewrite", "planning", "1. Add tokens table", "Plan done: split into two coding tasks."} {
		if !strings.Contains(h, want) {
			t.Errorf("handoff missing %q:\n%s", want, h)
		}
	}
}

func TestCreateChildTask_PlanFallsBackToParentPlanDocument(t *testing.T) {
	database, parent := createPlanningParent(t)
	if err := AddTaskDocument(database, parent.ID, "plan", "Stored plan body"); err != nil {
		t.Fatal(err)
	}
	child, err := CreateChildTask(database, ChildTaskOptions{ParentID: parent.ID, Name: "c", WorkKind: "qa"})
	if err != nil {
		t.Fatal(err)
	}
	h, _ := GetTaskHandoff(database, child.ID)
	if !strings.Contains(h, "Stored plan body") {
		t.Errorf("handoff should fall back to parent plan doc:\n%s", h)
	}
}

func TestCreateChildTask_RejectsInvalidKind(t *testing.T) {
	database, parent := createPlanningParent(t)
	for _, kind := range []string{"", "review", "CODING ", "deploy"} {
		_, err := CreateChildTask(database, ChildTaskOptions{ParentID: parent.ID, Name: "x", WorkKind: kind})
		if !errors.Is(err, ErrInvalidWorkKind) {
			t.Errorf("kind %q: want ErrInvalidWorkKind, got %v", kind, err)
		}
	}
}

func TestCreateChildTask_RejectsMissingParentAndEmptyName(t *testing.T) {
	database, parent := createPlanningParent(t)
	if _, err := CreateChildTask(database, ChildTaskOptions{ParentID: "task-nope", Name: "x", WorkKind: "coding"}); !errors.Is(err, ErrParentNotFound) {
		t.Errorf("missing parent: want ErrParentNotFound, got %v", err)
	}
	if _, err := CreateChildTask(database, ChildTaskOptions{ParentID: parent.ID, Name: "  ", WorkKind: "coding"}); err == nil {
		t.Error("empty name should be rejected")
	}
}

func TestCreateChildTask_ChildCountCap(t *testing.T) {
	database, parent := createPlanningParent(t)
	if err := SetChildTaskLimits(database, 2, 2); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := CreateChildTask(database, ChildTaskOptions{ParentID: parent.ID, Name: "c", WorkKind: "coding"}); err != nil {
			t.Fatalf("child %d: %v", i, err)
		}
	}
	if _, err := CreateChildTask(database, ChildTaskOptions{ParentID: parent.ID, Name: "c3", WorkKind: "coding"}); !errors.Is(err, ErrChildLimit) {
		t.Errorf("third child: want ErrChildLimit, got %v", err)
	}
}

func TestChildTaskLimits_Defaults(t *testing.T) {
	database, _ := createPlanningParent(t)
	maxChildren, maxDepth := ChildTaskLimits(database)
	if maxChildren != 10 || maxDepth != 2 {
		t.Errorf("defaults: want 10/2, got %d/%d", maxChildren, maxDepth)
	}
}

func TestCreateChildTask_DepthCap(t *testing.T) {
	database, root := createPlanningParent(t)
	c1, err := CreateChildTask(database, ChildTaskOptions{ParentID: root.ID, Name: "depth1", WorkKind: "architecture"})
	if err != nil {
		t.Fatal(err)
	}
	c2, err := CreateChildTask(database, ChildTaskOptions{ParentID: c1.ID, Name: "depth2", WorkKind: "coding"})
	if err != nil {
		t.Fatalf("depth 2 should be allowed: %v", err)
	}
	if _, err := CreateChildTask(database, ChildTaskOptions{ParentID: c2.ID, Name: "depth3", WorkKind: "coding"}); !errors.Is(err, ErrDepthLimit) {
		t.Errorf("depth 3: want ErrDepthLimit, got %v", err)
	}
	// Board override lifts the depth cap.
	if _, err := CreateChildTask(database, ChildTaskOptions{ParentID: c2.ID, Name: "depth3", WorkKind: "coding", BoardOverride: true}); err != nil {
		t.Errorf("depth 3 with board override: %v", err)
	}
}

func TestMarkTaskDone_BlockedByOpenChildren(t *testing.T) {
	database, parent := createPlanningParent(t)
	child, err := CreateChildTask(database, ChildTaskOptions{ParentID: parent.ID, Name: "c", WorkKind: "coding"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{parent.ID, child.ID} {
		if err := AddWorkProduct(database, id, "commit", "abc123"); err != nil {
			t.Fatal(err)
		}
	}

	if err := MarkTaskDone(database, parent.ID); !errors.Is(err, ErrOpenChildren) {
		t.Fatalf("parent with open child: want ErrOpenChildren, got %v", err)
	}
	if err := SetTaskExecutionStage(database, parent.ID, "done"); !errors.Is(err, ErrOpenChildren) {
		t.Fatalf("stage done with open child: want ErrOpenChildren, got %v", err)
	}
	// Non-done stages are unaffected.
	if err := SetTaskExecutionStage(database, parent.ID, "in_review"); err != nil {
		t.Fatalf("in_review should be allowed: %v", err)
	}

	if err := MarkTaskDone(database, child.ID); err != nil {
		t.Fatalf("child done: %v", err)
	}
	if err := MarkTaskDone(database, parent.ID); err != nil {
		t.Fatalf("parent done after children closed: %v", err)
	}
}

func TestMarkTaskDone_BoardOverride(t *testing.T) {
	database, parent := createPlanningParent(t)
	if _, err := CreateChildTask(database, ChildTaskOptions{ParentID: parent.ID, Name: "c", WorkKind: "coding"}); err != nil {
		t.Fatal(err)
	}
	if err := AddWorkProduct(database, parent.ID, "commit", "abc"); err != nil {
		t.Fatal(err)
	}
	if err := MarkTaskDoneWithOptions(database, parent.ID, DoneOptions{BoardOverride: true}); err != nil {
		t.Fatalf("board override done: %v", err)
	}
	got, _ := GetTask(database, parent.ID)
	if got.Status != "done" {
		t.Errorf("status: want done, got %q", got.Status)
	}
}

func TestOpenChildren_IgnoresClosedAndDeleted(t *testing.T) {
	database, parent := createPlanningParent(t)
	c1, _ := CreateChildTask(database, ChildTaskOptions{ParentID: parent.ID, Name: "a", WorkKind: "coding"})
	c2, _ := CreateChildTask(database, ChildTaskOptions{ParentID: parent.ID, Name: "b", WorkKind: "qa"})
	if _, err := database.Exec(`UPDATE tasks SET status='soft_deleted' WHERE id=?`, c1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE tasks SET execution_stage='cancelled' WHERE id=?`, c2.ID); err != nil {
		t.Fatal(err)
	}
	n, err := CountOpenChildren(database, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("open children: want 0, got %d", n)
	}
}

func TestIsValidWorkKind(t *testing.T) {
	for _, k := range ValidWorkKinds() {
		if !IsValidWorkKind(k) {
			t.Errorf("%q should be valid", k)
		}
	}
	if IsValidWorkKind("review") {
		t.Error("review is not a routing kind")
	}
}
