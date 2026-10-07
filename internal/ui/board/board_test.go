package board

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	tea "github.com/charmbracelet/bubbletea"
	_ "modernc.org/sqlite"
)

// setupTestDB creates a temporary SQLite database initialized with schema.
func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test_mesh.db")
	dbStore, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	t.Cleanup(func() { _ = dbStore.Close() })
	return dbStore.DB()
}

// MockSSEClient provides a mock SSE client for testing live push updates.
type MockSSEClient struct {
	connected  bool
	eventsChan chan<- DaemonEvent
	statusChan chan<- daemonStatusMsg
}

func (m *MockSSEClient) Start(ctx context.Context, events chan<- DaemonEvent, status chan<- daemonStatusMsg) {
	m.connected = true
	m.eventsChan = events
	m.statusChan = status
	status <- daemonStatusMsg{connected: true, err: nil}
}

func (m *MockSSEClient) Close() error {
	m.connected = false
	return nil
}

func (m *MockSSEClient) IsConnected() bool {
	return m.connected
}

func (m *MockSSEClient) EmitEvent(evt DaemonEvent) {
	if m.eventsChan != nil {
		m.eventsChan <- evt
	}
}

func TestBoard_ColumnPartitioningAndInitialLoad(t *testing.T) {
	database := setupTestDB(t)

	// Create tasks in different stages
	task1, err := meshContext.CreateTask(database, "Task 1 - Todo", "/repo", "main", "work")
	if err != nil {
		t.Fatalf("create task1: %v", err)
	}

	task2, err := meshContext.CreateTask(database, "Task 2 - Progress", "/repo", "feature", "work")
	if err != nil {
		t.Fatalf("create task2: %v", err)
	}
	_ = meshContext.SetTaskExecutionStage(database, task2.ID, "in_progress")

	task3, err := meshContext.CreateTask(database, "Task 3 - Review", "/repo", "feature-rev", "work")
	if err != nil {
		t.Fatalf("create task3: %v", err)
	}
	_ = meshContext.SetTaskExecutionStage(database, task3.ID, "in_review")

	task4, err := meshContext.CreateTask(database, "Task 4 - Done", "/repo", "main", "work")
	if err != nil {
		t.Fatalf("create task4: %v", err)
	}
	_ = meshContext.SetTaskExecutionStage(database, task4.ID, "done")

	m, err := NewModel(Config{
		CustomDB:   database,
		Standalone: true,
	})
	if err != nil {
		t.Fatalf("NewModel failed: %v", err)
	}

	// 1. Initial load cmd
	initCmd := m.Init()
	if initCmd == nil {
		t.Fatal("expected Init() to return commands")
	}

	// Execute loadTasksCmd
	msg := m.loadTasksCmd()()
	tasksMsg, ok := msg.(tasksLoadedMsg)
	if !ok || tasksMsg.err != nil {
		t.Fatalf("loadTasksCmd failed: %v", tasksMsg.err)
	}

	// Update model with loaded tasks
	newModel, _ := m.Update(tasksMsg)
	m = newModel.(*Model)

	// 2. Assert columns match DB state
	if len(m.columns[ColTodo]) != 1 || m.columns[ColTodo][0].ID != task1.ID {
		t.Errorf("expected 1 task in todo (%s), got %d", task1.ID, len(m.columns[ColTodo]))
	}
	if len(m.columns[ColInProgress]) != 1 || m.columns[ColInProgress][0].ID != task2.ID {
		t.Errorf("expected 1 task in in_progress (%s), got %d", task2.ID, len(m.columns[ColInProgress]))
	}
	if len(m.columns[ColInReview]) != 1 || m.columns[ColInReview][0].ID != task3.ID {
		t.Errorf("expected 1 task in in_review (%s), got %d", task3.ID, len(m.columns[ColInReview]))
	}
	if len(m.columns[ColDone]) != 1 || m.columns[ColDone][0].ID != task4.ID {
		t.Errorf("expected 1 task in done (%s), got %d", task4.ID, len(m.columns[ColDone]))
	}

	// 3. Assert View rendering contains columns and task titles
	viewOutput := m.View()
	for _, expected := range []string{"TODO", "IN PROGRESS", "IN REVIEW", "DONE", "Task 1 - Todo", "Task 2 - Prog"} {
		if !strings.Contains(viewOutput, expected) {
			t.Errorf("expected View to contain %q, view:\n%s", expected, viewOutput)
		}
	}
}

