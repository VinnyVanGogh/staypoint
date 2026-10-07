package server_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/gates"
	"github.com/VinnyVanGogh/staypoint/internal/security"
	"github.com/VinnyVanGogh/staypoint/internal/server"
)

// STA-868: allow rules, transactional decisions, batch decisions, advisory
// recommendations and the Gemini review, end to end over HTTP. No network:
// advisors and reviewers are fakes.

type fakeAdvisor struct {
	rec   string
	delay time.Duration
	calls atomic.Int32
}

func (f *fakeAdvisor) Name() string { return gates.AdvisorTogether }
func (f *fakeAdvisor) Advise(ctx context.Context, gr *security.GateRequest, _ []security.ScriptRef) (gates.Advice, error) {
	f.calls.Add(1)
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return gates.Advice{}, ctx.Err()
	}
	return gates.Advice{Recommendation: f.rec, Reason: "read-only du/git survey", Model: "fake"}, nil
}

type stubReviewer struct {
	res gates.ReviewResult
	err error
}

func (s stubReviewer) Review(_ context.Context, items []gates.ReviewItem) (gates.ReviewResult, error) {
	if s.err != nil {
		return gates.ReviewResult{}, s.err
	}
	res := s.res
	if len(res.Items) == 0 {
		for _, it := range items {
			res.Items = append(res.Items, gates.ItemReview{ID: it.Request.ID, Recommendation: gates.RecApprove, Reason: "fine"})
		}
	}
	return res, nil
}

type gateEnv struct {
	srv   *server.Server
	token string
	db    *sql.DB
}

func startGateServer(t *testing.T, adv gates.RequestAdvisor, rv gates.Reviewer) *gateEnv {
	t.Helper()
	database := setupTestDB(t)
	seedBoardWebAuthnCredential(t, database)
	token := "test-secret-token-1234567890abcdef"
	srv, err := server.New(server.Options{
		BindHost: "127.0.0.1", AuthToken: token, DB: database,
		TelemetryDBPath:  filepath.Join(t.TempDir(), "t.db"),
		ReplayBufferSize: 100, SubscriberBufferSize: 16,
		GateAdvisor: adv, GateReviewer: rv,
		GateResolver: &gates.Resolver{RepoRoot: func(string) string { return "" }},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	srv.SetWebAuthnVerifier(func(_ *http.Request, a string) error {
		if a == "good" {
			return nil
		}
		return errors.New("bad assertion")
	})
	return &gateEnv{srv: srv, token: token, db: database}
}

func (e *gateEnv) do(t *testing.T, method, path, body string, board bool, assertion string, cookies ...*http.Cookie) (int, map[string]any, *http.Response) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, e.srv.URL()+path, rd)
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", "application/json")
	if board {
		req.AddCookie(&http.Cookie{Name: "staypoint_board", Value: e.srv.BoardToken()})
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	if assertion != "" {
		req.Header.Set("X-WebAuthn-Assertion", assertion)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]any{"_raw": string(raw)}
	}
	return resp.StatusCode, out, resp
}

func (e *gateEnv) create(t *testing.T, cmd, taskID string, reasons ...string) map[string]any {
	t.Helper()
	if len(reasons) == 0 {
		reasons = []string{"bash: runs an opaque script"}
	}
	// Like the hook: snapshot the scripts and say the command is pinned.
	var scripts []map[string]string
	for _, r := range security.ScriptRefs(cmd, "", os.ReadFile, 0) {
		scripts = append(scripts, map[string]string{"path": r.Path, "content": string(r.Full)})
	}
	b, _ := json.Marshal(map[string]any{"cmdline": cmd, "reasons": reasons, "run_id": "sess", "task_id": taskID,
		"scripts": scripts, "pinned": len(scripts) > 0})
	st, out, _ := e.do(t, "POST", "/api/security/gate-requests", string(b), false, "")
	if st != http.StatusCreated {
		t.Fatalf("create: %d %v", st, out)
	}
	return out
}

func (e *gateEnv) status(t *testing.T, id string) string {
	t.Helper()
	gr, err := security.GetGateRequest(e.db, id)
	if err != nil || gr == nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return string(gr.Status)
}

