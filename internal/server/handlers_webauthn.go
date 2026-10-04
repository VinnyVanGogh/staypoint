package server

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
)

// boardWebAuthnUser implements webauthn.User for the single Board principal.
type boardWebAuthnUser struct {
	credentials []webauthn.Credential
}

func (u *boardWebAuthnUser) WebAuthnID() []byte                         { return []byte("staypoint-board") }
func (u *boardWebAuthnUser) WebAuthnName() string                       { return "StayPoint Board" }
func (u *boardWebAuthnUser) WebAuthnDisplayName() string                { return "StayPoint Board" }
func (u *boardWebAuthnUser) WebAuthnCredentials() []webauthn.Credential { return u.credentials }

// WebAuthnHandler manages Board passkey registration and credential lifecycle.
type WebAuthnHandler struct {
	db       *sql.DB
	hub      *EventHub
	wa       *webauthn.WebAuthn
	waInitMu sync.Mutex
	port     int // daemon TCP port; used for fixed RPID origin

	pairingMu   sync.Mutex
	pairingCode string
	pairingExp  time.Time

	sessionMu sync.Mutex
	sessions  map[string]*webauthn.SessionData
}

func NewWebAuthnHandler(db *sql.DB, hub *EventHub) *WebAuthnHandler {
	return &WebAuthnHandler{
		db:       db,
		hub:      hub,
		sessions: make(map[string]*webauthn.SessionData),
	}
}

// SetPort updates the port and resets the WebAuthn instance so initWebAuthn
// will recreate it with the correct origin on the next call.
func (h *WebAuthnHandler) SetPort(port int) {
	h.waInitMu.Lock()
	defer h.waInitMu.Unlock()
	h.port = port
	h.wa = nil // force re-init with correct origin
}

// initWebAuthn returns the webauthn.WebAuthn instance, creating it if needed.
// RPID is always "localhost" (never an IP); origin is http://localhost:<port>.
func (h *WebAuthnHandler) initWebAuthn() (*webauthn.WebAuthn, error) {
	h.waInitMu.Lock()
	defer h.waInitMu.Unlock()
	if h.wa != nil {
		return h.wa, nil
	}
	origin := "http://localhost"
	if h.port > 0 {
		origin = fmt.Sprintf("http://localhost:%d", h.port)
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPDisplayName: "StayPoint Board",
		RPID:          "localhost",
		RPOrigins:     []string{origin},
	})
	if err != nil {
		return nil, err
	}
	h.wa = wa
	return wa, nil
}

// loadUser returns a boardWebAuthnUser populated with all stored credentials.
func (h *WebAuthnHandler) loadUser() *boardWebAuthnUser {
	rows, err := h.db.Query(
		`SELECT credential_id, public_key, sign_count FROM board_webauthn_credentials`,
	)
	if err != nil {
		return &boardWebAuthnUser{}
	}
	defer rows.Close()
	var user boardWebAuthnUser
	for rows.Next() {
		var credID, pubKey []byte
		var signCount uint32
		if err := rows.Scan(&credID, &pubKey, &signCount); err != nil {
			continue
		}
		user.credentials = append(user.credentials, webauthn.Credential{
			ID:        credID,
			PublicKey: pubKey,
			Authenticator: webauthn.Authenticator{
				SignCount: signCount,
			},
		})
	}
	return &user
}

// evictSessions removes expired sessions (caller may hold sessionMu or not; this is
// called only while sessionMu is held).
func (h *WebAuthnHandler) evictSessions() {
	now := time.Now()
	for k, v := range h.sessions {
		if !v.Expires.IsZero() && v.Expires.Before(now) {
			delete(h.sessions, k)
		}
	}
}

// Status handles GET /api/board/webauthn/status
func (h *WebAuthnHandler) Status(w http.ResponseWriter, r *http.Request) {
	var count int
	_ = h.db.QueryRow(`SELECT COUNT(*) FROM board_webauthn_credentials`).Scan(&count)
	writeJSON(w, map[string]bool{"registered": count > 0})
}

