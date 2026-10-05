package server_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/server"
)

// STA-583 layer 2 (plan rev 2): every Board action needs a WebAuthn assertion, the
// cookie alone is never enough (fail-closed), first passkey registration needs an
// out-of-band pairing code, and adding a second passkey needs an assertion from an
// existing one.

// webAuthnVerifierSetter is the test seam the implementation must expose on
// *server.Server. Tests have no Touch ID hardware, so they swap the cryptographic
// verification for a stub; production keeps the real verifier. It is asserted at
// runtime (not referenced statically) so this file compiles before the feature exists
// and the rest of the package's tests keep running.
type webAuthnVerifierSetter interface {
	SetWebAuthnVerifier(func(r *http.Request, assertion string) error)
}

// pairingNotifierSetter is the optional seam for the macOS pairing-code notification,
// so register/begin does not run osascript during tests. Used when present.
type pairingNotifierSetter interface {
	SetPairingNotifier(func(code string) error)
}

// boardActionEndpoints are every route wrapped by WrapBoardAction.
var boardActionEndpoints = []struct {
	method, path, body string
}{
	{"POST", "/api/tasks/webauthn-task/ship-review/approve", ""},
	{"POST", "/api/tasks/webauthn-task/ship-review/send-back", `{"comment":"test"}`},
	{"POST", "/api/tasks/webauthn-task/ship-review/reject", `{"comment":"test"}`},
	{"POST", "/api/tasks/webauthn-task/ship-review/delete-branch", ""},
	{"POST", "/api/security/gate-requests/nonexistent-id/decide", `{"decision":"approved"}`},
	{"POST", "/api/settings/security-gate", `{"main_merge_approval":true}`},
	{"POST", "/api/settings/ship-review", `{"ship_review":true}`},
}

// seedBoardWebAuthnCredential registers one Board passkey. The schema mirrors
// migration 21 from the STA-583 plan; CREATE IF NOT EXISTS keeps this working both
// before the migration exists and after it lands.
func seedBoardWebAuthnCredential(t *testing.T, database *sql.DB) {
	t.Helper()
	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS board_webauthn_credentials (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		credential_id  TEXT NOT NULL UNIQUE,
		public_key     BLOB NOT NULL,
		sign_count     INTEGER NOT NULL DEFAULT 0,
		aaguid         TEXT NOT NULL DEFAULT '',
		created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
		updated_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
	)`); err != nil {
		t.Fatalf("create board_webauthn_credentials: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO board_webauthn_credentials (credential_id, public_key) VALUES (?, ?)`,
		"board-passkey-1", []byte("cose-public-key-1"),
	); err != nil {
		t.Fatalf("seed board_webauthn_credentials: %v", err)
	}
}

// silencePairingNotifier stubs the notification and returns a pointer to the last
// code the daemon generated (empty if the seam does not exist yet).
// The notifier is called in a goroutine by RegisterBegin; the mutex prevents data
// races when multiple begin requests are in flight concurrently.
func silencePairingNotifier(srv *server.Server) *string {
	var mu sync.Mutex
	var last string
	if s, ok := any(srv).(pairingNotifierSetter); ok {
		s.SetPairingNotifier(func(code string) error {
			mu.Lock()
			last = code
			mu.Unlock()
			return nil
		})
	}
	return &last
}

// wrongPairingCode returns a 6-digit code guaranteed to differ from the real one.
func wrongPairingCode(real string) string {
	if real == "123456" {
		return "654321"
	}
	return "123456"
}

type boardReq struct {
	method, path, body string
	cookie             bool
	assertion          string
	session            string // X-WebAuthn-Session to send on the request
}

