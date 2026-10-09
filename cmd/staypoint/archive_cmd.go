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

func init() {
	rootCmd.AddCommand(archiveCmd)
	archiveCmd.AddCommand(archiveRunCmd, archiveStatusCmd)
	for _, c := range []*cobra.Command{archiveRunCmd, archiveStatusCmd} {
		c.Flags().Bool("json", false, "Print JSON")
	}
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
