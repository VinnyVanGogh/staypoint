package server_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/server"
)

// STA-859: `override` (done / stage) and `allow_deep` (child create) are
// Board overrides. They are honored only when the request passes the Board
// gate (session cookie + passkey assertion); a daemon-token-only caller that
// sets them gets 403 and nothing changes.

const goodBoardAssertion = "board-ok"

// startBoardGateServer starts a server with one Board passkey and a stub
// verifier that accepts only goodBoardAssertion.
func startBoardGateServer(t *testing.T) (*sql.DB, *server.Server, string) {
	t.Helper()
	database := setupTestDB(t)
	seedBoardWebAuthnCredential(t, database)
	srv, token := startTestServer(t, database)
	setter, ok := any(srv).(webAuthnVerifierSetter)
	if !ok {
		t.Fatal("*server.Server must implement SetWebAuthnVerifier")
	}
	setter.SetWebAuthnVerifier(func(_ *http.Request, a string) error {
		if a == goodBoardAssertion {
			return nil
		}
		return errors.New("bad assertion")
	})
	return database, srv, token
}

func boardPost(t *testing.T, srv *server.Server, token, path string, body map[string]any, board bool) (int, string, string) {
	t.Helper()
	raw, _ := json.Marshal(body)
	br := boardReq{method: http.MethodPost, path: path, body: string(raw)}
	if board {
		br.cookie = true
		br.assertion = goodBoardAssertion
	}
	return doBoard(t, srv, token, br)
}

