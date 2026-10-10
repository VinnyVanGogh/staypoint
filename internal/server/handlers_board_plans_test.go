package server_test

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/server"
)

// task-e1b24d66: Board action plans. One passkey assertion, bound to the exact
// selected rows of one plan, runs a batch of Board actions through the same
// routes as the per-task buttons. Failure cases first.

type planFixture struct {
	t        *testing.T
	db       *sql.DB
	srv      *server.Server
	base     string
	token    string
	board    string
	taskID   string // has a pending Ship Review card on a real repo
	cardHead string
}

// newPlanFixture starts a daemon with one enrolled passkey and a task with a
// pending, approvable Ship Review card. The signature check is stubbed: an
// assertion is valid only if it is "signed:" + hex(the challenge the daemon
// minted), which stands in for the authenticator signing that challenge. No
// per-route verifier stub is installed, so a row can only pass
// WrapBoardAction through the signed plan.
func newPlanFixture(t *testing.T) *planFixture {
	t.Helper()
	database := setupTestDB(t)
	seedBoardWebAuthnCredential(t, database)
	srv, token := startTestServer(t, database)
	srv.SetPlanAssertionValidator(func(challenge []byte, assertion string) (string, error) {
		if assertion != "signed:"+hex.EncodeToString(challenge) {
			return "", errors.New("signature does not match the challenge")
		}
		return "cred-test-1", nil
	})
	f := &planFixture{t: t, db: database, srv: srv, base: srv.URL(), token: token, board: srv.BoardToken()}
	f.taskID, _ = createShipTask(t, database, f.base, token, &http.Client{})
	body, _ := json.Marshal(map[string]any{"test_steps": []string{"1. Open /"}})
	resp, rb := shipDoReq(t, &http.Client{}, token, "PUT", f.base+"/api/tasks/"+f.taskID+"/ship-review", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("UpsertCard: %d %s", resp.StatusCode, rb)
	}
	if err := database.QueryRow(`SELECT head_sha FROM ship_review_cards WHERE task_id = ? ORDER BY created_at DESC LIMIT 1`, f.taskID).Scan(&f.cardHead); err != nil {
		t.Fatalf("card head: %v", err)
	}
	return f
}

// do sends a request; cookie adds the Board session, hdr adds headers.
func (f *planFixture) do(method, path string, body any, cookie bool, hdr map[string]string) (int, []byte, http.Header) {
	f.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, f.base+path, rd)
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")
	if cookie {
		req.AddCookie(&http.Cookie{Name: "staypoint_board", Value: f.board})
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, resp.Header
}

func (f *planFixture) propose(actions ...map[string]any) string {
	f.t.Helper()
	status, raw, _ := f.do("POST", "/api/board/plans", map[string]any{"proposer": "test-session", "actions": actions}, false,
		map[string]string{"X-Board-Token": f.board})
	if status != http.StatusCreated {
		f.t.Fatalf("propose: %d %s", status, raw)
	}
	var out struct {
		Plan struct {
			ID string `json:"id"`
		} `json:"plan"`
	}
	_ = json.Unmarshal(raw, &out)
	return out.Plan.ID
}

// sign mints a plan challenge for selected and returns the session token and
// a valid assertion over it.
func (f *planFixture) sign(planID string, selected ...int) (string, string) {
	f.t.Helper()
	status, raw, hdr := f.do("POST", "/api/board/plans/"+planID+"/challenge", map[string]any{"selected": selected}, true, nil)
	if status != http.StatusOK {
		f.t.Fatalf("challenge: %d %s", status, raw)
	}
	var opts struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(raw, &opts); err != nil {
		f.t.Fatalf("challenge body: %v %s", err, raw)
	}
	ch, err := base64.RawURLEncoding.DecodeString(opts.PublicKey.Challenge)
	if err != nil {
		f.t.Fatalf("decode challenge: %v", err)
	}
	return hdr.Get("X-WebAuthn-Session"), "signed:" + hex.EncodeToString(ch)
}

type planExecOut struct {
	Error   string `json:"error"`
	Results []struct {
		Index  int    `json:"index"`
		Status string `json:"status"`
		Error  string `json:"error"`
	} `json:"results"`
}

