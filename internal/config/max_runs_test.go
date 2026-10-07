package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMaxConcurrentRuns covers the STA-773 parallel-run cap: default 3, read
// from config.toml, and non-positive values fall back to the default.
func TestMaxConcurrentRuns(t *testing.T) {
	if got := DefaultConfig().MaxConcurrentRunsOrDefault(); got != 3 {
		t.Fatalf("default = %d, want 3", got)
	}
	if got := (&Config{MaxConcurrentRuns: -2}).MaxConcurrentRunsOrDefault(); got != 3 {
		t.Fatalf("negative = %d, want 3", got)
	}

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("STAYPOINT_REPO_ROOT", "")
	dir := filepath.Join(tmp, ".staypoint")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("max_concurrent_runs = 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.MaxConcurrentRunsOrDefault(); got != 5 {
		t.Fatalf("from config.toml = %d, want 5", got)
	}
}