func seedTask(t *testing.T, d *sql.DB, id, repo, org string) {
	t.Helper()
	if _, err := d.Exec(`INSERT INTO tasks (id, name, repo_path, organization) VALUES (?, ?, ?, ?)`, id, id, repo, org); err != nil {
		t.Fatal(err)
	}
}

func TestGateDecide_AuditFailureRollsBack(t *testing.T) {
	e := startGateServer(t, nil, nil)
	gr := e.create(t, "sudo ls", "")
	id := gr["id"].(string)
	if _, err := e.db.Exec(`DROP TABLE security_gate_audit_log`); err != nil {
		t.Fatal(err)
	}
	st, out, _ := e.do(t, "POST", "/api/security/gate-requests/"+id+"/decide", `{"decision":"approved"}`, true, "good")
	if st != http.StatusInternalServerError || !strings.Contains(out["error"].(string), "audit log write failed") {
		t.Fatalf("want 500 audit failure, got %d %v", st, out)
	}
	if s := e.status(t, id); s != "pending" {
		t.Fatalf("decision must roll back with its audit row; status %s", s)
	}
}

func TestGateRule_RememberAutoApprovesAndPinsScript(t *testing.T) {
	e := startGateServer(t, nil, nil)
	seedTask(t, e.db, "T1", "/repo/a", "StayPoint")
	dir := t.TempDir()
	p := filepath.Join(dir, "wt-audit.sh")
	if err := os.WriteFile(p, []byte("du -sh .\ngit status\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := "bash " + p
	first := e.create(t, cmd, "T1")
	id := first["id"].(string)

	body := `{"decision":"approved","remember":{"scope":"repo","expires_in_minutes":60}}`
	st, out, _ := e.do(t, "POST", "/api/security/gate-requests/"+id+"/decide", body, true, "good")
	if st != http.StatusOK || out["rule"] == nil {
		t.Fatalf("approve & remember: %d %v", st, out)
	}
	rule := out["rule"].(map[string]any)
	ruleID := int64(rule["id"].(float64))
	if rule["scope_value"] != "/repo/a" {
		t.Fatalf("repo scope from task: %v", rule)
	}

	// Same command, same script, another task in the same repo: auto-approved.
	seedTask(t, e.db, "T2", "/repo/a", "StayPoint")
	second := e.create(t, cmd, "T2")
	if second["status"] != "approved" || second["decided_by"] != "rule:"+itoa(ruleID) {
		t.Fatalf("want auto-approval by rule, got %v", second)
	}
	entries := gateAudit(t, e.db, second["id"].(string))
	if len(entries) != 1 || !strings.Contains(entries[0], "auto-approved by rule #"+itoa(ruleID)) {
		t.Fatalf("audit: %v", entries)
	}
	rules, _ := gates.ListRules(e.db, false)
	if len(rules) != 1 || rules[0].HitCount != 1 {
		t.Fatalf("hit count: %+v", rules)
	}

	// Edited script: held again.
	if err := os.WriteFile(p, []byte("du -sh .\ngit push origin HEAD\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	third := e.create(t, cmd, "T2")
	if third["status"] != "pending" {
		t.Fatalf("edited script must be held, got %v", third)
	}
	// Different repo: held.
	seedTask(t, e.db, "T3", "/repo/b", "StayPoint")
	if err := os.WriteFile(p, []byte("du -sh .\ngit status\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := e.create(t, cmd, "T3"); got["status"] != "pending" {
		t.Fatalf("other repo must be held, got %v", got)
	}

	// Delete the rule (Board): the same request is held again.
	st, out, _ = e.do(t, "DELETE", "/api/security/gate-rules/"+itoa(ruleID), "", true, "good")
	if st != http.StatusOK {
		t.Fatalf("delete: %d %v", st, out)
	}
	if got := e.create(t, cmd, "T2"); got["status"] != "pending" {
		t.Fatalf("deleted rule still approves: %v", got)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func gateAudit(t *testing.T, d *sql.DB, id string) []string {
	t.Helper()
	rows, err := d.Query(`SELECT event_type || ' ' || COALESCE(payload,'') FROM security_gate_audit_log WHERE gate_id = ?`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func TestGateRule_CreateDeleteAreBoardOnly(t *testing.T) {
	e := startGateServer(t, nil, nil)
	seedTask(t, e.db, "T1", "/repo/a", "StayPoint")
	gr := e.create(t, "sudo ls", "T1")
	body := `{"gate_id":"` + gr["id"].(string) + `","scope":"task"}`
	if st, out, _ := e.do(t, "POST", "/api/security/gate-rules", body, false, ""); st != http.StatusForbidden {
		t.Fatalf("token-only rule create: want 403, got %d %v", st, out)
	}
	if st, _, _ := e.do(t, "POST", "/api/security/gate-rules", body, true, ""); st != http.StatusForbidden {
		t.Fatalf("board cookie without passkey: want 403, got %d", st)
	}
	st, out, _ := e.do(t, "POST", "/api/security/gate-rules", body, true, "good")
	if st != http.StatusCreated {
		t.Fatalf("board create: %d %v", st, out)
	}
	id := itoa(int64(out["id"].(float64)))
	if st, _, _ := e.do(t, "DELETE", "/api/security/gate-rules/"+id, "", false, ""); st != http.StatusForbidden {
		t.Fatalf("token-only rule delete: want 403, got %d", st)
	}
	if st, out, _ := e.do(t, "GET", "/api/security/gate-rules", "", false, ""); st != http.StatusOK || len(out["rules"].([]any)) != 1 {
		t.Fatalf("list: %d %v", st, out)
	}
	// Remember on a denial is refused.
	gr2 := e.create(t, "sudo ls -la", "T1")
	st, out, _ = e.do(t, "POST", "/api/security/gate-requests/"+gr2["id"].(string)+"/decide", `{"decision":"denied","remember":{"scope":"task"}}`, true, "good")
	if st != http.StatusBadRequest {
		t.Fatalf("remember denial: want 400, got %d %v", st, out)
	}
	if s := e.status(t, gr2["id"].(string)); s != "pending" {
		t.Fatalf("refused remember must not decide: %s", s)
	}
}

func TestGateBatch_PartialFailuresPerRow(t *testing.T) {
	e := startGateServer(t, nil, nil)
	a := e.create(t, "sudo a", "")["id"].(string)
	b := e.create(t, "sudo b", "")["id"].(string)
	// b is already decided: that row fails, a succeeds.
	if _, err := security.DecideGateRequest(e.db, b, false); err != nil {
		t.Fatal(err)
	}
	body := `{"ids":["` + a + `","` + b + `","missing"],"decision":"approved"}`
	if st, _, _ := e.do(t, "POST", "/api/security/gate-requests/decide-batch", body, false, ""); st != http.StatusForbidden {
		t.Fatalf("token-only batch: want 403, got %d", st)
	}
	st, out, _ := e.do(t, "POST", "/api/security/gate-requests/decide-batch", body, true, "good")
	if st != http.StatusOK || out["decided"].(float64) != 1 || out["failed"].(float64) != 2 {
		t.Fatalf("batch: %d %v", st, out)
	}
	res := out["results"].([]any)
	if res[0].(map[string]any)["ok"] != true || res[1].(map[string]any)["ok"] != false {
		t.Fatalf("per-row results: %v", res)
	}
	if e.status(t, a) != "approved" || len(gateAudit(t, e.db, a)) != 1 {
		t.Fatal("batch item must be decided with its own audit row")
	}
}

func TestGateAdvisor_StoresRecommendationWithoutBlockingOrDeciding(t *testing.T) {
	adv := &fakeAdvisor{rec: gates.RecApprove, delay: 300 * time.Millisecond}
	e := startGateServer(t, adv, nil)
	start := time.Now()
	gr := e.create(t, "bash /tmp/sta-cleanup/sizes.sh", "")
	if time.Since(start) > 250*time.Millisecond {
		t.Fatalf("create waited for the advisor (%s)", time.Since(start))
	}
	id := gr["id"].(string)
	deadline := time.Now().Add(5 * time.Second)
	var advice map[string]any
	for time.Now().Before(deadline) {
		_, out, _ := e.do(t, "GET", "/api/security/gate-requests?status=pending", "", false, "")
		reqs := out["gate_requests"].([]any)
		if len(reqs) == 1 {
			if a, ok := reqs[0].(map[string]any)["advice"].(map[string]any); ok {
				advice = a["together"].(map[string]any)
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if advice == nil || advice["recommendation"] != "approved" || advice["reason"] != "read-only du/git survey" {
		t.Fatalf("advice not stored: %v", advice)
	}
	if e.status(t, id) != "pending" {
		t.Fatal("advisor must never change the request")
	}

	// Board decides against the advisor: agreement is 0/1.
	if st, out, _ := e.do(t, "POST", "/api/security/gate-requests/"+id+"/decide", `{"decision":"denied"}`, true, "good"); st != http.StatusOK {
		t.Fatalf("decide: %d %v", st, out)
	}
	_, stats, _ := e.do(t, "GET", "/api/security/gate-stats", "", false, "")
	advs := stats["advisors"].([]any)
	s := advs[0].(map[string]any)
	if s["decided"].(float64) != 1 || s["agreed"].(float64) != 0 {
		t.Fatalf("stats: %v", stats)
	}
}

func TestGateAdvisor_KillSwitch(t *testing.T) {
	adv := &fakeAdvisor{rec: gates.RecApprove}
	e := startGateServer(t, adv, nil)
	if st, out, _ := e.do(t, "POST", "/api/settings/security-gate", `{"advisor_enabled":false}`, false, ""); st != http.StatusForbidden {
		t.Fatalf("token-only settings: want 403, got %d %v", st, out)
	}
	if st, out, _ := e.do(t, "POST", "/api/settings/security-gate", `{"advisor_enabled":false}`, true, "good"); st != http.StatusOK || out["advisor_enabled"] != false || out["main_merge_approval"] != true {
		t.Fatalf("disable advisor: %d %v", st, out)
	}
	e.create(t, "sudo ls", "")
	time.Sleep(200 * time.Millisecond)
	if n := adv.calls.Load(); n != 0 {
		t.Fatalf("advisor ran %d times with the kill switch off", n)
	}
}

func TestGateReview_AdvisoryOnly(t *testing.T) {
	e := startGateServer(t, nil, stubReviewer{res: gates.ReviewResult{Summary: "all read-only", Suggestion: "approve 2"}})
	a := e.create(t, "bash /tmp/a.sh", "")["id"].(string)
	e.create(t, "bash /tmp/b.sh", "")
	if st, _, _ := e.do(t, "POST", "/api/security/gate-requests/review", `{}`, false, ""); st != http.StatusForbidden {
		t.Fatalf("token-only review: want 403, got %d", st)
	}
	st, out, _ := e.do(t, "POST", "/api/security/gate-requests/review", `{}`, true, "")
	if st != http.StatusOK || out["suggestion"] != "approve 2" || len(out["items"].([]any)) != 2 {
		t.Fatalf("review: %d %v", st, out)
	}
	if e.status(t, a) != "pending" {
		t.Fatal("review must not decide anything")
	}

	failing := startGateServer(t, nil, stubReviewer{err: errors.New("gemini: HTTP 429")})
	failing.create(t, "bash /tmp/a.sh", "")
	st, out, _ = failing.do(t, "POST", "/api/security/gate-requests/review", `{}`, true, "")
	if st != http.StatusBadGateway || !strings.Contains(out["error"].(string), "429") {
		t.Fatalf("review failure: %d %v", st, out)
	}
	unconfigured := startGateServer(t, nil, nil)
	unconfigured.create(t, "sudo x", "")
	if st, out, _ := unconfigured.do(t, "POST", "/api/security/gate-requests/review", `{}`, true, ""); st != http.StatusBadGateway || !strings.Contains(out["error"].(string), "GEMINI_API_KEY") {
		t.Fatalf("unconfigured: %d %v", st, out)
	}
}

func graceCookie(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == "staypoint_board_grace" {
			return c
		}
	}
	return nil
}

func TestPasskeyGrace_CoversGateActionsOnly(t *testing.T) {
	e := startGateServer(t, nil, nil)
	a := e.create(t, "sudo a", "")["id"].(string)
	b := e.create(t, "sudo b", "")["id"].(string)
	st, _, resp := e.do(t, "POST", "/api/security/gate-requests/"+a+"/decide", `{"decision":"approved"}`, true, "good")
	gc := graceCookie(resp)
	if st != http.StatusOK || gc == nil || !gc.HttpOnly || gc.MaxAge != 120 {
		t.Fatalf("assertion should open a 2-minute HttpOnly grace window: %d %+v", st, gc)
	}
	// Within the window: no assertion needed for a gate action.
	if st, out, _ := e.do(t, "POST", "/api/security/gate-requests/"+b+"/decide", `{"decision":"denied"}`, true, "", gc); st != http.StatusOK {
		t.Fatalf("grace decide: %d %v", st, out)
	}
	// Status endpoint reports the countdown.
	_, out, _ := e.do(t, "GET", "/api/board/passkey-grace", "", true, "", gc)
	if out["minutes"].(float64) != 2 || out["remaining_seconds"].(float64) < 100 {
		t.Fatalf("grace status: %v", out)
	}
	// Grace does not cover non-gate Board actions.
	if st, _, _ := e.do(t, "POST", "/api/settings/ship-review", `{"ship_review":true}`, true, "", gc); st != http.StatusForbidden {
		t.Fatalf("grace must not cover settings: %d", st)
	}
	// Grace needs the Board cookie too.
	c := e.create(t, "sudo c", "")["id"].(string)
	if st, _, _ := e.do(t, "POST", "/api/security/gate-requests/"+c+"/decide", `{"decision":"denied"}`, false, "", gc); st != http.StatusForbidden {
		t.Fatalf("grace without board cookie: %d", st)
	}
	// A forged grace token is worthless.
	if st, _, _ := e.do(t, "POST", "/api/security/gate-requests/"+c+"/decide", `{"decision":"denied"}`, true, "", &http.Cookie{Name: "staypoint_board_grace", Value: "forged"}); st != http.StatusForbidden {
		t.Fatalf("forged grace: %d", st)
	}
	// Turning grace off (0) ends the window at once.
	if st, out, _ := e.do(t, "POST", "/api/settings/security-gate", `{"passkey_grace_minutes":0}`, true, "good"); st != http.StatusOK || out["passkey_grace_minutes"].(float64) != 0 {
		t.Fatalf("disable grace: %d %v", st, out)
	}
	if st, _, _ := e.do(t, "POST", "/api/security/gate-requests/"+c+"/decide", `{"decision":"denied"}`, true, "", gc); st != http.StatusForbidden {
		t.Fatalf("grace after disabling: want 403, got %d", st)
	}
	if st, _, _ := e.do(t, "POST", "/api/settings/security-gate", `{"passkey_grace_minutes":9}`, true, "good"); st != http.StatusBadRequest {
		t.Fatalf("grace above 5 minutes: want 400, got %d", st)
	}
}

// A request from an old hook (no pinned snapshot) is never auto-approved by a
// rule that pins a script, and cannot itself be remembered.
func TestGateRule_UnpinnedRequestsNotAutoApproved(t *testing.T) {
	e := startGateServer(t, nil, nil)
	seedTask(t, e.db, "T1", "/repo/a", "StayPoint")
	p := filepath.Join(t.TempDir(), "s.sh")
	if err := os.WriteFile(p, []byte("ls\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	first := e.create(t, "bash "+p, "T1")
	if st, out, _ := e.do(t, "POST", "/api/security/gate-requests/"+first["id"].(string)+"/decide",
		`{"decision":"approved","remember":{"scope":"task"}}`, true, "good"); st != http.StatusOK {
		t.Fatalf("remember: %d %v", st, out)
	}
	raw, _ := json.Marshal(map[string]any{"cmdline": "bash " + p, "reasons": []string{"bash: runs an opaque script"}, "run_id": "s", "task_id": "T1"})
	st, out, _ := e.do(t, "POST", "/api/security/gate-requests", string(raw), false, "")
	if st != http.StatusCreated || out["status"] != "pending" {
		t.Fatalf("unpinned request must be held: %d %v", st, out)
	}
	st, out, _ = e.do(t, "POST", "/api/security/gate-requests/"+out["id"].(string)+"/decide",
		`{"decision":"approved","remember":{"scope":"task"}}`, true, "good")
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("remembering an unpinned request: want 422, got %d %v", st, out)
	}
}
