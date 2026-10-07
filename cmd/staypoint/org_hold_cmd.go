package main

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

// Org hold (2026-10-07). Placing a hold only stops work, so the Board may do
// it from its own terminal; lifting one restarts work, so it needs Touch ID
// and is done in the Board UI (Settings / the organization card). Both are
// refused inside agent sessions.

var orgCmd = &cobra.Command{
	Use:   "org",
	Short: "Organization controls (Board-only hold)",
}

var orgHoldCmd = &cobra.Command{
	Use:   "hold <organization>",
	Short: "Put an organization on hold: no task in it is claimed or woken until the Board lifts the hold",
	Long: `Put an organization on hold. While it is held the daemon claims no task in
that organization: assignment, comment, blocker and Run Now wakes are refused
and logged as held on the task.

Board-only: refused inside agent sessions and without a terminal. Lifting the
hold needs Touch ID, so it is done in the Board UI (Settings, or the HELD badge
on the organization card).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		tty := isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
		release, _ := cmd.Flags().GetBool("release")
		store, err := db.Open(cfg.DBPath)
		if err != nil {
			return fmt.Errorf("open db: %w", err)
		}
		defer store.Close()
		return runOrgHold(cmd.OutOrStdout(), store.DB(), args[0], release, os.Getenv, tty)
	},
}

var orgHoldsCmd = &cobra.Command{
	Use:   "holds",
	Short: "List organizations on hold",
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := db.Open(cfg.DBPath)
		if err != nil {
			return fmt.Errorf("open db: %w", err)
		}
		defer store.Close()
		held, err := governance.HeldOrgs(store.DB())
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if len(held) == 0 {
			fmt.Fprintln(out, "No organization is on hold.")
			return nil
		}
		for _, o := range held {
			fmt.Fprintf(out, "%s  HELD\n", o)
		}
		return nil
	},
}

func runOrgHold(out io.Writer, conn *sql.DB, org string, release bool, getenv func(string) string, tty bool) error {
	org = strings.TrimSpace(org)
	if org == "" {
		return fmt.Errorf("an organization is required")
	}
	if err := refuseBoardOnlyInAgentContext("org hold", getenv, tty); err != nil {
		return err
	}
	if release {
		return fmt.Errorf("org hold: lifting a hold needs Touch ID; lift it in the Board UI (Settings → Organization holds)")
	}
	if err := governance.SetOrgHold(conn, org, true); err != nil {
		return err
	}
	_ = governance.LogBoardEvent(conn, "board", governance.AuditBoardAction,
		map[string]string{"action": "update_org_hold", "organization": org, "held": "true", "via": "cli"})
	fmt.Fprintf(out, "%s is on hold: no task in it will be claimed or woken. Lift it in the Board UI (Touch ID).\n", org)
	return nil
}

func init() {
	orgHoldCmd.Flags().Bool("release", false, "Lift the hold (refused here: needs Touch ID in the Board UI)")
	orgCmd.AddCommand(orgHoldCmd)
	orgCmd.AddCommand(orgHoldsCmd)
	rootCmd.AddCommand(orgCmd)
}
