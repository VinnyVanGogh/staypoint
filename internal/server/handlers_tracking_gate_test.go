package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/security"
	"github.com/VinnyVanGogh/staypoint/internal/trackgate"
)

// STA-854: an agent holding only the session token can read the tracking
// gate but cannot switch it off; the Board (cookie + passkey) can.
func TestTrackingGateSettings_BoardOnlyToggle(t *testing.T) {
	database := setupTestDB(t)
	seedBoardWebAuthnCredential(t, database)
	srv, token := startTestServer(t, database)

	// Agent: session token only.
	status, _, raw := doBoard(t, srv, token, boardReq{method: "POST", path: "/api/settings/tracking-gate",
		body: `{"company":"Managed Solution","enabled":false}`})
	if status != http.StatusForbidden {
		t.Fatalf("agent toggle: want 403, got %d %s", status, raw)
	}
	if on, _ := trackgate.Enabled(database, trackgate.CompanyManagedSolution); !on {
		t.Fatal("agent request switched the gate off")
	}

	// Defaults are visible to the session token.
	status, _, raw = doBoard(t, srv, token, boardReq{method: "GET", path: "/api/settings/tracking-gate"})
	if status != http.StatusOK {
		t.Fatalf("GET: %d %s", status, raw)
	}
	var got struct {
		Companies []struct {
			Company string `json:"company"`
			Enabled bool   `json:"enabled"`
			Source  string `json:"source"`
		} `json:"companies"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"Managed Solution": true, "Personal": false}
	for _, c := range got.Companies {
		if w, ok := want[c.Company]; ok && (c.Enabled != w || c.Source != "default") {
			t.Errorf("%s = %v (%s), want %v default", c.Company, c.Enabled, c.Source, w)
		}
	}

	// Board with a passkey assertion.
	setter, ok := any(srv).(webAuthnVerifierSetter)
	if !ok {
		t.Fatal("*server.Server must implement SetWebAuthnVerifier")
	}
	setter.SetWebAuthnVerifier(func(*http.Request, string) error { return nil })
	status, _, raw = doBoard(t, srv, token, boardReq{method: "POST", path: "/api/settings/tracking-gate",
		body: `{"company":"Managed Solution","enabled":false}`, cookie: true, assertion: "ok"})
	if status != http.StatusOK {
		t.Fatalf("Board toggle: %d %s", status, raw)
	}
	if on, _ := trackgate.Enabled(database, trackgate.CompanyManagedSolution); on {
		t.Fatal("Board toggle did not switch the gate off")
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM board_audit_log WHERE payload LIKE '%update_tracking_gate%'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("board audit rows = %d (err %v), want 1", n, err)
	}
}

// The override only exists after the Board approves the request through the
// passkey-gated decide endpoint; an agent cannot approve its own request.
func TestTrackingOverride_ApprovalIsBoardOnly(t *testing.T) {
	database := setupTestDB(t)
	seedBoardWebAuthnCredential(t, database)
	srv, token := startTestServer(t, database)

	body := `{"cmdline":` + jsonString(trackgate.OverrideCmdline("Managed Solution", 15)) +
		`,"reasons":["test"],"run_id":"` + trackgate.OverrideRunID + `"}`
	status, _, raw := doBoard(t, srv, token, boardReq{method: "POST", path: "/api/security/gate-requests", body: body})
	if status != http.StatusCreated {
		t.Fatalf("create request: %d %s", status, raw)
	}
	var gr security.GateRequest
	if err := json.Unmarshal([]byte(raw), &gr); err != nil || gr.ID == "" {
		t.Fatalf("decode request: %v %s", err, raw)
	}

	// Agent tries to approve with the session token.
	status, _, _ = doBoard(t, srv, token, boardReq{method: "POST",
		path: "/api/security/gate-requests/" + gr.ID + "/decide", body: `{"decision":"approved"}`})
	if status != http.StatusForbidden {
		t.Fatalf("agent approve: want 403, got %d", status)
	}
	if ov, _ := trackgate.ActiveOverride(database, "Managed Solution", time.Now()); ov != nil {
		t.Fatal("override active without Board approval")
	}

	setter := any(srv).(webAuthnVerifierSetter)
	setter.SetWebAuthnVerifier(func(*http.Request, string) error { return nil })
	status, _, raw = doBoard(t, srv, token, boardReq{method: "POST",
		path: "/api/security/gate-requests/" + gr.ID + "/decide", body: `{"decision":"approved"}`, cookie: true, assertion: "ok"})
	if status != http.StatusOK {
		t.Fatalf("Board approve: %d %s", status, raw)
	}
	ov, err := trackgate.ActiveOverride(database, "Managed Solution", time.Now())
	if err != nil || ov == nil {
		t.Fatalf("override not active after approval (err %v)", err)
	}
	if left := time.Until(ov.ExpiresAt); left <= 14*time.Minute || left > 15*time.Minute {
		t.Errorf("override expires in %v, want ~15m", left)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return strings.TrimSpace(string(b))
}
