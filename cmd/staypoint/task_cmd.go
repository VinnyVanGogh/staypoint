package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/paperclip"
	"github.com/spf13/cobra"
)

var taskCmd = &cobra.Command{
	Use:   "task",
	Short: "Manage development tasks and context in staypoint.db and Paperclip",
}

var taskListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all active tasks (Paperclip and local Staypoint)",
	Run: func(cmd *cobra.Command, args []string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		pclipClient := paperclip.NewClient("", "")
		var pclipIssues []paperclip.IssueResponse

		// Always fetch from all companies so tasks across different organizations are visible
		if companies, err := pclipClient.ListCompanies(ctx); err == nil {
			for _, c := range companies {
				if issues, err := pclipClient.ListActiveIssues(ctx, c.ID); err == nil && len(issues) > 0 {
					pclipIssues = append(pclipIssues, issues...)
				}
			}
		}

		if len(pclipIssues) > 0 {
			fmt.Println("\033[1;36m[Paperclip Active Issues]\033[0m")
			for _, iss := range pclipIssues {
				statusColor := "\033[1;32m"
				if iss.Status == "done" || iss.Status == "cancelled" {
					statusColor = "\033[0;37m"
				} else if iss.Status == "in_progress" {
					statusColor = "\033[1;33m"
				}
				fmt.Printf("  • %s[%s]\033[0m \033[1m%s\033[0m (%s, priority: %s)\n",
					statusColor, iss.Identifier, iss.Title, iss.Status, iss.Priority)
			}
			fmt.Println()
		}

		fmt.Println("\033[1;36m[Staypoint Tasks]\033[0m")
		store, err := db.Open(cfg.DBPath)
		if err != nil {
			if len(pclipIssues) == 0 {
				fmt.Fprintf(os.Stderr, "Error opening db: %v\n", err)
				os.Exit(1)
			}
			return
		}
		defer store.Close()
		all, _ := cmd.Flags().GetBool("all")
		includeLegacy, _ := cmd.Flags().GetBool("legacy")
		stageFilter, _ := cmd.Flags().GetString("stage")
		tasks, err := meshContext.ListTasks(store.DB(), all)
		if err != nil {
			if len(pclipIssues) == 0 {
				fmt.Fprintf(os.Stderr, "Error listing tasks: %v\n", err)
				os.Exit(1)
			}
			return
		}
		tasks = filterTaskList(tasks, includeLegacy, stageFilter)
		if len(tasks) == 0 {
			if len(pclipIssues) == 0 {
				fmt.Println("  No active tasks found.")
			} else {
				fmt.Println("  No local mesh tasks.")
			}
			return
		}
		for _, t := range tasks {
			statusColor := "\033[1;32m"
			if t.Status == "done" {
				statusColor = "\033[0;37m"
			}
			budgetInfo := ""
			if t.MaxBudgetUSD > 0 || t.MaxTurns > 0 {
				pct := 0.0
				if t.MaxBudgetUSD > 0 {
					pct = (t.SpentUSD / t.MaxBudgetUSD) * 100.0
				}
				budgetInfo = fmt.Sprintf(" [budget: $%.2f/$%.2f (%.0f%%), %d/%d turns]", t.SpentUSD, t.MaxBudgetUSD, pct, t.SpentTurns, t.MaxTurns)
			}
			orgProjInfo := ""
			if t.Organization != "" || t.Project != "" {
				orgProjInfo = fmt.Sprintf(" (Org: %s | Proj: %s)", t.Organization, t.Project)
			}
			originInfo := ""
			if t.Origin != "" && t.Origin != meshContext.OriginNative {
				originInfo = " [" + t.Origin + "]"
			}
			if t.SourceRef != "" {
				originInfo += " (imported from " + t.SourceRef + ")"
			}
			fmt.Printf("  • %s[%s/%s]\033[0m \033[1m%s\033[0m%s%s (branch: %s, role: %s)%s\n",
				statusColor, t.Status, t.ExecutionStage, t.Name, orgProjInfo, originInfo, t.GitBranch, t.AccountRole, budgetInfo)
		}
	},
}

