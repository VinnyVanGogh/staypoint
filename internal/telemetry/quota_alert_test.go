package telemetry

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/alerts"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/router"
)

func TestQuotaAlert_KindSeverityDedupe(t *testing.T) {
	cases := []struct {
		state    QuotaState
		kind     string
		severity alerts.Severity
		dedupe   string
	}{
		{QuotaWarning, "quota_warning", alerts.SeverityWarning, "quota:3p-claude:warning"},
		{QuotaLocked, "quota_locked", alerts.SeverityWarning, "quota:3p-claude:locked"},
		{QuotaReady, "quota_ready", alerts.SeverityInfo, "quota:3p-claude:ready"},
	}
	for _, c := range cases {
		a := quotaAlert(router.Pool3PClaude, c.state, "t", "m")
		if a.Kind != c.kind || a.Severity != c.severity || a.DedupeKey != c.dedupe {
			t.Errorf("%s: got kind=%q severity=%q dedupe=%q, want %q %q %q",
				c.state, a.Kind, a.Severity, a.DedupeKey, c.kind, c.severity, c.dedupe)
		}
	}
}

// The notifier (daemon) and the hook CLI raise the same pre-lock warning; one
// unacknowledged row must absorb both.
func TestDBAlertSink_QuotaWarningDedupesAcrossCallers(t *testing.T) {
	oldRun := runScript
	runScript = func(context.Context, string) error { return nil }
	t.Cleanup(func() { runScript = oldRun })

	store, err := db.Open(filepath.Join(t.TempDir(), "staypoint.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	SetAlertSink(DBAlertSink(store.DB()))
	t.Cleanup(func() { SetAlertSink(nil) })

	SendQuotaAlert(router.PoolPersonalClaude, QuotaWarning, "notifier warning", "85%")
	SendQuotaAlert(router.PoolPersonalClaude, QuotaWarning, "hook warning", "86%")

	list, err := alerts.ListUnacknowledged(store.DB(), 10)
	if err != nil {
		t.Fatalf("ListUnacknowledged: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d alerts, want 1", len(list))
	}
	if list[0].Occurrences != 2 || list[0].Title != "hook warning" {
		t.Errorf("got occurrences=%d title=%q, want 2 and the newer title", list[0].Occurrences, list[0].Title)
	}
}
