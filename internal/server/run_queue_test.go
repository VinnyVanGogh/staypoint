package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
)

// STA-773: GET /api/tasks/{id} reports the task's run-queue position so the
// task page can show "Queued: N runs ahead".
func TestServer_GetTaskReportsQueuePosition(t *testing.T) {
	prev := orchestrator.GlobalRunSlots
	slots := orchestrator.NewRunSlots(1)
	slots.Wake = func(string, string) {}
	orchestrator.GlobalRunSlots = slots
	t.Cleanup(func() { orchestrator.GlobalRunSlots = prev })

	database := setupTestDB(t)
	srv, token := startTestServer(t, database)

	get := func(id string) map[string]any {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL()+"/api/tasks/"+id, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET task: %d", resp.StatusCode)
		}
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		q, _ := out["queue"].(map[string]any)
		if q == nil {
			t.Fatalf("no queue field in %v", out)
		}
		return q
	}

	create := func(name string) string {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL()+"/api/tasks", strings.NewReader(`{"name":"`+name+`"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		id, _ := out["id"].(string)
		if id == "" {
			t.Fatalf("create task: %d %v", resp.StatusCode, out)
		}
		return id
	}
	first, second := create("queue one"), create("queue two")
	if q := get(second); q["queued"] != false {
		t.Fatalf("unqueued task: %v", q)
	}
	slots.Enqueue(first, orchestrator.SlotKey{Dir: "/r1"}, "run_now", orchestrator.WaitSlots)
	slots.Enqueue(second, orchestrator.SlotKey{Dir: "/r2"}, "run_now", orchestrator.WaitRepo)
	q := get(second)
	if q["queued"] != true || q["ahead"] != float64(1) || q["wait"] != "repo" {
		t.Fatalf("queued task: %v, want queued with 1 ahead waiting on repo", q)
	}
}