var taskAddCmd = &cobra.Command{
	Use:     "add [comment]",
	Aliases: []string{"new"},
	Short:   "Create and dispatch a structured engineering task using Gemini & Claude with TUI or CLI input",
	RunE:    runTaskCreate,
}

var taskDoneCmd = &cobra.Command{
	Use:   "done [id|name]",
	Short: "Mark a task as completed",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		store, err := db.Open(cfg.DBPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening db: %v\n", err)
			os.Exit(1)
		}
		defer store.Close()
		if err := meshContext.MarkTaskDone(store.DB(), args[0]); err != nil {
			fmt.Fprintf(os.Stderr, "Error updating task: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\033[1;32m✔ Task %q marked as done\033[0m\n", args[0])
	},
}

var taskBudgetCmd = &cobra.Command{
	Use:   "budget [id|name]",
	Short: "Set or update dollar/turn budget limits for a task",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		store, err := db.Open(cfg.DBPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening db: %v\n", err)
			os.Exit(1)
		}
		defer store.Close()
		budget, _ := cmd.Flags().GetFloat64("usd")
		turns, _ := cmd.Flags().GetInt("turns")
		if err := meshContext.UpdateTaskBudget(store.DB(), args[0], budget, turns); err != nil {
			fmt.Fprintf(os.Stderr, "Error updating budget: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\033[1;32m✔ Task %q budget updated to $%.2f USD / %d turns\033[0m\n", args[0], budget, turns)
	},
}

var taskShowCmd = &cobra.Command{
	Use:     "show [id|name]",
	Aliases: []string{"view", "info"},
	Short:   "Show details of a task",
	Args:    cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		store, err := db.Open(cfg.DBPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening db: %v\n", err)
			os.Exit(1)
		}
		defer store.Close()

		task, err := meshContext.GetTask(store.DB(), args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting task: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("Task ID: %s\n", task.ID)
		fmt.Printf("Name: %s\n", task.Name)
		fmt.Printf("Status: %s\n", task.Status)
		if task.IsBlocked {
			reason := task.BlockReason
			if reason == "" {
				reason = "Blocked — no specific reason recorded"
			}
			fmt.Printf("Blocked: YES (Reason: %s)\n", reason)
		}
		if len(task.BlockedBy) > 0 {
			fmt.Println("\nBlocked By (Upstream Tasks):")
			for _, b := range task.BlockedBy {
				r := b.Rationale
				if r != "" {
					fmt.Printf("  • %s (%s, stage: %s) — Rationale: %s\n", b.ID, b.Name, b.ExecutionStage, r)
				} else {
					fmt.Printf("  • %s (%s, stage: %s)\n", b.ID, b.Name, b.ExecutionStage)
				}
			}
		}
		if len(task.Blocks) > 0 {
			fmt.Println("\nBlocks (Downstream Tasks):")
			for _, b := range task.Blocks {
				fmt.Printf("  • %s (%s, stage: %s)\n", b.ID, b.Name, b.ExecutionStage)
			}
		}
		fmt.Printf("Repository: %s (Branch: %s)\n", task.RepoPath, task.GitBranch)
		if task.Organization != "" {
			fmt.Printf("Organization: %s\n", task.Organization)
		}
		if task.Project != "" {
			fmt.Printf("Project: %s\n", task.Project)
		}

		comments, err := meshContext.GetTaskComments(store.DB(), task.ID)
		if err == nil && len(comments) > 0 {
			fmt.Println("\nComments:")
			for _, c := range comments {
				fmt.Printf("  [%s] %s: %s\n", c.CreatedAt, c.Author, c.Message)
			}
		}
	},
}

