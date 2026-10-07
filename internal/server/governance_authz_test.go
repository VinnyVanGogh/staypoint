package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
)

// STA-355: governance and interaction endpoints refuse actors and ids that do
// not belong to the task.
func TestServer_GovernanceAuthz(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)

	do := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, srv.URL()+path, rd)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	mustStatus := func(want int, method, path string, body any) map[string]any {
		t.Helper()
		got, out := do(method, path, body)
		if got != want {
			t.Fatalf("%s %s: want %d, got %d (%v)", method, path, want, got, out)
		}
		return out
	}

	// Board-created tasks: agent-created ones cannot leave backlog by transition.
	_, ta := postTaskAs(t, srv, token, map[string]any{"name": "authz A"})
	a := ta["id"].(string)
	_, tb := postTaskAs(t, srv, token, map[string]any{"name": "authz B"})
	b := tb["id"].(string)

	mustStatus(http.StatusOK, "POST", "/api/tasks/"+a+"/governance", map[string]any{"approval_threshold": 1, "require_review": true})
	mustStatus(http.StatusCreated, "POST", "/api/tasks/"+a+"/reviewers", map[string]any{"reviewer_id": "rev-1"})
	mustStatus(http.StatusCreated, "POST", "/api/tasks/"+a+"/approvers", map[string]any{"approver_id": "app-1"})
	mustStatus(http.StatusOK, "POST", "/api/tasks/"+a+"/transition", map[string]any{"to": "in_progress"})
	mustStatus(http.StatusOK, "POST", "/api/tasks/"+a+"/transition", map[string]any{"to": "in_review"})

	mustStatus(http.StatusForbidden, "POST", "/api/tasks/"+a+"/review", map[string]any{"reviewer_id": "stranger", "decision": "approved"})
	mustStatus(http.StatusForbidden, "POST", "/api/tasks/"+a+"/approve", map[string]any{"approver_id": "stranger", "vote": "approved"})
	mustStatus(http.StatusUnprocessableEntity, "POST", "/api/tasks/"+a+"/transition", map[string]any{"to": "done"})

	snap := mustStatus(http.StatusOK, "GET", "/api/tasks/"+a+"/governance", nil)
	reviews, _ := snap["reviews"].([]any)
	votes, _ := snap["votes"].([]any)
	if len(reviews) != 0 || len(votes) != 0 {
		t.Fatalf("forbidden submissions were recorded: reviews=%v votes=%v", snap["reviews"], snap["votes"])
	}

	mustStatus(http.StatusOK, "POST", "/api/tasks/"+a+"/review", map[string]any{"reviewer_id": "rev-1", "decision": "approved"})
	mustStatus(http.StatusOK, "POST", "/api/tasks/"+a+"/approve", map[string]any{"approver_id": "app-1", "vote": "approved"})
	mustStatus(http.StatusOK, "POST", "/api/tasks/"+a+"/transition", map[string]any{"to": "done"})

	ix := mustStatus(http.StatusCreated, "POST", "/api/tasks/"+a+"/interactions", map[string]any{
		"kind": "request_confirmation", "payload": `{"prompt":"ok?"}`,
	})
	iid := int(ix["id"].(float64))
	mustStatus(http.StatusNotFound, "POST", fmt.Sprintf("/api/tasks/%s/interactions/%d/resolve", b, iid), map[string]any{"status": "accepted"})
	list := mustStatus(http.StatusOK, "GET", "/api/tasks/"+a+"/interactions", nil)
	items, _ := list["interactions"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["status"] != "pending" {
		t.Fatalf("cross-task resolve touched the interaction: %v", items)
	}
	mustStatus(http.StatusOK, "POST", fmt.Sprintf("/api/tasks/%s/interactions/%d/resolve", a, iid), map[string]any{"status": "accepted"})
}