func TestBoard_NavigationAndColumnSwitching(t *testing.T) {
	database := setupTestDB(t)

	_, _ = meshContext.CreateTask(database, "Todo 1", "/repo", "main", "work")
	_, _ = meshContext.CreateTask(database, "Todo 2", "/repo", "main", "work")

	m, _ := NewModel(Config{
		CustomDB:   database,
		Standalone: true,
	})

	// Load tasks
	msg := m.loadTasksCmd()()
	newModel, _ := m.Update(msg)
	m = newModel.(*Model)

	// The board opens on TODO; BACKLOG (index 0) sits to its left.
	if m.activeCol != 1 || AllColumns[m.activeCol] != ColTodo {
		t.Fatalf("expected initial activeCol=1 (todo), got %d", m.activeCol)
	}

	// Move cursor down within active column
	newModel, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = newModel.(*Model)
	if m.columnCursor[1] != 1 {
		t.Errorf("expected columnCursor[1]=1, got %d", m.columnCursor[1])
	}

	// Move cursor up
	newModel, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m = newModel.(*Model)
	if m.columnCursor[1] != 0 {
		t.Errorf("expected columnCursor[1]=0, got %d", m.columnCursor[1])
	}

	// Move column right ('l')
	newModel, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'l'}})
	m = newModel.(*Model)
	if m.activeCol != 2 {
		t.Errorf("expected activeCol=2 after 'l', got %d", m.activeCol)
	}

	// Move column right again
	newModel, _ = m.Update(tea.KeyMsg{Type: tea.KeyRight})
	m = newModel.(*Model)
	if m.activeCol != 3 {
		t.Errorf("expected activeCol=3 after right arrow, got %d", m.activeCol)
	}

	// Move column left ('h')
	newModel, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}})
	m = newModel.(*Model)
	if m.activeCol != 2 {
		t.Errorf("expected activeCol=2 after 'h', got %d", m.activeCol)
	}
}

func TestBoard_ThreadViewAndComments(t *testing.T) {
	database := setupTestDB(t)

	task, err := meshContext.CreateTask(database, "Threaded Task", "/repo/staypoint", "feature-x", "work")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	_ = meshContext.AddTaskComment(database, task.ID, "agent-1", "Initial assessment complete")
	_ = meshContext.AddTaskComment(database, task.ID, "user", "Looks good, proceed")
	_ = meshContext.AddWorkProduct(database, task.ID, "pull_request", "https://github.com/org/repo/pull/42")
	_ = meshContext.LogActivity(database, task.ID, "checkpoint", "sha: abc1234")

	m, _ := NewModel(Config{
		CustomDB:   database,
		Standalone: true,
	})

	// Load tasks
	newModel, _ := m.Update(m.loadTasksCmd()())
	m = newModel.(*Model)

	// Press Enter to open Thread View
	newModel, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = newModel.(*Model)

	if m.viewMode != ViewThread {
		t.Fatalf("expected viewMode=ViewThread, got %v", m.viewMode)
	}
	if m.activeTask == nil || m.activeTask.ID != task.ID {
		t.Fatalf("expected activeTask to be %s", task.ID)
	}

	// Execute loadThreadCmd
	if cmd != nil {
		threadMsg := m.loadThreadCmd(task.ID)()
		newModel, _ = m.Update(threadMsg)
		m = newModel.(*Model)
	}

	// Verify thread content
	if len(m.activeComments) != 2 {
		t.Errorf("expected 2 comments, got %d", len(m.activeComments))
	}
	if len(m.activeProducts) != 1 {
		t.Errorf("expected 1 work product, got %d", len(m.activeProducts))
	}
	// 2 comment_added (auto-logged by AddTaskComment since STA-542) + 1 checkpoint
	if len(m.activeActivity) != 3 {
		t.Errorf("expected 3 activity log entries, got %d", len(m.activeActivity))
	}

	// View output verification
	viewOutput := m.View()
	expectedSnippets := []string{
		"Threaded Task",
		"Initial assessment complete",
		"Looks good, proceed",
		"[pull_request]",
		"pull/42",
		"[checkpoint]",
		"abc1234",
	}
	for _, snip := range expectedSnippets {
		if !strings.Contains(viewOutput, snip) {
			t.Errorf("Thread View missing snippet %q\nView:\n%s", snip, viewOutput)
		}
	}

	// Press Esc to return to Board View
	newModel, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = newModel.(*Model)
	if m.viewMode != ViewBoard {
		t.Errorf("expected return to ViewBoard, got %v", m.viewMode)
	}
}

