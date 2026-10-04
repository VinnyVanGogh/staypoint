package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
	"github.com/VinnyVanGogh/staypoint/internal/server"
)

// openTestStore opens a real SQLite store in a temp dir with the full schema.
func openTestStore(t *testing.T) *db.Store {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// stubWM implements WorktreeManagerIface using a pre-existing directory so
// tests do not require a real git repository.
type stubWM struct{ dir string }

func (s *stubWM) CreateContext(_ context.Context, _, _ string) (string, error) {
	return s.dir, nil
}
func (s *stubWM) PruneContext(_ context.Context, _ string) error            { return nil }
func (s *stubWM) PruneWorktreeDirContext(_ context.Context, _ string) error { return nil }

func TestWireOnWake_SetsOnWake(t *testing.T) {
	orchestrator.GlobalDispatcher = orchestrator.NewDispatcher()
	if orchestrator.GlobalDispatcher.OnWake != nil {
		t.Fatal("pre-condition: OnWake should be nil before wiring")
	}
	wireOnWake(openTestStore(t), t.TempDir(), nil, nil)
	if orchestrator.GlobalDispatcher.OnWake == nil {
		t.Fatal("wireOnWake did not set GlobalDispatcher.OnWake")
	}
}

func TestWireOnWake_AdapterCalledOnWake(t *testing.T) {
	orchestrator.GlobalDispatcher = orchestrator.NewDispatcher()

	store := openTestStore(t)
	dir := t.TempDir()

	taskID := "wire-wake-test-abc1"
	if _, err := store.DB().Exec(
		`INSERT INTO tasks (id, name, repo_path, execution_stage, assignee_agent_id) VALUES (?, 'test task', '', 'todo', 'agent-test')`,
		taskID,
	); err != nil {
		t.Fatalf("insert task: %v", err)
	}

	var adapterCalls atomic.Int32
	stub := func(_ context.Context, _ string, _ string, _ []string, _ []string, stdout, _ io.Writer) error {
		adapterCalls.Add(1)
		// Signal completion so the harness exits after one turn.
		fmt.Fprintln(stdout, "[[TASK_COMPLETE]]")
		return nil
	}

	wireOnWake(store, dir, nil, stub, &stubWM{dir: dir})

	orchestrator.GlobalDispatcher.Wake(taskID, "test_wake", "test:"+taskID)

	// Drain waits for the goroutine spawned by Wake to return.
	drainDone := make(chan struct{})
	go func() { orchestrator.GlobalDispatcher.Drain(); close(drainDone) }()
	select {
	case <-drainDone:
	case <-time.After(15 * time.Second):
		t.Fatal("dispatcher did not drain within timeout")
	}

	if adapterCalls.Load() == 0 {
		t.Fatal("stub adapter was never called; harness did not run")
	}

	var stage string
	if err := store.DB().QueryRow(
		"SELECT execution_stage FROM tasks WHERE id=?", taskID,
	).Scan(&stage); err != nil {
		t.Fatalf("query task stage: %v", err)
	}
	if stage == "todo" {
		t.Errorf("task execution_stage still 'todo' after wake; harness did not claim it")
	}
}

func TestWireOnWake_MissingTaskSkipsRun(t *testing.T) {
	orchestrator.GlobalDispatcher = orchestrator.NewDispatcher()
	store := openTestStore(t)
	dir := t.TempDir()

	var adapterCalls atomic.Int32
	stub := func(_ context.Context, _ string, _ string, _ []string, _ []string, _ io.Writer, _ io.Writer) error {
		adapterCalls.Add(1)
		return nil
	}

	wireOnWake(store, dir, nil, stub, &stubWM{dir: dir})

	// Wake a task that does not exist in the DB.
	orchestrator.GlobalDispatcher.Wake("nonexistent-task-id", "test", "test:nonexistent")

	drainDone := make(chan struct{})
	go func() { orchestrator.GlobalDispatcher.Drain(); close(drainDone) }()
	select {
	case <-drainDone:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatcher did not drain")
	}

	if adapterCalls.Load() != 0 {
		t.Error("adapter should not be called for a missing task")
	}
}

// TestWireOnWake_ParseDeltaUsesClaudeAdapter asserts that parseDelta uses the
// Claude adapter (not the Agy/Gemini fallback) so tool steps from a Claude
// stream-json transcript are recorded in run_steps.
//
// Regression guard for: parseDelta called adapter.AdapterFor("") which
// returned AgyAdapter{} and silently dropped all Claude stream events.
func TestWireOnWake_ParseDeltaUsesClaudeAdapter(t *testing.T) {
	orchestrator.GlobalDispatcher = orchestrator.NewDispatcher()

	store := openTestStore(t)
	dir := t.TempDir()

	taskID := "wire-wake-claude-parse-01"
	if _, err := store.DB().Exec(
		`INSERT INTO tasks (id, name, repo_path, execution_stage, assignee_agent_id) VALUES (?, 'claude parse test', '', 'todo', 'agent-claude')`,
		taskID,
	); err != nil {
		t.Fatalf("insert task: %v", err)
	}

	// Read the captured Claude stream-json fixture.  The fixture contains a
	// Bash tool_use + tool_result pair which must produce at least one non-wake
	// step when parsed by ClaudeAdapter.
	fixturePath := filepath.Join("..", "..", "internal", "adapter", "testdata", "claude_stream.ndjson")
	fixtureBytes, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read claude fixture: %v", err)
	}

	stub := func(_ context.Context, _ string, prov string, _ []string, _ []string, stdout io.Writer, _ io.Writer) error {
		// Emit the captured Claude stream-json lines so parseDelta can process them.
		if _, werr := stdout.Write(fixtureBytes); werr != nil {
			return werr
		}
		// Signal harness completion.
		fmt.Fprintln(stdout, "[[TASK_COMPLETE]]")
		return nil
	}

	wireOnWake(store, dir, nil, stub, &stubWM{dir: dir})
	orchestrator.GlobalDispatcher.Wake(taskID, "test_claude_parse", "test:"+taskID)

	drainDone := make(chan struct{})
	go func() { orchestrator.GlobalDispatcher.Drain(); close(drainDone) }()
	select {
	case <-drainDone:
	case <-time.After(15 * time.Second):
		t.Fatal("dispatcher did not drain within timeout")
	}

	// Query run_steps for steps that are not wake/state/checkpoint bookkeeping.
	// With the correct ClaudeAdapter, the tool_use line in the fixture must produce
	// at least one such step (kind = 'run', 'read', 'edit', or similar tool step).
	rows, err := store.DB().QueryContext(context.Background(),
		`SELECT kind FROM run_steps WHERE task_id = ? AND kind NOT IN ('wake','state','checkpoint')`,
		taskID,
	)
	if err != nil {
		t.Fatalf("query run_steps: %v", err)
	}
	defer rows.Close()

	var toolSteps []string
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatalf("scan row: %v", err)
		}
		toolSteps = append(toolSteps, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows error: %v", err)
	}

	if len(toolSteps) == 0 {
		t.Error("no tool/text steps recorded in run_steps; ClaudeAdapter stream parsing is broken (AdapterFor may be using wrong provider)")
	}
}

