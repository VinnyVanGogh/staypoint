package main

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

var taskStageCmd = &cobra.Command{
	Use:   "stage <id> <stage>",
	Short: "Move a task to an execution stage (" + strings.Join(governance.BoardSettableStages, ", ") + ")",
	Long: `Move a task to an execution stage.

backlog parks the task: nothing claims or wakes it until it is moved to todo.
cancelled closes it (status soft_deleted); moving it to backlog or todo reopens
it. This command does not start a run; use Run Now on the board for that.
blocked is set with 'staypoint task block' because it needs a reason.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		stage := strings.ToLower(strings.TrimSpace(args[1]))
		if !governance.IsBoardSettableStage(stage) {
			return fmt.Errorf("invalid stage %q: must be one of %s", stage, strings.Join(governance.BoardSettableStages, ", "))
		}
		override, _ := cmd.Flags().GetBool("override")
		store, err := db.Open(cfg.DBPath)
		if err != nil {
			return fmt.Errorf("open db: %w", err)
		}
		defer store.Close()
		tty := isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
		boardStage, err := cliBoardStage(store.DB(), args[0], stage, os.Getenv, tty)
		if err != nil {
			return err
		}
		if err := meshContext.SetTaskExecutionStageWithOptions(store.DB(), args[0], stage, meshContext.DoneOptions{BoardOverride: override, BoardStage: boardStage}); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Task %s moved to %s\n", args[0], stage)
		return nil
	},
}

// cliBoardStage decides whether this CLI stage change counts as the Board's.
// Only moving an agent-created task out of a parked stage needs it: that is
// refused inside an agent session or without a terminal, and refused outright
// for a prod-targeting task, which needs Touch ID in the Board UI.
func cliBoardStage(conn *sql.DB, taskID, stage string, getenv func(string) string, tty bool) (bool, error) {
	task, err := meshContext.GetTask(conn, taskID)
	if err != nil {
		return false, err
	}
	if !meshContext.RequiresBoardToLeave(task, stage) {
		return false, nil
	}
	if err := refuseBoardOnlyInAgentContext("moving an agent-created task out of "+task.ExecutionStage, getenv, tty); err != nil {
		return false, err
	}
	if meshContext.NameTargetsProd(task.Name) {
		return false, fmt.Errorf("%s targets prod: moving it out of %s needs Touch ID; use Run Now or the stage menu in the Board UI", task.ID, task.ExecutionStage)
	}
	if task.RepoPath != "" {
		if _, gated, err := shipreview.LiveGateConfig(conn, task.RepoPath); err != nil || gated {
			return false, fmt.Errorf("%s targets a live_credentials (prod) repo: moving it out of %s needs Touch ID; use the Board UI", task.ID, task.ExecutionStage)
		}
	}
	return true, nil
}

// filterTaskList applies the task list flags: legacy tasks and archived
// imports are hidden unless includeLegacy, and stage (when set) keeps only that execution stage.
func filterTaskList(tasks []meshContext.Task, includeLegacy bool, stage string) []meshContext.Task {
	tasks = meshContext.FilterLegacy(tasks, includeLegacy)
	stage = strings.ToLower(strings.TrimSpace(stage))
	if stage == "" {
		return tasks
	}
	out := tasks[:0:0]
	for _, t := range tasks {
		if t.ExecutionStage == stage {
			out = append(out, t)
		}
	}
	return out
}

var taskSetRepoCmd = &cobra.Command{
	Use:   "set-repo <id> <path>",
	Short: "Set the repo a task runs in (imported tasks start without one)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		branch, _ := cmd.Flags().GetString("branch")
		store, err := db.Open(cfg.DBPath)
		if err != nil {
			return fmt.Errorf("open db: %w", err)
		}
		defer store.Close()
		task, err := meshContext.SetTaskRepo(store.DB(), args[0], args[1], branch)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Task %s repo set to %s (%s)\n", task.ID, task.RepoPath, task.GitBranch)
		return nil
	},
}

func init() {
	taskCmd.AddCommand(taskSetRepoCmd)
	taskSetRepoCmd.Flags().String("branch", "", "Git branch (default: the repo's current branch)")
	taskCmd.AddCommand(taskStageCmd)
	taskStageCmd.Flags().Bool("override", false, "Board override: mark done while child tasks are open")
}