// doBoard sends an authenticated request, optionally with the board cookie and an
// X-WebAuthn-Assertion header, and returns the status and the decoded "error" field.
func doBoard(t *testing.T, srv *server.Server, token string, br boardReq) (int, string, string) {
	t.Helper()
	var rd io.Reader
	if br.body != "" {
		rd = strings.NewReader(br.body)
	}
	req, _ := http.NewRequest(br.method, srv.URL()+br.path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	if br.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if br.cookie {
		req.AddCookie(&http.Cookie{Name: "staypoint_board", Value: srv.BoardToken()})
	}
	if br.assertion != "" {
		req.Header.Set("X-WebAuthn-Assertion", br.assertion)
	}
	if br.session != "" {
		req.Header.Set("X-WebAuthn-Session", br.session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", br.method, br.path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out.Error, string(raw)
}

// beginRegistrationSession calls POST /api/board/webauthn/register/begin and
// returns the X-WebAuthn-Session token from the response headers. It does not
// consume or inspect the challenge body. Callers must hold a board cookie.
func beginRegistrationSession(t *testing.T, srv *server.Server, token string) string {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL()+"/api/board/webauthn/register/begin",
		strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "staypoint_board", Value: srv.BoardToken()})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("register/begin: %v", err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	sessionToken := resp.Header.Get("X-WebAuthn-Session")
	if sessionToken == "" {
		t.Fatal("register/begin: X-WebAuthn-Session header missing")
	}
	return sessionToken
}

// (a) Fail-closed: with no passkey registered, the board cookie alone must not
// authorize any Board action. An agent that reads board_token and mints a cookie
// still cannot approve anything.
func TestBoardAction_NoPasskeyRegistered_Returns403(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)

	for _, ep := range boardActionEndpoints {
		status, code, raw := doBoard(t, srv, token, boardReq{method: ep.method, path: ep.path, body: ep.body, cookie: true})
		if status != http.StatusForbidden || code != "board_passkey_enrollment_required" {
			t.Errorf("%s %s, cookie only, no passkey registered: want 403 board_passkey_enrollment_required, got %d %s",
				ep.method, ep.path, status, strings.TrimSpace(raw))
		}
	}
}

// (b) First registration needs the 6-digit pairing code from the macOS notification.
// A cookie holder driving a headless browser with a virtual authenticator cannot see it.
func TestPasskeyRegister_NoPairingCode_Returns403(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	silencePairingNotifier(srv)

	credential := `{"id":"Y3JlZC0x","rawId":"Y3JlZC0x","type":"public-key","response":{"clientDataJSON":"e30","attestationObject":"o2NmbXRkbm9uZQ"}}`
	cases := []struct{ name, body string }{
		{"no code field", `{"credential":` + credential + `}`},
		{"empty code", `{"code":"","credential":` + credential + `}`},
		{"wrong code", `{"code":"000000","credential":` + credential + `}`},
	}
	for _, tc := range cases {
		// Each register/finish attempt needs a fresh session from register/begin
		// (sessions are single-use). The pairing code check is reached only after
		// the session is validated, so a valid session token is required.
		sessionToken := beginRegistrationSession(t, srv, token)
		status, code, raw := doBoard(t, srv, token, boardReq{
			method:  "POST",
			path:    "/api/board/webauthn/register/finish",
			body:    tc.body,
			cookie:  true,
			session: sessionToken,
		})
		if status != http.StatusForbidden || code != "board_passkey_pairing_required" {
			t.Errorf("register/finish %s: want 403 board_passkey_pairing_required, got %d %s", tc.name, status, strings.TrimSpace(raw))
		}
	}

	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM board_webauthn_credentials`).Scan(&n); err == nil && n != 0 {
		t.Errorf("register/finish without pairing code stored %d credential(s), want 0", n)
	}
}

// (c) With a passkey already registered, starting another registration needs an
// assertion from an existing passkey, so a cookie holder cannot add a rogue one.
func TestPasskeyRegister_SecondWithoutAssertion_Returns403(t *testing.T) {
	database := setupTestDB(t)
	seedBoardWebAuthnCredential(t, database)
	srv, token := startTestServer(t, database)
	silencePairingNotifier(srv)

	status, code, raw := doBoard(t, srv, token, boardReq{method: "POST", path: "/api/board/webauthn/register/begin", body: `{}`, cookie: true})
	if status != http.StatusForbidden || code != "board_passkey_assertion_required" {
		t.Fatalf("register/begin with existing passkey, no assertion: want 403 board_passkey_assertion_required, got %d %s",
			status, strings.TrimSpace(raw))
	}
}

// (d) With a passkey registered, the board cookie without an assertion is rejected.
func TestBoardAction_PasskeyRegistered_NoAssertion_Returns403(t *testing.T) {
	database := setupTestDB(t)
	seedBoardWebAuthnCredential(t, database)
	srv, token := startTestServer(t, database)

	for _, ep := range boardActionEndpoints {
		status, _, raw := doBoard(t, srv, token, boardReq{method: ep.method, path: ep.path, body: ep.body, cookie: true})
		if status != http.StatusForbidden {
			t.Errorf("%s %s, passkey registered, cookie without assertion: want 403, got %d %s",
				ep.method, ep.path, status, strings.TrimSpace(raw))
		}
	}
}

// TestBoardActionRequiresWebAuthn covers the assertion path through the verifier seam.
func TestBoardActionRequiresWebAuthn(t *testing.T) {
	const endpoint = "/api/settings/ship-review"
	const body = `{"ship_review":true}`

	newServer := func(t *testing.T) (*server.Server, string, webAuthnVerifierSetter) {
		t.Helper()
		database := setupTestDB(t)
		seedBoardWebAuthnCredential(t, database)
		srv, token := startTestServer(t, database)
		setter, ok := any(srv).(webAuthnVerifierSetter)
		if !ok {
			t.Fatal("*server.Server must implement SetWebAuthnVerifier(func(*http.Request, string) error) as the test seam for WebAuthn verification")
		}
		return srv, token, setter
	}

	t.Run("credential registered, invalid assertion -> 403", func(t *testing.T) {
		srv, token, setter := newServer(t)
		setter.SetWebAuthnVerifier(func(*http.Request, string) error { return errors.New("signature mismatch") })

		status, _, raw := doBoard(t, srv, token, boardReq{method: "POST", path: endpoint, body: body, cookie: true, assertion: "bogus-assertion"})
		if status != http.StatusForbidden {
			t.Fatalf("board cookie with rejected assertion: want 403, got %d %s", status, raw)
		}
	})

	t.Run("credential registered, mock assertion -> 200", func(t *testing.T) {
		srv, token, setter := newServer(t)
		var got string
		setter.SetWebAuthnVerifier(func(_ *http.Request, assertion string) error { got = assertion; return nil })

		status, _, raw := doBoard(t, srv, token, boardReq{method: "POST", path: endpoint, body: body, cookie: true, assertion: "mock-assertion"})
		if status != http.StatusOK {
			t.Fatalf("board cookie with verified assertion: want 200, got %d %s", status, raw)
		}
		if got != "mock-assertion" {
			t.Fatalf("verifier received assertion %q, want %q", got, "mock-assertion")
		}
	})

	t.Run("credential registered, assertion but no board cookie -> 403", func(t *testing.T) {
		srv, token, setter := newServer(t)
		setter.SetWebAuthnVerifier(func(*http.Request, string) error { return nil })

		status, _, raw := doBoard(t, srv, token, boardReq{method: "POST", path: endpoint, body: body, assertion: "mock-assertion"})
		if status != http.StatusForbidden {
			t.Fatalf("assertion without board cookie: want 403, got %d %s", status, raw)
		}
	})
}

// seedBoardAuditLog creates the board_audit_log table if it doesn't exist (migration 22).
func seedBoardAuditLog(t *testing.T, database *sql.DB) {
	t.Helper()
	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS board_audit_log (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		actor_id   TEXT NOT NULL,
		event_type TEXT NOT NULL,
		payload    TEXT,
		created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
	)`); err != nil {
		t.Fatalf("create board_audit_log: %v", err)
	}
}

