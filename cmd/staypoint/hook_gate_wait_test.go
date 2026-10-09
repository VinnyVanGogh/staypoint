package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/adapter"
	"github.com/VinnyVanGogh/staypoint/internal/gates"
)

// gateServer answers every long-poll with status after calls polls have
// returned "pending" (calls < 0: pending forever).
func gateServer(t *testing.T, status string, calls int32) (*httptest.Server, *int32) {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := atomic.AddInt32(&n, 1)
		s := "pending"
		if calls >= 0 && got > calls {
			s = status
		}
		fmt.Fprintf(w, `{"status":%q,"decided_by":"board"}`, s)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func withGateBudget(t *testing.T, d time.Duration) {
	t.Helper()
	old := gateWaitBudget
	gateWaitBudget = d
	t.Cleanup(func() { gateWaitBudget = old })
}

// The bug (task-cae83e7f): the hook waited forever, Claude Code killed it at
// its timeout, and a timed-out hook does not block, so the command ran
// unapproved. The wait must end on its own, as "expired", never as approval.
func TestWaitGateDecisionExpiresInsteadOfHangingPastHookTimeout(t *testing.T) {
	srv, n := gateServer(t, "", -1)
	withGateBudget(t, 50*time.Millisecond)

	start := time.Now()
	status, _ := waitGateDecision(srv.URL, "tok", "gr1")
	if status != "expired" {
		t.Fatalf("status = %q, want expired", status)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("wait took %s, budget was 50ms", el)
	}
	if atomic.LoadInt32(n) == 0 {
		t.Fatal("never polled the daemon")
	}
}

func TestWaitGateDecisionZeroBudgetNeverApproves(t *testing.T) {
	srv, n := gateServer(t, "approved", 0)
	withGateBudget(t, 0)
	if status, _ := waitGateDecision(srv.URL, "tok", "gr1"); status != "expired" {
		t.Fatalf("status = %q, want expired", status)
	}
	if atomic.LoadInt32(n) != 0 {
		t.Fatal("polled after the budget was spent")
	}
}

func TestWaitGateDecisionReturnsBoardDecision(t *testing.T) {
	for _, want := range []string{"approved", "denied", "deferred"} {
		srv, _ := gateServer(t, want, 2)
		withGateBudget(t, time.Minute)
		if got, by := waitGateDecision(srv.URL, "tok", "gr1"); got != want || by != "board" {
			t.Fatalf("got %q by %q, want %q by board", got, by, want)
		}
	}
}

func TestHeldPastWaitMessageSaysNotRun(t *testing.T) {
	msg := heldPastWaitMessage("gr9")
	for _, s := range []string{"NOT run", "gr9", "Do not retry"} {
		if !strings.Contains(msg, s) {
			t.Fatalf("message %q missing %q", msg, s)
		}
	}
}

// The budget has to end before the hook's registered timeout, with room for
// one in-flight long-poll (35s client timeout).
func TestGateWaitBudgetEndsBeforeHookTimeout(t *testing.T) {
	hook := time.Duration(adapter.PreToolHookTimeoutSeconds) * time.Second
	if gateWaitBudget+35*time.Second >= hook {
		t.Fatalf("budget %s + 35s poll >= hook timeout %s", gateWaitBudget, hook)
	}
	// It is the backstop: the daemon's defer deadline should fire first.
	if def := time.Duration(gates.DefaultTrustDeferMinutes) * time.Minute; gateWaitBudget <= def {
		t.Fatalf("budget %s is not behind the daemon defer deadline %s", gateWaitBudget, def)
	}
}
