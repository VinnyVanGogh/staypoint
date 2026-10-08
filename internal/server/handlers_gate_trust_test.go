package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/gates"
	"github.com/VinnyVanGogh/staypoint/internal/security"
)

// task-6c1ed91f: "Trust this task until…", end to end over HTTP.

type tev1Fake struct {
	rec   string
	p     float64
	err   error
	calls atomic.Int32
}

func (f *tev1Fake) Name() string { return gates.AdvisorTogether }
func (f *tev1Fake) Advise(_ context.Context, _ *security.GateRequest, _ []security.ScriptRef) (gates.Advice, error) {
	f.calls.Add(1)
	if f.err != nil {
		return gates.Advice{}, f.err
	}
	return gates.Advice{Recommendation: f.rec, Probability: f.p, Reason: "fake", Model: "tev1-fake"}, nil
}

// runningTask seeds an in-progress task that runs in its own (non-git) dir.
func runningTask(t *testing.T, e *gateEnv, id string) string {
	t.Helper()
	dir := t.TempDir()
	seedTask(t, e.db, id, dir, "StayPoint")
	if _, err := e.db.Exec(`UPDATE tasks SET execution_stage = 'in_progress', status = 'active' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	return dir
}

func (e *gateEnv) trust(t *testing.T, taskID, body string) map[string]any {
	t.Helper()
	st, out, _ := e.do(t, "POST", "/api/tasks/"+taskID+"/trust", body, true, "good")
	if st != http.StatusCreated {
		t.Fatalf("create trust: %d %v", st, out)
	}
	return out
}

// createIn posts a request like the hook does, from cwd.
func (e *gateEnv) createIn(t *testing.T, cmd, taskID, cwd string, runID ...string) map[string]any {
	t.Helper()
	rid := "sess"
	if len(runID) > 0 {
		rid = runID[0]
	}
	b, _ := json.Marshal(map[string]any{"cmdline": cmd, "reasons": []string{"red"}, "run_id": rid, "task_id": taskID, "cwd": cwd})
	st, out, _ := e.do(t, "POST", "/api/security/gate-requests", string(b), false, "")
	if st != http.StatusCreated {
		t.Fatalf("create: %d %v", st, out)
	}
	return out
}

func waitDecided(t *testing.T, e *gateEnv, id string) *security.GateRequest {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		gr, _ := security.GetGateRequest(e.db, id)
		if gr != nil && gr.Status != security.GateRequestPending {
			return gr
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("request %s never decided", id)
	return nil
}

func TestTrust_ProtectedMergesAndPushesStillWait(t *testing.T) {
	adv := &fakeAdvisor{rec: gates.RecApprove}
	e := startGateServer(t, adv, nil)
	dir := runningTask(t, e, "T1")
	rule := e.trust(t, "T1", `{"preset":"4h"}`)
	ruleID := int64(rule["id"].(float64))

	for _, cmd := range []string{
		"gh pr merge 12 --squash",
		"git push origin main",
		"git push origin HEAD:dev-server",
		"bash -c 'gh pr merge 3'",
		"git push", // bare push from a non-git dir: destination unknown
	} {
		if got := e.createIn(t, cmd, "T1", dir); got["status"] != "pending" {
			t.Errorf("%q auto-approved under trust: %v", cmd, got)
		}
	}

	// A non-excluded Red command (sudo; ssh is now a Board rule) is approved
	// by the trust, audited, and still rated by the advisor.
	got := e.createIn(t, "sudo ls /var/log", "T1", dir)
	if got["status"] != "approved" || got["decided_by"] != "rule:"+itoa(ruleID) {
		t.Fatalf("want trust approval, got %v", got)
	}
	entries := gateAudit(t, e.db, got["id"].(string))
	if len(entries) != 1 || !strings.Contains(entries[0], "security_gate_auto_approved") || !strings.Contains(entries[0], `"trust":true`) {
		t.Fatalf("audit: %v", entries)
	}
	deadline := time.Now().Add(3 * time.Second)
	for adv.calls.Load() < 6 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if adv.calls.Load() < 6 {
		t.Fatalf("auto-approved request was not rated by the advisor (calls %d)", adv.calls.Load())
	}

	// The task page lists it, newest first.
	st, view, _ := e.do(t, "GET", "/api/tasks/T1/trust", "", false, "")
	active, _ := view["active"].(map[string]any)
	if st != http.StatusOK || active == nil || active["auto_approved_count"].(float64) != 1 {
		t.Fatalf("trust view: %d %v", st, view)
	}
	if held := view["held"].([]any); len(held) != 5 {
		t.Fatalf("want 5 held requests, got %d", len(held))
	}
}

func TestTrust_OtherTaskAndPolicyRequestsNotApproved(t *testing.T) {
	e := startGateServer(t, nil, nil)
	dir := runningTask(t, e, "T1")
	runningTask(t, e, "T2")
	e.trust(t, "T1", `{"preset":"1h"}`)

	if got := e.createIn(t, "sudo ls", "T2", dir); got["status"] != "pending" {
		t.Fatalf("another task's request approved by T1's trust: %v", got)
	}
	if got := e.createIn(t, "sudo ls", "", dir); got["status"] != "pending" {
		t.Fatalf("request with no task approved: %v", got)
	}
	for _, rid := range []string{"tracking-gate-override"} {
		if got := e.createIn(t, "override tracking gate", "T1", dir, rid); got["status"] != "pending" {
			t.Fatalf("Board policy request %s auto-approved: %v", rid, got)
		}
	}
	if got := e.createIn(t, "sudo ls", "T1", dir); got["status"] != "approved" {
		t.Fatalf("control: trusted task's request not approved: %v", got)
	}
}

func TestTrust_ExpiredRevokedOrTaskLeftApprovesNothing(t *testing.T) {
	e := startGateServer(t, nil, nil)
	dir := runningTask(t, e, "T1")

	// Expired.
	rule := e.trust(t, "T1", `{"preset":"1h"}`)
	if _, err := e.db.Exec(`UPDATE security_gate_rules SET expires_at = ? WHERE id = ?`,
		time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), int64(rule["id"].(float64))); err != nil {
		t.Fatal(err)
	}
	if got := e.createIn(t, "sudo ls", "T1", dir); got["status"] != "pending" {
		t.Fatalf("expired trust approved: %v", got)
	}

	// Revoked: immediate, Board session only.
	e.trust(t, "T1", `{"preset":"1h"}`)
	if st, _, _ := e.do(t, "POST", "/api/tasks/T1/trust/revoke", "", false, ""); st != http.StatusForbidden {
		t.Fatalf("revoke without Board session: %d", st)
	}
	if st, out, _ := e.do(t, "POST", "/api/tasks/T1/trust/revoke", "", true, ""); st != http.StatusOK {
		t.Fatalf("revoke: %d %v", st, out)
	}
	if got := e.createIn(t, "sudo ls", "T1", dir); got["status"] != "pending" {
		t.Fatalf("revoked trust approved: %v", got)
	}

	// Task leaves in_progress: the trust ends and does not come back.
	e.trust(t, "T1", `{"preset":"1h"}`)
	if _, err := e.db.Exec(`UPDATE tasks SET execution_stage = 'in_review' WHERE id = 'T1'`); err != nil {
		t.Fatal(err)
	}
	if got := e.createIn(t, "sudo ls", "T1", dir); got["status"] != "pending" {
		t.Fatalf("trust approved after the task left in_progress: %v", got)
	}
	if _, err := e.db.Exec(`UPDATE tasks SET execution_stage = 'in_progress' WHERE id = 'T1'`); err != nil {
		t.Fatal(err)
	}
	if got := e.createIn(t, "sudo ls", "T1", dir); got["status"] != "pending" {
		t.Fatalf("ended trust came back when the task resumed: %v", got)
	}
}

func TestTrust_CreateNeedsTouchIDAndServerOwnsLimits(t *testing.T) {
	e := startGateServer(t, nil, nil)
	runningTask(t, e, "T1")
	if st, _, _ := e.do(t, "POST", "/api/tasks/T1/trust", `{"preset":"1h"}`, false, ""); st != http.StatusForbidden {
		t.Fatalf("no Board session: %d", st)
	}
	if st, _, _ := e.do(t, "POST", "/api/tasks/T1/trust", `{"preset":"1h"}`, true, ""); st != http.StatusForbidden {
		t.Fatalf("no Touch ID: %d", st)
	}
	if st, _, _ := e.do(t, "POST", "/api/tasks/T1/trust", `{"preset":"1h"}`, true, "bad"); st != http.StatusForbidden {
		t.Fatalf("bad Touch ID: %d", st)
	}
	for _, body := range []string{
		`{"preset":"1h","expires_at":"2099-01-01T00:00:00Z"}`,
		`{"preset":"1h","exclusions":[]}`,
		`{"preset":"1h","match_kind":"any","scope":"repo"}`,
	} {
		if st, _, _ := e.do(t, "POST", "/api/tasks/T1/trust", body, true, "good"); st != http.StatusBadRequest {
			t.Errorf("client-chosen limits accepted (%s): %d", body, st)
		}
	}
	for _, body := range []string{
		`{"preset":"forever"}`,
		`{"preset":"custom","minutes":100000}`,
		`{"preset":"custom","minutes":1}`,
		`{"preset":"1h","tev1":true}`, // warning not acknowledged
	} {
		if st, _, _ := e.do(t, "POST", "/api/tasks/T1/trust", body, true, "good"); st != http.StatusUnprocessableEntity {
			t.Errorf("%s: want 422, got %d", body, st)
		}
	}
	before := time.Now()
	rule := e.trust(t, "T1", `{"preset":"overnight"}`)
	exp, _ := time.Parse(time.RFC3339Nano, rule["expires_at"].(string))
	if d := exp.Sub(before); d < 719*time.Minute || d > 721*time.Minute {
		t.Fatalf("overnight expiry %v from now", d)
	}
	if rule["match_kind"] != "any" || rule["scope"] != "task" || rule["scope_value"] != "T1" {
		t.Fatalf("trust rule shape: %v", rule)
	}
	// A done task cannot be trusted.
	if _, err := e.db.Exec(`UPDATE tasks SET execution_stage = 'done' WHERE id = 'T1'`); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := e.do(t, "POST", "/api/tasks/T1/trust", `{"preset":"1h"}`, true, "good"); st != http.StatusConflict {
		t.Fatalf("trusting a done task: %d", st)
	}
	// Creation is audited as a Board action.
	var n int
	_ = e.db.QueryRow(`SELECT COUNT(*) FROM board_audit_log WHERE payload LIKE '%create_task_trust%'`).Scan(&n)
	if n != 1 {
		t.Fatalf("board audit rows for trust creation: %d", n)
	}
}

func TestTrust_DeleteOutsideWorktreeDefersAndNeverReplays(t *testing.T) {
	adv := &tev1Fake{rec: gates.RecApprove, p: 0.99}
	e := startGateServer(t, adv, nil)
	dir := runningTask(t, e, "T1")
	// tev1 mode on: the delete must still follow the deferred rule.
	e.trust(t, "T1", `{"preset":"4h","tev1":true,"tev1_ack":true}`)

	got := e.createIn(t, "rm -rf /opt/important", "T1", dir)
	id := got["id"].(string)
	if got["status"] != "pending" {
		t.Fatalf("delete outside the worktree not held: %v", got)
	}
	gr, _ := security.GetGateRequest(e.db, id)
	wantDefer := time.Duration(gates.DefaultTrustDeferMinutes) * time.Minute
	if gr.DeferAt == nil || gr.DeferAt.Sub(gr.CreatedAt) < wantDefer-time.Minute || gr.DeferAt.Sub(gr.CreatedAt) > wantDefer+time.Minute {
		t.Fatalf("defer_at: %v (created %v)", gr.DeferAt, gr.CreatedAt)
	}
	// Not yet due: still pending for the hook.
	_, out, _ := e.do(t, "GET", "/api/security/gate-requests/"+id, "", false, "")
	if out["status"] != "pending" {
		t.Fatalf("before the deadline: %v", out)
	}
	// Deadline passes: the waiting hook gets "deferred".
	if _, err := e.db.Exec(`UPDATE security_gate_requests SET defer_at = ? WHERE id = ?`,
		time.Now().Add(-time.Second).UTC().Format(security.DeferTimeFormat), id); err != nil {
		t.Fatal(err)
	}
	_, out, _ = e.do(t, "GET", "/api/security/gate-requests/"+id+"?wait=true", "", false, "")
	if out["status"] != "deferred" {
		t.Fatalf("after the deadline the hook must see deferred: %v", out)
	}
	// The Deferred queue lists it; the pending list does not.
	_, q, _ := e.do(t, "GET", "/api/security/gate-requests?status=deferred", "", false, "")
	if l := q["gate_requests"].([]any); len(l) != 1 || l[0].(map[string]any)["id"] != id {
		t.Fatalf("deferred queue: %v", q)
	}
	_, p, _ := e.do(t, "GET", "/api/security/gate-requests?status=pending", "", false, "")
	if l, _ := p["gate_requests"].([]any); len(l) != 0 {
		t.Fatalf("deferred request still in the pending list: %v", p)
	}
	// The Board approves it later: recorded, but the hook view stays
	// deferred, so nothing that still polls can run it.
	st, _, _ := e.do(t, "POST", "/api/security/gate-requests/"+id+"/decide", `{"decision":"approved"}`, true, "good")
	if st != http.StatusOK {
		t.Fatalf("deciding a deferred request: %d", st)
	}
	_, out, _ = e.do(t, "GET", "/api/security/gate-requests/"+id+"?wait=true", "", false, "")
	if out["status"] != "deferred" || out["board_status"] != "approved" {
		t.Fatalf("approving a deferred request must not replay it: %v", out)
	}
	audit := strings.Join(gateAudit(t, e.db, id), "\n")
	for _, want := range []string{"security_gate_trust_deferring", "security_gate_deferred", `"deferred":true`} {
		if !strings.Contains(audit, want) {
			t.Errorf("audit missing %s:\n%s", want, audit)
		}
	}
	// tev1 may rate it (advisory), but never decides it.
	if strings.Contains(audit, "security_gate_tev1_") {
		t.Fatalf("tev1 decided a delete outside the worktree:\n%s", audit)
	}
	// Inside the worktree is fine.
	if got := e.createIn(t, "rm -rf build", "T1", dir); got["status"] == "pending" {
		if g := waitDecided(t, e, got["id"].(string)); g.Status != security.GateRequestApproved {
			t.Fatalf("delete inside the worktree not approved: %v", g)
		}
	}
}

func blockedReason(t *testing.T, e *gateEnv, id string) (bool, string) {
	t.Helper()
	var b bool
	var r string
	if err := e.db.QueryRow(`SELECT is_blocked, COALESCE(block_reason,'') FROM tasks WHERE id = ?`, id).Scan(&b, &r); err != nil {
		t.Fatal(err)
	}
	return b, r
}

func TestTrust_Tev1DecidesAndParksOnlyThatTask(t *testing.T) {
	cases := []struct {
		name    string
		adv     *tev1Fake
		approve bool
	}{
		{"approve above threshold", &tev1Fake{rec: gates.RecApprove, p: 0.8}, true},
		{"approve at threshold", &tev1Fake{rec: gates.RecApprove, p: 0.7}, true},
		{"approve below threshold", &tev1Fake{rec: gates.RecApprove, p: 0.69}, false},
		{"deny", &tev1Fake{rec: gates.RecDeny, p: 0.95}, false},
		{"error fails closed", &tev1Fake{err: errors.New("connection refused")}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := startGateServer(t, c.adv, nil)
			dir := runningTask(t, e, "T1")
			runningTask(t, e, "T2")
			rule := e.trust(t, "T1", `{"preset":"overnight","tev1":true,"tev1_ack":true}`)
			e.trust(t, "T2", `{"preset":"overnight"}`)
			if rule["tev1_threshold"].(float64) != 0.7 {
				t.Fatalf("threshold %v", rule["tev1_threshold"])
			}
			got := e.createIn(t, "sudo ls /var/log", "T1", dir)
			gr := waitDecided(t, e, got["id"].(string))
			want := security.GateRequestDenied
			if c.approve {
				want = security.GateRequestApproved
			}
			if gr.Status != want || gr.DecidedBy != "rule:"+itoa(int64(rule["id"].(float64)))+":tev1" {
				t.Fatalf("tev1 decision: %s by %s, want %s", gr.Status, gr.DecidedBy, want)
			}
			blocked, reason := blockedReason(t, e, "T1")
			if c.approve == blocked {
				t.Fatalf("T1 blocked=%v (%s) after %s", blocked, reason, gr.Status)
			}
			if !c.approve && !strings.HasPrefix(reason, "tev1 denied: sudo ls /var/log") {
				t.Fatalf("block reason: %q", reason)
			}
			if b, _ := blockedReason(t, e, "T2"); b {
				t.Fatal("another task was parked")
			}
			if got := e.createIn(t, "sudo ls", "T2", dir); got["status"] != "approved" {
				t.Fatalf("other trusted task stopped running: %v", got)
			}
			// The morning summary lists the tev1 decision.
			_, view, _ := e.do(t, "GET", "/api/tasks/T1/trust", "", false, "")
			latest := view["latest"].(map[string]any)
			key := "tev1_denied"
			if c.approve {
				key = "tev1_approved"
			}
			if l := latest[key].([]any); len(l) != 1 {
				t.Fatalf("%s summary: %v", key, latest)
			}
			_, all, _ := e.do(t, "GET", "/api/security/trusts", "", false, "")
			if l := all["trusts"].([]any); len(l) != 2 {
				t.Fatalf("trust list: %v", all)
			}
		})
	}
}

func TestTrust_SettingsAreBoardOnlyAndBounded(t *testing.T) {
	e := startGateServer(t, nil, nil)
	if st, _, _ := e.do(t, "POST", "/api/settings/security-gate", `{"tev1_threshold":0.9}`, true, ""); st != http.StatusForbidden {
		t.Fatalf("settings without Touch ID: %d", st)
	}
	for _, body := range []string{`{"tev1_threshold":0.2}`, `{"trust_defer_minutes":0}`, `{"trust_defer_minutes":500}`} {
		if st, _, _ := e.do(t, "POST", "/api/settings/security-gate", body, true, "good"); st != http.StatusBadRequest {
			t.Errorf("%s: %d", body, st)
		}
	}
	st, out, _ := e.do(t, "POST", "/api/settings/security-gate", `{"tev1_threshold":0.85,"trust_defer_minutes":3}`, true, "good")
	if st != http.StatusOK || out["tev1_threshold"].(float64) != 0.85 || out["trust_defer_minutes"].(float64) != 3 {
		t.Fatalf("settings: %d %v", st, out)
	}
}
