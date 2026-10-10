package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Board review #2 H1: `HOME=/tmp/fakehome staypoint mcp` used to load the
// fake home's config.toml, so its dev host list (any IP, prod services,
// branch main) drove dev_host_run. The ops config now comes from the real
// account's home whatever HOME says, and HOME is reset for the children.
func TestPinMCPConfigIgnoresFakeHome(t *testing.T) {
	realHome, fakeHome := t.TempDir(), t.TempDir()
	write := func(home, hostName string) {
		dir := filepath.Join(home, ".staypoint")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		toml := "[gates.hosts]\ndev = [\"mansol-dev\"]\n[gates.ops.dev_hosts.mansol-dev]\nhost_name = \"" + hostName + "\"\napp_dir = \"/srv/app\"\n"
		if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(toml), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(realHome, "10.0.0.5")
	write(fakeHome, "203.0.113.66")

	old := mcpRealHome
	t.Cleanup(func() { mcpRealHome = old })
	mcpRealHome = func() (string, error) { return realHome, nil }
	t.Setenv("HOME", fakeHome)

	c, err := pinMCPConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Gates.Ops.DevHosts["mansol-dev"].HostName; got != "10.0.0.5" {
		t.Fatalf("host_name %q: config came from the fake HOME", got)
	}
	if c.DataDir != filepath.Join(realHome, ".staypoint") {
		t.Fatalf("data dir %q, want the real home's", c.DataDir)
	}
	if os.Getenv("HOME") != realHome {
		t.Fatalf("HOME = %q, want it reset to the real home", os.Getenv("HOME"))
	}
}

// Board review #2 H1: the Read tool must not hand an agent the ops key (it
// would mint a run token for any task), nor a Grep over the data dir.
func TestCredentialFileAccess(t *testing.T) {
	deny := map[string]map[string]string{
		"Read":  {"file_path": "/Users/x/.staypoint/ops_key"},
		"Read2": {"file_path": "/Users/x/.staypoint/auth_token"},
		"Read3": {"file_path": "/Users/x/.staypoint/handoffs/../board_token"},
		"Grep":  {"path": "/Users/x/.staypoint", "pattern": "."},
		"Grep2": {"path": "~/.staypoint/"},
		"Glob":  {"pattern": "/Users/x/.staypoint/ops_key*"},
	}
	for tool, in := range deny {
		raw, _ := json.Marshal(in)
		if credentialFileAccess(tool, raw) == "" {
			t.Errorf("%s %v allowed", tool, in)
		}
	}
	allow := map[string]map[string]any{
		"Read":  {"file_path": "/Users/x/.staypoint/handoffs/task-1/plan.md"},
		"Grep":  {"path": "/Users/x/.staypoint/handoffs/task-1", "pattern": "x"},
		"Edit":  {"file_path": "/repo/cmd/hook.go", "old_string": "auth_token", "new_string": "~/.staypoint/ops_key"},
		"Read2": {"file_path": "/repo/internal/opstools/origin.go"},
	}
	for tool, in := range allow {
		raw, _ := json.Marshal(in)
		if why := credentialFileAccess(tool, raw); why != "" {
			t.Errorf("%s %v denied: %s", tool, in, why)
		}
	}
}