func TestBoard_MoveTaskStage(t *testing.T) {
	database := setupTestDB(t)

	task, err := meshContext.CreateTask(database, "Moveable Task", "/repo", "main", "work")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	m, _ := NewModel(Config{
		CustomDB:   database,
		Standalone: true,
	})

	newModel, _ := m.Update(m.loadTasksCmd()())
	m = newModel.(*Model)

	// Open move modal with 'm'
	newModel, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	m = newModel.(*Model)
	if m.viewMode != ViewMove {
		t.Fatalf("expected viewMode=ViewMove, got %v", m.viewMode)
	}

	// Press '2' (in_progress)
	newModel, moveCmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	m = newModel.(*Model)

	if moveCmd == nil {
		t.Fatal("expected moveCmd to be returned")
	}

	// Execute moveCmd
	stageMsg := moveCmd()
	newModel, refreshCmd := m.Update(stageMsg)
	m = newModel.(*Model)

	// Execute refresh cmd
	if refreshCmd != nil {
		newModel, _ = m.Update(m.loadTasksCmd()())
		m = newModel.(*Model)
	}

	// Verify in DB
	dbTask, err := meshContext.GetTask(database, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if dbTask.ExecutionStage != "in_progress" {
		t.Errorf("expected DB execution_stage=in_progress, got %s", dbTask.ExecutionStage)
	}

	// Verify in columns
	if len(m.columns[ColInProgress]) != 1 || m.columns[ColInProgress][0].ID != task.ID {
		t.Errorf("expected task to be in ColInProgress, found %d items", len(m.columns[ColInProgress]))
	}
	if len(m.columns[ColTodo]) != 0 {
		t.Errorf("expected ColTodo to be empty, found %d", len(m.columns[ColTodo]))
	}
}

func TestBoard_LiveUpdatesFromDaemonSSE(t *testing.T) {
	database := setupTestDB(t)

	mockSSE := &MockSSEClient{}
	m, err := NewModel(Config{
		CustomDB:        database,
		CustomSSEClient: mockSSE,
		Standalone:      false,
	})
	if err != nil {
		t.Fatalf("NewModel failed: %v", err)
	}

	// Init starts SSE
	_ = m.Init()
	if !mockSSE.connected {
		t.Fatal("expected mockSSE to be connected after Init()")
	}

	// Initially empty
	newModel, _ := m.Update(m.loadTasksCmd()())
	m = newModel.(*Model)
	if len(m.allTasks) != 0 {
		t.Fatalf("expected 0 tasks initially, got %d", len(m.allTasks))
	}

	// External event: Task created in database by daemon or background worker
	newTask, err := meshContext.CreateTask(database, "External Daemon Task", "/repo", "main", "work")
	if err != nil {
		t.Fatalf("create external task: %v", err)
	}

	// Daemon pushes SSE event "task_created"
	daemonEvt := DaemonEvent{
		ID:   1,
		Type: "task_created",
		Data: map[string]any{"id": newTask.ID, "name": newTask.Name},
	}

	// Model handles daemonEventMsg
	newModel, eventCmd := m.Update(daemonEventMsg{event: daemonEvt})
	m = newModel.(*Model)

	if !m.daemonConnected {
		t.Error("expected daemonConnected=true")
	}

	// Execute triggered DB reload
	if eventCmd != nil {
		newModel, _ = m.Update(m.loadTasksCmd()())
		m = newModel.(*Model)
	}

	// Assert columns updated live and match DB state
	if len(m.allTasks) != 1 {
		t.Fatalf("expected 1 task after live event, got %d", len(m.allTasks))
	}
	if len(m.columns[ColTodo]) != 1 || m.columns[ColTodo][0].Name != "External Daemon Task" {
		t.Errorf("expected task in ColTodo to match DB state, got %v", m.columns[ColTodo])
	}
}

func TestBoard_AddCommentFlow(t *testing.T) {
	database := setupTestDB(t)

	task, _ := meshContext.CreateTask(database, "Commentable", "/repo", "main", "work")

	m, _ := NewModel(Config{
		CustomDB:   database,
		Standalone: true,
	})

	newModel, _ := m.Update(m.loadTasksCmd()())
	m = newModel.(*Model)

	// Open comment view
	newModel, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	m = newModel.(*Model)
	if m.viewMode != ViewCommentInput {
		t.Fatalf("expected viewMode=ViewCommentInput, got %v", m.viewMode)
	}

	// Set textarea value
	m.textarea.SetValue("Testing comment via TUI")

	// Press Enter to submit
	newModel, submitCmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = newModel.(*Model)

	if submitCmd == nil {
		t.Fatal("expected submitCmd on Enter")
	}

	// Execute add comment cmd
	commentMsg := submitCmd()
	newModel, _ = m.Update(commentMsg)
	m = newModel.(*Model)

	// Check DB
	comments, err := meshContext.GetTaskComments(database, task.ID)
	if err != nil {
		t.Fatalf("get comments: %v", err)
	}
	if len(comments) != 1 || comments[0].Message != "Testing comment via TUI" {
		t.Errorf("expected comment in DB, got %+v", comments)
	}
}

func TestBoard_WindowResize(t *testing.T) {
	database := setupTestDB(t)
	m, _ := NewModel(Config{
		CustomDB:   database,
		Standalone: true,
	})

	newModel, _ := m.Update(tea.WindowSizeMsg{Width: 140, Height: 45})
	m = newModel.(*Model)

	if m.width != 140 || m.height != 45 {
		t.Errorf("expected width=140, height=45, got %d, %d", m.width, m.height)
	}
}
