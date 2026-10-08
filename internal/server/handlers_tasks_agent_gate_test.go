package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
	"github.com/VinnyVanGogh/staypoint/internal/server"
)

// Surprise starts (2026-10-07): a requested backlog stage is honoured for
// children, tasks created without a Board session are agent tasks (backlog,
// Board-only to start), and an org hold is Board-only.

// postTaskBoard creates a task as the Board (session cookie, no passkey).
func postTaskBoard(t *testing.T, srv *server.Server, token string, body map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body)
	code, _, resp := doBoard(t, srv, token, boardReq{method: http.MethodPost, path: "/api/tasks", body: string(raw), cookie: true})
	var out map[string]any
	_ = json.Unmarshal([]byte(resp), &out)
	if code != http.StatusCreated {
		t.Fatalf("board create: %d %s", code, resp)
	}
	return out
}

// postTaskAs is postTask as the Board (session cookie), for tests that model
// Board-created tasks.
func postTaskAs(t *testing.T, srv *server.Server, token string, body map[string]any) (*http.Response, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, srv.URL()+"/api/tasks", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.AddCookie(&http.Cookie{Name: "staypoint_board", Value: srv.BoardToken()})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func stageReq(t *testing.T, srv *server.Server, token, id, stage string, cookie bool, assertion string) (int, string, string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"stage": stage})
	return doBoard(t, srv, token, boardReq{method: http.MethodPost, path: "/api/tasks/" + id + "/stage", body: string(raw), cookie: cookie, assertion: assertion})
}

// The 2026-10-07 incident: POST /api/tasks with parent_id and
// execution_stage=backlog came back todo and ran at once.
func TestCreateChild_BacklogIsHonouredAndNeverRuns(t *testing.T) {
	database, srv, token := startBoardGateServer(t)
	parent := postTaskBoard(t, srv, token, map[string]any{"name": "parent", "repo_path": "/repo/x", "organization": "acme"})
	child := postTaskBoard(t, srv, token, map[string]any{
		"name": "child", "parent_id": parent["id"], "work_kind": "coding", "execution_stage": "backlog",
	})
	if child["execution_stage"] != "backlog" {
		t.Fatalf("Board child created as backlog came back %v", child["execution_stage"])
	}
	id := child["id"].(string)
	h := &orchestrator.Harness{DB: database}
	if err := h.Claim(context.Background(), id, "run-1", "agent"); !errors.Is(err, orchestrator.ErrNotRunnable) {
		t.Fatalf("claim on backlog child: err = %v, want ErrNotRunnable", err)
	}
	// Default (no stage) Board child still starts in todo.
	todo := postTaskBoard(t, srv, token, map[string]any{"name": "child2", "parent_id": parent["id"]})
	if todo["execution_stage"] != "todo" || todo["origin"] != "native" {
		t.Errorf("Board child default: stage %v origin %v, want todo/native", todo["execution_stage"], todo["origin"])
	}
}

func TestCreate_AgentTokenLandsInBacklog(t *testing.T) {
	_, srv, token := startBoardGateServer(t)
	base := srv.URL()

	resp, top := postTask(t, base, token, map[string]any{"name": "agent top", "repo_path": "/repo/x", "execution_stage": "todo"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %v", resp.StatusCode, top)
	}
	if top["execution_stage"] != "backlog" || top["origin"] != "agent" {
		t.Errorf("agent top-level: stage %v origin %v, want backlog/agent", top["execution_stage"], top["origin"])
	}
	resp, child := postTask(t, base, token, map[string]any{"name": "agent child", "parent_id": top["id"]})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("child: %d %v", resp.StatusCode, child)
	}
	if child["execution_stage"] != "backlog" || child["origin"] != "agent" {
		t.Errorf("agent child: stage %v origin %v, want backlog/agent", child["execution_stage"], child["origin"])
	}
}

func TestStage_AgentTaskLeavesBacklogOnlyByBoard(t *testing.T) {
	_, srv, token := startBoardGateServer(t)
	_, task := postTask(t, srv.URL(), token, map[string]any{"name": "agent task", "repo_path": "/repo/x"})
	id := task["id"].(string)

	for _, stage := range []string{"todo", "in_progress", "in_review"} {
		if code, errCode, body := stageReq(t, srv, token, id, stage, false, ""); code != http.StatusForbidden || errCode != "board_session_required" {
			t.Errorf("agent token -> %s: %d %s, want 403 board_session_required", stage, code, body)
		}
	}
	// The governance transition endpoint is not a way around it.
	if code, body := postJSON(t, srv.URL()+"/api/tasks/"+id+"/transition", token, map[string]any{"to": "todo"}); code != http.StatusForbidden {
		t.Errorf("agent transition -> todo: %d %v, want 403", code, body)
	}
	// Agents may still close it.
	if code, _, body := stageReq(t, srv, token, id, "cancelled", false, ""); code != http.StatusOK {
		t.Errorf("agent cancel: %d %s", code, body)
	}
	if code, _, body := stageReq(t, srv, token, id, "backlog", false, ""); code != http.StatusOK {
		t.Errorf("agent reopen to backlog: %d %s", code, body)
	}
	// The Board session moves it (no passkey needed for a non-prod task).
	if code, _, body := stageReq(t, srv, token, id, "todo", true, ""); code != http.StatusOK {
		t.Fatalf("board -> todo: %d %s", code, body)
	}
}

