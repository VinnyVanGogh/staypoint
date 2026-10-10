package main

import (
	"fmt"
	"os"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/taskopen"
	"github.com/spf13/cobra"
)

// task-63a9779d: `staypoint task open` (alias `staypoint open`) opens task
// pages in the Board's configured browser profile. It replaces the
// ~/.local/bin/sp-open stopgap.

func newTaskOpenCmd(use string) *cobra.Command {
	c := &cobra.Command{
		Use:   use + " <id|ref|name>...",
		Short: "Open task pages in the configured browser profile",
		Long: `Open one or more task pages of the StayPoint web UI in one browser call.

Each argument is a task id (task-1234abcd), a reference recorded on the task
(STA-775), or the exact task name. Every argument must name exactly one task,
or nothing is opened.

The browser comes from the [browser] table of ~/.staypoint/config.toml:

  [browser]
  app = "Microsoft Edge"
  profile_directory = "Profile 4"   # or: profile = "Main"

With no [browser] app the macOS default browser is used. Edge and Chrome with
a profile open the pages in that profile's window; other apps use 'open -a'.

Agents may run this to put a page in front of the Board (Run Now, approve a
card). It only opens tabs; it changes no data.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			printOnly, _ := cmd.Flags().GetBool("print")
			store, err := db.Open(cfg.DBPath)
			if err != nil {
				return fmt.Errorf("open db: %w", err)
			}
			defer store.Close()
			targets, err := taskopen.Resolve(store.DB(), args)
			if err != nil {
				return err
			}
			if !printOnly {
				home, _ := os.UserHomeDir()
				if err := taskopen.Open(cfg.Browser, targets, home, taskopen.Launch); err != nil {
					return err
				}
			}
			for _, t := range targets {
				fmt.Fprintln(cmd.OutOrStdout(), t.URL)
			}
			return nil
		},
	}
	c.Flags().Bool("print", false, "Print the task URLs without opening a browser")
	return c
}

func init() {
	taskCmd.AddCommand(newTaskOpenCmd("open"))
	rootCmd.AddCommand(newTaskOpenCmd("open"))
}
