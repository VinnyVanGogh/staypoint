package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/gates"
	"github.com/VinnyVanGogh/staypoint/internal/security"
)

// task-cae83e7f: a task run with no trust used to have no deadline, so the
// hook waited until Claude Code's hook timeout (600s), which then RAN the held
// command. Now every task request is skipped at the defer deadline instead,
// and a later approval tells the task to perform it.
func TestUntrustedTaskRequestDefersAndApprovalNotifiesTask(t *testing.T) {
	e := startGateServer(t, nil, nil)
	dir := runningTask(t, e, "T1")

	got := e.createIn(t, "ssh prod 'sudo systemctl restart app'", "T1", dir)
	id := got["id"].(string)
	if got["status"] != "pending" {
		t.Fatalf("prod write not held: %v", got)
	}
	gr, _ := security.GetGateRequest(e.db, id)
	wantDefer := time.Duration(gates.DefaultTrustDeferMinutes) * time.Minute
	if gr.DeferAt == nil || gr.DeferAt.Sub(gr.CreatedAt) < wantDefer-time.Minute || gr.DeferAt.Sub(gr.CreatedAt) > wantDefer+time.Minute {
		t.Fatalf("untrusted task request has no defer deadline: defer_at %v (created %v)", gr.DeferAt, gr.CreatedAt)
	}

	if _, err := e.db.Exec(`UPDATE security_gate_requests SET defer_at = ? WHERE id = ?`,
		time.Now().Add(-time.Second).UTC().Format(security.DeferTimeFormat), id); err != nil {
		t.Fatal(err)
	}
	_, out, _ := e.do(t, "GET", "/api/security/gate-requests/"+id+"?wait=true", "", false, "")
	if out["status"] != "deferred" {
		t.Fatalf("after the deadline the hook must see deferred (skip, not run): %v", out)
	}

	st, _, _ := e.do(t, "POST", "/api/security/gate-requests/"+id+"/decide", `{"decision":"approved"}`, true, "good")
	if st != http.StatusOK {
		t.Fatalf("deciding a deferred request: %d", st)
	}
	_, out, _ = e.do(t, "GET", "/api/security/gate-requests/"+id+"?wait=true", "", false, "")
	if out["status"] != "deferred" {
		t.Fatalf("approving a deferred request must not replay it: %v", out)
	}
	audit := strings.Join(gateAudit(t, e.db, id), "\n")
	for _, want := range []string{"security_gate_deferred", `"deferred":true`} {
		if !strings.Contains(audit, want) {
			t.Errorf("audit missing %s:\n%s", want, audit)
		}
	}
}

// agy kills its hook at 30s, so its requests carry max_wait_seconds: the
// daemon skips them by then. It can only shorten the deadline, with or
// without a task.
func TestMaxWaitSecondsShortensDeferDeadline(t *testing.T) {
	e := startGateServer(t, nil, nil)
	dir := runningTask(t, e, "T1")
	post := func(task string, maxWait int) *security.GateRequest {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"cmdline": "git push --force origin main", "reasons": []string{"red"},
			"run_id": "conv", "task_id": task, "cwd": dir, "max_wait_seconds": maxWait})
		st, out, _ := e.do(t, "POST", "/api/security/gate-requests", string(b), false, "")
		if st != http.StatusCreated {
			t.Fatalf("create: %d %v", st, out)
		}
		gr, _ := security.GetGateRequest(e.db, out["id"].(string))
		return gr
	}
	near := func(gr *security.GateRequest, want time.Duration) {
		t.Helper()
		if gr.DeferAt == nil {
			t.Fatalf("no defer deadline, want ~%s", want)
		}
		if d := gr.DeferAt.Sub(gr.CreatedAt); d < want-2*time.Second || d > want+2*time.Second {
			t.Fatalf("defer after %s, want ~%s", d, want)
		}
	}
	near(post("T1", 15), 15*time.Second)
	near(post("", 15), 15*time.Second)
	// Longer than the task's own deadline: the task deadline wins.
	near(post("T1", 3600), time.Duration(gates.DefaultTrustDeferMinutes)*time.Minute)
}

// A request outside any task (interactive session) keeps waiting for the
// Board; the hook's own wait budget bounds it.
func TestRequestWithoutTaskHasNoDeferDeadline(t *testing.T) {
	e := startGateServer(t, nil, nil)
	got := e.createIn(t, "ssh prod 'sudo systemctl restart app'", "", t.TempDir())
	gr, _ := security.GetGateRequest(e.db, got["id"].(string))
	if gr.DeferAt != nil {
		t.Fatalf("no-task request got a defer deadline: %v", gr.DeferAt)
	}
}
