package testgate

import (
	"fmt"
	"strings"
)

// GapTask describes the change a backlog "Add tests" task is filed for.
type GapTask struct {
	PRNumber int
	PRURL    string
	HeadSHA  string
	MainSHA  string
	Branch   string
	// SourceTaskID / SourceTaskName / CardURL link back to the ship review.
	SourceTaskID   string
	SourceTaskName string
	CardURL        string
	Reason         string
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// targets is what the task asks tests for, most specific first: uncovered
// functions, else uncovered / untested files, else every changed source.
func (r *Report) targets() []string {
	var out []string
	for _, u := range r.Coverage.Uncovered {
		if len(u.Functions) > 0 {
			out = append(out, u.Path+" ("+strings.Join(u.Functions, ", ")+")")
		} else {
			out = append(out, u.Path)
		}
	}
	if len(out) == 0 {
		out = append(out, r.UntestedSources...)
	}
	if len(out) == 0 {
		out = append(out, r.Sources...)
	}
	return out
}

// TaskTitle is "Add tests for <files/functions> (merged untested in PR #N `<sha>`)".
func TaskTitle(r *Report, g GapTask) string {
	t := r.targets()
	list := strings.Join(t, ", ")
	if len(t) > 3 {
		list = strings.Join(t[:3], ", ") + fmt.Sprintf(" +%d more", len(t)-3)
	}
	where := fmt.Sprintf("at `%s`", short(g.HeadSHA))
	if g.PRNumber > 0 {
		where = fmt.Sprintf("in PR #%d `%s`", g.PRNumber, short(g.HeadSHA))
	}
	return fmt.Sprintf("Add tests for %s (merged untested %s)", list, where)
}

// TaskDescription lists everything the bypass skipped: the missing checks,
// the uncovered files, functions and lines, and links to the PR and card.
func TaskDescription(r *Report, g GapTask) string {
	var b strings.Builder
	b.WriteString("The Board merged this change with **Merge without tests**. Add the tests it is missing.\n\n")
	if g.PRNumber > 0 {
		fmt.Fprintf(&b, "- PR: #%d %s\n", g.PRNumber, g.PRURL)
	}
	fmt.Fprintf(&b, "- Reviewed head: `%s`", g.HeadSHA)
	if g.Branch != "" {
		fmt.Fprintf(&b, " on `%s`", g.Branch)
	}
	b.WriteString("\n")
	if g.MainSHA != "" {
		fmt.Fprintf(&b, "- Merge commit: `%s`\n", g.MainSHA)
	}
	if g.SourceTaskID != "" {
		fmt.Fprintf(&b, "- Ship review card: task `%s` %s", g.SourceTaskID, g.SourceTaskName)
		if g.CardURL != "" {
			fmt.Fprintf(&b, " ([open card](%s))", g.CardURL)
		}
		b.WriteString("\n")
	}
	if g.Reason != "" {
		fmt.Fprintf(&b, "- Board's reason: %s\n", g.Reason)
	}

	b.WriteString("\n## What was missing\n")
	for _, w := range r.Warnings {
		if !w.Blocking {
			continue
		}
		fmt.Fprintf(&b, "- %s\n", w.Message)
	}

	if len(r.UntestedSources) > 0 {
		b.WriteString("\n## Changed source files with no test change\n")
		for _, s := range r.UntestedSources {
			fmt.Fprintf(&b, "- `%s`\n", s)
		}
	}

	b.WriteString("\n## Changed code with 0% coverage\n")
	switch {
	case !r.Coverage.Available:
		note := r.Coverage.Note
		if note == "" {
			note = "no coverage report from CI"
		}
		fmt.Fprintf(&b, "No coverage data (%s), so the uncovered lines are unknown.\n", note)
	case len(r.Coverage.Uncovered) == 0 && len(r.Coverage.Ambiguous) == 0:
		fmt.Fprintf(&b, "None: every changed line in %s ran under a test.\n", r.Coverage.Source)
	default:
		fmt.Fprintf(&b, "From %s:\n", r.Coverage.Source)
		for _, u := range r.Coverage.Uncovered {
			fmt.Fprintf(&b, "- %s\n", u.Describe())
		}
		for _, p := range r.Coverage.Ambiguous {
			fmt.Fprintf(&b, "- %s: coverage unknown (more than one report entry matches)\n", p)
		}
	}
	return b.String()
}

// DedupeKey identifies the change a gap task covers: one task per PR, or per
// ship review task when there is no PR (direct merge).
func DedupeKey(repoPath string, prNumber int, sourceTaskID string) string {
	if prNumber > 0 {
		return fmt.Sprintf("%s#pr:%d", repoPath, prNumber)
	}
	return fmt.Sprintf("%s#task:%s", repoPath, sourceTaskID)
}
