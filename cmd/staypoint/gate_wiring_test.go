package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/config"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/server"
)

// Gate wiring (task-70e08aca): the Red-tier Board gate is proven through the
// production path, not the classifier alone. The real PreToolUse hook handler
// runs against a real booted server; a Red command must reach the Board queue
// and stay blocked until the Board denies it. A gate whose constructor had no
// production callers (security.NewGate) passed every unit test while nothing
// was enforced; these tests fail if the hook stops reaching the server.

const wiringAssertion = "board-ok"

type wiringServer struct {
	srv   *server.Server
	token string
}

// startWiringServer boots a real server on a loopback port with a fresh DB,
// one Board passkey and a stub verifier that accepts only wiringAssertion,
// and points the hook's daemon connection and config at it.
func startWiringServer(t *testing.T) *wiringServer {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dbPath := filepath.Join(t.TempDir(), "staypoint.db")
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.DB().Exec(
		`INSERT INTO board_webauthn_credentials (credential_id, public_key) VALUES ('wiring-passkey', x'00')`,
	); err != nil {
		t.Fatalf("seed passkey: %v", err)
	}
	if _, err := store.DB().Exec(
		`INSERT INTO tasks (id, name, repo_path, git_branch, execution_stage) VALUES ('wiring-task', 'wiring', '', '', 'in_progress')`,
	); err != nil {
		t.Fatalf("seed task: %v", err)
	}

	token := "wiring-token-1234567890abcdef0123"
	srv, err := server.New(server.Options{
		BindHost: "127.0.0.1", Port: 0, AuthToken: token, DB: store.DB(),
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

	oldCfg, oldConn := cfg, gateDaemonConn
	t.Cleanup(func() { cfg, gateDaemonConn = oldCfg, oldConn })
	cfg = config.DefaultConfig()
	cfg.DBPath = dbPath
	gateDaemonConn = func() (string, string) { return srv.URL(), token }
	t.Setenv("STAYPOINT_TASK_ID", "wiring-task")
	return &wiringServer{srv: srv, token: token}
}

func (w *wiringServer) do(t *testing.T, method, path, body string, board bool) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, w.srv.URL()+path, rd)
	req.Header.Set("Authorization", "Bearer "+w.token)
	req.Header.Set("Content-Type", "application/json")
	if board {
		req.AddCookie(&http.Cookie{Name: "staypoint_board", Value: w.srv.BoardToken()})
		req.Header.Set("X-WebAuthn-Assertion", wiringAssertion)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// gateRequests lists gate requests with the given status ("pending", "denied").
func (w *wiringServer) gateRequests(t *testing.T, status string) []gateRequestRef {
	t.Helper()
	code, body := w.do(t, http.MethodGet, "/api/security/gate-requests?status="+url.QueryEscape(status), "", false)
	if code != http.StatusOK {
		t.Fatalf("list gate requests: %d %s", code, body)
	}
	var out struct {
		GateRequests []gateRequestRef `json:"gate_requests"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode gate requests: %v %s", err, body)
	}
	return out.GateRequests
}

// runPreToolHook feeds a Claude Code Bash PreToolUse payload to the real hook
// handler and returns its stdout once it exits. It runs in the background so
// the caller can act as the Board while the hook holds the command.
func runPreToolHook(t *testing.T, command string) <-chan string {
	t.Helper()
	input, _ := json.Marshal(map[string]any{
		"tool_name":  "Bash",
		"tool_input": map[string]string{"command": command},
		"session_id": "wiring-session",
		"cwd":        t.TempDir(),
	})
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(input)
	_ = w.Close()
	oldIn := os.Stdin
	os.Stdin = r
	out := make(chan string, 1)
	go func() {
		s := captureStdout(t, handleHookPreTool)
		os.Stdin = oldIn
		_ = r.Close()
		out <- s
	}()
	return out
}

func hookDecision(t *testing.T, out string) (decision, reason string) {
	t.Helper()
	var d struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &d); err != nil {
		t.Fatalf("hook output is not JSON: %q", out)
	}
	return d.Decision, d.Reason
}

func TestGateWiring_GreenCommandNeverReachesBoard(t *testing.T) {
	w := startWiringServer(t)
	select {
	case out := <-runPreToolHook(t, "go version"):
		if strings.TrimSpace(out) != "{}" {
			t.Fatalf("green command not allowed: %q", out)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("hook held a green command")
	}
	if n := len(w.gateRequests(t, "pending")); n != 0 {
		t.Fatalf("green command created %d gate requests", n)
	}
}

func TestGateWiring_RedCommandHeldUntilBoardDenies(t *testing.T) {
	w := startWiringServer(t)
	done := runPreToolHook(t, "git push origin main")

	// The hook must register the command in the real server's Board queue.
	var id string
	deadline := time.Now().Add(15 * time.Second)
	for id == "" {
		if refs := w.gateRequests(t, "pending"); len(refs) == 1 {
			id = refs[0].ID
		} else if len(refs) > 1 {
			t.Fatalf("one Red command created %d gate requests", len(refs))
		}
		select {
		case out := <-done:
			t.Fatalf("hook returned before the Board decided: %q", out)
		default:
		}
		if id == "" && time.Now().After(deadline) {
			t.Fatal("Red command never reached the Board queue: the hook is not wired to the server")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// An agent token cannot decide its own gate, and the command stays held.
	if code, body := w.do(t, http.MethodPost, "/api/security/gate-requests/"+id+"/decide", `{"decision":"approved"}`, false); code != http.StatusForbidden {
		t.Fatalf("agent self-approve: %d %s, want 403", code, body)
	}
	select {
	case out := <-done:
		t.Fatalf("hook released after an agent self-approve attempt: %q", out)
	case <-time.After(300 * time.Millisecond):
	}

	// The Board denies: the hook returns a block, and the command never ran.
	if code, body := w.do(t, http.MethodPost, "/api/security/gate-requests/"+id+"/decide", `{"decision":"denied"}`, true); code != http.StatusOK {
		t.Fatalf("board deny: %d %s", code, body)
	}
	select {
	case out := <-done:
		decision, reason := hookDecision(t, out)
		if decision != "block" || !strings.Contains(reason, "Board denied") {
			t.Fatalf("hook after Board deny: %q", out)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("hook still holding after the Board denied")
	}
	if refs := w.gateRequests(t, "denied"); len(refs) != 1 || refs[0].ID != id {
		t.Fatalf("denied list = %+v, want [%s]", refs, id)
	}
}

func TestGateWiring_RedCommandFailsClosedWhenDaemonDown(t *testing.T) {
	w := startWiringServer(t)
	// Nothing listens on port 1: the hook cannot reach the Board.
	gateDaemonConn = func() (string, string) { return "http://127.0.0.1:1", w.token }
	select {
	case out := <-runPreToolHook(t, "git push origin main"):
		if decision, reason := hookDecision(t, out); decision != "block" || !strings.Contains(reason, "blocked") {
			t.Fatalf("Red command with no daemon: %q", out)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("hook hung with no daemon")
	}
}
