package server_test

import (
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// webAuthnVerifierSetter is the test seam the STA-583 implementation must expose on
// *server.Server. Tests have no Touch ID hardware, so they swap the cryptographic
// verification for a stub; production keeps the real verifier. It is asserted at
// runtime (not referenced statically) so this file compiles before the feature exists
// and the rest of the package's tests keep running.
type webAuthnVerifierSetter interface {
	SetWebAuthnVerifier(func(r *http.Request, assertion string) error)
}

// seedBoardWebAuthnCredential registers one Board passkey. CREATE IF NOT EXISTS keeps
// this working both before the migration exists and after it lands; the migration must
// provide at least these columns.
func seedBoardWebAuthnCredential(t *testing.T, database *sql.DB) {
	t.Helper()
	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS board_webauthn_credentials (
		id            TEXT PRIMARY KEY,
		credential_id BLOB NOT NULL,
		public_key    BLOB NOT NULL,
		sign_count    INTEGER NOT NULL DEFAULT 0,
		created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatalf("create board_webauthn_credentials: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO board_webauthn_credentials (id, credential_id, public_key) VALUES (?, ?, ?)`,
		"board-passkey-1", []byte("cred-id-1"), []byte("cose-public-key-1"),
	); err != nil {
		t.Fatalf("seed board_webauthn_credentials: %v", err)
	}
}

// TestBoardActionRequiresWebAuthn (STA-583): once a Board passkey is registered, the
// staypoint_board cookie alone must no longer authorize Board actions. A process that
// lifts the cookie (or drives a cookie-jar HTTP session) still gets 403 unless the
// request carries a WebAuthn assertion (X-WebAuthn-Assertion) from the human's Touch ID.
func TestBoardActionRequiresWebAuthn(t *testing.T) {
	const endpoint = "/api/settings/ship-review"
	const body = `{"ship_review":true}`

	boardPost := func(t *testing.T, url, token, boardToken, assertion string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "staypoint_board", Value: boardToken})
		if assertion != "" {
			req.Header.Set("X-WebAuthn-Assertion", assertion)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	t.Run("credential registered, no assertion header -> 403", func(t *testing.T) {
		database := setupTestDB(t)
		seedBoardWebAuthnCredential(t, database)
		srv, token := startTestServer(t, database)

		status, respBody := boardPost(t, srv.URL()+endpoint, token, srv.BoardToken(), "")
		if status != http.StatusForbidden {
			t.Fatalf("board cookie without WebAuthn assertion: want 403, got %d (%s)", status, respBody)
		}
	})

	t.Run("credential registered, invalid assertion -> 403", func(t *testing.T) {
		database := setupTestDB(t)
		seedBoardWebAuthnCredential(t, database)
		srv, token := startTestServer(t, database)

		setter, ok := any(srv).(webAuthnVerifierSetter)
		if !ok {
			t.Fatal("*server.Server must implement SetWebAuthnVerifier(func(*http.Request, string) error) as the test seam for WebAuthn verification")
		}
		setter.SetWebAuthnVerifier(func(_ *http.Request, _ string) error {
			return errors.New("signature mismatch")
		})

		status, respBody := boardPost(t, srv.URL()+endpoint, token, srv.BoardToken(), "bogus-assertion")
		if status != http.StatusForbidden {
			t.Fatalf("board cookie with rejected assertion: want 403, got %d (%s)", status, respBody)
		}
	})

	t.Run("credential registered, mock assertion -> 200", func(t *testing.T) {
		database := setupTestDB(t)
		seedBoardWebAuthnCredential(t, database)
		srv, token := startTestServer(t, database)

		setter, ok := any(srv).(webAuthnVerifierSetter)
		if !ok {
			t.Fatal("*server.Server must implement SetWebAuthnVerifier(func(*http.Request, string) error) as the test seam for WebAuthn verification")
		}
		var gotAssertion string
		setter.SetWebAuthnVerifier(func(_ *http.Request, assertion string) error {
			gotAssertion = assertion
			return nil
		})

		status, respBody := boardPost(t, srv.URL()+endpoint, token, srv.BoardToken(), "mock-assertion")
		if status != http.StatusOK {
			t.Fatalf("board cookie with verified assertion: want 200, got %d (%s)", status, respBody)
		}
		if gotAssertion != "mock-assertion" {
			t.Fatalf("verifier received assertion %q, want %q", gotAssertion, "mock-assertion")
		}
	})

	t.Run("credential registered, assertion but no board cookie -> 403", func(t *testing.T) {
		database := setupTestDB(t)
		seedBoardWebAuthnCredential(t, database)
		srv, token := startTestServer(t, database)

		if setter, ok := any(srv).(webAuthnVerifierSetter); ok {
			setter.SetWebAuthnVerifier(func(*http.Request, string) error { return nil })
		}

		req, _ := http.NewRequest(http.MethodPost, srv.URL()+endpoint, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-WebAuthn-Assertion", "mock-assertion")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("assertion without board cookie: want 403, got %d", resp.StatusCode)
		}
	})

	t.Run("no credentials registered, board cookie alone -> 200", func(t *testing.T) {
		// Before the Board enrolls a passkey, the cookie-only flow must keep working,
		// otherwise the Board is locked out of the action that enrolls it.
		database := setupTestDB(t)
		srv, token := startTestServer(t, database)

		status, respBody := boardPost(t, srv.URL()+endpoint, token, srv.BoardToken(), "")
		if status != http.StatusOK {
			t.Fatalf("board cookie with no registered credentials: want 200, got %d (%s)", status, respBody)
		}
	})
}
