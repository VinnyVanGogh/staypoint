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
	home := t.TempDir()
	t.Setenv("HOME", home)
	oldCfg := cfg
	t.Cleanup(func() { cfg = oldCfg })
	cfg = nil
	dd := filepath.Join(home, ".staypoint")
	type call struct {
		tool string
		in   map[string]string
	}
	deny := []call{
		{"Read", map[string]string{"file_path": dd + "/ops_key"}},
		{"Read", map[string]string{"file_path": dd + "/auth_token"}},
		{"Read", map[string]string{"file_path": dd + "/handoffs/../board_token"}},
		{"Read", map[string]string{"file_path": "~/.staypoint/ops_key"}},
		// macOS paths ignore case.
		{"Read", map[string]string{"file_path": filepath.Join(home, ".STAYPOINT", "OPS_KEY")}},
		{"Read", map[string]string{"file_path": dd + "/Ops_Key"}},
		{"Grep", map[string]string{"path": dd, "pattern": "."}},
		{"Grep", map[string]string{"path": "~/.staypoint/"}},
		{"Grep", map[string]string{"path": filepath.Join(home, ".StayPoint")}},
		{"Glob", map[string]string{"pattern": dd + "/ops_key*"}},
		{"Glob", map[string]string{"pattern": "**/.staypoint/ops_key"}},
		{"Glob", map[string]string{"path": home, "pattern": "**/ops_key"}},
		{"Glob", map[string]string{"path": "/", "pattern": "**/{ops_key,x}"}},
		{"Grep", map[string]string{"path": home, "glob": "**/auth_token", "pattern": "."}},
	}
	for _, c := range deny {
		raw, _ := json.Marshal(c.in)
		if credentialFileAccess(c.tool, raw) == "" {
			t.Errorf("%s %v allowed", c.tool, c.in)
		}
	}
	allow := []call{
		{"Read", map[string]string{"file_path": dd + "/handoffs/task-1/plan.md"}},
		{"Grep", map[string]string{"path": dd + "/handoffs/task-1", "pattern": "x"}},
		// Code that mentions the names is not a credential read.
		{"Edit", map[string]string{"file_path": "/repo/cmd/hook.go", "old_string": "auth_token", "new_string": "~/.staypoint/ops_key"}},
		{"Grep", map[string]string{"path": "/repo", "pattern": "auth_token"}},
		{"Read", map[string]string{"file_path": "/repo/internal/opstools/origin.go"}},
		{"Glob", map[string]string{"path": "/repo", "pattern": "**/*auth_token*.go"}},
		{"Read", map[string]string{"file_path": "/repo/ops_key"}},
	}
	for _, c := range allow {
		raw, _ := json.Marshal(c.in)
		if why := credentialFileAccess(c.tool, raw); why != "" {
			t.Errorf("%s %v denied: %s", c.tool, c.in, why)
		}
	}
}
