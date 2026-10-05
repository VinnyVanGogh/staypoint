package server

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
)

// STA-716: Apple / iCloud Keychain passkeys are backup-eligible (BE=1). The
// stored credential must carry that flag, or go-webauthn rejects every login
// with "Backup Eligible flag inconsistency" and every Board action is a 403.
// Chrome's CDP virtual authenticator defaults to BE=0, so these tests drive a
// software authenticator that can set BE and BS.

const (
	flagUP = 0x01
	flagUV = 0x04
	flagBE = 0x08
	flagBS = 0x10
	flagAT = 0x40
)

var b64 = base64.RawURLEncoding

// softAuthenticator is a minimal ES256 platform authenticator.
type softAuthenticator struct {
	t      *testing.T
	key    *ecdsa.PrivateKey
	credID []byte
	origin string
	rpID   string
	count  uint32
}

func newSoftAuthenticator(t *testing.T) *softAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credID := make([]byte, 16)
	_, _ = rand.Read(credID)
	return &softAuthenticator{t: t, key: key, credID: credID, origin: "http://localhost", rpID: "localhost"}
}

func (a *softAuthenticator) coseKey() []byte {
	a.t.Helper()
	x := a.key.PublicKey.X.FillBytes(make([]byte, 32))
	y := a.key.PublicKey.Y.FillBytes(make([]byte, 32))
	k, err := webauthncbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: x, -3: y})
	if err != nil {
		a.t.Fatal(err)
	}
	return k
}

func (a *softAuthenticator) authData(flags byte, attested bool) []byte {
	rpHash := sha256.Sum256([]byte(a.rpID))
	var buf bytes.Buffer
	buf.Write(rpHash[:])
	buf.WriteByte(flags)
	_ = binary.Write(&buf, binary.BigEndian, a.count)
	if attested {
		buf.Write(make([]byte, 16)) // AAGUID
		_ = binary.Write(&buf, binary.BigEndian, uint16(len(a.credID)))
		buf.Write(a.credID)
		buf.Write(a.coseKey())
	}
	return buf.Bytes()
}

func (a *softAuthenticator) clientData(typ, challenge string) []byte {
	cd, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": a.origin, "crossOrigin": false})
	return cd
}

// create returns the credential JSON the browser would post to register/finish.
func (a *softAuthenticator) create(challenge string, flags byte) json.RawMessage {
	a.t.Helper()
	attObj, err := webauthncbor.Marshal(map[string]any{
		"fmt":      "none",
		"attStmt":  map[string]any{},
		"authData": a.authData(flags|flagAT, true),
	})
	if err != nil {
		a.t.Fatal(err)
	}
	out, _ := json.Marshal(map[string]any{
		"id":    b64.EncodeToString(a.credID),
		"rawId": b64.EncodeToString(a.credID),
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64.EncodeToString(a.clientData("webauthn.create", challenge)),
			"attestationObject": b64.EncodeToString(attObj),
		},
	})
	return out
}

// get returns the assertion JSON the browser would send as X-WebAuthn-Assertion.
func (a *softAuthenticator) get(challenge string, flags byte) string {
	a.t.Helper()
	a.count++
	ad := a.authData(flags, false)
	cd := a.clientData("webauthn.get", challenge)
	cdHash := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, ad...), cdHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		a.t.Fatal(err)
	}
	out, _ := json.Marshal(map[string]any{
		"id":    b64.EncodeToString(a.credID),
		"rawId": b64.EncodeToString(a.credID),
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64.EncodeToString(cd),
			"authenticatorData": b64.EncodeToString(ad),
			"signature":         b64.EncodeToString(sig),
			"userHandle":        b64.EncodeToString([]byte("staypoint-board")),
		},
	})
	return string(out)
}

func newFlagsTestHandler(t *testing.T) (*WebAuthnHandler, *sql.DB) {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "staypoint.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	h := NewWebAuthnHandler(store.DB(), NewEventHub(16, 16))
	h.SetPairingNotifier(func(string) error { return nil })
	return h, store.DB()
}

// optionsChallenge pulls the base64url challenge and session token out of a
// begin/challenge response.
func optionsChallenge(t *testing.T, w *httptest.ResponseRecorder) (challenge, session string) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("options: status %d: %s", w.Code, w.Body.String())
	}
	var opts struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &opts); err != nil {
		t.Fatalf("decode options: %v", err)
	}
	return opts.PublicKey.Challenge, w.Header().Get("X-WebAuthn-Session")
}

func register(t *testing.T, h *WebAuthnHandler, a *softAuthenticator, flags byte) {
	t.Helper()
	w := httptest.NewRecorder()
	h.RegisterBegin(w, httptest.NewRequest("POST", "/api/board/webauthn/register/begin", nil))
	challenge, session := optionsChallenge(t, w)

	body, _ := json.Marshal(map[string]any{"code": h.LastPairingCode(), "credential": a.create(challenge, flags)})
	req := httptest.NewRequest("POST", "/api/board/webauthn/register/finish", bytes.NewReader(body))
	req.Header.Set("X-WebAuthn-Session", session)
	w = httptest.NewRecorder()
	h.RegisterFinish(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("register/finish: status %d: %s", w.Code, w.Body.String())
	}
}

