package context

import (
	"errors"
	"strings"
	"testing"
)

func TestCreateTask_DefaultsNativeOriginAndTodo(t *testing.T) {
	database := setupTestDB(t)
	task, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "n", RepoPath: "/repo/x", GitBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if task.Origin != OriginNative || task.ExecutionStage != "todo" {
		t.Fatalf("origin/stage = %s/%s, want native/todo", task.Origin, task.ExecutionStage)
	}
	backlog, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "b", RepoPath: "/repo/x", GitBranch: "main", ExecutionStage: "backlog", Origin: OriginPaperclipImport})
	if err != nil {
		t.Fatal(err)
	}
	if backlog.ExecutionStage != "backlog" || backlog.Origin != OriginPaperclipImport {
		t.Fatalf("got %s/%s, want backlog/paperclip_import", backlog.ExecutionStage, backlog.Origin)
	}
	if _, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "x", RepoPath: "/r", GitBranch: "main", ExecutionStage: "running"}); !errors.Is(err, ErrInvalidStage) {
		t.Errorf("unknown stage: err = %v, want ErrInvalidStage", err)
	}
	if _, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "x", RepoPath: "/r", GitBranch: "main", Origin: "imported"}); !errors.Is(err, ErrInvalidOrigin) {
		t.Errorf("unknown origin: err = %v, want ErrInvalidOrigin", err)
	}
}

func TestSetStage_BacklogRunNowPassesThroughTodo(t *testing.T) {
	database := setupTestDB(t)
	task, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "parked", RepoPath: "/repo/x", GitBranch: "main", ExecutionStage: "backlog"})
	if err != nil {
		t.Fatal(err)
	}
	if err := SetTaskExecutionStage(database, task.ID, "in_progress"); err != nil {
		t.Fatalf("run now from backlog: %v", err)
	}
	got, _ := GetTask(database, task.ID)
	if got.ExecutionStage != "in_progress" || got.Status != "active" {
		t.Fatalf("stage/status = %s/%s", got.ExecutionStage, got.Status)
	}
	logs, err := GetTaskActivityLog(database, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var seq []string
	for _, l := range logs {
		if l.EventType == "stage_change" {
			seq = append(seq, l.Details)
		}
	}
	joined := strings.Join(seq, " | ")
	if !strings.Contains(joined, "set to todo") || !strings.Contains(joined, "set to in_progress") {
		t.Fatalf("activity must record backlog -> todo -> in_progress, got %q", joined)
	}
}

func TestSetStage_CancelClosesAndReopenRestores(t *testing.T) {
	database := setupTestDB(t)
	task, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "c", RepoPath: "/repo/x", GitBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err := SetTaskExecutionStage(database, task.ID, "cancelled"); err != nil {
		t.Fatal(err)
	}
	got, _ := GetTask(database, task.ID)
	if got.Status != "soft_deleted" || got.ExecutionStage != "cancelled" || got.DeletedAt == nil {
		t.Fatalf("cancel: status=%s stage=%s deleted_at=%v", got.Status, got.ExecutionStage, got.DeletedAt)
	}
	if err := SetTaskExecutionStage(database, task.ID, "backlog"); err != nil {
		t.Fatal(err)
	}
	got, _ = GetTask(database, task.ID)
	if got.Status != "active" || got.ExecutionStage != "backlog" || got.DeletedAt != nil {
		t.Fatalf("reopen: status=%s stage=%s deleted_at=%v", got.Status, got.ExecutionStage, got.DeletedAt)
	}
	if err := SetTaskExecutionStage(database, task.ID, "paused"); !errors.Is(err, ErrInvalidStage) {
		t.Errorf("harness sub-state must not be settable: err = %v", err)
	}
}