func TestStage_ProdAgentTaskNeedsTouchID(t *testing.T) {
	_, srv, token := startBoardGateServer(t)
	_, task := postTask(t, srv.URL(), token, map[string]any{"name": "port the fix to prod", "repo_path": "/repo/x"})
	id := task["id"].(string)

	if code, errCode, body := stageReq(t, srv, token, id, "todo", true, ""); code != http.StatusForbidden || errCode != "board_passkey_assertion_required" {
		t.Fatalf("board session without passkey: %d %s, want 403 board_passkey_assertion_required", code, body)
	}
	if code, _, body := stageReq(t, srv, token, id, "todo", false, goodBoardAssertion); code != http.StatusForbidden {
		t.Fatalf("agent token with a passkey header but no session: %d %s, want 403", code, body)
	}
	if code, _, body := stageReq(t, srv, token, id, "todo", true, goodBoardAssertion); code != http.StatusOK {
		t.Fatalf("board + passkey: %d %s", code, body)
	}
}

func orgHoldReq(t *testing.T, srv *server.Server, token, org string, held, cookie bool, assertion string) (int, string, string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"organization": org, "held": held})
	return doBoard(t, srv, token, boardReq{method: http.MethodPost, path: "/api/settings/org-hold", body: string(raw), cookie: cookie, assertion: assertion})
}

func TestOrgHold_BoardOnlyAndBlocksRunNow(t *testing.T) {
	database, srv, token := startBoardGateServer(t)

	// The agent token cannot place or lift a hold; a Board session needs Touch ID.
	if code, _, body := orgHoldReq(t, srv, token, "Managed Solution", true, false, ""); code != http.StatusForbidden {
		t.Fatalf("agent token hold: %d %s, want 403", code, body)
	}
	if code, _, body := orgHoldReq(t, srv, token, "Managed Solution", true, true, ""); code != http.StatusForbidden {
		t.Fatalf("board session without passkey: %d %s, want 403", code, body)
	}
	if governance.OrgHeld(database, "Managed Solution") {
		t.Fatal("a refused request placed the hold")
	}
	if code, _, body := orgHoldReq(t, srv, token, "Managed Solution", true, true, goodBoardAssertion); code != http.StatusOK {
		t.Fatalf("board hold: %d %s", code, body)
	}
	if !governance.OrgHeld(database, "managed solution") {
		t.Fatal("hold not saved")
	}
	code, _, body := doBoard(t, srv, token, boardReq{method: http.MethodGet, path: "/api/settings/org-hold"})
	if code != http.StatusOK || body == "" {
		t.Fatalf("get holds: %d %s", code, body)
	}
	var got struct{ Held []string }
	_ = json.Unmarshal([]byte(body), &got)
	if len(got.Held) != 1 || got.Held[0] != "managed solution" {
		t.Errorf("held list = %v", got.Held)
	}

	// Run Now on a held org's task is refused, even for the Board.
	task := postTaskBoard(t, srv, token, map[string]any{"name": "work", "repo_path": "/repo/x", "organization": "Managed Solution"})
	id := task["id"].(string)
	if code, _, body := stageReq(t, srv, token, id, "in_progress", true, ""); code != http.StatusConflict {
		t.Fatalf("run now while held: %d %s, want 409", code, body)
	}
	var n int
	_ = database.QueryRow(`SELECT COUNT(*) FROM activity_log WHERE task_id = ? AND event_type = 'wake_held'`, id).Scan(&n)
	if n == 0 {
		t.Error("held Run Now was not logged as held")
	}

	// The agent token cannot lift it; the Board can.
	if code, _, body := orgHoldReq(t, srv, token, "Managed Solution", false, false, ""); code != http.StatusForbidden {
		t.Fatalf("agent token lift: %d %s, want 403", code, body)
	}
	if !governance.OrgHeld(database, "Managed Solution") {
		t.Fatal("agent lifted the hold")
	}
	if code, _, body := orgHoldReq(t, srv, token, "Managed Solution", false, true, goodBoardAssertion); code != http.StatusOK {
		t.Fatalf("board lift: %d %s", code, body)
	}
	if governance.OrgHeld(database, "Managed Solution") {
		t.Fatal("hold not lifted")
	}
	got1, _ := meshContext.GetTask(database, id)
	if got1.ExecutionStage != "todo" {
		t.Errorf("refused Run Now changed the stage to %s", got1.ExecutionStage)
	}
}
