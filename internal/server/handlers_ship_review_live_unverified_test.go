//go:build !windows

package server_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

// STA-799: LiveGateConfig gates start-dev when it cannot verify that the task
// repo is not a live_credentials project (STA-767), but the card only reported
// live_credentials. A non-live task next to a live row it cannot stat got no
// banner and no confirm, and start-dev refused it with a message about a
// live_credentials dev server. The card now says the dev server is gated and
// why, and the refusals name the unverified path.

// lockedLiveRow saves a live_credentials row for a repo inside a chmod 000
// directory, so stat on it fails with EACCES (not ENOENT) and the task repo
// cannot be ruled out as that repo.
func lockedLiveRow(t *testing.T, e *liveShipEnv) {
	t.Helper()
	locked := filepath.Join(t.TempDir(), "locked")
	live := filepath.Join(locked, "live")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := shipreview.UpsertProjectDevConfig(e.db, &shipreview.ProjectDevConfig{RepoPath: live, LiveCredentials: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if _, err := os.Stat(live); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Skipf("stat under a chmod 000 dir did not fail with a permission error (running as root?): %v", err)
	}
}

func getCardJSON(t *testing.T, e *liveShipEnv) map[string]any {
	t.Helper()
	resp, rb := shipDoReq(t, e.client, e.token, "GET", e.cardURL(), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET card: %d %s", resp.StatusCode, rb)
	}
	var card map[string]any
	if err := json.Unmarshal(rb, &card); err != nil {
		t.Fatalf("GET card: %v: %s", err, rb)
	}
	return card
}

func errorBody(t *testing.T, rb []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rb, &m); err != nil {
		t.Fatalf("error body: %v: %s", err, rb)
	}
	return m
}

// assertUnverifiedRefusal checks a refused start-dev names the unverified
// path rather than a live_credentials project.
func assertUnverifiedRefusal(t *testing.T, step string, rb []byte) {
	t.Helper()
	body := errorBody(t, rb)
	if body["live_gate_reason"] != "unverified_path" {
		t.Errorf("%s: live_gate_reason = %v, want unverified_path (%s)", step, body["live_gate_reason"], rb)
	}
	msg, _ := body["message"].(string)
	if !strings.Contains(msg, "could not be verified") {
		t.Errorf("%s: message %q does not say the repo path could not be verified", step, msg)
	}
	if strings.Contains(msg, "live_credentials dev server") || strings.Contains(msg, "LIVE PRODUCTION DATA") {
		t.Errorf("%s: message %q describes a live_credentials project; the gate fired on an unverified path", step, msg)
	}
}

func TestShipReviewLive_UnverifiedPathGate(t *testing.T) {
	e := newLiveShipEnv(t)
	e.stopDevOnCleanup(t)
	e.putDevConfig(t, map[string]any{"dev_command": "exec sleep 60", "dev_url": "http://127.0.0.1:3999"})
	lockedLiveRow(t, e)

	// GET ship-review reports the gate and why.
	card := getCardJSON(t, e)
	if card["live_gate"] != true {
		t.Errorf("card live_gate = %v, want true", card["live_gate"])
	}
	if card["live_gate_reason"] != "unverified_path" {
		t.Errorf("card live_gate_reason = %v, want unverified_path", card["live_gate_reason"])
	}
	if card["live_credentials"] != false {
		t.Errorf("card live_credentials = %v, want false (the task's own row is not live)", card["live_credentials"])
	}

	startURL := e.cardURL() + "/start-dev"
	confirm := []byte(`{"confirm_live":true}`)

	// Agent token: 403 with the unverified-path message.
	resp, rb := shipDoReq(t, e.client, e.token, "POST", startURL, confirm)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("agent start-dev: want 403, got %d %s", resp.StatusCode, rb)
	} else {
		if code := errorCode(rb); code != "board_session_required" {
			t.Errorf("agent start-dev: error = %q, want board_session_required", code)
		}
		assertUnverifiedRefusal(t, "agent start-dev", rb)
	}
	e.assertDevNotStarted(t, "agent start-dev")

	// Board session + passkey, no confirm_live: 409 with the unverified-path message.
	resp, rb = shipDoReq(t, e.client, e.token, "POST", startURL, []byte(`{}`), e.board, "", "mock-assertion")
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("Board start-dev without confirm: want 409, got %d %s", resp.StatusCode, rb)
	} else {
		if code := errorCode(rb); code != "live_confirmation_required" {
			t.Errorf("Board start-dev without confirm: error = %q, want live_confirmation_required", code)
		}
		assertUnverifiedRefusal(t, "Board start-dev without confirm", rb)
	}
	e.assertDevNotStarted(t, "Board start-dev without confirm")

	// Board session + passkey + confirm_live: starts, audited with the reason.
	resp, rb = shipDoReq(t, e.client, e.token, "POST", startURL, confirm, e.board, "", "mock-assertion")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("confirmed start-dev: want 202, got %d %s", resp.StatusCode, rb)
	}
	rows := boardAuditRows(t, e.db, "live_dev_start_confirmed")
	if len(rows) != 1 {
		t.Fatalf("want 1 live_dev_start_confirmed audit row, got %d", len(rows))
	}
	if rows[0]["live_gate_reason"] != "unverified_path" {
		t.Errorf("audit live_gate_reason = %v, want unverified_path", rows[0]["live_gate_reason"])
	}
}

func TestShipReviewLive_GetCardLiveGate(t *testing.T) {
	t.Run("live_credentials", func(t *testing.T) {
		e := newLiveShipEnv(t)
		e.putDevConfig(t, map[string]any{"dev_url": "http://127.0.0.1:3999", "live_credentials": true})
		card := getCardJSON(t, e)
		if card["live_gate"] != true || card["live_gate_reason"] != "live_credentials" {
			t.Errorf("live project: live_gate=%v live_gate_reason=%v, want true, live_credentials", card["live_gate"], card["live_gate_reason"])
		}
	})
	t.Run("not_gated", func(t *testing.T) {
		e := newLiveShipEnv(t)
		e.putDevConfig(t, map[string]any{"dev_url": "http://127.0.0.1:3999"})
		card := getCardJSON(t, e)
		if card["live_gate"] != false {
			t.Errorf("non-live project: live_gate = %v, want false", card["live_gate"])
		}
		if r, ok := card["live_gate_reason"]; ok && r != "" {
			t.Errorf("non-live project: live_gate_reason = %v, want empty", r)
		}
	})
}
