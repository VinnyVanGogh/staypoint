package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// STA-696: a dropped macOS notification (osascript denied, no GUI session) must
// be logged with its cause, not discarded.
func TestSendNotification_LogsOSAScriptFailure(t *testing.T) {
	var buf bytes.Buffer
	oldLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(oldLog) })

	const cause = "osascript exit 1: execution error: Not authorized to send Apple events (-1743)"
	var gotScript string
	oldRun := runScript
	runScript = func(_ context.Context, script string) error {
		gotScript = script
		return errors.New(cause)
	}
	t.Cleanup(func() { runScript = oldRun })

	SendNotification(`Breaker "tripped"`, "Agent paused")

	if !strings.Contains(gotScript, `display notification "Agent paused" with title "Breaker \"tripped\""`) {
		t.Errorf("script = %q, want an escaped display notification", gotScript)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "notification failed") || !strings.Contains(out, "-1743") {
		t.Errorf("want a WARN log naming the osascript failure, got:\n%s", out)
	}
}
