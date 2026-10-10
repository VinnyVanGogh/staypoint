package router

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDrainLabel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "drain.json")
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

	if got := drainLabel(path, now); got != "" {
		t.Fatalf("no file: %q, want empty", got)
	}
	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"label":"Draining for deploy: 2 runs left, 1 queued","updated_at":"2026-10-10T11:59:58Z"}`)
	if got := drainLabel(path, now); got != "Draining for deploy: 2 runs left, 1 queued" {
		t.Fatalf("fresh file: %q", got)
	}
	// Left by a daemon that died mid-drain: never shown.
	write(`{"label":"Draining for deploy: 2 runs left, 1 queued","updated_at":"2026-10-10T11:58:00Z"}`)
	if got := drainLabel(path, now); got != "" {
		t.Fatalf("stale file: %q, want empty", got)
	}
	write(`not json`)
	if got := drainLabel(path, now); got != "" {
		t.Fatalf("garbage file: %q, want empty", got)
	}
}
