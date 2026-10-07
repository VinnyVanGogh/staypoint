package server_test

import (
	"net/http"
	"path/filepath"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
)

// STA-861: the Board marks any open task done from the task page without a
// work product ({"board": true}, Board gate required); agents and token-only
// callers keep the work-product requirement; work products and work_kind can
// be set through the API.

func newAPITask(t *testing.T, srvURL, token string, body map[string]any) string {
	t.Helper()
	resp, task := postTask(t, srvURL, token, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create task: %d %v", resp.StatusCode, task)
	}
	return task["id"].(string)
}

func TestDone_BoardWithoutWorkProduct(t *testing.T) {
	database, srv, token := startBoardGateServer(t)
	id := newAPITask(t, srv.URL(), token, map[string]any{"name": "research", "repo_path": "/repo/x", "work_kind": "review"})

	// Token-only caller, no product: still refused.
	if status, _, raw := boardPost(t, srv, token, "/api/tasks/"+id+"/done", map[string]any{}, false); status != http.StatusConflict {
		t.Fatalf("agent done without product: want 409, got %d %s", status, raw)
	}
	// Token-only caller asking for the Board path: refused by the gate.
	if status, code, raw := boardPost(t, srv, token, "/api/tasks/"+id+"/done", map[string]any{"board": true}, false); status != http.StatusForbidden || code != "board_session_required" {
		t.Fatalf("token-only board done: want 403 board_session_required, got %d %s", status, raw)
	}
	assertStillOpen(t, database, id)

	// The Board, through the gate: done, with the note on the timeline.
	if status, _, raw := boardPost(t, srv, token, "/api/tasks/"+id+"/done", map[string]any{"board": true, "note": "nothing to ship"}, true); status != http.StatusOK {
		t.Fatalf("board done: want 200, got %d %s", status, raw)
	}
	task, _ := meshContext.GetTask(database, id)
	if task.Status != "done" || task.ExecutionStage != "done" {
		t.Fatalf("after board done: status=%s stage=%s", task.Status, task.ExecutionStage)
	}
	comments, _ := meshContext.GetTaskComments(database, id)
	found := false
	for _, c := range comments {
		if c.Author == "board" && c.Message == "Marked done by Board: nothing to ship" {
			found = true
		}
	}
	if !found {
		t.Errorf("timeline note missing: %+v", comments)
	}
}

func TestDone_BoardStillRespectsOpenChildren(t *testing.T) {
	database, srv, token := startBoardGateServer(t)
	parentID := parentWithOpenChild(t, database, srv, token)
	if status, _, raw := boardPost(t, srv, token, "/api/tasks/"+parentID+"/done", map[string]any{"board": true}, true); status != http.StatusConflict {
		t.Fatalf("board done with open child: want 409, got %d %s", status, raw)
	}
	assertStillOpen(t, database, parentID)
	if status, _, raw := boardPost(t, srv, token, "/api/tasks/"+parentID+"/done", map[string]any{"board": true, "override": true}, true); status != http.StatusOK {
		t.Fatalf("board done + override: want 200, got %d %s", status, raw)
	}
}

func TestAddWorkProduct_API(t *testing.T) {
	database, srv, token := startBoardGateServer(t)
	id := newAPITask(t, srv.URL(), token, map[string]any{"name": "w", "repo_path": "/repo/x"})

	status, _, raw := boardPost(t, srv, token, "/api/tasks/"+id+"/work-products", map[string]any{"type": "pr", "ref": "https://github.com/o/r/pull/9"}, false)
	if status != http.StatusCreated {
		t.Fatalf("add product: want 201, got %d %s", status, raw)
	}
	if status, _, raw := boardPost(t, srv, token, "/api/tasks/"+id+"/work-products", map[string]any{"type": "tweet", "ref": "x"}, false); status != http.StatusBadRequest {
		t.Errorf("bad type: want 400, got %d %s", status, raw)
	}
	if status, _, raw := boardPost(t, srv, token, "/api/tasks/task-missing/work-products", map[string]any{"type": "pr", "ref": "x"}, false); status != http.StatusNotFound {
		t.Errorf("missing task: want 404, got %d %s", status, raw)
	}
	// The agent's done now passes the gate.
	if status, _, raw := boardPost(t, srv, token, "/api/tasks/"+id+"/done", map[string]any{}, false); status != http.StatusOK {
		t.Fatalf("done after product: want 200, got %d %s", status, raw)
	}
	products, _ := meshContext.GetTaskWorkProducts(database, id)
	if len(products) != 1 || products[0].ProductType != "pull_request" {
		t.Errorf("products = %+v", products)
	}
}

func TestSetKind_API(t *testing.T) {
	database, srv, token := startBoardGateServer(t)
	id := newAPITask(t, srv.URL(), token, map[string]any{"name": "k", "repo_path": "/repo/x"})
	url := srv.URL() + "/api/tasks/" + id + "/kind"

	if status, body := putJSON(t, url, token, map[string]any{"work_kind": "docs"}); status != http.StatusOK || body["work_kind"] != "docs" {
		t.Fatalf("set docs: %d %v", status, body)
	}
	if status, body := putJSON(t, url, token, map[string]any{"work_kind": "poetry"}); status != http.StatusBadRequest {
		t.Errorf("bad kind: want 400, got %d %v", status, body)
	}
	if _, err := database.Exec(`UPDATE tasks SET execution_stage = 'in_progress', checkout_run_id = 'run-1' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if status, body := putJSON(t, url, token, map[string]any{"work_kind": "review"}); status != http.StatusConflict {
		t.Errorf("mid-run: want 409, got %d %v", status, body)
	}
	if task, _ := meshContext.GetTask(database, id); task.WorkKind != "docs" {
		t.Errorf("kind changed mid-run: %s", task.WorkKind)
	}
}

// #231 applies to a kind change: a gemini task in a work repo cannot become a
// code kind.
func TestSetKind_GeminiCodeKindInWorkRepoRefused(t *testing.T) {
	database, srv, token := startBoardGateServer(t)
	work := filepath.Join(t.TempDir(), "mansol-app")
	task, err := meshContext.CreateTaskWithOptions(database, meshContext.TaskCreateOptions{
		Name: "g", RepoPath: work, GitBranch: "main", WorkKind: "docs", Provider: "gemini", ExecutionStage: "backlog",
	})
	if err != nil {
		t.Fatal(err)
	}
	url := srv.URL() + "/api/tasks/" + task.ID + "/kind"
	if status, body := putJSON(t, url, token, map[string]any{"work_kind": "coding"}); status != http.StatusBadRequest {
		t.Fatalf("gemini + coding in work repo: want 400, got %d %v", status, body)
	}
	if status, body := putJSON(t, url, token, map[string]any{"work_kind": "planning"}); status != http.StatusOK {
		t.Fatalf("gemini + planning in work repo: want 200, got %d %v", status, body)
	}
}
