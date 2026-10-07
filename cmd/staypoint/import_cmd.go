package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/paperclip"
	"github.com/VinnyVanGogh/staypoint/internal/paperclipimport"
	"github.com/spf13/cobra"
)

var importCmd = &cobra.Command{
	Use:   "import",
	Short: "Import work from other trackers (Board only)",
}

var importPaperclipCmd = &cobra.Command{
	Use:   "paperclip",
	Short: "Import all Paperclip issues: open as backlog, finished as archive (Board only)",
	Long: `Import every Paperclip issue from every company into StayPoint.

Open issues (backlog, todo, in_progress, in_review, blocked) go under one
parent per company, "Paperclip backlog — <Company>". Finished issues (done,
cancelled) go flat under "Paperclip archive — <Company>" (itself done) as done
or cancelled tasks closed at their Paperclip completion time. The archive is
hidden by default like legacy tasks (board "Show archive & legacy", API
include_legacy/include_archive, 'task list --all --archive') and never runs.

Both parents use the matching organization (STA -> StayPoint, MAN -> Managed Solution, RES ->
Research, PER -> Maintenance, RUN -> RuneLite). Its issues become backlog
children, unassigned, origin paperclip_import, titled "[STA-772] ...".
Paperclip parent/child links are kept up to tasks.max_child_depth; deeper
issues are flattened and say so in their description. Only the title,
description, priority, company and source reference are kept. repo_path
comes from the Paperclip project's workspace when it has one; otherwise it
is empty and the task stays in backlog until 'staypoint task set-repo'.

Re-running skips issues already imported. --dry-run reads Paperclip and the
task database read-only (no migrations) and prints what would be created.

Board only: refused inside an agent run (STAYPOINT_TASK_ID set), and the real
import asks for confirmation on a terminal.`,
	RunE: runImportPaperclip,
}

func init() {
	rootCmd.AddCommand(importCmd)
	importCmd.AddCommand(importPaperclipCmd)
	importPaperclipCmd.Flags().Bool("dry-run", false, "Print per-company counts and sample titles; write nothing")
	importPaperclipCmd.Flags().StringSlice("company", nil, "Only these companies (issue prefix or id), e.g. --company STA,MAN")
	importPaperclipCmd.Flags().String("url", "", "Paperclip API base URL (default PAPERCLIP_API_URL or http://127.0.0.1:3100)")
	importPaperclipCmd.Flags().Duration("timeout", 3*time.Minute, "Per-request Paperclip timeout (the API is slow)")
	importPaperclipCmd.Flags().Int("sample", 5, "Titles shown per company in the dry run")
	importPaperclipCmd.Flags().Bool("full-archive-descriptions", false, "Also fetch full text for truncated archived descriptions (one slow Paperclip request each)")
}

// importConfirm reads the confirmation; tests replace it.
var importConfirm = func(in io.Reader, out io.Writer, n int) bool {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		fmt.Fprintln(out, "import paperclip: stdin is not a terminal; the Board must confirm the import interactively")
		return false
	}
	fmt.Fprintf(out, "Create %d backlog tasks? Type 'import' to continue: ", n)
	line, _ := bufio.NewReader(in).ReadString('\n')
	return strings.TrimSpace(line) == "import"
}

