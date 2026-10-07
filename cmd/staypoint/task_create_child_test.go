package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/config"
	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/spf13/cobra"
)

func newChildTestCmd(t *testing.T, flags ...string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cmd := &cobra.Command{Use: "create"}
	addChildCreateFlags(cmd)
	cmd.Flags().Float64("budget", 0, "")
	cmd.Flags().Int("max-turns", 0, "")
	if err := cmd.Flags().Parse(flags); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd.SetOut(&out)
	return cmd, &out
}

// STA-820: `staypoint task create --parent <id> --kind <k>` creates a local child.
func TestRunTaskCreateChild(t *testing.T) {
	oldCfg := cfg
	t.Cleanup(func() { cfg = oldCfg })
	cfg = config.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.DBPath = filepath.Join(cfg.DataDir, "staypoint.db")

	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := meshContext.CreateTaskWithOptions(store.DB(), meshContext.TaskCreateOptions{
		Name: "plan", RepoPath: "/repo/x", GitBranch: "main", AccountRole: "personal", Organization: "acme", Project: "core", WorkKind: "planning",
	})
	store.Close()
	if err != nil {
		t.Fatal(err)
	}

	planFile := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(planFile, []byte("plan from file"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd, out := newChildTestCmd(t, "--parent", parent.ID, "--kind", "qa", "--handoff-file", planFile)
	if !childCreateRequested(cmd) {
		t.Fatal("--parent should select the child path")
	}
	if err := runTaskCreateChild(cmd, []string{"Verify", "login"}); err != nil {
		t.Fatalf("runTaskCreateChild: %v", err)
	}
	if !strings.Contains(out.String(), "Created child task") {
		t.Errorf("output: %q", out.String())
	}

	store, err = db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tasks, _ := meshContext.ListTasks(store.DB(), true)
	var child *meshContext.Task
	for i := range tasks {
		if tasks[i].ParentID == parent.ID {
			child, err = meshContext.GetTask(store.DB(), tasks[i].ID)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if child == nil {
		t.Fatal("child not created")
	}
	if child.Name != "Verify login" || child.WorkKind != "qa" || child.RepoPath != "/repo/x" || child.Project != "core" {
		t.Errorf("child: %+v", child)
	}
	if h, _ := meshContext.GetTaskHandoff(store.DB(), child.ID); !strings.Contains(h, "plan from file") {
		t.Errorf("handoff: %q", h)
	}

	bad, _ := newChildTestCmd(t, "--parent", parent.ID, "--kind", "deploy")
	if err := runTaskCreateChild(bad, []string{"x"}); err == nil {
		t.Error("--kind deploy should be rejected")
	}
	noTitle, _ := newChildTestCmd(t, "--parent", parent.ID)
	if err := runTaskCreateChild(noTitle, nil); err == nil {
		t.Error("missing title should be rejected")
	}
	none, _ := newChildTestCmd(t)
	if childCreateRequested(none) {
		t.Error("no --parent should keep the normal create path")
	}
}
