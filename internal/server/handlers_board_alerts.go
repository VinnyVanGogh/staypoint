package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/alerts"
)

// DefaultAlertPollInterval is how often the server looks for new board_alerts
// rows when Options.AlertPollInterval is unset.
const DefaultAlertPollInterval = 3 * time.Second

// boardAlertsListLimit caps GET /api/board/alerts.
const boardAlertsListLimit = 50

// BoardAlertsHandler serves the Board alert feed (STA-705).
type BoardAlertsHandler struct {
	db  *sql.DB
	hub *EventHub
}

// NewBoardAlertsHandler creates a BoardAlertsHandler.
func NewBoardAlertsHandler(db *sql.DB, hub *EventHub) *BoardAlertsHandler {
	return &BoardAlertsHandler{db: db, hub: hub}
}

// List handles GET /api/board/alerts: unacknowledged alerts, newest first.
// Alerts carry no secrets, so the session token is enough to read them.
func (h *BoardAlertsHandler) List(w http.ResponseWriter, r *http.Request) {
	list, err := alerts.ListUnacknowledged(h.db, boardAlertsListLimit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list alerts")
		return
	}
	writeJSON(w, map[string]any{"alerts": list})
}

// Ack handles POST /api/board/alerts/{id}/ack. It is wrapped in
// WrapBoardSession so agents holding only the session token can't silence
// alerts.
func (h *BoardAlertsHandler) Ack(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid alert id")
		return
	}
	if err := alerts.Acknowledge(h.db, id); err != nil {
		if errors.Is(err, alerts.ErrNotFound) {
			writeError(w, http.StatusNotFound, "alert not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to acknowledge alert")
		return
	}
	h.hub.Publish("board_alert_acknowledged", map[string]any{"id": id})
	w.WriteHeader(http.StatusNoContent)
}

// Seed handles POST /api/board/alerts/test/seed (test-only). It records the
// alert in the body; the poller publishes it like any other new row.
// Only registered when server.Options.TestMode is true.
func (h *BoardAlertsHandler) Seed(w http.ResponseWriter, r *http.Request) {
	var a alerts.Alert
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		writeError(w, http.StatusBadRequest, "invalid alert body")
		return
	}
	rec, err := alerts.Record(h.db, a)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, rec)
}

// pollAlerts publishes board_alert for every row whose last_seen_at moves past
// the cursor, until stop closes. Rows come from this process and from other
// processes sharing the DB (the hook CLI), which is why this polls instead of
// publishing from the sink.
//
// The cursor is the newest last_seen_at already published. alerts.Record
// stamps last_seen_at while holding the write lock, so rows commit in
// timestamp order and a later commit can't land behind the cursor.
//
// since is the server start time, taken before the goroutine starts so a row
// written right after Start returns is never behind it. The UI's initial GET
// covers older alerts, so a restart doesn't replay them over SSE.
func (s *Server) pollAlerts(stop <-chan struct{}, since time.Time, interval time.Duration) {
	cursor := since
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	failing := false
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		list, err := alerts.ListSeenSince(s.opts.DB, cursor)
		if err != nil {
			// Log the first error of a streak only; this runs every few seconds.
			if !failing {
				slog.Warn("board alert poll failed", slog.String("error", err.Error()))
			}
			failing = true
			continue
		}
		failing = false
		for _, a := range list {
			s.hub.Publish("board_alert", a)
			if a.LastSeenAt.After(cursor) {
				cursor = a.LastSeenAt
			}
		}
	}
}
