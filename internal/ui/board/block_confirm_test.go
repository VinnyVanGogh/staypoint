package board

import (
	"strings"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	tea "github.com/charmbracelet/bubbletea"
)

// runCmd executes cmd (unwrapping tea.Batch) and returns the first blockToggledMsg.
func runCmd(t *testing.T, cmd tea.Cmd) (blockToggledMsg, bool) {
	t.Helper()
	if cmd == nil {
		return blockToggledMsg{}, false
	}
	switch msg := cmd().(type) {
	case blockToggledMsg:
		return msg, true
	case tea.BatchMsg:
		for _, c := range msg {
			if got, ok := runCmd(t, c); ok {
				return got, true
			}
		}
	}
	return blockToggledMsg{}, false
}

func pressKey(m *Model, r rune) (*Model, tea.Cmd) {
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	return nm.(*Model), cmd
}

func openThreadOn(t *testing.T) (*Model, string) {
	t.Helper()
	database := setupTestDB(t)
	task, err := meshContext.CreateTask(database, "Block Me", "/repo", "main", "work")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	m, _ := NewModel(Config{CustomDB: database, Standalone: true})
	nm, _ := m.Update(m.loadTasksCmd()())
	m = nm.(*Model)
	nm, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(*Model)
	if m.viewMode != ViewThread || m.activeTask == nil || m.activeTask.ID != task.ID {
		t.Fatalf("thread view not open on task")
	}
	return m, task.ID
}

// A lone 'b' (or 'b' then anything but 'y') must never block the task.
func TestBlockKey_RequiresConfirm(t *testing.T) {
	m, id := openThreadOn(t)

	m, cmd := pressKey(m, 'b')
	if !m.confirmBlock {
		t.Fatal("'b' should ask for confirmation")
	}
	if _, ran := runCmd(t, cmd); ran {
		t.Fatal("'b' alone must not toggle the block")
	}
	if v := m.View(); !containsAll(v, "Block this task?", "[y]") {
		t.Fatalf("confirm prompt not shown:\n%s", v)
	}

	m, cmd = pressKey(m, 'n')
	if m.confirmBlock {
		t.Fatal("'n' should clear the prompt")
	}
	if _, ran := runCmd(t, cmd); ran {
		t.Fatal("'n' must not toggle the block")
	}
	task, _ := meshContext.GetTask(m.db, id)
	if task.IsBlocked {
		t.Fatal("task blocked without confirmation")
	}
	if ev, _ := meshContext.LatestBlockEvent(m.db, id); ev != nil {
		t.Fatalf("cancelled toggle logged an event: %+v", ev)
	}
}

// 'b' then 'y' blocks and logs who did it; the same again unblocks and logs that.
func TestBlockKey_ConfirmLogsActor(t *testing.T) {
	t.Setenv("USER", "boardtester")
	m, id := openThreadOn(t)

	m, _ = pressKey(m, 'b')
	m, cmd := pressKey(m, 'y')
	msg, ran := runCmd(t, cmd)
	if !ran || msg.err != nil || !msg.blocked {
		t.Fatalf("expected a successful block, got ran=%v msg=%+v", ran, msg)
	}
	nm, _ := m.Update(msg)
	m = nm.(*Model)
	if m.statusMessage != "Task blocked" {
		t.Errorf("status = %q", m.statusMessage)
	}

	task, _ := meshContext.GetTask(m.db, id)
	if !task.IsBlocked {
		t.Fatal("task not blocked after y")
	}
	ev, err := meshContext.LatestBlockEvent(m.db, id)
	if err != nil || ev == nil {
		t.Fatalf("no block event: %v", err)
	}
	if !ev.Blocked || ev.By != "boardtester" || ev.Via != "tui" || ev.At == "" || ev.Reason != task.BlockReason {
		t.Fatalf("bad block event: %+v (reason %q)", ev, task.BlockReason)
	}

	// Reload the active task so the toggle sees is_blocked=1, then unblock.
	nm, _ = m.Update(m.loadTasksCmd()())
	m = nm.(*Model)
	m, _ = pressKey(m, 'b')
	if v := m.View(); !containsAll(v, "Unblock this task?") {
		t.Fatalf("unblock prompt not shown:\n%s", v)
	}
	m, cmd = pressKey(m, 'Y')
	msg, ran = runCmd(t, cmd)
	if !ran || msg.err != nil || msg.blocked {
		t.Fatalf("expected a successful unblock, got ran=%v msg=%+v", ran, msg)
	}
	task, _ = meshContext.GetTask(m.db, id)
	if task.IsBlocked {
		t.Fatal("task still blocked after unblock")
	}
	ev, _ = meshContext.LatestBlockEvent(m.db, id)
	if ev == nil || ev.Blocked || ev.By != "boardtester" {
		t.Fatalf("bad unblock event: %+v", ev)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
