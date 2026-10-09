package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
	"github.com/VinnyVanGogh/staypoint/internal/server"
)

// Gate wiring (task-70e08aca): the daemon's gates are proven through the
// production wiring, a real server plus wireOnWake, not by calling the gate
// functions directly. Each test drives the HTTP API the Board and agents use
// and asserts the run never reaches the adapter. A positive control in each
// test shows the same setup does run the adapter once the gate is cleared, so
// a broken wake path cannot make a gate test pass by accident.

const wiringAssertion = "board-ok"

type wiringDaemon struct {
	store    *db.Store
	srv      *server.Server
	token    string
	repo     string
	adapterN atomic.Int32
}

// startWiringDaemon boots a real server and wires its dispatcher to a harness
// whose adapter only counts calls.
func startWiringDaemon(t *testing.T) *wiringDaemon {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	orchestrator.GlobalDispatcher = orchestrator.NewDispatcher()
	d := &wiringDaemon{store: openTestStore(t), repo: t.TempDir(), token: "wiring-token-1234567890abcdef0123"}
	if _, err := d.store.DB().Exec(
		`INSERT INTO board_webauthn_credentials (credential_id, public_key) VALUES ('wiring-passkey', x'00')`,
	); err != nil {
		t.Fatalf("seed passkey: %v", err)
	}
	srv, err := server.New(server.Options{
		BindHost: "127.0.0.1", Port: 0, AuthToken: d.token, DB: d.store.DB(),
		TelemetryDBPath:  filepath.Join(t.TempDir(), "telemetry.db"),
		ReplayBufferSize: 100, SubscriberBufferSize: 16,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("server start: %v", err)
	}
	t.Cleanup(func() {
		orchestrator.GlobalDispatcher.Drain()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	srv.SetWebAuthnVerifier(func(_ *http.Request, a string) error {
		if a == wiringAssertion {
			return nil
		}
		return errors.New("bad assertion")
	})
	d.srv = srv

	stub := func(_ context.Context, _ string, _ string, _ []string, _ []string, stdout, _ io.Writer) error {
		d.adapterN.Add(1)
		fmt.Fprintln(stdout, "[[TASK_COMPLETE]]")
		return nil
	}
	wireOnWake(d.store, d.repo, srv, stub, &stubWM{dir: d.repo})
	return d
}

func (d *wiringDaemon) task(t *testing.T, id, org string) {
	t.Helper()
	if _, err := d.store.DB().Exec(
		`INSERT INTO tasks (id, name, repo_path, git_branch, execution_stage, organization) VALUES (?, ?, ?, '', 'todo', ?)`,
		id, "wiring "+id, d.repo, org,
	); err != nil {
		t.Fatalf("insert task %s: %v", id, err)
	}
}

func (d *wiringDaemon) do(t *testing.T, path, body string, board bool) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, d.srv.URL()+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+d.token)
	req.Header.Set("Content-Type", "application/json")
	if board {
		req.AddCookie(&http.Cookie{Name: "staypoint_board", Value: d.srv.BoardToken()})
		req.Header.Set("X-WebAuthn-Assertion", wiringAssertion)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// runNow presses Run Now as the Board and waits for the run it starts.
func (d *wiringDaemon) runNow(t *testing.T, id string) (int, string) {
	t.Helper()
	code, body := d.do(t, "/api/tasks/"+id+"/stage", `{"stage":"in_progress"}`, true)
	d.drain(t)
	return code, body
}

func (d *wiringDaemon) drain(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() { orchestrator.GlobalDispatcher.Drain(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("dispatcher did not drain")
	}
}

func (d *wiringDaemon) stage(t *testing.T, id string) string {
	t.Helper()
	task, err := meshContext.GetTask(d.store.DB(), id)
	if err != nil {
		t.Fatalf("get task %s: %v", id, err)
	}
	return task.ExecutionStage
}

func TestGateWiring_BudgetGateCapsRunNow(t *testing.T) {
	d := startWiringDaemon(t)

	d.task(t, "wiring-budget", "StayPoint")
	if err := meshContext.UpdateTaskBudget(d.store.DB(), "wiring-budget", 1.00, 0); err != nil {
		t.Fatal(err)
	}
	if err := meshContext.RecordTaskSpend(d.store.DB(), "wiring-budget", 1000, 1.25, 1); err != nil {
		t.Fatal(err)
	}
	if code, body := d.runNow(t, "wiring-budget"); code != http.StatusOK {
		t.Fatalf("run now: %d %s", code, body)
	}
	if n := d.adapterN.Load(); n != 0 {
		t.Fatalf("adapter ran %d times on a task over its USD budget", n)
	}
	if got := d.stage(t, "wiring-budget"); got != "capped" {
		t.Fatalf("over-budget task stage = %s, want capped", got)
	}

	// Control: the same Run Now on a task under its budget reaches the adapter.
	d.task(t, "wiring-under", "StayPoint")
	if err := meshContext.UpdateTaskBudget(d.store.DB(), "wiring-under", 1.00, 0); err != nil {
		t.Fatal(err)
	}
	if code, body := d.runNow(t, "wiring-under"); code != http.StatusOK {
		t.Fatalf("control run now: %d %s", code, body)
	}
	if n := d.adapterN.Load(); n == 0 {
		t.Fatal("control: adapter never ran; the wake path is not wired")
	}
}

func TestGateWiring_OrgHoldBlocksRunNowAndWakes(t *testing.T) {
	d := startWiringDaemon(t)
	d.task(t, "wiring-held", "Managed Solution")

	// An agent cannot place a hold; the Board can.
	hold := `{"organization":"Managed Solution","held":true}`
	if code, body := d.do(t, "/api/settings/org-hold", hold, false); code != http.StatusForbidden {
		t.Fatalf("agent org hold: %d %s, want 403", code, body)
	}
	if code, body := d.do(t, "/api/settings/org-hold", hold, true); code != http.StatusOK {
		t.Fatalf("board org hold: %d %s", code, body)
	}

	// Run Now is refused at the API.
	if code, body := d.runNow(t, "wiring-held"); code != http.StatusConflict {
		t.Fatalf("run now while held: %d %s, want 409", code, body)
	}
	// A wake from any other source (comment, schedule, blocker cleared) is
	// refused by the daemon's wake handler.
	orchestrator.GlobalDispatcher.Wake("wiring-held", "comment", "comment:wiring-held")
	d.drain(t)
	if n := d.adapterN.Load(); n != 0 {
		t.Fatalf("adapter ran %d times for a held organization", n)
	}
	var held int
	_ = d.store.DB().QueryRow(`SELECT COUNT(*) FROM activity_log WHERE task_id='wiring-held' AND event_type='wake_held'`).Scan(&held)
	if held < 2 {
		t.Fatalf("wake_held logged %d times, want one per refused wake (2)", held)
	}

	// Control: once the Board lifts the hold, Run Now reaches the adapter.
	if code, body := d.do(t, "/api/settings/org-hold", `{"organization":"Managed Solution","held":false}`, true); code != http.StatusOK {
		t.Fatalf("board lift: %d %s", code, body)
	}
	if code, body := d.runNow(t, "wiring-held"); code != http.StatusOK {
		t.Fatalf("run now after lift: %d %s", code, body)
	}
	if n := d.adapterN.Load(); n == 0 {
		t.Fatal("control: adapter never ran after the hold was lifted")
	}
}

func TestGateWiring_DoneGateRefusesAgentWithoutWorkProduct(t *testing.T) {
	d := startWiringDaemon(t)
	d.task(t, "wiring-done", "StayPoint")

	if code, body := d.do(t, "/api/tasks/wiring-done/done", `{}`, false); code != http.StatusConflict {
		t.Fatalf("agent done without work product: %d %s, want 409", code, body)
	}
	if code, body := d.do(t, "/api/tasks/wiring-done/done", `{"board":true}`, false); code != http.StatusForbidden {
		t.Fatalf("agent claiming the Board done path: %d %s, want 403", code, body)
	}
	// The stage endpoint is a second way to reach done; it let an agent token
	// close a task with no work product until this test caught it.
	if code, body := d.do(t, "/api/tasks/wiring-done/stage", `{"stage":"done"}`, false); code != http.StatusConflict {
		t.Fatalf("agent stage=done without work product: %d %s, want 409", code, body)
	}
	if code, body := d.do(t, "/api/tasks/wiring-done/stage", `{"stage":"done","override":true}`, false); code != http.StatusForbidden {
		t.Fatalf("agent stage=done with override: %d %s, want 403", code, body)
	}
	if got := d.stage(t, "wiring-done"); got == "done" {
		t.Fatal("an agent closed a task with no work product")
	}

	// Control: with a work product registered the agent may close it.
	if code, body := d.do(t, "/api/tasks/wiring-done/work-products", `{"type":"pr","ref":"https://github.com/o/r/pull/1"}`, false); code >= 300 {
		t.Fatalf("add work product: %d %s", code, body)
	}
	if code, body := d.do(t, "/api/tasks/wiring-done/stage", `{"stage":"done"}`, false); code != http.StatusOK {
		t.Fatalf("agent stage=done with work product: %d %s", code, body)
	}
	if got := d.stage(t, "wiring-done"); got != "done" {
		t.Fatalf("stage after done with work product = %s", got)
	}
}
