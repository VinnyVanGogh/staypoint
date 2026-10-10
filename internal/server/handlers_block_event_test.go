package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
)

func getTaskJSON(t *testing.T, base, token, id string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/api/tasks/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET task: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET task: want 200, got %d", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

// GET /api/tasks/{id} names who hand-blocked a task, and only while that block stands.
func TestGetTask_BlockEvent(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	task, err := meshContext.CreateTask(database, "block-event", "/repo", "main", "work")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if ev := getTaskJSON(t, base, token, task.ID)["block_event"]; ev != nil {
		t.Fatalf("unblocked task: want null block_event, got %v", ev)
	}

	if err := meshContext.BlockTask(database, task.ID, "Blocked via TUI"); err != nil {
		t.Fatal(err)
	}
	if err := meshContext.LogBlockEvent(database, task.ID, true, "boardtester", "tui", "Blocked via TUI"); err != nil {
		t.Fatal(err)
	}
	ev, _ := getTaskJSON(t, base, token, task.ID)["block_event"].(map[string]any)
	if ev == nil || ev["by"] != "boardtester" || ev["via"] != "tui" || ev["at"] == "" {
		t.Fatalf("blocked task: bad block_event %v", ev)
	}

	// Re-blocked elsewhere with a different reason: don't credit the old toggle.
	if err := meshContext.BlockTask(database, task.ID, "waiting on vendor"); err != nil {
		t.Fatal(err)
	}
	if ev := getTaskJSON(t, base, token, task.ID)["block_event"]; ev != nil {
		t.Fatalf("stale block_event credited to a different block: %v", ev)
	}

	if err := meshContext.UnblockTask(database, task.ID); err != nil {
		t.Fatal(err)
	}
	if ev := getTaskJSON(t, base, token, task.ID)["block_event"]; ev != nil {
		t.Fatalf("after unblock: want null block_event, got %v", ev)
	}
}
