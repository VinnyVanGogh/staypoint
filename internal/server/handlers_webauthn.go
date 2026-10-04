package server

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/google/uuid"
)

// WebAuthnHandler manages Board passkey registration and credential lifecycle.
type WebAuthnHandler struct {
	db  *sql.DB
	hub *EventHub

	pairingMu   sync.Mutex
	pairingCode string
	pairingExp  time.Time

	challengeMu sync.Mutex
	challenges  map[string]time.Time // challenge → expiry
}

func NewWebAuthnHandler(db *sql.DB, hub *EventHub) *WebAuthnHandler {
	return &WebAuthnHandler{db: db, hub: hub, challenges: make(map[string]time.Time)}
}

// Status handles GET /api/board/webauthn/status
func (h *WebAuthnHandler) Status(w http.ResponseWriter, r *http.Request) {
	var count int
	_ = h.db.QueryRow(`SELECT COUNT(*) FROM board_webauthn_credentials`).Scan(&count)
	writeJSON(w, map[string]bool{"registered": count > 0})
}

// RegisterBegin handles POST /api/board/webauthn/register/begin
func (h *WebAuthnHandler) RegisterBegin(w http.ResponseWriter, r *http.Request) {
	var count int
	_ = h.db.QueryRow(`SELECT COUNT(*) FROM board_webauthn_credentials`).Scan(&count)
	if count > 0 {
		assertion := r.Header.Get("X-WebAuthn-Assertion")
		if assertion == "" {
			writeBoardError(w, "board_passkey_assertion_required", "an existing passkey assertion is required to register a second passkey")
			return
		}
	}

	n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
	code := fmt.Sprintf("%06d", n.Int64())

	h.pairingMu.Lock()
	h.pairingCode = code
	h.pairingExp = time.Now().Add(2 * time.Minute)
	h.pairingMu.Unlock()

	// Fire-and-forget macOS notification with the pairing code.
	go exec.Command("osascript", "-e",
		fmt.Sprintf(`display notification "StayPoint Board registration code: %s" with title "StayPoint Board"`, code),
	).Run() //nolint:errcheck

	challenge := h.mintChallenge()
	writeJSON(w, map[string]any{
		"challenge": challenge,
		"rp":        map[string]string{"name": "StayPoint Board"},
		"authenticatorSelection": map[string]string{
			"authenticatorAttachment": "platform",
			"userVerification":        "required",
			"residentKey":             "preferred",
		},
		"attestation": "indirect",
	})
}

// RegisterFinish handles POST /api/board/webauthn/register/finish
func (h *WebAuthnHandler) RegisterFinish(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code       string          `json:"code"`
		Credential json.RawMessage `json:"credential"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}

	h.pairingMu.Lock()
	validCode := h.pairingCode != "" && req.Code == h.pairingCode && time.Now().Before(h.pairingExp)
	if validCode {
		h.pairingCode = "" // consume
	}
	h.pairingMu.Unlock()

	if !validCode {
		writeBoardError(w, "board_passkey_pairing_required", "missing, wrong, or expired pairing code")
		return
	}

	// Extract credential_id from the credential JSON for storage.
	var cred struct {
		ID        string `json:"id"`
		PublicKey string `json:"publicKey"`
		Type      string `json:"type"`
	}
	_ = json.Unmarshal(req.Credential, &cred)
	if cred.ID == "" {
		writeError(w, http.StatusBadRequest, "credential.id required")
		return
	}

	if _, err := h.db.Exec(
		`INSERT INTO board_webauthn_credentials (id, credential_id, public_key) VALUES (?, ?, ?)`,
		uuid.New().String(), cred.ID, []byte(cred.PublicKey),
	); err != nil {
		writeError(w, http.StatusInternalServerError, "store credential: "+err.Error())
		return
	}

	_ = governance.LogEvent(h.db, "board", "board", governance.AuditPasskeyEvent, nil, nil,
		map[string]string{"action": "register", "credential_id": cred.ID})

	h.hub.Publish("board_passkey_registered", map[string]string{"credential_id": cred.ID})
	writeJSON(w, map[string]string{"status": "ok"})
}

// Challenge handles POST /api/board/webauthn/challenge — mints a one-time assertion challenge.
func (h *WebAuthnHandler) Challenge(w http.ResponseWriter, r *http.Request) {
	challenge := h.mintChallenge()
	writeJSON(w, map[string]string{"challenge": challenge})
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
		CredentialID string `json:"credential_id"`
		SignCount     int64  `json:"sign_count"`
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
	var credentialID string
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

	_ = governance.LogEvent(h.db, "board", "board", governance.AuditPasskeyEvent, nil, nil,
		map[string]string{"action": "delete", "credential_id": credentialID,
			"ip": r.RemoteAddr, "user_agent": r.UserAgent()})

	h.hub.Publish("board_passkey_deleted", map[string]string{"credential_id": credentialID})
	writeJSON(w, map[string]string{"status": "ok"})
}

// mintChallenge creates a random 32-byte hex challenge, stores it with a 90-second TTL.
func (h *WebAuthnHandler) mintChallenge() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	ch := fmt.Sprintf("%x", b)

	exp := time.Now().Add(90 * time.Second)
	h.challengeMu.Lock()
	h.challenges[ch] = exp
	// evict expired challenges
	now := time.Now()
	for k, v := range h.challenges {
		if now.After(v) {
			delete(h.challenges, k)
		}
	}
	h.challengeMu.Unlock()
	return ch
}
