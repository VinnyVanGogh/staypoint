package testgate

import (
	"fmt"
	"sort"
	"strings"
)

// Warning kinds on a Report.
const (
	KindNoCI            = "no_ci"
	KindNoTests         = "no_tests"
	KindUntestedSources = "untested_sources"
	KindUncovered       = "uncovered"
	KindNoCoverageData  = "no_coverage_data"
)

// Warning is one finding shown in the card's "Test coverage" section. A
// blocking warning disables Merge until the Board chooses "Merge without
// tests".
type Warning struct {
	Kind     string   `json:"kind"`
	Blocking bool     `json:"blocking"`
	Message  string   `json:"message"`
	Items    []string `json:"items,omitempty"`
}

// CoverageResult is what a CI coverage report says about the change.
type CoverageResult struct {
	Available bool `json:"available"`
	// Source names where the report came from, e.g. the CI artifact.
	Source string `json:"source,omitempty"`
	// Note explains why there is no report (no CI run, no artifact, ...).
	Note      string          `json:"note,omitempty"`
	Uncovered []UncoveredFile `json:"uncovered,omitempty"`
	// Final is set once nothing more can arrive for this head (a report was
	// read, or every CI run for it has finished); until then it is re-checked.
	Final bool `json:"final,omitempty"`
}

// CIState is what is known about CI for the change.
type CIState struct {
	// Workflows maps .github/workflows/* paths at the head commit to their
	// content. Nil with WorkflowsErr set means they could not be read.
	Workflows    map[string]string
	WorkflowsErr error
	// PRNumber > 0 with PRChecksRead means the PR's checks were read for the
	// head; PRCheckCount is how many check runs and statuses it had.
	PRNumber     int
	PRChecksRead bool
	PRCheckCount int
}

// Input is everything Evaluate needs about one change.
type Input struct {
	HeadSHA      string
	Files        []string
	ExtraExempt  []string
	ChangedLines map[string][]int
	CI           CIState
	Coverage     CoverageResult
}

// Report is the "Test coverage" verdict for one head commit.
type Report struct {
	HeadSHA string `json:"head_sha"`
	Classification
	// ExemptOnly: every changed file is docs, config or style.
	ExemptOnly bool `json:"exempt_only"`
	// TestWorkflows are the workflow files that run a test command.
	TestWorkflows   []string       `json:"test_workflows"`
	UntestedSources []string       `json:"untested_sources"`
	Coverage        CoverageResult `json:"coverage"`
	Warnings        []Warning      `json:"warnings"`
	// Blocking: Merge needs "Merge without tests".
	Blocking bool `json:"blocking"`
	// Missing names each blocking gap in a short phrase, for the bypass
	// confirm, the audit row and the backlog task.
	Missing    []string `json:"missing"`
	ComputedAt string   `json:"computed_at,omitempty"`
}

// NoCIMessage is the Board's wording for a repo with no CI.
const NoCIMessage = "This repo has no CI. Nothing checked this change automatically."

// Evaluate turns the inputs into the card's warnings. Changes with no source
// files (docs / config / CSS only, or tests only) get no warnings.
func Evaluate(in Input) *Report {
	r := &Report{
		HeadSHA:        in.HeadSHA,
		Classification: Classify(in.Files, in.ExtraExempt),
		TestWorkflows:  []string{},
		Coverage:       in.Coverage,
		Warnings:       []Warning{},
		Missing:        []string{},
	}
	for p, c := range in.CI.Workflows {
		if WorkflowRunsTests(c) {
			r.TestWorkflows = append(r.TestWorkflows, p)
		}
	}
	sort.Strings(r.TestWorkflows)
	r.ExemptOnly = len(r.Sources) == 0 && len(r.Tests) == 0 && len(r.Exempt) > 0
	if len(r.Sources) == 0 {
		r.UntestedSources = []string{}
		return r
	}
	r.UntestedSources = UnmatchedSources(r.Sources, r.Tests)
	if r.UntestedSources == nil {
		r.UntestedSources = []string{}
	}

	// 1. No CI.
	switch {
	case in.CI.WorkflowsErr != nil:
		r.add(Warning{Kind: KindNoCI, Blocking: true,
			Message: "Could not read this repo's CI workflows (" + in.CI.WorkflowsErr.Error() + "), so it is unknown whether anything checks this change."},
			"CI unknown: workflows unreadable")
	case len(in.CI.Workflows) == 0:
		r.add(Warning{Kind: KindNoCI, Blocking: true, Message: NoCIMessage}, "no CI")
	case len(r.TestWorkflows) == 0:
		r.add(Warning{Kind: KindNoCI, Blocking: true,
			Message: "This repo has no CI that runs tests: none of its workflows runs a test command. Nothing checked this change automatically.",
			Items:   sortedKeys(in.CI.Workflows)}, "no CI that runs tests")
	case in.CI.PRNumber > 0 && in.CI.PRChecksRead && in.CI.PRCheckCount == 0:
		r.add(Warning{Kind: KindNoCI, Blocking: true,
			Message: fmt.Sprintf("No CI ran on PR #%d: it has no check runs or statuses. Nothing checked this change automatically.", in.CI.PRNumber)},
			fmt.Sprintf("no CI checks on PR #%d", in.CI.PRNumber))
	}

	// 2. No tests changed or added.
	if len(r.Tests) == 0 {
		r.add(Warning{Kind: KindNoTests, Blocking: true,
			Message: "No tests changed or added. These changed source files have no test change:",
			Items:   r.UntestedSources}, "no tests changed or added")
	} else if len(r.UntestedSources) > 0 {
		r.Warnings = append(r.Warnings, Warning{Kind: KindUntestedSources,
			Message: "Tests changed, but these source files have no matching test change:",
			Items:   r.UntestedSources})
	}

	// 3. Changed code no test touches, from CI coverage.
	if !in.Coverage.Available {
		note := in.Coverage.Note
		if note == "" {
			note = "no coverage report from CI"
		}
		r.Warnings = append(r.Warnings, Warning{Kind: KindNoCoverageData,
			Message: "No coverage data: " + note + ". Can't tell which changed lines run under tests."})
	} else if len(in.Coverage.Uncovered) > 0 {
		items := make([]string, 0, len(in.Coverage.Uncovered))
		for _, u := range in.Coverage.Uncovered {
			items = append(items, u.Describe())
		}
		r.add(Warning{Kind: KindUncovered, Blocking: true,
			Message: "Changed code no test runs (0% coverage in " + in.Coverage.Source + "):",
			Items:   items}, "changed code with 0% coverage")
	}
	return r
}

func (r *Report) add(w Warning, missing string) {
	r.Warnings = append(r.Warnings, w)
	r.Blocking = true
	r.Missing = append(r.Missing, missing)
}

// Describe renders one uncovered file as a single line.
func (u UncoveredFile) Describe() string {
	if u.NotInReport {
		return u.Path + ": not in the coverage report (no test loads it)"
	}
	var parts []string
	if len(u.Functions) > 0 {
		parts = append(parts, "functions "+strings.Join(u.Functions, ", "))
	}
	if len(u.Lines) > 0 {
		parts = append(parts, "lines "+FormatLines(u.Lines))
	}
	return u.Path + ": " + strings.Join(parts, "; ")
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
