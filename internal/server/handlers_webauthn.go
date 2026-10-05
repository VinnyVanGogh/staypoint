package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/osascript"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
)

// boardWebAuthnUser implements webauthn.User for the single Board principal.
type boardWebAuthnUser struct {
	credentials []webauthn.Credential
	// flagsUnknown holds the IDs (as strings) of credentials enrolled before
	// their authenticator flags were stored (STA-716).
	flagsUnknown map[string]bool
}

// adoptLegacyFlags gives a credential with unknown flags the flags of the
// assertion presented for it, so go-webauthn's BackupEligible consistency check
// compares the assertion with itself on first use. The signature is still
// verified; the caller persists the flags only if it is. Reports whether
// anything was adopted.
func (u *boardWebAuthnUser) adoptLegacyFlags(credID []byte, flags protocol.AuthenticatorFlags) bool {
	if !u.flagsUnknown[string(credID)] {
		return false
	}
	for i := range u.credentials {
		if bytes.Equal(u.credentials[i].ID, credID) {
			u.credentials[i].Flags = webauthn.NewCredentialFlags(flags)
			return true
		}
	}
	return false
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

	pairingMu       sync.Mutex
	pairingCode     string
	pairingExp      time.Time
	lastPairingCode string // kept for test-only endpoint; never cleared after use

	pairingNotifierMu sync.RWMutex
	pairingNotifier   func(code string) error // nil means showPairingCode

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

// SetPairingNotifier replaces the macOS pairing-code dialog with a custom function.
// When fn is non-nil, it is called instead of showPairingCode. Used in tests and
// for the Playwright e2e suite (via the test-only last-pairing-code endpoint).
func (h *WebAuthnHandler) SetPairingNotifier(fn func(code string) error) {
	h.pairingNotifierMu.Lock()
	defer h.pairingNotifierMu.Unlock()
	h.pairingNotifier = fn
}

// LastPairingCode returns the most recently generated pairing code (cleared on
// expiry, but kept across successful RegisterFinish for test retrieval).
// Must only be exposed via a TestMode-gated endpoint.
func (h *WebAuthnHandler) LastPairingCode() string {
	h.pairingMu.Lock()
	defer h.pairingMu.Unlock()
	return h.lastPairingCode
}

// TestLastPairingCode handles GET /api/board/webauthn/test/last-pairing-code.
// Only registered when server.Options.TestMode is true.
func (h *WebAuthnHandler) TestLastPairingCode(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{"code": h.LastPairingCode()})
}

// TestSetPairingNotifierError handles PUT /api/board/webauthn/test/pairing-notifier.
// Only registered when server.Options.TestMode is true. A non-empty "error" makes
// every later pairing notifier call fail with that text, as a denied osascript
// would; an empty one restores a notifier that succeeds without showing anything.
func (h *WebAuthnHandler) TestSetPairingNotifierError(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Error == "" {
		h.SetPairingNotifier(func(string) error { return nil })
	} else {
		msg := body.Error
		h.SetPairingNotifier(func(string) error { return errors.New(msg) })
	}
	writeJSON(w, map[string]string{"ok": "set"})
}

// pairingDialogGrace is how long register/begin waits for the pairing dialog to
// fail before treating it as shown. A modal dialog keeps osascript running
// until the Board clicks OK; a denied permission or missing GUI session makes it
// exit almost at once.
const pairingDialogGrace = 1500 * time.Millisecond

// showPairingCode is the default pairing notifier. The code goes in a modal
// dialog, which is visible regardless of notification settings and which
// agents cannot read, with a best-effort notification as a second channel.
// Only a dialog failure is returned. A notification that exits non-zero is
// logged, but macOS can also drop one silently with exit 0, so the dialog is
// the delivery channel. Neither path logs the code itself.
func showPairingCode(code string) error {
	text := "StayPoint Board pairing code: " + code
	dialog := fmt.Sprintf(`display dialog %s with title "StayPoint Board" buttons {"OK"} default button "OK" giving up after 120`,
		osascript.Quote(text))
	err := osascript.Start(dialog, pairingDialogGrace, func(err error) {
		slog.Warn("pairing code dialog failed", slog.String("error", redactPairingCode(err.Error(), code)))
	})
	if err != nil {
		return err
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		notification := fmt.Sprintf(`display notification %s with title "StayPoint Board"`, osascript.Quote(text))
		if err := osascript.Run(ctx, notification); err != nil {
			slog.Warn("pairing code notification failed", slog.String("error", redactPairingCode(err.Error(), code)))
		}
	}()
	return nil
}

// redactPairingCode keeps the pairing code out of logs and API errors, in case
// osascript echoes part of the script in its stderr.
func redactPairingCode(s, code string) string {
	if code == "" {
		return s
	}
	return strings.ReplaceAll(s, code, "******")
}