var taskCheckoutCmd = &cobra.Command{
	Use:     "checkout [id|name]",
	Aliases: []string{"switch"},
	Short:   "Set the active task and checkout its git branch",
	Args:    cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		store, err := db.Open(cfg.DBPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening db: %v\n", err)
			os.Exit(1)
		}
		defer store.Close()

		task, err := meshContext.GetTask(store.DB(), args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting task: %v\n", err)
			os.Exit(1)
		}

		if task.IsBlocked {
			fmt.Fprintf(os.Stderr, "Cannot start a blocked task (Reason: %s)\n", task.BlockReason)
			os.Exit(1)
		}

		// Set as active repository task by touching the updated_at
		if err := meshContext.TouchTask(store.DB(), task.ID); err != nil {
			fmt.Fprintf(os.Stderr, "Error updating task: %v\n", err)
			os.Exit(1)
		}

		// Switch or create corresponding git branch
		branch := task.GitBranch
		if branch == "" {
			branch = fmt.Sprintf("feature/%s", task.ID)

			// Update the DB with the new branch name
			updateQuery := "UPDATE tasks SET git_branch = ? WHERE id = ?"
			_, _ = store.DB().Exec(updateQuery, branch, task.ID)
		}

		if task.RepoPath != "" {
			gitCmd := exec.Command("git", "checkout", branch)
			gitCmd.Dir = task.RepoPath
			if err := gitCmd.Run(); err != nil {
				// Try to create the branch
				gitCmd = exec.Command("git", "checkout", "-b", branch)
				gitCmd.Dir = task.RepoPath
				if err := gitCmd.Run(); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to checkout branch %s: %v\n", branch, err)
				}
			}
		}

		fmt.Printf("\033[1;32m✔ Task %q checked out\033[0m\n", task.ID)
	},
}

