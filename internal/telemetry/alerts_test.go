package telemetry

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/alerts"
)

func TestBreakerAlertSink_TrippedOnThreeFailures(t *testing.T) {
	// Don't let tests pop real macOS notifications. Use the injectable runner from internal/osascript.
	oldRun := runScript
	runScript = func(_ context.Context, _ string) error {
		return nil
	}
	t.Cleanup(func() { runScript = oldRun })

	var mu sync.Mutex
	var captured []alerts.Alert
	SetAlertSink(func(a alerts.Alert) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, a)
	})
	t.Cleanup(func() { SetAlertSink(nil) })

	meshDB, cleanup := setupTestMeshDB(t)
	defer cleanup()

	tracker := NewBreakerTracker()
	sessionID := "sess-breaker-alert-1"
	repoPath := "/Users/dev/project"
	tool := "run_command"
	cmd := "npm run build"
	errText := "Error: TS2307: Cannot find module '@types/node'"

	for i := 0; i < 3; i++ {
		tripped, _, err := tracker.RecordFailure(meshDB, sessionID, repoPath, "claude", tool, cmd, errText)
		if err != nil {
			t.Fatalf("RecordFailure failure %d: %v", i+1, err)
		}
		if i < 2 && tripped {
			t.Errorf("unexpected trip on failure %d", i+1)
		}
	}

	mu.Lock()
	count := len(captured)
	var first alerts.Alert
	if count > 0 {
		first = captured[0]
	}
	mu.Unlock()

	if count != 1 {
		t.Fatalf("expected exactly 1 alert captured, got %d", count)
	}
	if first.Kind != "circuit_breaker_tripped" {
		t.Errorf("expected alert kind 'circuit_breaker_tripped', got %q", first.Kind)
	}
	if first.Severity != alerts.SeverityCritical {
		t.Errorf("expected alert severity %q, got %q", alerts.SeverityCritical, first.Severity)
	}
}

func TestBreakerAlertSink_SecretScrubbing(t *testing.T) {
	oldRun := runScript
	runScript = func(_ context.Context, _ string) error {
		return nil
	}
	t.Cleanup(func() { runScript = oldRun })

	var mu sync.Mutex
	var captured []alerts.Alert
	SetAlertSink(func(a alerts.Alert) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, a)
	})
	t.Cleanup(func() { SetAlertSink(nil) })

	meshDB, cleanup := setupTestMeshDB(t)
	defer cleanup()

	tracker := NewBreakerTracker()
	sessionID := "sess-secret-check"
	repoPath := "/Users/dev/project"
	tool := "bash"
	cmd := "export API_TOKEN=sk-live-123 && make"
	errText := "fatal: authentication failure with sk-live-123"

	for i := 0; i < 3; i++ {
		_, _, err := tracker.RecordFailure(meshDB, sessionID, repoPath, "claude", tool, cmd, errText)
		if err != nil {
			t.Fatalf("RecordFailure %d: %v", i+1, err)
		}
	}

	mu.Lock()
	count := len(captured)
	var first alerts.Alert
	if count > 0 {
		first = captured[0]
	}
	mu.Unlock()

	if count != 1 {
		t.Fatalf("expected 1 alert captured, got %d", count)
	}
	for _, secret := range []string{"sk-live-123", "API_TOKEN"} {
		if strings.Contains(first.Title, secret) {
			t.Errorf("alert title leaked secret %q: %s", secret, first.Title)
		}
		if strings.Contains(first.Message, secret) {
			t.Errorf("alert message leaked secret %q: %s", secret, first.Message)
		}
	}
}

func TestAlertSink_DisableAndNoPanic(t *testing.T) {
	oldRun := runScript
	runScript = func(_ context.Context, _ string) error {
		return nil
	}
	t.Cleanup(func() { runScript = oldRun })

	var mu sync.Mutex
	var captured []alerts.Alert
	SetAlertSink(func(a alerts.Alert) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, a)
	})

	// SetAlertSink(nil) disables the sink
	SetAlertSink(nil)

	// SendAlert with no sink must not panic
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("SendAlert panicked with nil sink: %v", r)
		}
	}()

	SendAlert(alerts.Alert{
		Kind:     "quota_ready",
		Severity: alerts.SeverityInfo,
		Title:    "Pool ready",
		Message:  "Quota reset",
	})

	mu.Lock()
	count := len(captured)
	mu.Unlock()

	if count != 0 {
		t.Errorf("expected 0 alerts captured after disabling sink, got %d", count)
	}
}
