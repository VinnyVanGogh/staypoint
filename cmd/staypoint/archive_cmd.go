package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/VinnyVanGogh/staypoint/internal/archive"
	"github.com/spf13/cobra"
)

var archiveCmd = &cobra.Command{
	Use:   "archive",
	Short: "Compressed, redacted archive of every agent transcript",
	Long: `The daemon copies Claude Code (both profiles), Antigravity and Gemini CLI
transcripts into ~/.staypoint/archive (zstd, secrets redacted, mode 0700,
excluded from Time Machine) shortly after it starts and every night at 03:00.
Originals are only read, never deleted. STAYPOINT_ARCHIVE=off disables the
daemon pass.`,
}

var archiveRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Archive new and changed transcripts now",
	RunE: func(cmd *cobra.Command, _ []string) error {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		a, err := archive.Open(archive.Options{
			Dir:               archive.DefaultDir(cfg.DataDir),
			Sources:           archive.DefaultSources(home),
			ExcludeFromBackup: true,
		})
		if err != nil {
			return err
		}
		defer a.Close()
		rep, err := a.Run(context.Background())
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
			return json.NewEncoder(out).Encode(rep)
		}
		fmt.Fprintf(out, "Scanned %d, archived %d, unchanged %d, failed %d (%s in, %s stored)\n",
			rep.Scanned, rep.Archived, rep.Unchanged, rep.Failed, human(rep.SrcBytes), human(rep.StoredBytes))
		for _, e := range rep.Errors {
			fmt.Fprintln(out, "  error:", e)
		}
		st, err := archive.ComputeStats(a.DB())
		if err != nil {
			return err
		}
		printArchiveStats(out, st)
		return nil
	},
}

var archiveStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Archive size, compression ratio and projected GB/year",
	RunE: func(cmd *cobra.Command, _ []string) error {
		conn, err := archive.OpenIndexReadOnly(archive.DefaultDir(cfg.DataDir))
		if err != nil {
			return fmt.Errorf("no archive yet (run 'staypoint archive run'): %w", err)
		}
		defer conn.Close()
		st, err := archive.ComputeStats(conn)
		if err != nil {
			return err
		}
		if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(st)
		}
		printArchiveStats(cmd.OutOrStdout(), st)
		return nil
	},
}

var archivePaperclipCmd = &cobra.Command{
	Use:   "paperclip",
	Short: "One-time export of the frozen Paperclip database (Board only)",
	Long: `Dump every public table of Paperclip's Postgres (issues, comments, runs,
decisions, ...) to ~/.staypoint/archive/paperclip/<timestamp>/<table>.jsonl.zst
with a manifest.json of row counts and checksums, so ~/.paperclip can later
be cleaned without losing history. Every statement runs in a read-only
transaction; nothing in Paperclip is changed or deleted. Secrets are redacted.
Stops before a table if less than 1 GiB of disk is free.

Paperclip's embedded Postgres must be running (start Paperclip). Run with
--dry-run first to see the tables and their sizes; --exclude skips bulky
tables you don't need (e.g. raw run logs).`,
	RunE: runArchivePaperclip,
}

func init() {
	rootCmd.AddCommand(archiveCmd)
	archiveCmd.AddCommand(archiveRunCmd, archiveStatusCmd, archivePaperclipCmd)
	for _, c := range []*cobra.Command{archiveRunCmd, archiveStatusCmd} {
		c.Flags().Bool("json", false, "Print JSON")
	}
	archivePaperclipCmd.Flags().String("dsn", "", "Postgres URL (default PAPERCLIP_DATABASE_URL or "+archive.DefaultPaperclipDSN+")")
	archivePaperclipCmd.Flags().String("psql", "", "psql binary (default PATH, then ~/.paperclip)")
	archivePaperclipCmd.Flags().StringSlice("tables", nil, "Only these tables")
	archivePaperclipCmd.Flags().StringSlice("exclude", nil, "Skip these tables")
	archivePaperclipCmd.Flags().Bool("dry-run", false, "List tables and sizes; write nothing")
}

func runArchivePaperclip(cmd *cobra.Command, _ []string) error {
	if os.Getenv("STAYPOINT_TASK_ID") != "" {
		return fmt.Errorf("archive paperclip: refused in agent context (STAYPOINT_TASK_ID is set); this is a Board command")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	o := archive.PaperclipOptions{OutDir: archive.DefaultDir(cfg.DataDir)}
	o.DSN, _ = cmd.Flags().GetString("dsn")
	if o.DSN == "" {
		o.DSN = os.Getenv("PAPERCLIP_DATABASE_URL")
	}
	if o.DSN == "" {
		o.DSN = archive.DefaultPaperclipDSN
	}
	if o.PSQL, _ = cmd.Flags().GetString("psql"); o.PSQL == "" {
		if o.PSQL, err = archive.FindPSQL(home); err != nil {
			return err
		}
	}
	o.Tables, _ = cmd.Flags().GetStringSlice("tables")
	o.Exclude, _ = cmd.Flags().GetStringSlice("exclude")
	out := cmd.OutOrStdout()
	ctx := context.Background()

	if dry, _ := cmd.Flags().GetBool("dry-run"); dry {
		tables, err := archive.PlanPaperclip(ctx, o)
		if err != nil {
			return err
		}
		var total int64
		for _, t := range tables {
			fmt.Fprintf(out, "  %-40s %10s\n", t.Name, human(t.SourceBytes))
			total += t.SourceBytes
		}
		fmt.Fprintf(out, "%d tables, %s in Postgres (export is compressed JSON, usually far smaller)\n", len(tables), human(total))
		return nil
	}
	m, err := archive.ExportPaperclip(ctx, o)
	if m != nil {
		var rows, stored int64
		for _, t := range m.Tables {
			fmt.Fprintf(out, "  %-40s %10d rows %10s\n", t.Name, t.Rows, human(t.StoredBytes))
			rows += t.Rows
			stored += t.StoredBytes
		}
		fmt.Fprintf(out, "%d tables, %d rows, %s stored in %s (complete=%v)\n", len(m.Tables), rows, human(stored), m.Dir, m.Complete)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "Verify the manifest before cleaning ~/.paperclip; this command never deletes it.")
	return nil
}

func printArchiveStats(out io.Writer, st *archive.Stats) {
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SOURCE\tTRANSCRIPTS\tONLY IN ARCHIVE\tORIGINAL\tSTORED\tOLDEST\tNEWEST")
	for _, s := range st.Sources {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\t%s\t%s\n", s.Source, s.Transcripts, s.OnlyInArchive,
			human(s.SrcBytes), human(s.StoredBytes), s.Oldest, s.Newest)
	}
	tw.Flush()
	fmt.Fprintf(out, "Total %d transcripts: %s -> %s (%.1fx), %.1f days covered, projected %.2f GB/year stored\n",
		st.Transcripts, human(st.SrcBytes), human(st.StoredBytes), st.Ratio, st.SpanDays, st.ProjectedGBPerYear)
	if st.LastRun != "" {
		fmt.Fprintln(out, "Last pass:", st.LastRun)
	}
	if st.Transcripts > 0 {
		fmt.Fprintln(out, "Every archived copy is checksum-verified; keep cleanupPeriodDays low (7) in ~/.claude*/settings.json so raw transcripts don't refill the disk.")
	}
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
