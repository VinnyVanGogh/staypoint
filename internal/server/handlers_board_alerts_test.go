package server_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/alerts"
	"github.com/VinnyVanGogh/staypoint/internal/server"
)

func seedBoardAlertsTable(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`
	CREATE TABLE IF NOT EXISTS board_alerts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		kind TEXT NOT NULL,
		severity TEXT NOT NULL CHECK (severity IN ('critical', 'warning', 'info')),
		title TEXT NOT NULL,
		message TEXT NOT NULL,
		dedupe_key TEXT,
		occurrences INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL,
		last_seen_at TEXT NOT NULL,
		acknowledged_at TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_board_alerts_unack ON board_alerts (acknowledged_at, last_seen_at DESC);
	CREATE INDEX IF NOT EXISTS idx_board_alerts_seen ON board_alerts (last_seen_at);
	`)
	if err != nil {
		t.Fatalf("failed to create board_alerts table: %v", err)
	}
}

func TestGetBoardAlerts_ReturnsOnlyUnacknowledged(t *testing.T) {
	database := setupTestDB(t)
	seedBoardAlertsTable(t, database)
	srv, token := startTestServer(t, database)

	// Record unacknowledged alert 1
	a1, err := alerts.Record(database, alerts.Alert{
		Kind:      "quota_warning",
		Severity:  alerts.SeverityWarning,
		Title:     "Warning 1",
		Message:   "Approaching quota",
		DedupeKey: "quota:warn:1",
	})
	if err != nil {
		t.Fatalf("Record a1: %v", err)
	}

	time.Sleep(10 * time.Millisecond)

	// Record unacknowledged alert 2
	a2, err := alerts.Record(database, alerts.Alert{
		Kind:      "circuit_breaker_tripped",
		Severity:  alerts.SeverityCritical,
		Title:     "Breaker 2",
		Message:   "Tripped",
		DedupeKey: "breaker:2",
	})
	if err != nil {
		t.Fatalf("Record a2: %v", err)
	}

	// Record alert 3 and acknowledge it
	a3, err := alerts.Record(database, alerts.Alert{
		Kind:      "quota_ready",
		Severity:  alerts.SeverityInfo,
		Title:     "Ready 3",
		Message:   "Ready",
		DedupeKey: "quota:ready:3",
	})
	if err != nil {
		t.Fatalf("Record a3: %v", err)
	}
	if err := alerts.Acknowledge(database, a3.ID); err != nil {
		t.Fatalf("Acknowledge a3: %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, srv.URL()+"/api/board/alerts", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/board/alerts status = %d, want 200", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	var res struct {
		Alerts []alerts.Alert `json:"alerts"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("Unmarshal: %v; raw body: %s", err, string(body))
	}

	if len(res.Alerts) != 2 {
		t.Fatalf("expected 2 unacknowledged alerts, got %d", len(res.Alerts))
	}
	// Newest last_seen_at first: a2 then a1
	if res.Alerts[0].ID != a2.ID {
		t.Errorf("expected newest alert first (ID=%d), got ID=%d", a2.ID, res.Alerts[0].ID)
	}
	if res.Alerts[1].ID != a1.ID {
		t.Errorf("expected older alert second (ID=%d), got ID=%d", a1.ID, res.Alerts[1].ID)
	}
}

func TestAckBoardAlert_AuthAndValidation(t *testing.T) {
	database := setupTestDB(t)
	seedBoardAlertsTable(t, database)
	srv, token := startTestServer(t, database)

	alert, err := alerts.Record(database, alerts.Alert{
		Kind:     "circuit_breaker_tripped",
		Severity: alerts.SeverityCritical,
		Title:    "Trip",
		Message:  "Details",
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	ackURL := fmt.Sprintf("%s/api/board/alerts/%d/ack", srv.URL(), alert.ID)

	// 1. No Board session gives 401/403 (whatever WrapBoardSession returns today)
	{
		req, err := http.NewRequest(http.MethodPost, ackURL, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
			t.Errorf("expected 401 or 403 without Board session, got %d", resp.StatusCode)
		}
	}

	// 2. Non-numeric id gives 400
	{
		req, err := http.NewRequest(http.MethodPost, srv.URL()+"/api/board/alerts/not-a-number/ack", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.AddCookie(&http.Cookie{Name: "staypoint_board", Value: srv.BoardToken()})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400 Bad Request for non-numeric id, got %d", resp.StatusCode)
		}
	}

	// 3. Unknown id gives 404
	{
		req, err := http.NewRequest(http.MethodPost, srv.URL()+"/api/board/alerts/999999/ack", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.AddCookie(&http.Cookie{Name: "staypoint_board", Value: srv.BoardToken()})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 Not Found for unknown alert id, got %d", resp.StatusCode)
		}
	}

	// 4. With Board session gives 204 and an SSE board_alert_acknowledged event
	{
		sub := srv.Hub().Subscribe()
		defer srv.Hub().Unsubscribe(sub)

		req, err := http.NewRequest(http.MethodPost, ackURL, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.AddCookie(&http.Cookie{Name: "staypoint_board", Value: srv.BoardToken()})

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("expected 204 No Content for successful ack, got %d", resp.StatusCode)
		}

		// Wait for SSE board_alert_acknowledged event
		received := false
		timeout := time.After(2 * time.Second)
		for !received {
			select {
			case evt := <-sub.Channel():
				if evt.Type == "board_alert_acknowledged" {
					received = true
				}
			case <-timeout:
				t.Fatalf("timed out waiting for board_alert_acknowledged SSE event")
			}
		}
	}
}

func TestBoardAlerts_PollerPublishesSSE(t *testing.T) {
	database := setupTestDB(t)
	seedBoardAlertsTable(t, database)

	token := "test-secret-token-poller-12345678"
	opts := server.Options{
		BindHost:             "127.0.0.1",
		Port:                 0,
		AuthToken:            token,
		DB:                   database,
		TelemetryDBPath:      filepath.Join(t.TempDir(), "test_telemetry.db"),
		ReplayBufferSize:     100,
		SubscriberBufferSize: 16,
		AlertPollInterval:    50 * time.Millisecond,
	}

	srv, err := server.New(opts)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("srv.Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	sub := srv.Hub().Subscribe()
	defer srv.Hub().Unsubscribe(sub)

	// An alert written straight through alerts.Record (simulating the hook CLI process)
	// produces an SSE board_alert event with that id within 2s.
	alert, err := alerts.Record(database, alerts.Alert{
		Kind:     "quota_warning",
		Severity: alerts.SeverityWarning,
		Title:    "Hook Pre-Lock Warning",
		Message:  "Quota at 87%",
	})
	if err != nil {
		t.Fatalf("alerts.Record: %v", err)
	}

	found := false
	timeout := time.After(2 * time.Second)
	for !found {
		select {
		case evt := <-sub.Channel():
			if evt.Type == "board_alert" {
				// Parse event data to verify alert ID
				dataBytes, err := json.Marshal(evt.Data)
				if err != nil {
					continue
				}
				var rec alerts.Alert
				if err := json.Unmarshal(dataBytes, &rec); err == nil && rec.ID == alert.ID {
					found = true
				}
			}
		case <-timeout:
			t.Fatalf("timed out waiting for board_alert SSE event with id=%d within 2s", alert.ID)
		}
	}
}