// RegisterBegin handles POST /api/board/webauthn/register/begin
func (h *WebAuthnHandler) RegisterBegin(w http.ResponseWriter, r *http.Request) {
	wa, err := h.initWebAuthn()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "webauthn init: "+err.Error())
		return
	}

	var count int
	_ = h.db.QueryRow(`SELECT COUNT(*) FROM board_webauthn_credentials`).Scan(&count)
	if count > 0 {
		assertion := r.Header.Get("X-WebAuthn-Assertion")
		if assertion == "" {
			writeBoardError(w, "board_passkey_assertion_required", "an existing passkey assertion is required to register a second passkey")
			return
		}
		if err := h.VerifyAssertion(r, assertion); err != nil {
			writeBoardError(w, "board_passkey_assertion_invalid", "forbidden: WebAuthn assertion verification failed for second passkey registration")
			return
		}
	}

	// Issue pairing code via macOS notification.
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "pairing code generation failed")
		return
	}
	code := fmt.Sprintf("%06d", n.Int64())
	h.pairingMu.Lock()
	h.pairingCode = code
	h.pairingExp = time.Now().Add(2 * time.Minute)
	h.pairingMu.Unlock()
	go exec.Command("osascript", "-e",
		fmt.Sprintf(`display notification "StayPoint Board registration code: %s" with title "StayPoint Board"`, code),
	).Run() //nolint:errcheck

	user := h.loadUser()
	// Residual risk: attestation statements are requested but not cryptographically
	// verified against a trusted AAGUID list. The macOS notification pairing code is
	// the sole anti-automation barrier during first enrollment.
	options, sessionData, err := wa.BeginRegistration(user,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			AuthenticatorAttachment: protocol.Platform,
			UserVerification:        protocol.VerificationRequired,
			ResidentKey:             protocol.ResidentKeyRequirementPreferred,
		}),
		webauthn.WithConveyancePreference(protocol.PreferIndirectAttestation),
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "begin registration: "+err.Error())
		return
	}

	sessionToken := uuid.New().String()
	h.sessionMu.Lock()
	h.evictSessions()
	h.sessions[sessionToken] = sessionData
	h.sessionMu.Unlock()

	w.Header().Set("X-WebAuthn-Session", sessionToken)
	writeJSON(w, options)
}

