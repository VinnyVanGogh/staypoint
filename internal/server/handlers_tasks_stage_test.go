package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
)

func listTaskIDs(t *testing.T, base, token, query string) (int, map[string]bool) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/api/tasks"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Tasks []meshContext.Task `json:"tasks"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	ids := map[string]bool{}
	for _, tk := range out.Tasks {
		ids[tk.ID] = true
	}
	return resp.StatusCode, ids
}

func TestSetStage_AcceptsBacklogAndCancelled(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	resp, task := postTask(t, base, token, map[string]any{"name": "s", "repo_path": "/repo/x", "git_branch": "main"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %v", resp.StatusCode, task)
	}
	id := task["id"].(string)
	if task["origin"] != "native" {
		t.Errorf("new task origin = %v, want native", task["origin"])
	}

	for _, stage := range []string{"backlog", "todo", "in_review", "cancelled", "backlog"} {
		if code, body := postJSON(t, base+"/api/tasks/"+id+"/stage", token, map[string]any{"stage": stage}); code != http.StatusOK {
			t.Fatalf("stage %s: %d %v", stage, code, body)
		}
		got, _ := meshContext.GetTask(database, id)
		if got.ExecutionStage != stage {
			t.Fatalf("after %s: stage = %s", stage, got.ExecutionStage)
		}
	}
	for _, bad := range []string{"bogus", "paused", "blocked"} {
		if code, body := postJSON(t, base+"/api/tasks/"+id+"/stage", token, map[string]any{"stage": bad}); code != http.StatusBadRequest {
			t.Errorf("stage %s: want 400, got %d %v", bad, code, body)
		}
	}
}

// Run Now on a backlog task moves it out of backlog before waking a run.
func TestSetStage_RunNowFromBacklog(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	resp, task := postTask(t, base, token, map[string]any{"name": "parked", "repo_path": "/repo/x", "git_branch": "main", "execution_stage": "backlog"})
	if resp.StatusCode != http.StatusCreated || task["execution_stage"] != "backlog" {
		t.Fatalf("create backlog: %d %v", resp.StatusCode, task)
	}
	id := task["id"].(string)
	if code, body := postJSON(t, base+"/api/tasks/"+id+"/stage", token, map[string]any{"stage": "in_progress"}); code != http.StatusOK {
		t.Fatalf("run now: %d %v", code, body)
	}
	got, _ := meshContext.GetTask(database, id)
	if got.ExecutionStage != "in_progress" {
		t.Fatalf("stage = %s, want in_progress", got.ExecutionStage)
	}

	if resp, body := postTask(t, base, token, map[string]any{"name": "x", "repo_path": "/repo/x", "git_branch": "main", "execution_stage": "done"}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("create in done: want 400, got %d %v", resp.StatusCode, body)
	}
}

func TestListTasks_HidesLegacyByDefault(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	_, native := postTask(t, base, token, map[string]any{"name": "native", "repo_path": "/repo/x", "git_branch": "main"})
	_, legacy := postTask(t, base, token, map[string]any{"name": "old", "repo_path": "/repo/x", "git_branch": "main"})
	nativeID, legacyID := native["id"].(string), legacy["id"].(string)
	if _, err := database.Exec(`UPDATE tasks SET origin = 'legacy' WHERE id = ?`, legacyID); err != nil {
		t.Fatal(err)
	}

	_, ids := listTaskIDs(t, base, token, "")
	if !ids[nativeID] || ids[legacyID] {
		t.Errorf("default list: %v (legacy must be hidden)", ids)
	}
	_, ids = listTaskIDs(t, base, token, "?include_legacy=1")
	if !ids[nativeID] || !ids[legacyID] {
		t.Errorf("include_legacy: %v", ids)
	}
	_, ids = listTaskIDs(t, base, token, "?origin=legacy")
	if ids[nativeID] || !ids[legacyID] {
		t.Errorf("origin=legacy: %v", ids)
	}
	_, ids = listTaskIDs(t, base, token, "?stage=todo&include_legacy=true")
	if !ids[nativeID] || !ids[legacyID] {
		t.Errorf("stage=todo: %v", ids)
	}
	_, ids = listTaskIDs(t, base, token, "?stage=backlog")
	if len(ids) != 0 {
		t.Errorf("stage=backlog: %v", ids)
	}
	if code, _ := listTaskIDs(t, base, token, "?origin=bogus"); code != http.StatusBadRequest {
		t.Errorf("origin=bogus: want 400, got %d", code)
	}
}
