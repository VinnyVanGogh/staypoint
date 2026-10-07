package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// ---------------------------------------------------------------------------
// 6. tasks.work_kind — POST /api/tasks work_kind field
// ---------------------------------------------------------------------------

func postTask(t *testing.T, baseURL, token string, body map[string]any) (*http.Response, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/tasks", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestCreateTask_DefaultWorkKindIsCoding(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	resp, body := postTask(t, base, token, map[string]any{
		"name": "test-default-work-kind",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("want 201 Created, got %d: %v", resp.StatusCode, body)
	}

	workKind, _ := body["work_kind"].(string)
	if workKind != "coding" {
		t.Errorf("default work_kind: want %q, got %q", "coding", workKind)
	}
}

func TestCreateTask_ExplicitWorkKindStored(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	for _, kind := range []string{"coding", "review", "architecture", "planning", "qa"} {
		t.Run(kind, func(t *testing.T) {
			resp, body := postTask(t, base, token, map[string]any{
				"name":      "task-" + kind,
				"work_kind": kind,
			})
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("want 201, got %d: %v", resp.StatusCode, body)
			}
			got, _ := body["work_kind"].(string)
			if got != kind {
				t.Errorf("want work_kind=%q, got %q", kind, got)
			}
		})
	}
}

func TestCreateTask_UnknownWorkKindRejected(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	for _, bad := range []string{"security", "CODING", "Code", "debug", ""} {
		if bad == "" {
			// empty is OK — defaults to "coding"
			continue
		}
		t.Run(bad, func(t *testing.T) {
			resp, body := postTask(t, base, token, map[string]any{
				"name":      "task-bad",
				"work_kind": bad,
			})
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("unknown work_kind %q: want 400, got %d (body: %v)", bad, resp.StatusCode, body)
			}
		})
	}
}

func TestCreateTask_WorkKindPersistedAndFetchable(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	resp, body := postTask(t, base, token, map[string]any{
		"name":      "fetchable-task",
		"work_kind": "architecture",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: want 201, got %d: %v", resp.StatusCode, body)
	}

	taskID, _ := body["id"].(string)
	if taskID == "" {
		t.Fatal("no id in create response")
	}

	// Fetch the task and verify work_kind is persisted
	getReq, _ := http.NewRequest(http.MethodGet, base+"/api/tasks/"+taskID, nil)
	getReq.Header.Set("Authorization", "Bearer "+token)
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatalf("GET task: %v", err)
	}
	defer getResp.Body.Close()

	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET task: want 200, got %d", getResp.StatusCode)
	}

	var fetched map[string]any
	_ = json.NewDecoder(getResp.Body).Decode(&fetched)

	got, _ := fetched["work_kind"].(string)
	if got != "architecture" {
		t.Errorf("fetched work_kind: want %q, got %q", "architecture", got)
	}
}
