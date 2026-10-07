package main

import (
	"fmt"
	"os"
	"time"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/VinnyVanGogh/staypoint/internal/telemetry"
	"github.com/VinnyVanGogh/staypoint/internal/telemetry/quota"
	"github.com/spf13/cobra"
)

var quotaCmd = &cobra.Command{
	Use:     "quota",
	Aliases: []string{"spend"},
	Short:   "Granular breakdown of model spend velocity and dollar burn",
	Long:    "Displays rate limit reset countdowns (Anthropic Tier 4 / Google AI Studio) and model dollar burn by task/project.",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("\033[1;36m[StayPoint Quota & Spend Report]\033[0m")
		fmt.Println("======================================================")

		// 0. Live provider quota (throttled in-process poll, cached in SQLite)
		if cfg != nil && cfg.DBPath != "" {
			fmt.Println("\n\033[1;35m--- Live Provider Quota ---\033[0m")
			telemetry.PollQuotas(cmd.Context())
			if database, err := db.Open(cfg.DBPath); err != nil {
				fmt.Printf("Warning: Could not open db: %v\n", err)
			} else {
				renderLiveQuota(os.Stdout, quota.Store{DB: database.DB()}, time.Now())
				database.Close()
			}
		}

		// 1. Quotas and Rate Limits
		fmt.Println("\n\033[1;35m--- API Rate Limits & Pacer State ---\033[0m")
		pacerState, err := router.LoadPacerState()
		if err != nil {
			fmt.Printf("Warning: Could not load pacer state: %v\n", err)
		} else {
			now := time.Now()
			for _, pool := range pacerState.Pools {
				fmt.Printf("\n\033[1mProvider:\033[0m %s\n", pool.Name)
				if pool.AccountEmail != "" {
					fmt.Printf("  Account: %s\n", pool.AccountEmail)
				}

				statusStr := "\033[0;32mActive\033[0m"
				if pool.IsLocked {
					statusStr = fmt.Sprintf("\033[0;31mLocked\033[0m (Reason: %s)", pool.LockoutReason)
				}
				fmt.Printf("  Status:  %s\n", statusStr)
				fmt.Printf("  Runway:  %d turns\n", pool.TurnsRunway)

				// 5-Hour Window
				fmt.Printf("  [5-Hour Window] Used: %5.1f%% | Remaining: %5.1f%%\n", pool.FiveHour.UsedPct, pool.FiveHour.RemainingPct)
				if !pool.FiveHour.ResetsAt.IsZero() && pool.FiveHour.ResetsAt.After(now) {
					fmt.Printf("                  Resets in: %s (at %s)\n", router.FormatDuration(pool.FiveHour.ResetsAt.Sub(now)), pool.FiveHour.ResetsAt.Format("15:04:05"))
				}

				// Weekly Window
				fmt.Printf("  [Weekly Window] Used: %5.1f%% | Remaining: %5.1f%%\n", pool.Weekly.UsedPct, pool.Weekly.RemainingPct)
				if !pool.Weekly.ResetsAt.IsZero() && pool.Weekly.ResetsAt.After(now) {
					fmt.Printf("                  Resets in: %s (at %s)\n", router.FormatDuration(pool.Weekly.ResetsAt.Sub(now)), pool.Weekly.ResetsAt.Format("Jan 02, 15:04"))
				}
			}
		}

		// 2. Dollar Burn by Task
		fmt.Println("\n\033[1;35m--- Task Spend Velocity & Dollar Burn ---\033[0m")
		if cfg != nil && cfg.DBPath != "" {
			database, err := db.Open(cfg.DBPath)
			if err != nil {
				fmt.Printf("Warning: Could not open db: %v\n", err)
				return
			}
			defer database.Close()

			tasks, err := meshContext.ListTasks(database.DB(), true)
			if err != nil {
				fmt.Printf("Warning: Could not list tasks: %v\n", err)
				return
			}
			// Legacy tasks and archived imports are hidden like every list.
			tasks = meshContext.FilterLegacy(tasks, false)

			if len(tasks) == 0 {
				fmt.Println("No tasks found.")
				return
			}

			fmt.Printf("%-20s %-20s %-12s %-10s %-10s %-10s %-20s\n", "TASK ID", "NAME", "STATUS", "SPENT($)", "BUDGET($)", "TURNS", "PROJECT")
			fmt.Println(stringsRepeat("-", 108))
			var totalSpent float64
			for _, t := range tasks {
				totalSpent += t.SpentUSD

				statusColor := "\033[0m"
				if t.Status == "active" {
					statusColor = "\033[0;32m"
				} else if t.Status == "done" {
					statusColor = "\033[0;34m"
				} else if t.Status == "soft_deleted" {
					statusColor = "\033[0;90m"
				}

				eval := meshContext.EvaluateTaskBudget(&t)
				spendColor := "\033[0;32m"
				if eval.IsBlocked {
					spendColor = "\033[0;31m"
				} else if eval.IsWarning {
					spendColor = "\033[0;33m"
				}

				fmt.Printf("%-20.20s %-20.20s %s%-12s\033[0m %s$%8.2f\033[0m $%8.2f %10d %-20.20s\n",
					t.ID,
					t.Name,
					statusColor, t.Status,
					spendColor, t.SpentUSD,
					t.MaxBudgetUSD,
					t.SpentTurns,
					t.Project,
				)
			}
			fmt.Println(stringsRepeat("=", 108))
			fmt.Printf("Total Task Spend: \033[1;32m$%.2f\033[0m\n", totalSpent)
		} else {
			fmt.Println("Database path not configured. Cannot display task burn.")
		}
	},
}

func init() {
	rootCmd.AddCommand(quotaCmd)
}

func stringsRepeat(s string, count int) string {
	res := ""
	for i := 0; i < count; i++ {
		res += s
	}
	return res
}
