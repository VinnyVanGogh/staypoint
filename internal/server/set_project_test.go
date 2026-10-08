package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// PUT /api/tasks/{id}/project groups a task without touching updated_at, so
// regrouping does not reorder Recent Tasks.
func TestSetProject_GroupsWithoutBumpingUpdatedAt(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())
	_, task := postTask(t, base, token, map[string]any{"name": "group me", "repo_path": "/repo/x", "execution_stage": "backlog"})
	id := task["id"].(string)
	if _, err := database.Exec(`UPDATE tasks SET updated_at = '2026-01-01T00:00:00.000Z' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}

	put := func(taskID, project string) int {
		body, _ := json.Marshal(map[string]string{"project": project})
		req, _ := http.NewRequest(http.MethodPut, base+"/api/tasks/"+taskID+"/project", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := put(id, "  Web UI  "); code != http.StatusOK {
		t.Fatalf("set project: %d", code)
	}
	var project, updated string
	_ = database.QueryRow(`SELECT COALESCE(project, ''), updated_at FROM tasks WHERE id = ?`, id).Scan(&project, &updated)
	if project != "Web UI" {
		t.Errorf("project = %q, want trimmed %q", project, "Web UI")
	}
	if updated != "2026-01-01T00:00:00.000Z" {
		t.Errorf("updated_at changed to %s; grouping must not bump it", updated)
	}
	if code := put("task-missing", "Web UI"); code != http.StatusNotFound {
		t.Errorf("missing task: %d, want 404", code)
	}
	if code := put(id, strings.Repeat("x", 121)); code != http.StatusBadRequest {
		t.Errorf("long name: %d, want 400", code)
	}
	if code := put(id, ""); code != http.StatusOK {
		t.Errorf("clear project: %d", code)
	}
}
