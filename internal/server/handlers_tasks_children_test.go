package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
)

func postJSON(t *testing.T, url, token string, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// STA-820: POST /api/tasks with parent_id creates an inheriting child task.
func TestCreateTask_ChildInheritsParentAndStoresHandoff(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	resp, parent := postTask(t, base, token, map[string]any{
		"name": "plan it", "repo_path": "/repo/x", "git_branch": "main",
		"organization": "acme", "project": "core", "work_kind": "planning",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("parent: %d %v", resp.StatusCode, parent)
	}
	parentID := parent["id"].(string)

	resp, child := postTask(t, base, token, map[string]any{
		"name": "code it", "parent_id": parentID, "work_kind": "coding", "handoff": "the plan text",
		// Inherited fields win over anything the caller sends.
		"repo_path": "/elsewhere",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("child: %d %v", resp.StatusCode, child)
	}
	if child["parent_id"] != parentID || child["repo_path"] != "/repo/x" || child["organization"] != "acme" || child["project"] != "core" {
		t.Errorf("child did not inherit parent: %v", child)
	}
	if child["work_kind"] != "coding" || child["execution_stage"] != "todo" {
		t.Errorf("child kind/stage: %v / %v", child["work_kind"], child["execution_stage"])
	}
	h, err := meshContext.GetTaskHandoff(database, child["id"].(string))
	if err != nil || !strings.Contains(h, "the plan text") || !strings.Contains(h, parentID) {
		t.Errorf("handoff not stored: %q %v", h, err)
	}

	// Parent detail lists the child under dependencies.subtasks.
	r, err := http.NewRequest(http.MethodGet, base+"/api/tasks/"+parentID, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	gr, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer gr.Body.Close()
	var detail struct {
		Dependencies struct {
			Subtasks []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"subtasks"`
		} `json:"dependencies"`
	}
	_ = json.NewDecoder(gr.Body).Decode(&detail)
	if len(detail.Dependencies.Subtasks) != 1 || detail.Dependencies.Subtasks[0].ID != child["id"] {
		t.Errorf("parent subtasks: %+v", detail.Dependencies.Subtasks)
	}
}

func TestCreateTask_ChildErrors(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())
	_, parent := postTask(t, base, token, map[string]any{"name": "p", "repo_path": "/repo/x", "work_kind": "planning"})
	parentID := parent["id"].(string)

	if resp, body := postTask(t, base, token, map[string]any{"name": "c", "parent_id": parentID, "work_kind": "review"}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid kind: want 400, got %d %v", resp.StatusCode, body)
	}
	if resp, body := postTask(t, base, token, map[string]any{"name": "c", "parent_id": "task-missing"}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing parent: want 404, got %d %v", resp.StatusCode, body)
	}
	if err := meshContext.SetChildTaskLimits(database, 1, 2); err != nil {
		t.Fatal(err)
	}
	if resp, body := postTask(t, base, token, map[string]any{"name": "c1", "parent_id": parentID}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first child: %d %v", resp.StatusCode, body)
	}
	if resp, body := postTask(t, base, token, map[string]any{"name": "c2", "parent_id": parentID}); resp.StatusCode != http.StatusConflict {
		t.Errorf("over cap: want 409, got %d %v", resp.StatusCode, body)
	}
}

func TestMarkDone_ParentWithOpenChildrenNeedsOverride(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())
	_, parent := postTask(t, base, token, map[string]any{"name": "p", "repo_path": "/repo/x", "work_kind": "planning"})
	parentID := parent["id"].(string)
	if resp, body := postTask(t, base, token, map[string]any{"name": "c", "parent_id": parentID}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("child: %d %v", resp.StatusCode, body)
	}
	if err := meshContext.AddWorkProduct(database, parentID, "commit", "abc"); err != nil {
		t.Fatal(err)
	}

	if code, body := postJSON(t, base+"/api/tasks/"+parentID+"/done", token, nil); code != http.StatusConflict {
		t.Errorf("done with open child: want 409, got %d %v", code, body)
	}
	if code, body := postJSON(t, base+"/api/tasks/"+parentID+"/stage", token, map[string]any{"stage": "done"}); code != http.StatusConflict {
		t.Errorf("stage done with open child: want 409, got %d %v", code, body)
	}
	if code, body := postJSON(t, base+"/api/tasks/"+parentID+"/done", token, map[string]any{"override": true}); code != http.StatusOK {
		t.Errorf("board override: want 200, got %d %v", code, body)
	}
}
