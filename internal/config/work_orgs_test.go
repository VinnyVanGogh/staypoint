package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/workorgs"
)

// work_orgs adds work orgs; Managed Solution stays in even when omitted, and
// LoadConfig publishes the list process-wide.
func TestLoadConfig_WorkOrgs(t *testing.T) {
	t.Cleanup(func() { workorgs.Set(nil) })
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	stDir := filepath.Join(dir, ".staypoint")
	if err := os.MkdirAll(stDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stDir, "config.toml"), []byte(`work_orgs = ["Power Platform"]`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if want := []string{"Managed Solution", "Power Platform"}; !reflect.DeepEqual(cfg.WorkOrgs, want) {
		t.Fatalf("WorkOrgs = %v, want %v", cfg.WorkOrgs, want)
	}
	if !workorgs.IsWork("Power Platform") || !workorgs.IsWork("Managed Solution") || workorgs.IsWork("StayPoint") {
		t.Fatal("LoadConfig did not publish work_orgs")
	}

	// Deleting the config file must drop the extra work orgs on the next load,
	// not leave the running daemon with the old list.
	if err := os.Remove(filepath.Join(stDir, "config.toml")); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig without file: %v", err)
	}
	if want := []string{"Managed Solution"}; !reflect.DeepEqual(cfg.WorkOrgs, want) {
		t.Fatalf("WorkOrgs without file = %v, want %v", cfg.WorkOrgs, want)
	}
	if workorgs.IsWork("Power Platform") || !workorgs.IsWork("Managed Solution") {
		t.Fatal("missing config file did not reset work orgs to Managed Solution")
	}
}
