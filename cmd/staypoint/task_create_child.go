package main

import (
	"fmt"
	"os"
	"strings"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
)

// STA-820: `staypoint task create --parent <id> --kind coding "title"` creates
// a local child task directly (no AI inference, no Paperclip dispatch). The
// child inherits repo/branch/org/project; the daemon stores the handoff.

func init() {
	addChildCreateFlags(taskCreateCmd)
}

func addChildCreateFlags(cmd *cobra.Command) {
	cmd.Flags().String("parent", "", "Create a child of this local task ID (skips AI inference and Paperclip)")
	cmd.Flags().String("kind", "", "work_kind (with --parent or --org): "+strings.Join(meshContext.ValidWorkKinds(), ", ")+" (default coding)")
	cmd.Flags().String("handoff", "", "Plan text handed to the child (defaults to the parent's latest plan document)")
	cmd.Flags().String("handoff-file", "", "Read the handoff plan from this file")
	cmd.Flags().Bool("allow-deep", false, "Board override: allow nesting past the child depth cap (Board terminal only; refused in agent sessions)")
}

// childCreateRequested reports whether runTaskCreate should take the local
// child-task path.
func childCreateRequested(cmd *cobra.Command) bool {
	p, _ := cmd.Flags().GetString("parent")
	return strings.TrimSpace(p) != ""
}

// childTaskOptionsFromFlags builds the create options from flags and args.
func childTaskOptionsFromFlags(cmd *cobra.Command, args []string) (meshContext.ChildTaskOptions, error) {
	parent, _ := cmd.Flags().GetString("parent")
	kind, _ := cmd.Flags().GetString("kind")
	handoff, _ := cmd.Flags().GetString("handoff")
	handoffFile, _ := cmd.Flags().GetString("handoff-file")
	allowDeep, _ := cmd.Flags().GetBool("allow-deep")
	budget, _ := cmd.Flags().GetFloat64("budget")
	maxTurns, _ := cmd.Flags().GetInt("max-turns")

	if allowDeep {
		// STA-859: --allow-deep is a Board override. This path writes the DB
		// directly, so the daemon's passkey gate never sees it; refuse it in
		// agent sessions and without a terminal, as `gate override` does.
		tty := isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
		if err := refuseBoardOnlyInAgentContext("--allow-deep", os.Getenv, tty); err != nil {
			return meshContext.ChildTaskOptions{}, err
		}
	}

	title := strings.TrimSpace(strings.Join(args, " "))
	if title == "" {
		return meshContext.ChildTaskOptions{}, fmt.Errorf("a title is required: staypoint task create --parent <id> --kind coding \"title\"")
	}
	if handoffFile != "" {
		if handoff != "" {
			return meshContext.ChildTaskOptions{}, fmt.Errorf("use --handoff or --handoff-file, not both")
		}
		data, err := os.ReadFile(handoffFile)
		if err != nil {
			return meshContext.ChildTaskOptions{}, fmt.Errorf("read handoff file: %w", err)
		}
		handoff = string(data)
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "coding"
	}
	return meshContext.ChildTaskOptions{
		ParentID:      strings.TrimSpace(parent),
		Name:          title,
		WorkKind:      kind,
		Handoff:       handoff,
		MaxBudgetUSD:  budget,
		MaxTurns:      maxTurns,
		BoardOverride: allowDeep,
		Origin:        cliTaskOrigin(os.Getenv),
	}, nil
}

func runTaskCreateChild(cmd *cobra.Command, args []string) error {
	opts, err := childTaskOptionsFromFlags(cmd, args)
	if err != nil {
		return err
	}
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer store.Close()
	child, err := meshContext.CreateChildTask(store.DB(), opts)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Created child task %s (%s) under %s: %s\n", child.ID, child.WorkKind, child.ParentID, child.Name)
	if child.Identifier != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "  %s %s\n", child.Identifier, child.URL)
	}
	return nil
}
