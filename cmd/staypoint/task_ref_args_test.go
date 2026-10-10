package main

import (
	"path/filepath"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/spf13/cobra"
)

func TestResolveTaskRefArgs(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mesh.db")
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name, org string) *meshContext.Task {
		task, err := meshContext.CreateTaskWithOptions(store.DB(), meshContext.TaskCreateOptions{
			Name: name, RepoPath: "/tmp/r", GitBranch: "main", AccountRole: "personal",
			Organization: org, ExecutionStage: "backlog",
		})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	sta1, sta2, man1 := mk("One", "StayPoint"), mk("Two", ""), mk("Work", "Managed Solution")
	store.Close()

	root := &cobra.Command{Use: "staypoint"}
	task := &cobra.Command{Use: "task"}
	show := &cobra.Command{Use: "show"}
	block := &cobra.Command{Use: "block"}
	create := &cobra.Command{Use: "create"}
	create.Flags().String("parent", "", "")
	comment := &cobra.Command{Use: "comment"}
	checklist := &cobra.Command{Use: "checklist"}
	verify := &cobra.Command{Use: "verify"}
	root.AddCommand(task, checklist)
	task.AddCommand(show, block, create, comment)
	checklist.AddCommand(verify)

	run := func(cmd *cobra.Command, args ...string) []string {
		resolveTaskRefArgs(cmd, args, dbPath)
		return args
	}
	// Failure cases first: unknown references and non-task commands untouched.
	if got := run(show, "STA-99"); got[0] != "STA-99" {
		t.Errorf("unknown reference rewritten to %q", got[0])
	}
	if got := run(verify, "STA-1"); got[0] != "STA-1" {
		t.Errorf("non-task command arg rewritten to %q", got[0])
	}
	if got := run(create, "STA-1"); got[0] != "STA-1" {
		t.Errorf("create's free-text comment rewritten to %q", got[0])
	}
	if got := run(comment, "STA-1", "MAN-1"); got[0] != sta1.ID || got[1] != "MAN-1" {
		t.Errorf("comment args = %v, want [%s MAN-1] (message kept)", got, sta1.ID)
	}

	if got := run(show, "sta-2"); got[0] != sta2.ID {
		t.Errorf("show sta-2 = %q, want %s", got[0], sta2.ID)
	}
	if got := run(block, "STA-1", "MAN-1", sta2.ID); got[0] != sta1.ID || got[1] != man1.ID || got[2] != sta2.ID {
		t.Errorf("block args = %v", got)
	}
	if err := create.Flags().Set("parent", "MAN-1"); err != nil {
		t.Fatal(err)
	}
	run(create, "some comment")
	if p, _ := create.Flags().GetString("parent"); p != man1.ID {
		t.Errorf("--parent = %q, want %s", p, man1.ID)
	}
}
