package main

import (
	"database/sql"
	"fmt"
	"io"
	"strings"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/spf13/cobra"
)

// STA-861: register a task's work products from the CLI, so an interactive
// task can satisfy the done gate, and change a task's work_kind.
//
//	staypoint task product add <id> --type pr --ref https://github.com/o/r/pull/1
//	staypoint task done <id> --pr https://github.com/o/r/pull/1
//	staypoint task set-kind <id> docs

var taskProductCmd = &cobra.Command{
	Use:   "product",
	Short: "Manage a task's work products (PRs, commits, docs) that the done gate requires",
}

var taskProductAddCmd = &cobra.Command{
	Use:   "add <id>",
	Short: "Register a work product (pr, commit, branch, doc, workspace_file) for a task",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		typ, _ := cmd.Flags().GetString("type")
		ref, _ := cmd.Flags().GetString("ref")
		store, err := db.Open(cfg.DBPath)
		if err != nil {
			return fmt.Errorf("open db: %w", err)
		}
		defer store.Close()
		return addTaskProduct(cmd.OutOrStdout(), store.DB(), args[0], typ, ref)
	},
}

var setKindCmd = &cobra.Command{
	Use:   "set-kind <id> <kind>",
	Short: "Change a task's work kind (coding, review, architecture, planning, qa, docs); refused while a run is in progress",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := db.Open(cfg.DBPath)
		if err != nil {
			return fmt.Errorf("open db: %w", err)
		}
		defer store.Close()
		return setTaskKind(cmd.OutOrStdout(), store.DB(), args[0], args[1])
	},
}

func init() {
	taskProductAddCmd.Flags().String("type", "", "Work product type: "+meshContext.WorkProductTypeNames)
	taskProductAddCmd.Flags().String("ref", "", "Reference: PR URL, commit SHA, branch name, doc URL or file path")
	_ = taskProductAddCmd.MarkFlagRequired("type")
	_ = taskProductAddCmd.MarkFlagRequired("ref")
	taskProductCmd.AddCommand(taskProductAddCmd)
	taskCmd.AddCommand(taskProductCmd)
	taskCmd.AddCommand(setKindCmd)

	taskDoneCmd.Flags().String("pr", "", "Register this PR URL as the task's work product, then mark it done")
}

func addTaskProduct(out io.Writer, conn *sql.DB, id, typ, ref string) error {
	p, err := meshContext.RegisterWorkProduct(conn, id, typ, ref)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Registered %s %s for task %s\n", p.ProductType, p.Reference, p.TaskID)
	return nil
}

// markTaskDoneCLI is `staypoint task done <id> [--pr <url>]`: the --pr
// shortcut registers the PR first, so the work-product gate passes.
func markTaskDoneCLI(out io.Writer, conn *sql.DB, id, pr string) error {
	if strings.TrimSpace(pr) != "" {
		if err := addTaskProduct(out, conn, id, "pr", pr); err != nil {
			return err
		}
	}
	if err := meshContext.MarkTaskDone(conn, id); err != nil {
		return err
	}
	fmt.Fprintf(out, "\033[1;32m✔ Task %q marked as done\033[0m\n", id)
	return nil
}

func setTaskKind(out io.Writer, conn *sql.DB, id, kind string) error {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if !meshContext.IsValidWorkKind(kind) {
		return fmt.Errorf("invalid kind %q: must be one of %s", kind, strings.Join(meshContext.ValidWorkKinds(), ", "))
	}
	task, err := meshContext.GetTask(conn, id)
	if err != nil {
		return err
	}
	// #231: a gemini task cannot take a code kind in a work repo.
	if _, err := router.ValidateTaskChoice(kind, cliRepoIsWork(task.RepoPath), task.Provider, task.ModelOverride); err != nil {
		return err
	}
	updated, err := meshContext.SetTaskWorkKind(conn, task.ID, kind)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Task %s work kind: %s\n", updated.ID, updated.WorkKind)
	return nil
}
