package main

import (
	"fmt"
	"strings"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
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
		if err := meshContext.SetTaskExecutionStageWithOptions(store.DB(), args[0], stage, meshContext.DoneOptions{BoardOverride: override}); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Task %s moved to %s\n", args[0], stage)
		return nil
	},
}

// filterTaskList applies the task list flags: legacy tasks are hidden unless
// includeLegacy, and stage (when set) keeps only that execution stage.
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

func init() {
	taskCmd.AddCommand(taskStageCmd)
	taskStageCmd.Flags().Bool("override", false, "Board override: mark done while child tasks are open")
}
