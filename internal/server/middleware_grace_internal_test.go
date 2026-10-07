package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/db"
)

// The grace window ends at its fixed deadline; using it never extends it
// (STA-868).
func TestGraceWindowExpiresAndNeverExtends(t *testing.T) {
	store, err := db.Open(t.TempDir() + "/staypoint.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.DB().Exec(`INSERT INTO board_webauthn_credentials (credential_id, public_key) VALUES ('c', x'00')`); err != nil {
		t.Fatal(err)
	}
	sm := NewSecurityMiddlewareWithBoardToken("tok", "board", 0, false)
	sm.SetDB(store.DB())
	sm.setWebAuthnVerifier(func(*http.Request, string) error { return nil })
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	sm.now = func() time.Time { return now }
	h := sm.WrapBoardGateAction(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))

	call := func(assertion string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/x", nil)
		r.AddCookie(&http.Cookie{Name: boardCookieName, Value: "board"})
		for _, c := range cookies {
			r.AddCookie(c)
		}
		if assertion != "" {
			r.Header.Set("X-WebAuthn-Assertion", assertion)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := call("ok")
	var grace *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == boardGraceCookieName {
			grace = c
		}
	}
	if w.Code != 200 || grace == nil {
		t.Fatalf("assertion: %d cookie %v", w.Code, grace)
	}
	now = now.Add(110 * time.Second)
	if w := call("", grace); w.Code != 200 {
		t.Fatalf("inside window: %d", w.Code)
	}
	now = now.Add(9 * time.Second) // 1:59 after the assertion
	if w := call("", grace); w.Code != 200 {
		t.Fatalf("still inside window: %d", w.Code)
	}
	now = now.Add(2 * time.Second) // 2:01: uses above did not extend it
	if w := call("", grace); w.Code != http.StatusForbidden {
		t.Fatalf("after window: want 403, got %d", w.Code)
	}
	// WrapBoardAction never accepts grace.
	plain := sm.WrapBoardAction(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	r := httptest.NewRequest("POST", "/x", nil)
	r.AddCookie(&http.Cookie{Name: boardCookieName, Value: "board"})
	r.AddCookie(grace)
	rec := httptest.NewRecorder()
	now = now.Add(-30 * time.Second)
	plain.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("plain board action with grace: want 403, got %d", rec.Code)
	}
}