// TestBoardAuditLog_WrittenOnBoardAction verifies that a successful Board action
// writes a row to board_audit_log and that the row is readable back.
func TestBoardAuditLog_WrittenOnBoardAction(t *testing.T) {
	database := setupTestDB(t)
	seedBoardAuditLog(t, database)
	seedBoardWebAuthnCredential(t, database)
	srv, token := startTestServer(t, database)

	setter, ok := any(srv).(webAuthnVerifierSetter)
	if !ok {
		t.Fatal("*server.Server must implement SetWebAuthnVerifier")
	}
	setter.SetWebAuthnVerifier(func(*http.Request, string) error { return nil })

	// Before: no audit rows.
	var before int
	_ = database.QueryRow(`SELECT COUNT(*) FROM board_audit_log`).Scan(&before)

	doBoard(t, srv, token, boardReq{
		method:    "POST",
		path:      "/api/settings/ship-review",
		body:      `{"ship_review":true}`,
		cookie:    true,
		assertion: "mock-assertion",
	})

	var after int
	if err := database.QueryRow(`SELECT COUNT(*) FROM board_audit_log`).Scan(&after); err != nil {
		t.Fatalf("count board_audit_log: %v", err)
	}
	if after <= before {
		t.Errorf("expected audit row after Board action, got %d rows (was %d)", after, before)
	}
}