func runImportPaperclip(cmd *cobra.Command, _ []string) error {
	if os.Getenv("STAYPOINT_TASK_ID") != "" {
		return fmt.Errorf("import paperclip: refused in agent context (STAYPOINT_TASK_ID is set); this is a Board command")
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	companies, _ := cmd.Flags().GetStringSlice("company")
	baseURL, _ := cmd.Flags().GetString("url")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	sample, _ := cmd.Flags().GetInt("sample")
	fullArchive, _ := cmd.Flags().GetBool("full-archive-descriptions")
	opts := paperclipimport.Options{Companies: companies, FullArchiveDescriptions: fullArchive}
	out := cmd.OutOrStdout()

	client := paperclip.NewClient(baseURL, "")
	client.HTTPClient.Timeout = timeout
	ctx := context.Background()

	if dryRun {
		conn, err := db.OpenReadOnly(cfg.DBPath)
		if err != nil {
			return err
		}
		defer conn.Close()
		plan, err := paperclipimport.BuildPlan(ctx, client, conn, opts)
		if err != nil {
			return err
		}
		printImportPlan(out, plan, sample, true)
		return nil
	}

	store, err := db.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer store.Close()
	plan, err := paperclipimport.BuildPlan(ctx, client, store.DB(), opts)
	if err != nil {
		return err
	}
	printImportPlan(out, plan, sample, false)
	if plan.Total() == 0 {
		fmt.Fprintln(out, "Nothing to import.")
		return nil
	}
	if !importConfirm(cmd.InOrStdin(), out, plan.Total()) {
		return fmt.Errorf("import paperclip: not confirmed; nothing written")
	}
	res, err := paperclipimport.Apply(ctx, client, store.DB(), plan, time.Now())
	if res != nil {
		seen := map[string]bool{}
		var orgs []string
		total, archived := 0, 0
		for _, m := range []map[string]int{res.TasksCreated, res.Archived} {
			for org := range m {
				if !seen[org] {
					seen[org] = true
					orgs = append(orgs, org)
				}
			}
		}
		sort.Strings(orgs)
		for _, org := range orgs {
			fmt.Fprintf(out, "  %s: %d open, %d archived\n", org, res.TasksCreated[org], res.Archived[org])
			total += res.TasksCreated[org]
			archived += res.Archived[org]
		}
		fmt.Fprintf(out, "Imported %d open and %d archived tasks (%d parent tasks created, %d skipped as already imported).\n", total, archived, res.ParentsCreated, res.Skipped)
	}
	return err
}

func printImportPlan(out io.Writer, plan *paperclipimport.Plan, sample int, dryRun bool) {
	if dryRun {
		fmt.Fprintln(out, "Paperclip import (dry run): nothing is written.")
	}
	fmt.Fprintf(out, "Depth cap tasks.max_child_depth = %d (company parent is depth 0)\n\n", plan.MaxDepth)
	for _, c := range plan.Companies {
		statuses := make([]string, 0, len(c.ByStatus))
		for s, n := range c.ByStatus {
			statuses = append(statuses, fmt.Sprintf("%s %d", s, n))
		}
		sort.Strings(statuses)
		fmt.Fprintf(out, "%s (%s) -> organization %q\n", c.Company.Name, c.Company.IssuePrefix, c.Organization)
		fmt.Fprintf(out, "  open %d [%s]; already imported %d; to import %d under %s (flattened %d, repo inferred %d)\n",
			c.Open, strings.Join(statuses, ", "), c.AlreadyImported, len(c.ToImport), parentLabel(c.ParentTaskID), c.Flattened, c.WithRepo)
		closed := make([]string, 0, len(c.ClosedByStatus))
		for s, n := range c.ClosedByStatus {
			closed = append(closed, fmt.Sprintf("%s %d", s, n))
		}
		sort.Strings(closed)
		fmt.Fprintf(out, "  archived %d [%s]; already imported %d; to archive %d under %s\n",
			c.Closed, strings.Join(closed, ", "), c.ArchivedImported, len(c.ToArchive), parentLabel(c.ArchiveTaskID))
		for i, pi := range c.ToImport {
			if i >= sample {
				fmt.Fprintf(out, "    ... %d more\n", len(c.ToImport)-sample)
				break
			}
			title := paperclipimport.TaskTitle(pi.Issue)
			if len(title) > 100 {
				title = title[:97] + "..."
			}
			fmt.Fprintf(out, "    %s\n", title)
		}
	}
	fmt.Fprintf(out, "\nTotal to import: %d open + %d archived = %d tasks across %d companies\n", plan.ToImport(), plan.ToArchive(), plan.Total(), len(plan.Companies))
}

func parentLabel(id string) string {
	if id == "" {
		return "a new parent"
	}
	return id
}
