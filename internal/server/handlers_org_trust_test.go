package server_test

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
)

// task-33692ffb: "Trust this organization until…", end to end over HTTP.

func boardAuditHas(t *testing.T, e *gateEnv, action string) bool {
	t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM board_audit_log WHERE payload LIKE ?`, `%"action":"`+action+`"%`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func TestOrgTrust_CreateNeedsBoardAndTouchID(t *testing.T) {
	e := startGateServer(t, nil, nil)
	runningTask(t, e, "T1") // organization "StayPoint"
	body := `{"org":"staypoint","preset":"1h"}`
	// Agent token only (no Board session): refused.
	if st, _, _ := e.do(t, "POST", "/api/settings/org-trust", body, false, ""); st != http.StatusForbidden {
		t.Fatalf("agent token: %d", st)
	}
	if st, _, _ := e.do(t, "POST", "/api/settings/org-trust", body, false, "good"); st != http.StatusForbidden {
		t.Fatalf("agent token with assertion: %d", st)
	}
	if st, _, _ := e.do(t, "POST", "/api/settings/org-trust", body, true, ""); st != http.StatusForbidden {
		t.Fatalf("no Touch ID: %d", st)
	}
	if st, _, _ := e.do(t, "POST", "/api/settings/org-trust", body, true, "bad"); st != http.StatusForbidden {
		t.Fatalf("bad Touch ID: %d", st)
	}
	for body, want := range map[string]int{
		`{"org":"staypoint","preset":"1h","expires_at":"2099-01-01T00:00:00Z"}`: http.StatusBadRequest,
		`{"org":"staypoint","preset":"1h","scope":"repo"}`:                      http.StatusBadRequest,
		`{"org":"staypoint","preset":"custom","minutes":961}`:                   http.StatusUnprocessableEntity,
		`{"org":"staypoint"}`:            http.StatusUnprocessableEntity,
		`{"org":"nobody","preset":"1h"}`: http.StatusNotFound,
	} {
		if st, out, _ := e.do(t, "POST", "/api/settings/org-trust", body, true, "good"); st != want {
			t.Errorf("%s: %d %v (want %d)", body, st, out, want)
		}
	}
	st, out, _ := e.do(t, "POST", "/api/settings/org-trust", `{"org":"  StayPoint ","preset":"custom","minutes":960}`, true, "good")
	if st != http.StatusCreated || out["scope"] != "org" || out["scope_value"] != "staypoint" {
		t.Fatalf("create: %d %v", st, out)
	}
	if !boardAuditHas(t, e, "create_org_trust") {
		t.Fatal("create not in the Board audit log")
	}
	// Revoke needs the Board session too.
	if st, _, _ := e.do(t, "POST", "/api/settings/org-trust/revoke", `{"org":"staypoint"}`, false, ""); st != http.StatusForbidden {
		t.Fatalf("agent revoke: %d", st)
	}
}

func TestOrgTrust_ApprovesOrgTasksAndBoardRulesStillWait(t *testing.T) {
	e := startGateServer(t, nil, nil)
	dir := runningTask(t, e, "T1") // org StayPoint
	seedTask(t, e.db, "O1", t.TempDir(), "Other")
	if _, err := e.db.Exec(`UPDATE tasks SET execution_stage = 'in_progress', status = 'active' WHERE id = 'O1'`); err != nil {
		t.Fatal(err)
	}
	st, rule, _ := e.do(t, "POST", "/api/settings/org-trust", `{"org":"STAYPOINT","preset":"4h"}`, true, "good")
	if st != http.StatusCreated {
		t.Fatalf("create: %d %v", st, rule)
	}
	ruleID := int64(rule["id"].(float64))

	// The Board's five unattended-run rules, and the task-trust exclusions,
	// all still wait under org trust.
	for _, cmd := range []string{
		"git push origin main",                                       // (1) push to main
		"gh pr merge 12 --squash",                                    // (1) merging is never auto-approved
		"wrangler deploy",                                            // (2) prod deploy
		"psql $PROD_URL -c 'update users set a=1'",                   // (2) prod write
		"curl -X POST https://api.stripe.com/v1/charges -d amount=1", // (3) external API write
		`sqlite3 app.db "DELETE FROM customers"`,                     // (4) destructive delete
		"rm -rf /var/lib/data",                                       // (4) delete outside the worktree (deferred)
		"mail -s report boss@example.com < customers.csv",            // (5) sending PII
	} {
		if got := e.createIn(t, cmd, "T1", dir); got["status"] != "pending" {
			t.Errorf("%q auto-approved under org trust: %v", cmd, got)
		}
	}

	// Ordinary Red commands and opening a PR are approved by the org trust.
	for _, cmd := range []string{"sudo ls /var/log", "gh pr create --title x --body y"} {
		got := e.createIn(t, cmd, "T1", dir)
		if got["status"] != "approved" || got["decided_by"] != "rule:"+itoa(ruleID) {
			t.Fatalf("%q: want org trust approval, got %v", cmd, got)
		}
		entries := gateAudit(t, e.db, got["id"].(string))
		if len(entries) != 1 || !strings.Contains(entries[0], "security_gate_auto_approved") || !strings.Contains(entries[0], "org staypoint") ||
			!strings.Contains(entries[0], `"rule_id":`+itoa(ruleID)) {
			t.Fatalf("audit: %v", entries)
		}
	}
	// Another organization's task is not covered.
	if got := e.createIn(t, "sudo ls /var/log", "O1", t.TempDir()); got["status"] != "pending" {
		t.Fatalf("other org approved: %v", got)
	}
	// Board policy requests are never approved by a rule.
	if got := e.createIn(t, "sudo ls /var/log", "T1", dir, "tracking-gate-override"); got["status"] == "approved" && got["decided_by"] == "rule:"+itoa(ruleID) {
		t.Fatalf("policy request approved by org trust: %v", got)
	}

	st, list, _ := e.do(t, "GET", "/api/settings/org-trust", "", false, "")
	trusts, _ := list["trusts"].([]any)
	if st != http.StatusOK || len(trusts) != 1 {
		t.Fatalf("list: %d %v", st, list)
	}
	v := trusts[0].(map[string]any)
	if v["scope_value"] != "staypoint" || v["auto_approved_count"].(float64) != 2 || v["held_count"].(float64) < 7 {
		t.Fatalf("view: %v", v)
	}

	// Revoke ends it at once.
	if st, out, _ := e.do(t, "POST", "/api/settings/org-trust/revoke", `{"org":"StayPoint"}`, true, ""); st != http.StatusOK {
		t.Fatalf("revoke: %d %v", st, out)
	}
	if !boardAuditHas(t, e, "revoke_org_trust") {
		t.Fatal("revoke not in the Board audit log")
	}
	if got := e.createIn(t, "sudo ls /var/log", "T1", dir); got["status"] != "pending" {
		t.Fatalf("revoked org trust approved: %v", got)
	}
	if st, _, _ := e.do(t, "POST", "/api/settings/org-trust/revoke", `{"org":"StayPoint"}`, true, ""); st != http.StatusNotFound {
		t.Fatalf("second revoke: %d", st)
	}
}

// task-9d94997c: the Board rules hold under a task trust too, every held
// request gets a deferral deadline, and deciding a deferred one tells the
// task (approval also wakes it).
func TestTrust_BoardRulesDeferAndDecideNotifiesTask(t *testing.T) {
	e := startGateServer(t, nil, nil)
	dir := runningTask(t, e, "T1")
	e.trust(t, "T1", `{"preset":"4h"}`)

	for _, cmd := range []string{"wrangler deploy", "cat ~/.staypoint/board_token", "git push origin main", "$GIT push origin main"} {
		got := e.createIn(t, cmd, "T1", dir)
		if got["status"] != "pending" || got["defer_at"] == nil {
			t.Fatalf("%q under task trust: want pending with a deadline, got %v", cmd, got)
		}
	}
	if got := e.createIn(t, "gh pr create --title x --body y", "T1", dir); got["status"] != "approved" {
		t.Fatalf("gh pr create under task trust: %v", got)
	}

	woke := make(chan string, 4)
	prev := orchestrator.GlobalDispatcher.OnWake
	orchestrator.GlobalDispatcher.OnWake = func(taskID, reason string) { woke <- taskID + " " + reason }
	t.Cleanup(func() { orchestrator.GlobalDispatcher.OnWake = prev })

	held := func(cmd string) string {
		t.Helper()
		id := e.createIn(t, cmd, "T1", dir)["id"].(string)
		if _, err := e.db.Exec(`UPDATE security_gate_requests SET defer_at = ? WHERE id = ?`, "2000-01-01T00:00:00Z", id); err != nil {
			t.Fatal(err)
		}
		if st, out, _ := e.do(t, "GET", "/api/security/gate-requests/"+id, "", false, ""); st != http.StatusOK || out["status"] != "deferred" {
			t.Fatalf("not deferred: %d %v", st, out)
		}
		return id
	}
	comment := func(id string) string {
		var msg string
		_ = e.db.QueryRow(`SELECT message FROM task_comments WHERE task_id = 'T1' AND message LIKE ? ORDER BY id DESC LIMIT 1`, "%"+id+"%").Scan(&msg)
		return msg
	}

	ok := held("wrangler deploy --env staging")
	if st, out, _ := e.do(t, "POST", "/api/security/gate-requests/"+ok+"/decide", `{"decision":"approved"}`, true, "good"); st != http.StatusOK {
		t.Fatalf("approve: %d %v", st, out)
	}
	if msg := comment(ok); !strings.Contains(msg, "Board approved held action "+ok) || !strings.Contains(msg, "perform it now") {
		t.Fatalf("approve comment: %q", msg)
	}
	select {
	case w := <-woke:
		if w != "T1 gate_approved_deferred" {
			t.Fatalf("wake: %q", w)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("approved deferred request did not wake the task")
	}

	no := held("terraform apply")
	// The Gates page's Deferred queue lists it.
	_, list, _ := e.do(t, "GET", "/api/security/gate-requests?status=deferred", "", false, "")
	if !strings.Contains(fmt.Sprint(list["gate_requests"]), no) {
		t.Fatalf("deferred queue missing %s: %v", no, list)
	}
	if st, _, _ := e.do(t, "POST", "/api/security/gate-requests/"+no+"/decide", `{"decision":"denied"}`, true, "good"); st != http.StatusOK {
		t.Fatalf("deny: %d", st)
	}
	if msg := comment(no); !strings.Contains(msg, "Board rejected held action "+no) {
		t.Fatalf("deny comment: %q", msg)
	}
	select {
	case w := <-woke:
		t.Fatalf("denied request woke the task: %q", w)
	case <-time.After(200 * time.Millisecond):
	}

}

// task-9d94997c item C: an org trust never covers a work-repo task or a task
// without a repo.
func TestOrgTrust_SkipsWorkReposAndRepoless(t *testing.T) {
	e := startGateServer(t, nil, nil)
	for id, repo := range map[string]string{"W1": filepath.Join(t.TempDir(), "mansol-client"), "N1": "", "P1": t.TempDir()} {
		seedTask(t, e.db, id, repo, "StayPoint")
		if _, err := e.db.Exec(`UPDATE tasks SET execution_stage = 'in_progress', status = 'active' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
	if st, out, _ := e.do(t, "POST", "/api/settings/org-trust", `{"org":"staypoint","preset":"1h"}`, true, "good"); st != http.StatusCreated {
		t.Fatalf("create: %d %v", st, out)
	}
	for id, want := range map[string]string{"W1": "pending", "N1": "pending", "P1": "approved"} {
		if got := e.createIn(t, "sudo ls /var/log", id, t.TempDir()); got["status"] != want {
			t.Errorf("%s: want %s, got %v", id, want, got)
		}
	}
}