// TestBoardAuditLog_LogBoardEvent_ReadBack verifies that governance.LogBoardEvent
// writes and the row can be read back with the correct fields.
func TestBoardAuditLog_LogBoardEvent_ReadBack(t *testing.T) {
	database := setupTestDB(t)
	seedBoardAuditLog(t, database)

	payload := map[string]string{"action": "test_event", "ip": "127.0.0.1"}
	if err := governance.LogBoardEvent(database, "board", governance.AuditBoardAction, payload); err != nil {
		t.Fatalf("LogBoardEvent: %v", err)
	}

	var id int64
	var actorID, eventType, payloadJSON string
	var createdAt string
	if err := database.QueryRow(
		`SELECT id, actor_id, event_type, payload, created_at FROM board_audit_log ORDER BY id DESC LIMIT 1`,
	).Scan(&id, &actorID, &eventType, &payloadJSON, &createdAt); err != nil {
		t.Fatalf("read board_audit_log row: %v", err)
	}
	if actorID != "board" {
		t.Errorf("actor_id: want %q, got %q", "board", actorID)
	}
	if eventType != governance.AuditBoardAction {
		t.Errorf("event_type: want %q, got %q", governance.AuditBoardAction, eventType)
	}
	if !strings.Contains(payloadJSON, "test_event") {
		t.Errorf("payload JSON missing expected content, got: %s", payloadJSON)
	}
	if createdAt == "" {
		t.Error("created_at is empty")
	}
}

// (e) STA-696: when the pairing code cannot be shown (osascript denied, no GUI
// session), register/begin must fail with the real cause instead of letting the
// UI ask for a code that never arrived. The code itself must not leak into the
// response, and it must not stay valid.
func TestPasskeyRegisterBegin_NotifierError_Surfaced(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)

	// A session from an earlier, successful begin. The failed begin below must
	// not leave a pairing code this session could finish with.
	silencePairingNotifier(srv)
	sessionToken := beginRegistrationSession(t, srv, token)

	const cause = "osascript exit 1: execution error: No user interaction allowed. (-1713)"
	var mu sync.Mutex
	var shown string
	s, ok := any(srv).(pairingNotifierSetter)
	if !ok {
		t.Fatal("server has no SetPairingNotifier seam")
	}
	s.SetPairingNotifier(func(code string) error {
		mu.Lock()
		shown = code
		mu.Unlock()
		// Echo the code in the error to prove the handler scrubs it.
		return errors.New(cause + " code=" + code)
	})

	req, _ := http.NewRequest("POST", srv.URL()+"/api/board/webauthn/register/begin", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "staypoint_board", Value: srv.BoardToken()})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("register/begin: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var out struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode != http.StatusInternalServerError || out.Error != "board_pairing_code_undelivered" {
		t.Fatalf("register/begin with failing notifier: want 500 board_pairing_code_undelivered, got %d %s",
			resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if want := "Couldn't show the pairing code: " + cause; !strings.HasPrefix(out.Message, want) {
		t.Errorf("message = %q, want prefix %q", out.Message, want)
	}
	if resp.Header.Get("X-WebAuthn-Session") != "" {
		t.Error("register/begin failed but still issued an X-WebAuthn-Session")
	}

	mu.Lock()
	code := shown
	mu.Unlock()
	if code == "" {
		t.Fatal("notifier was never called")
	}
	if strings.Contains(string(raw), code) {
		t.Errorf("response leaks the pairing code: %s", raw)
	}

	// The undelivered code must not be usable.
	credential := `{"id":"Y3JlZC0x","rawId":"Y3JlZC0x","type":"public-key","response":{"clientDataJSON":"e30","attestationObject":"o2NmbXRkbm9uZQ"}}`
	status, errCode, body := doBoard(t, srv, token, boardReq{
		method:  "POST",
		path:    "/api/board/webauthn/register/finish",
		body:    `{"code":"` + code + `","credential":` + credential + `}`,
		cookie:  true,
		session: sessionToken,
	})
	if status != http.StatusForbidden || errCode != "board_passkey_pairing_required" {
		t.Errorf("register/finish with undelivered code: want 403 board_passkey_pairing_required, got %d %s",
			status, strings.TrimSpace(body))
	}
}
