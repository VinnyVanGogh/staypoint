package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
)

func putJSON(t *testing.T, url, token string, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(raw))
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

// An imported task with no repo stays in backlog until the Board sets one.
func TestRepolessTask_SetRepoThenLeaveBacklog(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	task, err := meshContext.CreateTaskWithOptions(database, meshContext.TaskCreateOptions{
		Name: "[STA-1] imported", NoRepo: true, ExecutionStage: "backlog",
		Origin: meshContext.OriginPaperclipImport, SourceRef: "STA-1", SourceID: "uuid-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.RepoPath != "" || task.SourceRef != "STA-1" {
		t.Fatalf("created: %+v", task)
	}
	stageURL := base + "/api/tasks/" + task.ID + "/stage"
	if code, body := postJSON(t, stageURL, token, map[string]any{"stage": "in_progress"}); code != http.StatusConflict {
		t.Fatalf("run now without repo: want 409, got %d %v", code, body)
	}
	if code, _ := postJSON(t, stageURL, token, map[string]any{"stage": "cancelled"}); code != http.StatusOK {
		t.Fatalf("cancel without repo: %d", code)
	}
	if code, body := putJSON(t, base+"/api/tasks/"+task.ID+"/repo", token, map[string]any{"repo_path": "relative/path"}); code != http.StatusBadRequest {
		t.Fatalf("relative repo: want 400, got %d %v", code, body)
	}
	code, body := putJSON(t, base+"/api/tasks/"+task.ID+"/repo", token, map[string]any{"repo_path": "/repo/x/", "git_branch": "main"})
	if code != http.StatusOK || body["repo_path"] != "/repo/x" || body["source_ref"] != "STA-1" {
		t.Fatalf("set repo: %d %v", code, body)
	}
	if code, body := postJSON(t, stageURL, token, map[string]any{"stage": "todo"}); code != http.StatusOK {
		t.Fatalf("todo after repo: %d %v", code, body)
	}
}