func TestFilterLegacy(t *testing.T) {
	tasks := []Task{{ID: "a", Origin: OriginNative}, {ID: "b", Origin: OriginLegacy}, {ID: "c", Origin: OriginPaperclipImport}}
	if got := FilterLegacy(tasks, false); len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Fatalf("hide legacy: %+v", got)
	}
	if got := FilterLegacy(tasks, true); len(got) != 3 {
		t.Fatalf("include legacy: %d", len(got))
	}
	if len(tasks) != 3 || tasks[1].ID != "b" {
		t.Fatalf("FilterLegacy must not mutate its input: %+v", tasks)
	}
}

func TestListTasks_CarriesOrigin(t *testing.T) {
	database := setupTestDB(t)
	task, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "o", RepoPath: "/repo/x", GitBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE tasks SET origin = 'legacy' WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	tasks, err := ListTasks(database, false)
	if err != nil || len(tasks) != 1 || tasks[0].Origin != OriginLegacy {
		t.Fatalf("ListTasks origin: %+v err=%v", tasks, err)
	}
	if got, _ := GetActiveTaskForRepo(database, "/repo/x"); got == nil || got.Origin != OriginLegacy {
		t.Fatalf("GetActiveTaskForRepo origin: %+v", got)
	}
}

// A parked task, however recently touched, never becomes the repo's active
// task, and neither does one with no repo.
func TestGetActiveTaskForRepo_SkipsBacklogAndRepoless(t *testing.T) {
	database := setupTestDB(t)
	working, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "working", RepoPath: "/repo/x", GitBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	parked, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "parked", RepoPath: "/repo/x", GitBranch: "main", ExecutionStage: "backlog"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE tasks SET updated_at = '2999-01-01T00:00:00Z' WHERE id = ?`, parked.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO tasks (id, name, repo_path, git_branch, status, account_role, execution_stage, updated_at) VALUES ('task-norepo', 'no repo', '', '', 'active', 'personal', 'todo', '2999-01-02T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"/repo/x", "/elsewhere"} {
		got, err := GetActiveTaskForRepo(database, dir)
		if err != nil || got.ID != working.ID {
			t.Fatalf("active task for %s = %+v (err %v), want %s", dir, got, err, working.ID)
		}
	}
}

// An imported finished issue is created closed: status follows the stage,
// the Paperclip completion time becomes updated_at (and deleted_at for
// cancelled), the daemon is not woken, and lists hide it by default.
func TestCreateTask_ClosedImportIsArchived(t *testing.T) {
	database := setupTestDB(t)
	done, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "[STA-5] done", NoRepo: true, ExecutionStage: "done",
		Origin: OriginPaperclipImport, SourceRef: "STA-5", SourceID: "u5", ClosedAt: "2026-09-01T10:00:00.000Z"})
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != "done" || done.UpdatedAt != "2026-09-01T10:00:00.000Z" || done.DeletedAt != nil {
		t.Fatalf("done: %+v", done)
	}
	cancelled, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "[STA-7] gone", NoRepo: true, ExecutionStage: "cancelled",
		Origin: OriginPaperclipImport, SourceID: "u7", ClosedAt: "2026-09-02T11:30:00.000Z"})
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != "soft_deleted" || cancelled.DeletedAt == nil || *cancelled.DeletedAt != "2026-09-02T11:30:00.000Z" {
		t.Fatalf("cancelled: %+v", cancelled)
	}
	open, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "open", NoRepo: true, ExecutionStage: "backlog", Origin: OriginPaperclipImport, SourceID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	native, err := CreateTaskWithOptions(database, TaskCreateOptions{Name: "native done", RepoPath: "/r", GitBranch: "main", ExecutionStage: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if !IsArchived(*done) || !IsArchived(*cancelled) || IsArchived(*open) || IsArchived(*native) {
		t.Fatal("IsArchived must be exactly the finished imports")
	}
	all, _ := ListTasks(database, true)
	visible := map[string]bool{}
	for _, tk := range FilterLegacy(all, false) {
		visible[tk.ID] = true
	}
	if visible[done.ID] || !visible[open.ID] || !visible[native.ID] {
		t.Fatalf("default visibility: %v", visible)
	}
	if len(FilterLegacy(all, true)) != len(all) {
		t.Fatal("include switch must show everything")
	}
}