// TestClearCredentials handles DELETE /api/board/webauthn/test/clear-credentials.
// Only registered when server.Options.TestMode is true. Wipes all stored passkeys
// so that the Playwright boardPage fixture can re-enroll on each test without
// hitting the "assertion required to add a second passkey" gate.
func (h *WebAuthnHandler) TestClearCredentials(w http.ResponseWriter, _ *http.Request) {
	if _, err := h.db.Exec(`DELETE FROM board_webauthn_credentials`); err != nil {
		writeError(w, http.StatusInternalServerError, "clear credentials: "+err.Error())
		return
	}
	writeJSON(w, map[string]string{"ok": "cleared"})
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
		`SELECT credential_id, public_key, sign_count,
			flags_user_present, flags_user_verified, flags_backup_eligible, flags_backup_state,
			attestation_type, transports
		FROM board_webauthn_credentials`,
	)
	if err != nil {
		slog.Warn("load board passkeys", slog.String("error", err.Error()))
		return &boardWebAuthnUser{}
	}
	defer rows.Close()
	user := boardWebAuthnUser{flagsUnknown: map[string]bool{}}
	for rows.Next() {
		var credID, pubKey []byte
		var signCount uint32
		var up, uv, be, bs sql.NullBool
		var attType, transportsJSON string
		if err := rows.Scan(&credID, &pubKey, &signCount, &up, &uv, &be, &bs, &attType, &transportsJSON); err != nil {
			slog.Warn("load board passkey row", slog.String("error", err.Error()))
			continue
		}
		var transports []protocol.AuthenticatorTransport
		_ = json.Unmarshal([]byte(transportsJSON), &transports)
		cred := webauthn.Credential{
			ID:              credID,
			PublicKey:       pubKey,
			AttestationType: attType,
			Transport:       transports,
			Authenticator: webauthn.Authenticator{
				SignCount: signCount,
			},
		}
		if be.Valid {
			cred.Flags = webauthn.NewCredentialFlags(authenticatorFlags(up.Bool, uv.Bool, be.Bool, bs.Bool))
		} else {
			user.flagsUnknown[string(credID)] = true
		}
		user.credentials = append(user.credentials, cred)
	}
	return &user
}

// authenticatorFlags packs stored flag columns back into the protocol octet.
func authenticatorFlags(up, uv, be, bs bool) protocol.AuthenticatorFlags {
	var f protocol.AuthenticatorFlags
	if up {
		f |= protocol.FlagUserPresent
	}
	if uv {
		f |= protocol.FlagUserVerified
	}
	if be {
		f |= protocol.FlagBackupEligible
	}
	if bs {
		f |= protocol.FlagBackupState
	}
	return f
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

	// Issue pairing code via the macOS dialog (or test notifier).
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "pairing code generation failed")
		return
	}
	code := fmt.Sprintf("%06d", n.Int64())
	h.pairingMu.Lock()
	h.pairingCode = code
	h.lastPairingCode = code
	h.pairingExp = time.Now().Add(2 * time.Minute)
	h.pairingMu.Unlock()

	h.pairingNotifierMu.RLock()
	notifier := h.pairingNotifier
	h.pairingNotifierMu.RUnlock()
	if notifier == nil {
		notifier = showPairingCode
	}
	if err := notifier(code); err != nil {
		// The Board never saw this code, so it must not stay valid, and the UI
		// must say why instead of asking for it.
		h.pairingMu.Lock()
		if h.pairingCode == code {
			h.pairingCode = ""
		}
		h.pairingMu.Unlock()
		msg := "Couldn't show the pairing code: " + redactPairingCode(err.Error(), code)
		slog.Warn("register/begin: " + msg)
		writeErrorJSON(w, http.StatusInternalServerError, map[string]any{
			"error":   "board_pairing_code_undelivered",
			"message": msg,
		})
		return
	}

	user := h.loadUser()
	// Residual risk: attestation statements are requested but not cryptographically
	// verified against a trusted AAGUID list. The macOS dialog pairing code is
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
	// Consume the pairing code on every attempt (success or failure) to prevent brute-force.
	// Trade-off: a Board-session holder can send a request with the correct code but an
	// intentionally invalid X-WebAuthn-Session header, burning the code and forcing a new
	// RegisterBegin cycle. This is acceptable — the attacker must already hold the Board
	// session cookie (a strong first factor) and cannot register a rogue credential, only
	// cause the operator to repeat the pairing flow.
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
	transports, _ := json.Marshal(cred.Transport)
	if cred.Transport == nil {
		transports = []byte("[]")
	}
	if _, err := h.db.Exec(
		`INSERT INTO board_webauthn_credentials (id, credential_id, public_key, sign_count, aaguid,
			flags_user_present, flags_user_verified, flags_backup_eligible, flags_backup_state,
			attestation_type, transports)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.New().String(), cred.ID, cred.PublicKey, cred.Authenticator.SignCount,
		fmt.Sprintf("%x", cred.Authenticator.AAGUID),
		cred.Flags.UserPresent, cred.Flags.UserVerified, cred.Flags.BackupEligible, cred.Flags.BackupState,
		cred.AttestationType, string(transports),
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

	parsed, err := protocol.ParseCredentialRequestResponseBytes([]byte(assertion))
	if err != nil {
		return fmt.Errorf("assertion parse failed: %w", err)
	}
	adopted := user.adoptLegacyFlags(parsed.RawID, parsed.Response.AuthenticatorData.Flags)

	cred, err := wa.ValidateLogin(user, *sessionData, parsed)
	if err != nil {
		return fmt.Errorf("assertion verification failed: %w", err)
	}

	// Persist signCount (cloned-authenticator defence) and the flags as of this
	// assertion: backup state can change, and a legacy row gets its first flags.
	if _, err := h.db.Exec(
		`UPDATE board_webauthn_credentials SET sign_count = ?,
			flags_user_present = ?, flags_user_verified = ?, flags_backup_eligible = ?, flags_backup_state = ?,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE credential_id = ?`,
		cred.Authenticator.SignCount,
		cred.Flags.UserPresent, cred.Flags.UserVerified, cred.Flags.BackupEligible, cred.Flags.BackupState,
		cred.ID,
	); err != nil {
		slog.Warn("persist board passkey state", slog.String("credential_id", fmt.Sprintf("%x", cred.ID)), slog.String("error", err.Error()))
	} else if adopted {
		slog.Info("board passkey flags recorded on first use",
			slog.String("credential_id", fmt.Sprintf("%x", cred.ID)),
			slog.Bool("backup_eligible", cred.Flags.BackupEligible),
			slog.Bool("backup_state", cred.Flags.BackupState))
	}
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
