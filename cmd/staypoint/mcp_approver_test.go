package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/mcp"
	"github.com/VinnyVanGogh/staypoint/internal/opstools"
)

// task-7d279c9d: a prod_write ops call is a real Board gate request whose
// cmdline is the call's canonical form; only a Board decision releases it,
// and a resumed approval binds to the same call.
func TestBoardApproverHoldsProdWriteForBoard(t *testing.T) {
	w := startWiringServer(t)
	call := opstools.Call{Tool: "pr_merge", Effect: opstools.ProdWrite,
		Summary: "repo=/r pr=5 base=main method=merge head=" + strings.Repeat("c", 40)}
	req := mcp.ApprovalRequest{Call: call, Reason: "prod_write: merge PR #5 into main", TaskID: "wiring-task"}

	type verdict struct {
		ok  bool
		msg string
	}
	done := make(chan verdict, 1)
	go func() {
		ok, msg := boardApprover(context.Background(), req)
		done <- verdict{ok, msg}
	}()

	var id string
	deadline := time.Now().Add(15 * time.Second)
	for id == "" {
		if refs := w.gateRequests(t, "pending"); len(refs) == 1 {
			id = refs[0].ID
		}
		select {
		case v := <-done:
			t.Fatalf("approver returned before the Board decided: %+v", v)
		default:
		}
		if id == "" && time.Now().After(deadline) {
			t.Fatal("prod_write never reached the Board queue")
		}
		time.Sleep(50 * time.Millisecond)
	}
	gr := fetchGateRequest(w.srv.URL(), w.token, id)
	if gr == nil || gr.Cmdline != call.Canonical() || gr.TaskID != "wiring-task" {
		t.Fatalf("gate request = %+v, want cmdline %q", gr, call.Canonical())
	}

	// An agent token cannot approve it.
	if code, _ := w.do(t, http.MethodPost, "/api/security/gate-requests/"+id+"/decide", `{"decision":"approved"}`, false); code != http.StatusForbidden {
		t.Fatalf("agent self-approve: %d, want 403", code)
	}
	if code, body := w.do(t, http.MethodPost, "/api/security/gate-requests/"+id+"/decide", `{"decision":"approved"}`, true); code != http.StatusOK {
		t.Fatalf("board approve: %d %s", code, body)
	}
	select {
	case v := <-done:
		if !v.ok {
			t.Fatalf("approved call refused: %s", v.msg)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("approver still holding after the Board approved")
	}

	// Resuming with that approval works for the same call only.
	req.GateID = id
	if ok, msg := boardApprover(context.Background(), req); !ok {
		t.Fatalf("resume same call: %s", msg)
	}
	other := req
	other.Call.Summary = strings.Replace(call.Summary, "pr=5", "pr=6", 1)
	if ok, msg := boardApprover(context.Background(), other); ok || !strings.Contains(msg, "different call") {
		t.Fatalf("approval reused for another PR: ok=%v %s", ok, msg)
	}
}

func TestBoardApproverFailsClosedWithoutDaemon(t *testing.T) {
	old := gateDaemonConn
	t.Cleanup(func() { gateDaemonConn = old })
	gateDaemonConn = func() (string, string) { return "http://127.0.0.1:1", "tok" }
	ok, msg := boardApprover(context.Background(), mcp.ApprovalRequest{Call: opstools.Call{Tool: "pr_merge", Effect: opstools.ProdWrite}})
	if ok || !strings.Contains(msg, "not run") {
		t.Fatalf("no daemon: ok=%v %s", ok, msg)
	}
}