func login(t *testing.T, h *WebAuthnHandler, a *softAuthenticator, flags byte) error {
	t.Helper()
	w := httptest.NewRecorder()
	h.Challenge(w, httptest.NewRequest("POST", "/api/board/webauthn/challenge", nil))
	challenge, session := optionsChallenge(t, w)
	req := httptest.NewRequest("POST", "/api/tasks/x/ship-review/approve", nil)
	req.Header.Set("X-WebAuthn-Session", session)
	return h.VerifyAssertion(req, a.get(challenge, flags))
}

type storedFlags struct {
	UP, UV, BE, BS sql.NullBool
	SignCount      uint32
}

func readFlags(t *testing.T, database *sql.DB, credID []byte) storedFlags {
	t.Helper()
	var f storedFlags
	if err := database.QueryRow(`SELECT flags_user_present, flags_user_verified, flags_backup_eligible, flags_backup_state, sign_count
		FROM board_webauthn_credentials WHERE credential_id = ?`, credID).Scan(&f.UP, &f.UV, &f.BE, &f.BS, &f.SignCount); err != nil {
		t.Fatalf("read flags: %v", err)
	}
	return f
}

func TestWebAuthn_BackupEligiblePasskey_RegisterThenLogin(t *testing.T) {
	h, database := newFlagsTestHandler(t)
	a := newSoftAuthenticator(t)
	register(t, h, a, flagUP|flagUV|flagBE|flagBS)

	f := readFlags(t, database, a.credID)
	if !f.BE.Valid || !f.BE.Bool || !f.BS.Valid || !f.BS.Bool || !f.UP.Bool || !f.UV.Bool {
		t.Fatalf("registration flags not stored: %+v", f)
	}

	if err := login(t, h, a, flagUP|flagUV|flagBE|flagBS); err != nil {
		t.Fatalf("login with BE=1/BS=1 passkey: %v", err)
	}
	if got := readFlags(t, database, a.credID).SignCount; got != a.count {
		t.Errorf("sign_count = %d, want %d", got, a.count)
	}
}

func TestWebAuthn_NonBackupEligiblePasskey_StillWorks(t *testing.T) {
	h, _ := newFlagsTestHandler(t)
	a := newSoftAuthenticator(t)
	register(t, h, a, flagUP|flagUV)
	if err := login(t, h, a, flagUP|flagUV); err != nil {
		t.Fatalf("login with BE=0 passkey: %v", err)
	}
}

func TestWebAuthn_BackupStateUpdatedFromAssertion(t *testing.T) {
	h, database := newFlagsTestHandler(t)
	a := newSoftAuthenticator(t)
	register(t, h, a, flagUP|flagUV|flagBE)
	if f := readFlags(t, database, a.credID); f.BS.Bool {
		t.Fatalf("BS stored true at registration: %+v", f)
	}
	if err := login(t, h, a, flagUP|flagUV|flagBE|flagBS); err != nil {
		t.Fatalf("login: %v", err)
	}
	if f := readFlags(t, database, a.credID); !f.BS.Valid || !f.BS.Bool {
		t.Errorf("flags_backup_state not updated from assertion: %+v", f)
	}
}

// A passkey enrolled before the flag columns existed has NULL flags. The first
// valid assertion adopts its BE/BS flags (trust on first use); after that BE is
// enforced like any other credential.
func TestWebAuthn_LegacyCredentialNullFlags_AdoptsOnFirstAssertion(t *testing.T) {
	h, database := newFlagsTestHandler(t)
	a := newSoftAuthenticator(t)
	if _, err := database.Exec(
		`INSERT INTO board_webauthn_credentials (id, credential_id, public_key, sign_count, aaguid) VALUES (?, ?, ?, 0, '')`,
		"legacy-row", a.credID, a.coseKey(),
	); err != nil {
		t.Fatalf("seed legacy credential: %v", err)
	}
	if f := readFlags(t, database, a.credID); f.BE.Valid {
		t.Fatalf("legacy row should have NULL flags, got %+v", f)
	}

	if err := login(t, h, a, flagUP|flagUV|flagBE|flagBS); err != nil {
		t.Fatalf("first login on legacy credential: %v", err)
	}
	f := readFlags(t, database, a.credID)
	if !f.BE.Valid || !f.BE.Bool || !f.BS.Valid || !f.BS.Bool {
		t.Fatalf("flags not persisted after first assertion: %+v", f)
	}

	if err := login(t, h, a, flagUP|flagUV|flagBE|flagBS); err != nil {
		t.Fatalf("second login on adopted credential: %v", err)
	}
	if err := login(t, h, a, flagUP|flagUV); err == nil {
		t.Fatal("assertion with BE=0 accepted after BE=1 was adopted; BE must be enforced once known")
	}
}

// A bad signature on a legacy row must not adopt anything.
func TestWebAuthn_LegacyCredential_InvalidAssertionDoesNotAdoptFlags(t *testing.T) {
	h, database := newFlagsTestHandler(t)
	a := newSoftAuthenticator(t)
	if _, err := database.Exec(
		`INSERT INTO board_webauthn_credentials (id, credential_id, public_key, sign_count, aaguid) VALUES (?, ?, ?, 0, '')`,
		"legacy-row", a.credID, a.coseKey(),
	); err != nil {
		t.Fatalf("seed legacy credential: %v", err)
	}
	impostor := newSoftAuthenticator(t)
	impostor.credID = a.credID // same ID, different key
	if err := login(t, h, impostor, flagUP|flagUV|flagBE|flagBS); err == nil {
		t.Fatal("assertion signed by the wrong key was accepted")
	}
	if f := readFlags(t, database, a.credID); f.BE.Valid {
		t.Fatalf("flags adopted from a failed assertion: %+v", f)
	}
}
