package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
)

// task-e3fe2c0b: resume:false on a card answer ("Answer only") records the
// answer without waking the task; an answer without resume still wakes it.
func TestResolveInteraction_AnswerOnlyDoesNotWake(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())
	_, task := postTask(t, base, token, map[string]any{"name": "cards", "repo_path": "/repo/x", "execution_stage": "backlog"})
	taskID := task["id"].(string)

	woke := make(chan string, 8)
	prev := orchestrator.GlobalDispatcher.OnWake
	orchestrator.GlobalDispatcher.OnWake = func(id, reason string) {
		if id == taskID {
			woke <- reason
		}
	}
	t.Cleanup(func() { orchestrator.GlobalDispatcher.OnWake = prev })

	newCard := func() int {
		body, _ := json.Marshal(map[string]string{"kind": "request_confirmation", "payload": `{"prompt":"approve?"}`})
		req, _ := http.NewRequest(http.MethodPost, base+"/api/tasks/"+taskID+"/interactions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var created map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&created)
		id, _ := created["id"].(float64)
		if resp.StatusCode != http.StatusCreated || id == 0 {
			t.Fatalf("create card: %d %v", resp.StatusCode, created)
		}
		return int(id)
	}
	resolve := func(iid int, resume *bool) {
		m := map[string]any{"status": "accepted", "response": map[string]any{"confirmed": true}}
		if resume != nil {
			m["resume"] = *resume
		}
		body, _ := json.Marshal(m)
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/tasks/%s/interactions/%d/resolve", base, taskID, iid), bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("resolve %d: status %d", iid, resp.StatusCode)
		}
	}

	no := false
	resolve(newCard(), &no)
	select {
	case r := <-woke:
		t.Fatalf("answer-only woke the task (reason %q)", r)
	case <-time.After(300 * time.Millisecond):
	}

	resolve(newCard(), nil)
	select {
	case r := <-woke:
		if r != "interaction_resolved" {
			t.Errorf("wake reason = %q, want interaction_resolved", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a plain answer no longer wakes the task")
	}
}
