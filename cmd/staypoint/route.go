package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/spf13/cobra"
)

var routeCmd = &cobra.Command{
	Use:   "route [cwd]",
	Short: "Dynamic routing recommendation and quota-aware dispatch engine",
	Args:  cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		evalFlag, _ := cmd.Flags().GetBool("eval")
		jsonFlag, _ := cmd.Flags().GetBool("json")
		noSSH, _ := cmd.Flags().GetBool("no-ssh")

		cwd := ""
		if len(args) > 0 {
			cwd = args[0]
		}
		if cwd == "" {
			cwd, _ = os.Getwd()
		}

		remoteHost := "company-mbp"
		if cfg != nil && cfg.RemoteHost != "" {
			remoteHost = cfg.RemoteHost
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		pacerState, err := router.LoadPacerState()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading pacer state: %v\n", err)
			os.Exit(1)
		}

		modelFlag, _ := cmd.Flags().GetString("model")
		effortFlag, _ := cmd.Flags().GetString("effort")
		priorityFlag, _ := cmd.Flags().GetString("priority")
		highPriorityFlag, _ := cmd.Flags().GetBool("high-priority")
		isHighPriority := highPriorityFlag || strings.EqualFold(priorityFlag, "high")

		decision, err := router.Route(ctx, cwd, pacerState, router.RouteOptions{
			CheckSSH:        !noSSH,
			RemoteHost:      remoteHost,
			PreferredModel:  modelFlag,
			PreferredEffort: effortFlag,
			HighPriority:    isHighPriority,
			UIOLI:           router.UIOLIFromConfig(cfg),
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Routing error: %v\n", err)
			os.Exit(1)
		}

		if jsonFlag {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(decision)
			return
		}

		if evalFlag {
			fmt.Printf("export STAYPOINT_ROUTE_TARGET=%q\n", decision.Target)
			fmt.Printf("export STAYPOINT_ROUTE_TOOL=%q\n", decision.Tool)
			fmt.Printf("export STAYPOINT_ROUTE_MODEL=%q\n", decision.Model)
			fmt.Printf("export STAYPOINT_ROUTE_COMMAND=%q\n", decision.Command)
			fmt.Printf("export STAYPOINT_ROUTE_WORKSPACE=%q\n", decision.Workspace)
			fmt.Printf("export STAYPOINT_ROUTE_ACCOUNT=%q\n", decision.AccountRole)
			fmt.Printf("export STAYPOINT_ROUTE_EMAIL=%q\n", decision.AccountEmail)
			fmt.Printf("export STAYPOINT_ROUTE_IS_WORK=%t\n", decision.IsWorkRepo)
			fmt.Printf("export STAYPOINT_ROUTE_REASON=%q\n", decision.Reason)
			// Deprecated legacy aliases
			fmt.Printf("export MESH_ROUTE_TARGET=%q\n", decision.Target)
			fmt.Printf("export MESH_ROUTE_TOOL=%q\n", decision.Tool)
			fmt.Printf("export MESH_ROUTE_MODEL=%q\n", decision.Model)
			fmt.Printf("export MESH_ROUTE_COMMAND=%q\n", decision.Command)
			fmt.Printf("export MESH_ROUTE_WORKSPACE=%q\n", decision.Workspace)
			fmt.Printf("export MESH_ROUTE_ACCOUNT=%q\n", decision.AccountRole)
			fmt.Printf("export MESH_ROUTE_EMAIL=%q\n", decision.AccountEmail)
			fmt.Printf("export MESH_ROUTE_IS_WORK=%t\n", decision.IsWorkRepo)
			fmt.Printf("export MESH_ROUTE_REASON=%q\n", decision.Reason)
			return
		}

		// Human-readable output
		fmt.Println("\033[1;36m[Staypoint :: Dynamic Router]\033[0m")
		fmt.Printf("  • Workspace:          %s\n", decision.Workspace)
		if decision.IsWorkRepo {
			fmt.Printf("  • Context Type:       \033[1;32mEnterprise Work Repo\033[0m (%s)\n", decision.WorkRepoSource)
			if decision.SSHReachable {
				fmt.Printf("  • Node Reachability:  \033[1;32m✔ %s is reachable via SSH\033[0m\n", decision.RemoteHost)
			} else if !noSSH {
				fmt.Printf("  • Node Reachability:  \033[1;31m✖ %s unreachable via SSH\033[0m (using local fallback)\n", decision.RemoteHost)
			}
		} else {
			fmt.Printf("  • Context Type:       \033[1;34mPersonal Development\033[0m\n")
		}

		fmt.Printf("  • Recommended Target: \033[1;32m%s\033[0m (tool: \033[1m%s\033[0m, model: \033[1m%s\033[0m)\n",
			decision.Target, decision.Tool, decision.Model)
		fmt.Printf("  • Dispatch Command:   \033[1;33m%s\033[0m\n", decision.Command)
		fmt.Printf("  • Routing Rationale:  %s\n", decision.Reason)

		if len(decision.Warnings) > 0 {
			for _, w := range decision.Warnings {
				fmt.Printf("  • \033[1;33m⚠ Warning:\033[0m           %s\n", w)
			}
		}

		// Quota Headroom breakdown
		if pacerState != nil {
			fmt.Println("\n\033[1m[Quota Headroom & Turns Runway]\033[0m")
			for _, poolID := range []router.PoolID{router.PoolGeminiNative, router.PoolWorkClaude, router.PoolPersonalClaude, router.Pool3PClaude} {
				pool := pacerState.Pools[poolID]
				if pool == nil {
					continue
				}
				statusIcon := "\033[1;32m✔\033[0m"
				if pool.IsLocked {
					statusIcon = "\033[1;31m🔒\033[0m"
				} else if pool.Weekly.Known && pool.Weekly.RemainingPct < 10 {
					statusIcon = "\033[1;33m⚠\033[0m"
				}
				fmt.Printf("  %s %-18s | 5h Left: %5s | Week Left: %5s | Runway: %3d turns",
					statusIcon, pool.Name, pool.FiveHour.FormatPct(true, 1), pool.Weekly.FormatPct(true, 1), pool.TurnsRunway)
				if pool.IsLocked {
					fmt.Printf(" (resets %s)", router.FormatReset(pool.LockoutUntil, time.Now()))
				}
				fmt.Println()
			}
		}
	},
}

func init() {
	rootCmd.AddCommand(routeCmd)
	routeCmd.Flags().BoolP("eval", "e", false, "Output recommendation as shell environment variables for eval")
	routeCmd.Flags().BoolP("json", "j", false, "Output recommendation in JSON format")
	routeCmd.Flags().Bool("no-ssh", false, "Skip SSH connectivity probe for work repo routing")
	routeCmd.Flags().StringP("model", "m", "", "Target model tier (e.g. opus, sonnet, claude-opus-5, claude-sonnet-4-6)")
	routeCmd.Flags().String("effort", "", "Reasoning effort (high, medium, low)")
	routeCmd.Flags().Bool("high-priority", false, "Treat task as high-priority (steers to top-tier model under UIOLI)")
	routeCmd.Flags().StringP("priority", "p", "", "Task priority tier (e.g. high, medium, low)")
}
