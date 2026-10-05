package telemetry

import (
	"database/sql"
	"log/slog"
	"sync"

	"github.com/VinnyVanGogh/staypoint/internal/alerts"
)

// AlertSink receives every alert raised through SendAlert.
type AlertSink func(alerts.Alert)

var (
	alertSinkMu sync.RWMutex
	alertSink   AlertSink
)

// SetAlertSink sets the process-wide alert sink. nil disables it.
func SetAlertSink(fn AlertSink) {
	alertSinkMu.Lock()
	defer alertSinkMu.Unlock()
	alertSink = fn
}

// DBAlertSink persists alerts to board_alerts, where the daemon's server
// picks them up and pushes them to the Board UI. Errors are logged, never
// returned: an alert must not break the caller.
func DBAlertSink(db *sql.DB) AlertSink {
	return func(a alerts.Alert) {
		if _, err := alerts.Record(db, a); err != nil {
			slog.Warn("board alert not recorded",
				slog.String("kind", a.Kind), slog.String("title", a.Title), slog.String("error", err.Error()))
		}
	}
}

// SendAlert raises a Board alert through the sink, if one is set, then shows
// the best-effort macOS notification.
func SendAlert(a alerts.Alert) {
	alertSinkMu.RLock()
	sink := alertSink
	alertSinkMu.RUnlock()
	if sink != nil {
		sink(a)
	}
	SendNotification(a.Title, a.Message)
}