// TestStaypointd_BoardToken_PersistedAndUsable is an E2E test on the staypointd
// options path (not just the apitest server). It verifies:
//  1. When BoardTokenPath is set (as it now is in runDaemon), the board_token
//     file is written to DataDir at server startup.
//  2. POST /api/board/fresh-nonce (auth token + X-Board-Token) mints a one-time nonce.
//  3. GET /?token=…&board_nonce=… sets the staypoint_board cookie (nonce flow, STA-583).
//  4. Reusing the same nonce does NOT set the cookie (single-use).
//  5. The legacy ?board_token= URL bootstrap is rejected (removed in STA-583).
//  6. A board session can call a WrapBoardAction-protected endpoint (not 403).
//  7. An agent-only request (no board cookie) gets 403.
func TestStaypointd_BoardToken_PersistedAndUsable(t *testing.T) {
	dataDir := t.TempDir()
	boardTokenPath := filepath.Join(dataDir, "board_token")

	store := openTestStore(t)

	// Seed one Board passkey so WrapBoardAction passes the enrollment gate (post-STA-583).
	if _, err := store.DB().Exec(`INSERT INTO board_webauthn_credentials (id, credential_id, public_key) VALUES (?, ?, ?)`,
		"staypointd-test-passkey", []byte("cred-id-1"), []byte("pub-key-1"),
	); err != nil {
		t.Fatalf("seed board_webauthn_credentials: %v", err)
	}

	// Mirror the server.Options now used by runDaemon.
	srv, err := server.New(server.Options{
		BindHost:       "127.0.0.1",
		Port:           0, // ephemeral
		TokenPath:      filepath.Join(dataDir, "auth_token"),
		BoardTokenPath: boardTokenPath,
		DB:             store.DB(),
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("srv.Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	// Stub out WebAuthn assertion verification so tests pass without Touch ID hardware.
	srv.SetWebAuthnVerifier(func(_ *http.Request, _ string) error { return nil })

	// 1. board_token file must have been written to DataDir.
	data, err := os.ReadFile(boardTokenPath)
	if err != nil {
		t.Fatalf("board_token file not created at %s: %v", boardTokenPath, err)
	}
	boardToken := strings.TrimSpace(string(data))
	if len(boardToken) < 16 {
		t.Fatalf("board_token too short: %q", boardToken)
	}
	if boardToken != srv.BoardToken() {
		t.Errorf("persisted board_token %q != srv.BoardToken() %q", boardToken, srv.BoardToken())
	}

	authToken := srv.Token()
	base := srv.URL()

	// 2. Mint a one-time nonce via POST /api/board/fresh-nonce.
	// Requires bearer auth token + X-Board-Token with the board credential.
	nonceReq, _ := http.NewRequest("POST", base+"/api/board/fresh-nonce", nil)
	nonceReq.Header.Set("Authorization", "Bearer "+authToken)
	nonceReq.Header.Set("X-Board-Token", boardToken)
	nonceResp, err := http.DefaultClient.Do(nonceReq)
	if err != nil {
		t.Fatalf("fresh-nonce POST: %v", err)
	}
	nonceBody, _ := io.ReadAll(nonceResp.Body)
	nonceResp.Body.Close()
	if nonceResp.StatusCode != http.StatusOK {
		t.Fatalf("fresh-nonce: expected 200, got %d: %s", nonceResp.StatusCode, nonceBody)
	}
	var noncePayload struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(nonceBody, &noncePayload); err != nil {
		t.Fatalf("fresh-nonce: unmarshal: %v; body=%s", err, nonceBody)
	}
	nonce := noncePayload.Nonce
	if len(nonce) < 16 {
		t.Fatalf("fresh-nonce: nonce too short: %q", nonce)
	}

	// 3. Bootstrap a board session: GET /?token=…&board_nonce=… sets the staypoint_board cookie.
	jar := newTestCookieJar()
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return nil
		},
	}
	bootstrapURL := fmt.Sprintf("%s/?token=%s&board_nonce=%s", base, authToken, nonce)
	resp, err := client.Get(bootstrapURL)
	if err != nil {
		t.Fatalf("bootstrap GET: %v", err)
	}
	resp.Body.Close()

	var gotBoardCookie bool
	for _, c := range jar.cookies {
		if c.Name == "staypoint_board" {
			gotBoardCookie = true
			break
		}
	}
	if !gotBoardCookie {
		t.Fatal("staypoint_board cookie was not set after bootstrap with valid board_nonce")
	}

	// 4. Nonce reuse must be rejected: the same nonce must NOT set the cookie a second time.
	jar2 := newTestCookieJar()
	client2 := &http.Client{Jar: jar2, CheckRedirect: func(req *http.Request, via []*http.Request) error { return nil }}
	resp4, err := client2.Get(bootstrapURL)
	if err != nil {
		t.Fatalf("nonce-reuse GET: %v", err)
	}
	resp4.Body.Close()
	for _, c := range jar2.cookies {
		if c.Name == "staypoint_board" {
			t.Error("staypoint_board cookie was set on nonce reuse; nonce must be single-use")
			break
		}
	}

	// 5. Legacy ?board_token= bootstrap is rejected (removed in STA-583).
	jar3 := newTestCookieJar()
	client3 := &http.Client{Jar: jar3, CheckRedirect: func(req *http.Request, via []*http.Request) error { return nil }}
	legacyURL := fmt.Sprintf("%s/?token=%s&board_token=%s", base, authToken, boardToken)
	resp5, err := client3.Get(legacyURL)
	if err != nil {
		t.Fatalf("legacy board_token GET: %v", err)
	}
	resp5.Body.Close()
	for _, c := range jar3.cookies {
		if c.Name == "staypoint_board" {
			t.Error("staypoint_board cookie was set via ?board_token= URL; this bootstrap path was removed in STA-583")
			break
		}
	}

	// 6. Board session + WebAuthn assertion → POST /api/settings/security-gate must return
	// something other than 403. (It may return 400/422 due to missing body, but not 403 —
	// the board gate is passed.) Post-STA-583: a passkey assertion is also required.
	req6, _ := http.NewRequest("POST", base+"/api/settings/security-gate", strings.NewReader(`{}`))
	req6.Header.Set("Authorization", "Bearer "+authToken)
	req6.Header.Set("Content-Type", "application/json")
	req6.Header.Set("X-WebAuthn-Assertion", "mock-assertion-for-test")
	for _, c := range jar.cookies {
		req6.AddCookie(c)
	}
	resp6, err := client.Do(req6)
	if err != nil {
		t.Fatalf("board-session POST: %v", err)
	}
	resp6.Body.Close()
	if resp6.StatusCode == http.StatusForbidden {
		t.Errorf("board session got 403 Forbidden on /api/settings/security-gate; board cookie gate is broken")
	}

	// 7. Agent-only (no board cookie) → must get 403.
	req7, _ := http.NewRequest("POST", base+"/api/settings/security-gate", strings.NewReader(`{}`))
	req7.Header.Set("Authorization", "Bearer "+authToken)
	req7.Header.Set("Content-Type", "application/json")
	resp7, err := http.DefaultClient.Do(req7)
	if err != nil {
		t.Fatalf("agent-only POST: %v", err)
	}
	resp7.Body.Close()
	if resp7.StatusCode != http.StatusForbidden {
		t.Errorf("agent-only request expected 403, got %d", resp7.StatusCode)
	}
}

// testCookieJar is a minimal http.CookieJar for tests.
type testCookieJar struct{ cookies []*http.Cookie }

func newTestCookieJar() *testCookieJar { return &testCookieJar{} }

func (j *testCookieJar) SetCookies(_ *url.URL, cookies []*http.Cookie) {
	j.cookies = append(j.cookies, cookies...)
}
func (j *testCookieJar) Cookies(_ *url.URL) []*http.Cookie { return j.cookies }
