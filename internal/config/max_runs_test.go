package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMaxConcurrentRuns covers the STA-773 parallel-run cap: default 9
// (STA-867), read from config.toml, and non-positive values fall back to the
// default.
func TestMaxConcurrentRuns(t *testing.T) {
	if got := DefaultConfig().MaxConcurrentRunsOrDefault(); got != 9 {
		t.Fatalf("default = %d, want 9", got)
	}
	if got := (&Config{MaxConcurrentRuns: -2}).MaxConcurrentRunsOrDefault(); got != 9 {
		t.Fatalf("negative = %d, want 9", got)
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

// TestRunLimitKeys covers the STA-867 keys: max_runs_per_repo and
// max_runs_per_org default to 3, and [run_limits.orgs] overrides parse.
func TestRunLimitKeys(t *testing.T) {
	d := DefaultConfig()
	if d.MaxRunsPerRepoOrDefault() != 3 || d.MaxRunsPerOrgOrDefault() != 3 || d.OrgRunLimits() != nil {
		t.Fatalf("defaults: repo %d org %d overrides %v", d.MaxRunsPerRepoOrDefault(), d.MaxRunsPerOrgOrDefault(), d.OrgRunLimits())
	}

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("STAYPOINT_REPO_ROOT", "")
	dir := filepath.Join(tmp, ".staypoint")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := "max_concurrent_runs = 12\nmax_runs_per_repo = 2\nmax_runs_per_org = 4\n\n[run_limits.orgs]\n\"Managed Solution\" = 5\nStayPoint = 1\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.MaxConcurrentRunsOrDefault() != 12 || cfg.MaxRunsPerRepoOrDefault() != 2 || cfg.MaxRunsPerOrgOrDefault() != 4 {
		t.Fatalf("caps = %d/%d/%d, want 12/2/4", cfg.MaxConcurrentRunsOrDefault(), cfg.MaxRunsPerRepoOrDefault(), cfg.MaxRunsPerOrgOrDefault())
	}
	orgs := cfg.OrgRunLimits()
	if orgs["Managed Solution"] != 5 || orgs["StayPoint"] != 1 || len(orgs) != 2 {
		t.Fatalf("org overrides = %v", orgs)
	}
}
