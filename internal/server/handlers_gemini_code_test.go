package server_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/geminiapproval"
	"github.com/VinnyVanGogh/staypoint/internal/security"
)

func geminiCodeBody(s geminiapproval.Scope) string {
	return `{"cmdline":` + jsonString(s.Cmdline()) + `,"reasons":["test"],"run_id":"` + geminiapproval.RunID + `"}`
}

// Board addition (2026-10-06): an agent can file a Gemini-code request for a
// personal repo but never approve it; the Board approves with a passkey; and
// work repos are refused at create and at approve, even for the Board.
func TestGeminiCodeApproval_BoardOnlyAndPersonalOnly(t *testing.T) {
	database := setupTestDB(t)
	seedBoardWebAuthnCredential(t, database)
	srv, token := startTestServer(t, database)
	personal := filepath.Join(t.TempDir(), "personal-app")
	work := filepath.Join(t.TempDir(), "mansol-app")

	// Work repo: refused at create.
	status, _, raw := doBoard(t, srv, token, boardReq{method: "POST", path: "/api/security/gate-requests",
		body: geminiCodeBody(geminiapproval.Scope{SessionID: "conv-1", Repo: work})})
	if status != http.StatusForbidden || !strings.Contains(raw, "work repo") {
		t.Fatalf("work repo create: want 403, got %d %s", status, raw)
	}
	// Malformed scope: refused.
	status, _, _ = doBoard(t, srv, token, boardReq{method: "POST", path: "/api/security/gate-requests",
		body: `{"cmdline":"staypoint gate gemini-code --session x","run_id":"` + geminiapproval.RunID + `"}`})
	if status != http.StatusBadRequest {
		t.Fatalf("malformed create: want 400, got %d", status)
	}

	// Personal repo: the agent files it...
	status, _, raw = doBoard(t, srv, token, boardReq{method: "POST", path: "/api/security/gate-requests",
		body: geminiCodeBody(geminiapproval.Scope{SessionID: "conv-1", Repo: personal})})
	if status != http.StatusCreated {
		t.Fatalf("personal create: %d %s", status, raw)
	}
	var gr security.GateRequest
	if err := json.Unmarshal([]byte(raw), &gr); err != nil || gr.ID == "" {
		t.Fatalf("decode: %v %s", err, raw)
	}
	// ...but cannot approve it with the session token.
	status, _, _ = doBoard(t, srv, token, boardReq{method: "POST",
		path: "/api/security/gate-requests/" + gr.ID + "/decide", body: `{"decision":"approved"}`})
	if status != http.StatusForbidden {
		t.Fatalf("agent self-approve: want 403, got %d", status)
	}
	isWork := func(p string) bool { return strings.Contains(p, "mansol") }
	if ok, _ := geminiapproval.SessionApproved(database, "conv-1", personal, isWork, time.Now()); ok {
		t.Fatal("session approved without the Board")
	}

	setter := any(srv).(webAuthnVerifierSetter)
	setter.SetWebAuthnVerifier(func(*http.Request, string) error { return nil })
	status, _, raw = doBoard(t, srv, token, boardReq{method: "POST",
		path: "/api/security/gate-requests/" + gr.ID + "/decide", body: `{"decision":"approved"}`, cookie: true, assertion: "ok"})
	if status != http.StatusOK {
		t.Fatalf("Board approve: %d %s", status, raw)
	}
	if ok, _ := geminiapproval.SessionApproved(database, "conv-1", personal, isWork, time.Now()); !ok {
		t.Fatal("session not approved after Board approval")
	}

	// A pending work-repo row slipped into the DB: the Board cannot approve it.
	forged := geminiapproval.Scope{TaskID: "task-x", Repo: work}
	if _, err := database.Exec(`INSERT INTO security_gate_requests (id, cmdline, run_id, status, created_at) VALUES ('forged', ?, ?, 'pending', ?)`,
		forged.Cmdline(), geminiapproval.RunID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	status, _, raw = doBoard(t, srv, token, boardReq{method: "POST",
		path: "/api/security/gate-requests/forged/decide", body: `{"decision":"approved"}`, cookie: true, assertion: "ok"})
	if status != http.StatusForbidden {
		t.Fatalf("Board approve of a work-repo request: want 403, got %d %s", status, raw)
	}
	if gr, _ := security.GetGateRequest(database, "forged"); gr == nil || gr.Status != security.GateRequestPending {
		t.Fatalf("work-repo request changed: %+v", gr)
	}
	// Denying it is allowed.
	status, _, _ = doBoard(t, srv, token, boardReq{method: "POST",
		path: "/api/security/gate-requests/forged/decide", body: `{"decision":"denied"}`, cookie: true, assertion: "ok"})
	if status != http.StatusOK {
		t.Fatalf("Board deny of a work-repo request: %d", status)
	}
}

// STA-838 API: provider/model on create and PUT /provider, with gemini on a
// code kind refused in a work repo and accepted (approval-gated) in a
// personal one.
func TestTaskProviderChoiceAPI(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	personal := filepath.Join(t.TempDir(), "personal-app")
	work := filepath.Join(t.TempDir(), "mansol-app")
	post := func(body map[string]any) (int, map[string]any) {
		b, _ := json.Marshal(body)
		status, _, raw := doBoard(t, srv, token, boardReq{method: "POST", path: "/api/tasks", body: string(b)})
		var out map[string]any
		_ = json.Unmarshal([]byte(raw), &out)
		return status, out
	}

	status, out := post(map[string]any{"name": "docs", "work_kind": "docs", "repo_path": work, "provider": "gemini", "model_override": "flash"})
	if status != http.StatusCreated || out["provider"] != "gemini" || out["model_override"] != "gemini-3.8-flash-high" {
		t.Fatalf("gemini docs in work repo: %d %v", status, out)
	}
	status, out = post(map[string]any{"name": "code", "work_kind": "coding", "repo_path": work, "provider": "gemini"})
	if status != http.StatusBadRequest || !strings.Contains(out["error"].(string), "Gemini never writes code") {
		t.Fatalf("gemini coding in work repo: want 400, got %d %v", status, out)
	}
	status, out = post(map[string]any{"name": "code", "work_kind": "coding", "repo_path": personal, "provider": "gemini"})
	if status != http.StatusCreated || out["provider"] != "gemini" {
		t.Fatalf("gemini coding in personal repo: %d %v", status, out)
	}
	id := out["id"].(string)
	status, out = post(map[string]any{"name": "bad", "provider": "openai"})
	if status != http.StatusBadRequest {
		t.Fatalf("bad provider: %d %v", status, out)
	}

	put := func(taskID, body string) (int, string) {
		status, _, raw := doBoard(t, srv, token, boardReq{method: "PUT", path: "/api/tasks/" + taskID + "/provider", body: body})
		return status, raw
	}
	if status, raw := put(id, `{"provider":"claude","model_override":"sonnet"}`); status != http.StatusOK || !strings.Contains(raw, `"model_override":"sonnet"`) {
		t.Fatalf("PUT claude sonnet: %d %s", status, raw)
	}
	status, out = post(map[string]any{"name": "wcode", "work_kind": "qa", "repo_path": work})
	if status != http.StatusCreated {
		t.Fatalf("work qa: %d %v", status, out)
	}
	if status, raw := put(out["id"].(string), `{"provider":"gemini"}`); status != http.StatusBadRequest || !strings.Contains(raw, "work repo") {
		t.Fatalf("PUT gemini on work qa: want 400, got %d %s", status, raw)
	}
	if status, raw := put("task-nope", `{"provider":"claude"}`); status != http.StatusNotFound {
		t.Fatalf("PUT unknown task: %d %s", status, raw)
	}
}