var taskBlockCmd = &cobra.Command{
	Use:   "block [id|name] [blocked-by-id...]",
	Short: "Flag a task as blocked",
	Args:  cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		store, err := db.Open(cfg.DBPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening db: %v\n", err)
			os.Exit(1)
		}
		defer store.Close()

		reason, _ := cmd.Flags().GetString("reason")
		var blockers []string
		if len(args) > 1 {
			blockers = args[1:]
		}
		if err := meshContext.BlockTask(store.DB(), args[0], reason, blockers...); err != nil {
			fmt.Fprintf(os.Stderr, "Error blocking task: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("\033[1;33m✔ Task %q marked as blocked\033[0m\n", args[0])
	},
}

var taskUnblockCmd = &cobra.Command{
	Use:   "unblock [id|name] [unblocked-by-id...]",
	Short: "Unflag a task as blocked",
	Args:  cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		store, err := db.Open(cfg.DBPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening db: %v\n", err)
			os.Exit(1)
		}
		defer store.Close()

		var unblockFrom []string
		if len(args) > 1 {
			unblockFrom = args[1:]
		}
		if err := meshContext.UnblockTask(store.DB(), args[0], unblockFrom...); err != nil {
			fmt.Fprintf(os.Stderr, "Error unblocking task: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("\033[1;32m✔ Task %q unblocked\033[0m\n", args[0])
	},
}

var taskCommentCmd = &cobra.Command{
	Use:   "comment [id|name] [message]",
	Short: "Append a structured comment/audit log note to the task",
	Args:  cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		store, err := db.Open(cfg.DBPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening db: %v\n", err)
			os.Exit(1)
		}
		defer store.Close()

		author := os.Getenv("USER")
		if author == "" {
			author = "staypoint-user"
		}

		if err := meshContext.AddTaskComment(store.DB(), args[0], author, args[1]); err != nil {
			fmt.Fprintf(os.Stderr, "Error adding comment: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("\033[1;32m✔ Comment added to task %q\033[0m\n", args[0])
	},
}

var taskCancelCmd = &cobra.Command{
	Use:     "cancel [id|name]",
	Aliases: []string{"archive"},
	Short:   "Transition task to cancelled/archived state",
	Args:    cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		store, err := db.Open(cfg.DBPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening db: %v\n", err)
			os.Exit(1)
		}
		defer store.Close()

		if err := meshContext.ArchiveTask(store.DB(), args[0]); err != nil {
			fmt.Fprintf(os.Stderr, "Error canceling task: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("\033[1;32m✔ Task %q cancelled/archived\033[0m\n", args[0])
	},
}

func init() {

	rootCmd.AddCommand(taskCmd)
	taskCmd.AddCommand(taskListCmd)
	taskCmd.AddCommand(taskAddCmd)
	taskCmd.AddCommand(taskDoneCmd)
	taskCmd.AddCommand(taskBudgetCmd)
	taskCmd.AddCommand(taskShowCmd)
	taskCmd.AddCommand(taskCheckoutCmd)
	taskCmd.AddCommand(taskBlockCmd)
	taskCmd.AddCommand(taskUnblockCmd)
	taskCmd.AddCommand(taskTreeCmd)
	taskCmd.AddCommand(taskCommentCmd)
	taskCmd.AddCommand(taskCancelCmd)

	taskBlockCmd.Flags().String("reason", "", "Reason for blocking the task")

	taskListCmd.Flags().BoolP("all", "a", false, "Include done and soft-deleted tasks")
	taskListCmd.Flags().Bool("legacy", false, "Include legacy tasks (created before task origins were tracked)")
	taskListCmd.Flags().String("stage", "", "Only tasks in this execution stage (backlog, todo, in_progress, in_review, blocked, done, cancelled)")
	taskAddCmd.Flags().Float64("budget", 0.0, "Maximum budget limit in USD")
	taskAddCmd.Flags().Int("max-turns", 0, "Maximum allowed turns")
	taskAddCmd.Flags().Bool("tui", false, "Launch Bubble Tea TUI interactive textarea")
	taskAddCmd.Flags().Bool("ai", true, "Force dynamic AI inference")
	taskAddCmd.Flags().Bool("dry-run", false, "Preview generated task without dispatching to Paperclip")
	taskAddCmd.Flags().BoolP("yes", "y", false, "Skip interactive confirmation/action card and dispatch immediately")
	taskAddCmd.Flags().BoolP("interactive", "i", false, "Force interactive disposition and clarification prompters")
	taskAddCmd.Flags().BoolP("backlog", "b", false, "Park task unassigned in backlog (zero prompts)")
	taskAddCmd.Flags().BoolP("start", "s", false, "Assign task immediately to Chief of Staff and schedule active execution")
	taskAddCmd.Flags().Bool("assign", false, "Alias for --start")
	taskAddCmd.Flags().String("company", "", "Target Paperclip company ID (defaults to PAPERCLIP_COMPANY_ID)")
	taskAddCmd.Flags().String("project", "", "Target project ID (defaults to current project)")
	taskAddCmd.Flags().String("priority", "", "Override priority (low, medium, high, urgent)")
	taskAddCmd.Flags().String("role", "", "Override assignee role or agent")
	taskBudgetCmd.Flags().Float64("usd", 0.0, "Budget limit in USD")
	taskBudgetCmd.Flags().Int("turns", 0, "Maximum allowed turns")
}

var taskTreeCmd = &cobra.Command{
	Use:   "tree",
	Short: "Show the task dependency tree",
	Run: func(cmd *cobra.Command, args []string) {
		store, err := db.Open(cfg.DBPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening db: %v\n", err)
			os.Exit(1)
		}
		defer store.Close()

		tasks, err := meshContext.ListTasks(store.DB(), true)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error listing tasks: %v\n", err)
			os.Exit(1)
		}

		fmt.Println("\033[1;36mTask Dependency Tree:\033[0m")

		var printTree func(parentID string, indent string)
		printTree = func(parentID string, indent string) {
			for _, t := range tasks {
				if t.ParentID == parentID {
					status := t.Status
					if t.IsBlocked {
						status = "blocked"
					}
					fmt.Printf("%s- %s (%s) [%s]\n", indent, t.ID, t.Name, status)
					printTree(t.ID, indent+"  ")
				}
			}
		}

		printTree("", "")
	},
}
