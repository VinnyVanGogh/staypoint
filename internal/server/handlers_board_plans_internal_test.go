package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/go-webauthn/webauthn/webauthn"
)

func planWebAuthn(t *testing.T) *WebAuthnHandler {
	t.Helper()
	store, err := db.Open(t.TempDir() + "/staypoint.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err := store.DB().Exec(`INSERT INTO board_webauthn_credentials (credential_id, public_key) VALUES ('c', x'00')`); err != nil {
		t.Fatal(err)
	}
	h := NewWebAuthnHandler(store.DB(), nil)
	h.validateLogin = func(webauthn.SessionData, string) (string, error) { return "cred", nil }
	return h
}

// A signed selection is usable for 10 minutes at most (task-e1b24d66).
func TestPlanChallenge_ExpiresAfterTTL(t *testing.T) {
	h := planWebAuthn(t)
	sel := []byte("0123456789abcdef0123456789abcdef")
	_, token, err := h.PlanChallenge("plan-1", sel)
	if err != nil {
		t.Fatal(err)
	}
	h.sessionMu.Lock()
	sd := h.sessions[token]
	if until := time.Until(sd.Expires); until > planChallengeTTL || until <= 0 {
		h.sessionMu.Unlock()
		t.Fatalf("challenge expires in %v; want within %v", until, planChallengeTTL)
	}
	sd.Expires = time.Now().Add(-time.Second)
	h.sessionMu.Unlock()
	if _, err := h.VerifyPlanAssertion(token, "a", "plan-1", sel); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired selection: want expired error, got %v", err)
	}
}

// A plan challenge never authorizes an ordinary Board action, and an
// ordinary challenge never signs a plan.
func TestPlanChallenge_NotInterchangeable(t *testing.T) {
	h := planWebAuthn(t)
	sel := []byte("0123456789abcdef0123456789abcdef")
	_, token, err := h.PlanChallenge("plan-1", sel)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/x", nil)
	r.Header.Set("X-WebAuthn-Session", token)
	if err := h.VerifyAssertion(r, "a"); err == nil {
		t.Fatal("plan challenge accepted for an ordinary Board action")
	}

	w := httptest.NewRecorder()
	h.Challenge(w, httptest.NewRequest("POST", "/api/board/webauthn/challenge", nil))
	plain := w.Header().Get("X-WebAuthn-Session")
	if plain == "" {
		t.Fatalf("ordinary challenge failed: %d %s", w.Code, w.Body.String())
	}
	if _, err := h.VerifyPlanAssertion(plain, "a", "plan-1", sel); err == nil {
		t.Fatal("ordinary challenge accepted for a plan")
	}
}
