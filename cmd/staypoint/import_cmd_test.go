package main

import (
	"bytes"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/config"
	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/paperclip"
	"github.com/VinnyVanGogh/staypoint/internal/paperclip/paperclipfake"
	"github.com/spf13/cobra"
)

func newImportTestCmd(t *testing.T, args ...string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cmd := &cobra.Command{RunE: runImportPaperclip}
	cmd.Flags().Bool("dry-run", false, "")
	cmd.Flags().StringSlice("company", nil, "")
	cmd.Flags().String("url", "", "")
	cmd.Flags().Duration("timeout", 0, "")
	cmd.Flags().Int("sample", 5, "")
	if err := cmd.Flags().Parse(args); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	return cmd, &out
}

func importFixture(t *testing.T) *paperclipfake.Server {
	t.Helper()
	oldCfg := cfg
	t.Cleanup(func() { cfg = oldCfg })
	cfg = config.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.DBPath = filepath.Join(cfg.DataDir, "staypoint.db")
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	fake := paperclipfake.New(t)
	fake.AddCompany("co-res", "Research & Intelligence", "RES")
	fake.AddIssue(paperclip.ImportIssue{ID: "r1", Identifier: "RES-1", IssueNumber: 1, Title: "Read the paper", Status: "backlog", CompanyID: "co-res"})
	fake.AddIssue(paperclip.ImportIssue{ID: "r2", Identifier: "RES-2", IssueNumber: 2, Title: "Done already", Status: "done", CompanyID: "co-res"})
	return fake
}

func countTasks(t *testing.T) int {
	t.Helper()
	conn, err := db.OpenReadOnly(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var n int
	_ = conn.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&n)
	return n
}

func TestImportPaperclip_DryRunWritesNothing(t *testing.T) {
	fake := importFixture(t)
	cmd, out := newImportTestCmd(t, "--dry-run", "--url", fake.URL)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"dry run", `Research & Intelligence (RES) -> organization "Research"`, "to import 1", "[RES-1] Read the paper", "Total to import: 1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out.String())
		}
	}
	if n := countTasks(t); n != 0 {
		t.Fatalf("dry run created %d tasks", n)
	}
}

func TestImportPaperclip_BoardOnly(t *testing.T) {
	fake := importFixture(t)
	t.Setenv("STAYPOINT_TASK_ID", "task-agent")
	cmd, _ := newImportTestCmd(t, "--dry-run", "--url", fake.URL)
	if err := cmd.RunE(cmd, nil); err == nil || !strings.Contains(err.Error(), "agent context") {
		t.Fatalf("agent context: err = %v", err)
	}
}

func TestImportPaperclip_RequiresConfirmation(t *testing.T) {
	fake := importFixture(t)
	old := importConfirm
	t.Cleanup(func() { importConfirm = old })

	importConfirm = func(io.Reader, io.Writer, int) bool { return false }
	cmd, _ := newImportTestCmd(t, "--url", fake.URL)
	if err := cmd.RunE(cmd, nil); err == nil || countTasks(t) != 0 {
		t.Fatalf("unconfirmed import: err = %v, tasks = %d", err, countTasks(t))
	}

	importConfirm = func(io.Reader, io.Writer, int) bool { return true }
	cmd, out := newImportTestCmd(t, "--url", fake.URL)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Imported 1 tasks (1 parent tasks created") {
		t.Errorf("output: %s", out.String())
	}
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tasks, _ := meshContext.ListTasks(store.DB(), false)
	if len(tasks) != 2 {
		t.Fatalf("tasks = %d, want parent + RES-1", len(tasks))
	}
	for _, tk := range tasks {
		if tk.Organization != "Research" || tk.ExecutionStage != "backlog" || tk.Origin != meshContext.OriginPaperclipImport {
			t.Errorf("task %+v", tk)
		}
	}
}