func (f *planFixture) execute(planID, session, assertion string, selected ...int) (int, planExecOut, string) {
	f.t.Helper()
	status, raw, _ := f.do("POST", "/api/board/plans/"+planID+"/execute", map[string]any{"selected": selected}, true,
		map[string]string{"X-WebAuthn-Session": session, "X-WebAuthn-Assertion": assertion})
	var out planExecOut
	_ = json.Unmarshal(raw, &out)
	return status, out, string(raw)
}

func (f *planFixture) cardStatus() string {
	f.t.Helper()
	var s string
	if err := f.db.QueryRow(`SELECT status FROM ship_review_cards WHERE task_id = ? ORDER BY created_at DESC LIMIT 1`, f.taskID).Scan(&s); err != nil {
		f.t.Fatalf("card status: %v", err)
	}
	return s
}

func (f *planFixture) planStatus(id string) string {
	f.t.Helper()
	var s string
	if err := f.db.QueryRow(`SELECT status FROM board_action_plans WHERE id = ?`, id).Scan(&s); err != nil {
		f.t.Fatalf("plan status: %v", err)
	}
	return s
}

func (f *planFixture) planRowAudits(planID string) []map[string]any {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT payload FROM board_audit_log WHERE event_type = 'board_action' ORDER BY id`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var p string
		_ = rows.Scan(&p)
		var m map[string]any
		if json.Unmarshal([]byte(p), &m) == nil && m["action"] == "board_plan_row" && m["plan_id"] == planID {
			out = append(out, m)
		}
	}
	return out
}

func sendBack(taskID, text string) map[string]any {
	return map[string]any{"task_id": taskID, "action": "send_back", "text": text, "reason": "test"}
}

// An agent holding only the session token, or a request flagged as a daemon
// run, cannot propose; nothing is stored.
func TestBoardPlan_ProposeRefusedForAgents(t *testing.T) {
	f := newPlanFixture(t)
	body := map[string]any{"actions": []map[string]any{sendBack(f.taskID, "x")}}

	if status, raw, _ := f.do("POST", "/api/board/plans", body, false, nil); status != http.StatusForbidden {
		t.Fatalf("agent token only: want 403, got %d %s", status, raw)
	}
	if status, raw, _ := f.do("POST", "/api/board/plans", body, false, map[string]string{"X-Board-Token": "wrong-token-wrong-token"}); status != http.StatusForbidden {
		t.Fatalf("wrong board token: want 403, got %d %s", status, raw)
	}
	if status, raw, _ := f.do("POST", "/api/board/plans", body, false,
		map[string]string{"X-Board-Token": f.board, "X-StayPoint-Task-ID": f.taskID}); status != http.StatusForbidden {
		t.Fatalf("daemon run: want 403, got %d %s", status, raw)
	}
	var n int
	_ = f.db.QueryRow(`SELECT COUNT(*) FROM board_action_plans`).Scan(&n)
	if n != 0 {
		t.Fatalf("refused proposals stored %d plans", n)
	}
}

// A plan changed after the Board signed it is refused and nothing runs.
func TestBoardPlan_TamperedAfterSigningRefused(t *testing.T) {
	f := newPlanFixture(t)
	id := f.propose(sendBack(f.taskID, "please add tests"))
	session, assertion := f.sign(id, 0)

	// Swap the comment (and keep the content hash consistent, as a careful
	// attacker with DB access would).
	var actions string
	_ = f.db.QueryRow(`SELECT actions_json FROM board_action_plans WHERE id = ?`, id).Scan(&actions)
	tampered := strings.Replace(actions, "please add tests", "ship it as is", 1)
	if _, err := f.db.Exec(`UPDATE board_action_plans SET actions_json = ? WHERE id = ?`, tampered, id); err != nil {
		t.Fatal(err)
	}

	status, _, raw := f.execute(id, session, assertion, 0)
	if status != http.StatusForbidden {
		t.Fatalf("tampered plan: want 403, got %d %s", status, raw)
	}
	if got := f.cardStatus(); got != "pending" {
		t.Fatalf("card changed to %q by a tampered plan", got)
	}
	if got := f.planStatus(id); got != "pending" {
		t.Fatalf("plan status %q after refused execute", got)
	}
}

// An assertion signed for one plan cannot run another, cannot be used twice,
// and an executed plan cannot run again.
func TestBoardPlan_AssertionReplayRefused(t *testing.T) {
	f := newPlanFixture(t)
	a := f.propose(sendBack(f.taskID, "plan A"))
	b := f.propose(sendBack(f.taskID, "plan B"))
	session, assertion := f.sign(a, 0)

	if status, _, raw := f.execute(b, session, assertion, 0); status != http.StatusForbidden {
		t.Fatalf("plan A's assertion on plan B: want 403, got %d %s", status, raw)
	}
	// The session was consumed by the refused attempt: single use.
	if status, _, raw := f.execute(a, session, assertion, 0); status != http.StatusForbidden {
		t.Fatalf("reused session: want 403, got %d %s", status, raw)
	}
	// A forged assertion over the right challenge shape but wrong bytes fails.
	s2, _ := f.sign(a, 0)
	if status, _, raw := f.execute(a, s2, "signed:00", 0); status != http.StatusForbidden {
		t.Fatalf("bad signature: want 403, got %d %s", status, raw)
	}
	// A normal Board action cannot spend a plan challenge either.
	s3, a3 := f.sign(a, 0)
	if status, raw, _ := f.do("POST", "/api/tasks/"+f.taskID+"/ship-review/send-back", map[string]string{"comment": "x"}, true,
		map[string]string{"X-WebAuthn-Session": s3, "X-WebAuthn-Assertion": a3}); status != http.StatusForbidden {
		t.Fatalf("plan challenge on a per-task route: want 403, got %d %s", status, raw)
	}

	s4, a4 := f.sign(a, 0)
	if status, out, raw := f.execute(a, s4, a4, 0); status != http.StatusOK || len(out.Results) != 1 || out.Results[0].Status != "ok" {
		t.Fatalf("valid execute: got %d %s", status, raw)
	}
	s5, a5 := f.signExpectingConflict(a)
	if s5 != "" {
		if status, _, raw := f.execute(a, s5, a5, 0); status == http.StatusOK {
			t.Fatalf("second execute of an executed plan succeeded: %s", raw)
		}
	}
}

// signExpectingConflict asserts an executed plan can no longer be signed.
func (f *planFixture) signExpectingConflict(planID string) (string, string) {
	f.t.Helper()
	status, raw, _ := f.do("POST", "/api/board/plans/"+planID+"/challenge", map[string]any{"selected": []int{0}}, true, nil)
	if status != http.StatusConflict {
		f.t.Fatalf("challenge on executed plan: want 409, got %d %s", status, raw)
	}
	return "", ""
}

// approve_merge refuses when the card's head is no longer the one proposed.
func TestBoardPlan_ApproveMergeMovedHeadRefused(t *testing.T) {
	f := newPlanFixture(t)
	id := f.propose(map[string]any{"task_id": f.taskID, "action": "approve_merge", "expected_head_sha": "0000000000000000000000000000000000000000"})
	session, assertion := f.sign(id, 0)
	status, out, raw := f.execute(id, session, assertion, 0)
	if status != http.StatusOK || len(out.Results) != 1 {
		t.Fatalf("execute: %d %s", status, raw)
	}
	if out.Results[0].Status != "failed" || !strings.Contains(out.Results[0].Error, "head moved") {
		t.Fatalf("moved head: want failed/head moved, got %+v", out.Results[0])
	}
	if got := f.cardStatus(); got != "pending" {
		t.Fatalf("card %q after a moved-head row", got)
	}
}

// One failing row does not stop the rest; unticked rows neither run nor are
// covered by the signature; each executed row is audited with plan and signer.
func TestBoardPlan_FailingRowContinuesAndUncheckedRowsSkipped(t *testing.T) {
	f := newPlanFixture(t)
	other, _ := createShipTask(t, f.db, f.base, f.token, &http.Client{})
	id := f.propose(
		map[string]any{"task_id": "task-does-not-exist", "action": "unblock"},
		sendBack(f.taskID, "needs a failing test first"),
		map[string]any{"task_id": other, "action": "mark_done"},
	)

	// The signature covers rows 0 and 1 only: it cannot run row 2 too.
	session, assertion := f.sign(id, 0, 1)
	if status, _, raw := f.execute(id, session, assertion, 0, 1, 2); status != http.StatusForbidden {
		t.Fatalf("selection wider than signed: want 403, got %d %s", status, raw)
	}

	session, assertion = f.sign(id, 0, 1)
	status, out, raw := f.execute(id, session, assertion, 0, 1)
	if status != http.StatusOK || len(out.Results) != 2 {
		t.Fatalf("execute: %d %s", status, raw)
	}
	if out.Results[0].Index != 0 || out.Results[0].Status != "failed" || out.Results[0].Error == "" {
		t.Fatalf("row 0: want failed with error, got %+v", out.Results[0])
	}
	if out.Results[1].Index != 1 || out.Results[1].Status != "ok" {
		t.Fatalf("row 1: want ok, got %+v (%s)", out.Results[1], raw)
	}
	if got := f.cardStatus(); got != "sent_back" {
		t.Fatalf("card %q, want sent_back", got)
	}
	var stage string
	_ = f.db.QueryRow(`SELECT COALESCE(execution_stage, status) FROM tasks WHERE id = ?`, other).Scan(&stage)
	if stage == "done" {
		t.Fatal("unticked mark_done row ran")
	}
	audits := f.planRowAudits(id)
	if len(audits) != 2 {
		t.Fatalf("want 2 board_plan_row audit rows, got %d", len(audits))
	}
	for _, a := range audits {
		if a["signer"] != "cred-test-1" || a["proposer"] != "test-session" {
			t.Fatalf("audit row missing signer/proposer: %v", a)
		}
	}
	if got := f.planStatus(id); got != "executed" {
		t.Fatalf("plan status %q, want executed", got)
	}
}

// The happy path: one signature approves and merges through the Ship Review
// route (whose WrapBoardAction has no verifier stub in this test).
func TestBoardPlan_ApproveMergeRunsThroughShipReview(t *testing.T) {
	f := newPlanFixture(t)
	id := f.propose(map[string]any{"task_id": f.taskID, "action": "approve_merge", "expected_head_sha": f.cardHead, "reason": "tests green"})

	status, raw, _ := f.do("GET", "/api/board/plans/"+id, nil, false, nil)
	if status != http.StatusOK || !strings.Contains(string(raw), f.cardHead) {
		t.Fatalf("plan view should show the head it will merge: %d %s", status, raw)
	}

	session, assertion := f.sign(id, 0)
	status, out, body := f.execute(id, session, assertion, 0)
	if status != http.StatusOK || len(out.Results) != 1 || out.Results[0].Status != "ok" {
		t.Fatalf("approve_merge: %d %s", status, body)
	}
	if got := f.cardStatus(); got != "approved" {
		t.Fatalf("card %q, want approved", got)
	}
	// The same route called directly still demands its own passkey.
	if status, raw, _ := f.do("POST", "/api/tasks/"+f.taskID+"/ship-review/send-back", map[string]string{"comment": "x"}, true, nil); status != http.StatusForbidden {
		t.Fatalf("per-task route without assertion: want 403, got %d %s", status, raw)
	}
}

// Discarding a plan runs nothing, clears its alert and stops it being signed.
func TestBoardPlan_DiscardRunsNothing(t *testing.T) {
	f := newPlanFixture(t)
	id := f.propose(sendBack(f.taskID, "never sent"))
	openAlerts := func() int {
		var n int
		_ = f.db.QueryRow(`SELECT COUNT(*) FROM board_alerts WHERE dedupe_key = ? AND acknowledged_at IS NULL`, "board_plan:"+id).Scan(&n)
		return n
	}
	if n := openAlerts(); n != 1 {
		t.Fatalf("want one open alert for the proposed plan, got %d", n)
	}
	if status, raw, _ := f.do("POST", "/api/board/plans/"+id+"/discard", nil, false, nil); status != http.StatusForbidden {
		t.Fatalf("discard without Board session: want 403, got %d %s", status, raw)
	}
	if status, raw, _ := f.do("POST", "/api/board/plans/"+id+"/discard", nil, true, nil); status != http.StatusOK {
		t.Fatalf("discard: %d %s", status, raw)
	}
	if n := openAlerts(); n != 0 {
		t.Fatal("alert still open after discard")
	}
	if status, raw, _ := f.do("POST", "/api/board/plans/"+id+"/challenge", map[string]any{"selected": []int{0}}, true, nil); status != http.StatusConflict {
		t.Fatalf("challenge on discarded plan: want 409, got %d %s", status, raw)
	}
	if got := f.cardStatus(); got != "pending" {
		t.Fatalf("card %q after discard", got)
	}
}
