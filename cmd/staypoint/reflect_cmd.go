package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/archive"
	"github.com/VinnyVanGogh/staypoint/internal/reflection"
	"github.com/spf13/cobra"
)

var reflectCmd = &cobra.Command{
	Use:   "reflect",
	Short: "Look back over the last month, quarter, half or year",
	Long: `Computed facts for the period first: tasks shipped by org, merged ship
reviews, run hours, tokens and cost by seat and model, gate requests, Board
decisions, failures, blocked reasons and the most-touched repos.

--summary then asks Claude on the personal seat (subscription login, never an
API key, tools disabled) to summarise a sampled, size-capped corpus of Board
comments, failures, decisions and archived transcript messages into themes,
frustrations, breakages, decisions and suggestions. Every claim cites its
evidence; uncited claims are dropped. Work data (Managed Solution tasks, work
repos, the work profile) is left out unless --include-work. Without
--summary the last cached summary for the period is shown.`,
	RunE: runReflect,
}

func init() {
	rootCmd.AddCommand(reflectCmd)
	reflectCmd.Flags().String("since", "30d", "Period: 30d, 90d, 180d, 365d (or Nd, Nw, Nm, 1y)")
	reflectCmd.Flags().Bool("summary", false, "Run the LLM summary on the personal Claude seat (takes a few minutes)")
	reflectCmd.Flags().Bool("include-work", false, "Let work data into the summary corpus (Board decision)")
	reflectCmd.Flags().String("model", "sonnet", "Claude model for the summary")
	reflectCmd.Flags().Int("max-chars", 60000, "Corpus size cap")
	reflectCmd.Flags().Bool("json", false, "Print JSON")
}

type reflectOutput struct {
	Facts   *reflection.Facts   `json:"facts"`
	Summary *reflection.Summary `json:"summary,omitempty"`
}

func runReflect(cmd *cobra.Command, _ []string) error {
	period, _ := cmd.Flags().GetString("since")
	days, err := reflection.ParsePeriod(period)
	if err != nil {
		return err
	}
	src, closeAll := reflection.OpenSources(cfg)
	defer closeAll()
	now := time.Now()
	out := reflectOutput{Facts: reflection.Compute(src, days, now)}
	dir := archive.DefaultDir(cfg.DataDir)

	if want, _ := cmd.Flags().GetBool("summary"); want {
		includeWork, _ := cmd.Flags().GetBool("include-work")
		model, _ := cmd.Flags().GetString("model")
		maxChars, _ := cmd.Flags().GetInt("max-chars")
		opts := reflection.CorpusOptions{IncludeWork: includeWork, MaxChars: maxChars}
		corpus, err := reflection.BuildCorpus(src, dir, out.Facts, opts)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "Summarising %d evidence items (%d chars) on the personal Claude seat…\n", len(corpus.Items), corpus.Chars)
		s, err := reflection.Summarize(context.Background(), reflection.PersonalClaude(model, 10*time.Minute), out.Facts, corpus, model, opts, now)
		if err != nil {
			return err
		}
		if err := reflection.SaveSummary(dir, s); err != nil {
			return err
		}
		out.Summary = s
	} else if s, err := reflection.LoadSummary(dir, days); err == nil {
		out.Summary = s
	}

	w := cmd.OutOrStdout()
	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	printReflect(w, out)
	return nil
}

func printReflect(w io.Writer, o reflectOutput) {
	f := o.Facts
	fmt.Fprintf(w, "Reflect: last %d days (%s → %s)\n\n", f.Days, f.Since.Format("2006-01-02"), f.Until.Format("2006-01-02"))
	fmt.Fprintf(w, "Tasks shipped %d (created %d) · merged ship reviews %d · PRs recorded %d\n", f.TasksShippedN, f.TasksCreatedN, f.MergedReviewsN, f.PRLinks)
	fmt.Fprintf(w, "Runs %d · %.1f run hours · %s tokens · $%.2f\n", f.Runs, f.RunHours, compact(f.TotalTokens), f.TotalCostUSD)
	section(w, "Shipped by org", f.TasksShipped)
	section(w, "Merged by org", f.MergedReviews)
	if len(f.Usage) > 0 {
		fmt.Fprintln(w, "\nUsage by seat / model")
		for _, u := range f.Usage {
			fmt.Fprintf(w, "  %-9s %-28s %8s tokens  $%8.2f  (%d req)\n", u.Seat, u.Model, compact(u.Tokens), u.CostUSD, u.Requests)
		}
	}
	section(w, "Gate requests", f.GateRequests)
	section(w, "Board decisions", f.BoardDecisions)
	section(w, "Board actions", f.BoardEvents)
	section(w, "Run failures", f.RunFailures)
	section(w, "Blocked reasons", f.BlockedReasons)
	section(w, "Most-touched repos", f.Repos)
	section(w, "Archived sessions by profile", f.Sessions)
	for _, warn := range f.Warnings {
		fmt.Fprintln(w, "\n  note:", warn)
	}
	s := o.Summary
	if s == nil {
		fmt.Fprintln(w, "\nNo summary yet for this period: run with --summary.")
		return
	}
	fmt.Fprintf(w, "\nSummary (%s, %s seat, %d evidence items%s, generated %s)\n", s.Model, s.Seat, s.CorpusItems,
		map[bool]string{true: ", includes work data", false: ""}[s.IncludesWork], s.GeneratedAt.Local().Format("2006-01-02 15:04"))
	for _, sec := range []struct {
		name   string
		claims []reflection.Claim
	}{{"Themes", s.Themes}, {"Frustrations & corrections", s.Frustrations}, {"What kept breaking", s.Breakages},
		{"Decisions", s.Decisions}, {"Suggestions", s.Suggestions}} {
		if len(sec.claims) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s\n", sec.name)
		for _, c := range sec.claims {
			fmt.Fprintf(w, "  • %s\n    ↳ %s\n", c.Text, refList(c.Sources))
		}
	}
	if s.DroppedClaims > 0 {
		fmt.Fprintf(w, "\n(%d uncited claims dropped)\n", s.DroppedClaims)
	}
}

func section(w io.Writer, title string, rows []reflection.Count) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s\n", title)
	for _, r := range rows {
		fmt.Fprintf(w, "  %5d  %s", r.Count, r.Key)
		if len(r.Refs) > 0 {
			fmt.Fprintf(w, "  [%s]", refList(r.Refs[:min(3, len(r.Refs))]))
		}
		fmt.Fprintln(w)
	}
}

func refList(refs []reflection.Ref) string {
	parts := make([]string, 0, len(refs))
	for _, r := range refs {
		id := r.ID
		if r.Kind == "task" || r.URL == "" {
			parts = append(parts, r.Kind+":"+id)
			continue
		}
		parts = append(parts, r.Kind+":"+id+" ("+r.URL+")")
	}
	return strings.Join(parts, ", ")
}

func compact(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}