// RegisterFinish handles POST /api/board/webauthn/register/finish
func (h *WebAuthnHandler) RegisterFinish(w http.ResponseWriter, r *http.Request) {
	wa, err := h.initWebAuthn()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "webauthn init: "+err.Error())
		return
	}

	// Decode outer envelope FIRST: { "code": "123456", "credential": {...} }
	// Pairing code is the human-in-the-loop gate; check it before any session lookup
	// so a request with the wrong code always gets board_passkey_pairing_required.
	body, _ := io.ReadAll(r.Body)
	var env struct {
		Code       string          `json:"code"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}

	h.pairingMu.Lock()
	validCode := h.pairingCode != "" && env.Code == h.pairingCode && time.Now().Before(h.pairingExp)
	// Consume on any attempt (success or failure) to prevent brute-force.
	h.pairingCode = ""
	h.pairingMu.Unlock()
	if !validCode {
		writeBoardError(w, "board_passkey_pairing_required", "missing, wrong, or expired pairing code")
		return
	}

	sessionToken := r.Header.Get("X-WebAuthn-Session")
	h.sessionMu.Lock()
	sessionData := h.sessions[sessionToken]
	if sessionData != nil {
		delete(h.sessions, sessionToken)
	}
	h.sessionMu.Unlock()
	if sessionData == nil {
		writeBoardError(w, "board_passkey_session_invalid", "missing or expired registration session")
		return
	}

	// Forward the credential JSON to FinishRegistration via a synthetic request body.
	syntheticReq := r.Clone(r.Context())
	syntheticReq.Body = io.NopCloser(bytes.NewReader(env.Credential))

	user := h.loadUser()
	cred, err := wa.FinishRegistration(user, *sessionData, syntheticReq)
	if err != nil {
		writeError(w, http.StatusBadRequest, "finish registration: "+err.Error())
		return
	}

	credIDHex := fmt.Sprintf("%x", cred.ID)
	if _, err := h.db.Exec(
		`INSERT INTO board_webauthn_credentials (id, credential_id, public_key, sign_count, aaguid) VALUES (?, ?, ?, ?, ?)`,
		uuid.New().String(), cred.ID, cred.PublicKey, cred.Authenticator.SignCount,
		fmt.Sprintf("%x", cred.Authenticator.AAGUID),
	); err != nil {
		writeError(w, http.StatusInternalServerError, "store credential: "+err.Error())
		return
	}

	if err := governance.LogBoardEvent(h.db, "board", governance.AuditPasskeyEvent,
		map[string]string{"action": "register", "credential_id": credIDHex,
			"ip": r.RemoteAddr, "user_agent": r.UserAgent()}); err != nil {
		writeError(w, http.StatusInternalServerError, "audit write failed: "+err.Error())
		return
	}
	h.hub.Publish("board_passkey_registered", map[string]string{"credential_id": credIDHex})
	writeJSON(w, map[string]string{"status": "ok"})
}

// Challenge handles POST /api/board/webauthn/challenge — mints a one-time assertion challenge.
func (h *WebAuthnHandler) Challenge(w http.ResponseWriter, r *http.Request) {
	wa, err := h.initWebAuthn()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "webauthn init: "+err.Error())
		return
	}

	user := h.loadUser()
	if len(user.credentials) == 0 {
		writeError(w, http.StatusPreconditionFailed, "no credentials registered")
		return
	}

	options, sessionData, err := wa.BeginLogin(user,
		webauthn.WithUserVerification(protocol.VerificationRequired),
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "begin login: "+err.Error())
		return
	}

	sessionToken := uuid.New().String()
	h.sessionMu.Lock()
	h.evictSessions()
	h.sessions[sessionToken] = sessionData
	h.sessionMu.Unlock()

	w.Header().Set("X-WebAuthn-Session", sessionToken)
	writeJSON(w, options)
}

// VerifyAssertion is the default production verifier wired into WrapBoardAction.
// It calls go-webauthn FinishLogin for full cryptographic assertion verification.
func (h *WebAuthnHandler) VerifyAssertion(r *http.Request, assertion string) error {
	wa, err := h.initWebAuthn()
	if err != nil {
		return fmt.Errorf("webauthn init: %w", err)
	}

	sessionToken := r.Header.Get("X-WebAuthn-Session")
	h.sessionMu.Lock()
	sessionData := h.sessions[sessionToken]
	if sessionData != nil {
		delete(h.sessions, sessionToken)
	}
	h.sessionMu.Unlock()
	if sessionData == nil {
		return fmt.Errorf("missing or expired WebAuthn session (X-WebAuthn-Session header required)")
	}

	user := h.loadUser()
	if len(user.credentials) == 0 {
		return fmt.Errorf("no credentials registered")
	}

	// FinishLogin reads from r.Body; forward the assertion JSON there.
	syntheticReq := r.Clone(r.Context())
	syntheticReq.Body = io.NopCloser(bytes.NewReader([]byte(assertion)))

	cred, err := wa.FinishLogin(user, *sessionData, syntheticReq)
	if err != nil {
		return fmt.Errorf("assertion verification failed: %w", err)
	}

	// Update signCount to defend against cloned authenticators.
	_, _ = h.db.Exec(
		`UPDATE board_webauthn_credentials SET sign_count = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE credential_id = ?`,
		cred.Authenticator.SignCount, cred.ID,
	)
	return nil
}

// ListCredentials handles GET /api/board/webauthn/credentials
func (h *WebAuthnHandler) ListCredentials(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.Query(
		`SELECT id, credential_id, sign_count, aaguid, created_at FROM board_webauthn_credentials ORDER BY created_at`,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	type credRow struct {
		ID           string `json:"id"`
		CredentialID []byte `json:"credential_id"`
		SignCount    uint32 `json:"sign_count"`
		AAGUID       string `json:"aaguid"`
		CreatedAt    string `json:"created_at"`
	}
	var creds []credRow
	for rows.Next() {
		var c credRow
		if err := rows.Scan(&c.ID, &c.CredentialID, &c.SignCount, &c.AAGUID, &c.CreatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		creds = append(creds, c)
	}
	writeJSON(w, map[string]any{"credentials": creds})
}

// DeleteCredential handles DELETE /api/board/webauthn/credentials/{id} (Board action)
func (h *WebAuthnHandler) DeleteCredential(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var credentialID []byte
	if err := h.db.QueryRow(
		`SELECT credential_id FROM board_webauthn_credentials WHERE id = ?`, id,
	).Scan(&credentialID); err != nil {
		writeError(w, http.StatusNotFound, "credential not found")
		return
	}
	if _, err := h.db.Exec(`DELETE FROM board_webauthn_credentials WHERE id = ?`, id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	credIDHex := fmt.Sprintf("%x", credentialID)
	if err := governance.LogBoardEvent(h.db, "board", governance.AuditPasskeyEvent,
		map[string]string{"action": "delete", "credential_id": credIDHex,
			"ip": r.RemoteAddr, "user_agent": r.UserAgent()}); err != nil {
		writeError(w, http.StatusInternalServerError, "audit write failed: "+err.Error())
		return
	}
	h.hub.Publish("board_passkey_deleted", map[string]string{"credential_id": credIDHex})
	writeJSON(w, map[string]string{"status": "ok"})
}
