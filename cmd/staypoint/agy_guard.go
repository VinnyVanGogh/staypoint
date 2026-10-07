package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/spf13/cobra"
)

// STA-854 / Board decision 2026-10-06: Gemini (agy) never writes code in a
// Managed Solution work repo. Every interactive agy launch path — `staypoint
// --gemini`, the generated `agy`/`ai` shell wrappers (including `agy --force`),
// and `staypoint --continue/--resume` into an agy session — refuses in a work
// repo, attached task or not. The agy PreToolUse hook denies code writes there
// as defense in depth.

// agyWorkRepoRefusal returns a non-nil error when agy must not launch in dir.
func agyWorkRepoRefusal(dir string, isWorkRepo func(string) bool) error {
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if !isWorkRepo(dir) {
		return nil
	}
	return fmt.Errorf("STAYPOINT: agy (Gemini) is not allowed in Managed Solution work repos (%s).\n"+
		"Board decision 2026-10-06: Gemini never writes code in work repos, with or without a task.\n"+
		"Use Claude on the work seat instead:  claude --work", dir)
}

func routerIsWorkRepo(dir string) bool {
	ok, _, err := router.IsWorkRepo(dir)
	return err == nil && ok
}

// isAgyBinary reports whether a launch target is agy.
func isAgyBinary(name string) bool {
	return strings.EqualFold(filepath.Base(name), "agy")
}

// exitIfAgyInWorkRepo stops the process before an agy exec in a work repo.
func exitIfAgyInWorkRepo(bin, dir string) {
	if !isAgyBinary(bin) {
		return
	}
	if err := agyWorkRepoRefusal(dir, routerIsWorkRepo); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// agyGuardCmd backs the shell wrappers: exit 1 (with the message) when agy
// must not launch in the given directory.
var agyGuardCmd = &cobra.Command{
	Use:    "agy-guard [dir]",
	Short:  "Exit non-zero if agy (Gemini) may not launch in dir (work repos)",
	Hidden: true,
	Args:   cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		dir := ""
		if len(args) == 1 {
			dir = args[0]
		}
		if err := agyWorkRepoRefusal(dir, routerIsWorkRepo); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	},
}

func init() {
	rootCmd.AddCommand(agyGuardCmd)
}