// parentWithOpenChild creates a parent task (with a work product, so only the
// open child blocks done) and one open child.
func parentWithOpenChild(t *testing.T, database *sql.DB, srv *server.Server, token string) string {
	t.Helper()
	base := srv.URL()
	resp, parent := postTask(t, base, token, map[string]any{"name": "p", "repo_path": "/repo/x", "work_kind": "planning"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("parent: %d %v", resp.StatusCode, parent)
	}
	parentID := parent["id"].(string)
	if resp, body := postTask(t, base, token, map[string]any{"name": "c", "parent_id": parentID}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("child: %d %v", resp.StatusCode, body)
	}
	if err := meshContext.AddWorkProduct(database, parentID, "commit", "abc"); err != nil {
		t.Fatal(err)
	}
	return parentID
}

func assertStillOpen(t *testing.T, database *sql.DB, id string) {
	t.Helper()
	task, err := meshContext.GetTask(database, id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status == "done" || task.ExecutionStage == "done" {
		t.Errorf("parent closed without the Board gate: status=%s stage=%s", task.Status, task.ExecutionStage)
	}
}

func TestDoneOverride_TokenOnlyRefused(t *testing.T) {
	database, srv, token := startBoardGateServer(t)
	parentID := parentWithOpenChild(t, database, srv, token)

	for _, tc := range []struct {
		name, path string
		body       map[string]any
	}{
		{"done", "/api/tasks/" + parentID + "/done", map[string]any{"override": true}},
		{"stage", "/api/tasks/" + parentID + "/stage", map[string]any{"stage": "done", "override": true}},
	} {
		status, code, raw := boardPost(t, srv, token, tc.path, tc.body, false)
		if status != http.StatusForbidden || code != "board_session_required" {
			t.Errorf("%s token-only override: want 403 board_session_required, got %d %s", tc.name, status, raw)
		}
		assertStillOpen(t, database, parentID)
	}

	// Board cookie without a valid passkey assertion is not enough either.
	status, code, raw := doBoard(t, srv, token, boardReq{method: http.MethodPost, path: "/api/tasks/" + parentID + "/done",
		body: `{"override":true}`, cookie: true, assertion: "forged"})
	if status != http.StatusForbidden || code != "board_passkey_assertion_invalid" {
		t.Errorf("cookie + bad assertion: want 403 board_passkey_assertion_invalid, got %d %s", status, raw)
	}
	assertStillOpen(t, database, parentID)
}

func TestDoneOverride_BoardGatedAllowed(t *testing.T) {
	t.Run("done", func(t *testing.T) {
		database, srv, token := startBoardGateServer(t)
		parentID := parentWithOpenChild(t, database, srv, token)
		if status, _, raw := boardPost(t, srv, token, "/api/tasks/"+parentID+"/done", map[string]any{"override": true}, true); status != http.StatusOK {
			t.Fatalf("board-gated done override: want 200, got %d %s", status, raw)
		}
		if task, _ := meshContext.GetTask(database, parentID); task == nil || task.Status != "done" {
			t.Errorf("parent not done after Board override: %+v", task)
		}
	})
	t.Run("stage", func(t *testing.T) {
		database, srv, token := startBoardGateServer(t)
		parentID := parentWithOpenChild(t, database, srv, token)
		if status, _, raw := boardPost(t, srv, token, "/api/tasks/"+parentID+"/stage", map[string]any{"stage": "done", "override": true}, true); status != http.StatusOK {
			t.Fatalf("board-gated stage override: want 200, got %d %s", status, raw)
		}
		if task, _ := meshContext.GetTask(database, parentID); task == nil || task.ExecutionStage != "done" {
			t.Errorf("parent stage not done after Board override: %+v", task)
		}
	})
}

// Without the flag the routes stay agent-callable exactly as before.
func TestDoneWithoutOverride_StillTokenCallable(t *testing.T) {
	database, srv, token := startBoardGateServer(t)
	resp, task := postTask(t, srv.URL(), token, map[string]any{"name": "solo", "repo_path": "/repo/x"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("task: %d %v", resp.StatusCode, task)
	}
	id := task["id"].(string)
	if err := meshContext.AddWorkProduct(database, id, "commit", "abc"); err != nil {
		t.Fatal(err)
	}
	if status, _, raw := boardPost(t, srv, token, "/api/tasks/"+id+"/done", map[string]any{"override": false}, false); status != http.StatusOK {
		t.Errorf("token-only done without override: want 200, got %d %s", status, raw)
	}
}

// deepParent builds a chain at the depth cap and returns its leaf.
func deepParent(t *testing.T, database *sql.DB, srv *server.Server, token string) string {
	t.Helper()
	if err := meshContext.SetChildTaskLimits(database, 5, 1); err != nil {
		t.Fatal(err)
	}
	_, root := postTask(t, srv.URL(), token, map[string]any{"name": "root", "repo_path": "/repo/x", "work_kind": "planning"})
	resp, child := postTask(t, srv.URL(), token, map[string]any{"name": "d1", "parent_id": root["id"]})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("depth-1 child: %d %v", resp.StatusCode, child)
	}
	// Without allow_deep the next level is over the cap.
	if resp, body := postTask(t, srv.URL(), token, map[string]any{"name": "d2", "parent_id": child["id"]}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("over depth cap without allow_deep: want 409, got %d %v", resp.StatusCode, body)
	}
	return child["id"].(string)
}

func countChildren(t *testing.T, database *sql.DB, parentID string) int {
	t.Helper()
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM tasks WHERE parent_id = ?`, parentID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAllowDeep_TokenOnlyRefused(t *testing.T) {
	database, srv, token := startBoardGateServer(t)
	leaf := deepParent(t, database, srv, token)
	status, code, raw := boardPost(t, srv, token, "/api/tasks", map[string]any{"name": "d2", "parent_id": leaf, "allow_deep": true}, false)
	if status != http.StatusForbidden || code != "board_session_required" {
		t.Errorf("token-only allow_deep: want 403 board_session_required, got %d %s", status, raw)
	}
	if n := countChildren(t, database, leaf); n != 0 {
		t.Errorf("refused allow_deep still created %d child(ren)", n)
	}
}

func TestAllowDeep_BoardGatedAllowed(t *testing.T) {
	database, srv, token := startBoardGateServer(t)
	leaf := deepParent(t, database, srv, token)
	status, _, raw := boardPost(t, srv, token, "/api/tasks", map[string]any{"name": "d2", "parent_id": leaf, "allow_deep": true}, true)
	if status != http.StatusCreated {
		t.Fatalf("board-gated allow_deep: want 201, got %d %s", status, raw)
	}
	if n := countChildren(t, database, leaf); n != 1 {
		t.Errorf("board-gated allow_deep: want 1 child, got %d", n)
	}
}
