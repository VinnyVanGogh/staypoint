package testgate

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

var testWorkflow = map[string]string{".github/workflows/ci.yml": "jobs:\n  t:\n    steps:\n      - run: go test ./...\n"}

func kinds(r *Report) []string {
	var out []string
	for _, w := range r.Warnings {
		k := w.Kind
		if w.Blocking {
			k += "!"
		}
		out = append(out, k)
	}
	return out
}

func TestEvaluateExemptOnly(t *testing.T) {
	r := Evaluate(Input{HeadSHA: "abc", Files: []string{"README.md", "web/style.css", ".github/workflows/ci.yml"}})
	if !r.ExemptOnly || r.Blocking || len(r.Warnings) != 0 {
		t.Fatalf("docs/config/CSS-only change must pass clean, got %+v", r)
	}
}

func TestEvaluateProjectExemptList(t *testing.T) {
	r := Evaluate(Input{Files: []string{"scripts/release.sh"}, ExtraExempt: []string{"scripts/**"}})
	if !r.ExemptOnly || r.Blocking {
		t.Fatalf("project exempt glob not applied: %+v", r)
	}
}

func TestEvaluateTestsOnlyChangeHasNoWarnings(t *testing.T) {
	r := Evaluate(Input{Files: []string{"internal/a/a_test.go"}})
	if r.ExemptOnly || r.Blocking || len(r.Warnings) != 0 {
		t.Fatalf("tests-only change should not warn: %+v", r)
	}
}

func TestEvaluateNoCINoTests(t *testing.T) {
	r := Evaluate(Input{Files: []string{"main.go", "README.md"}, CI: CIState{Workflows: map[string]string{}}})
	if want := []string{"no_ci!", "no_tests!", "no_coverage_data"}; !reflect.DeepEqual(kinds(r), want) {
		t.Fatalf("kinds = %v, want %v", kinds(r), want)
	}
	if r.Warnings[0].Message != NoCIMessage {
		t.Errorf("no-CI message = %q", r.Warnings[0].Message)
	}
	if !reflect.DeepEqual(r.Warnings[1].Items, []string{"main.go"}) {
		t.Errorf("no-tests items = %v", r.Warnings[1].Items)
	}
	if !r.Blocking || !reflect.DeepEqual(r.Missing, []string{"no CI", "no tests changed or added"}) {
		t.Errorf("Blocking=%v Missing=%v", r.Blocking, r.Missing)
	}
}

func TestEvaluateWorkflowWithoutTests(t *testing.T) {
	r := Evaluate(Input{Files: []string{"main.go", "main_test.go"},
		CI: CIState{Workflows: map[string]string{".github/workflows/lint.yml": "      - run: golangci-lint run\n"}}})
	if want := []string{"no_ci!", "no_coverage_data"}; !reflect.DeepEqual(kinds(r), want) {
		t.Fatalf("kinds = %v, want %v", kinds(r), want)
	}
	if !strings.Contains(r.Warnings[0].Message, "no CI that runs tests") {
		t.Errorf("message = %q", r.Warnings[0].Message)
	}
}

func TestEvaluateWorkflowsUnreadableFailsClosed(t *testing.T) {
	r := Evaluate(Input{Files: []string{"main.go", "main_test.go"}, CI: CIState{WorkflowsErr: errors.New("git timed out")}})
	if !r.Blocking || r.Warnings[0].Kind != KindNoCI {
		t.Fatalf("unreadable workflows must block: %+v", r.Warnings)
	}
}

func TestEvaluatePRWithNoChecks(t *testing.T) {
	r := Evaluate(Input{Files: []string{"main.go", "main_test.go"},
		CI: CIState{Workflows: testWorkflow, PRNumber: 7, PRChecksRead: true, PRCheckCount: 0}})
	if !r.Blocking || r.Warnings[0].Kind != KindNoCI || !strings.Contains(r.Warnings[0].Message, "PR #7") {
		t.Fatalf("PR with zero checks must warn no-CI: %+v", r.Warnings)
	}
	// Same, but the checks were never read: no claim either way.
	r = Evaluate(Input{Files: []string{"main.go", "main_test.go"}, CI: CIState{Workflows: testWorkflow, PRNumber: 7}})
	if r.Blocking {
		t.Fatalf("unread checks must not count as no checks: %+v", r.Warnings)
	}
}

func TestEvaluateTestedChangeWithCoverage(t *testing.T) {
	r := Evaluate(Input{Files: []string{"internal/a/a.go", "internal/a/a_test.go"},
		CI:       CIState{Workflows: testWorkflow, PRNumber: 7, PRChecksRead: true, PRCheckCount: 3},
		Coverage: CoverageResult{Available: true, Source: "artifact coverage", Final: true}})
	if r.Blocking || len(r.Warnings) != 0 {
		t.Fatalf("tested, covered change should be clean: %+v", r.Warnings)
	}
	if !reflect.DeepEqual(r.TestWorkflows, []string{".github/workflows/ci.yml"}) {
		t.Errorf("TestWorkflows = %v", r.TestWorkflows)
	}
}

func TestEvaluateUntestedSourcesIsInfoOnly(t *testing.T) {
	r := Evaluate(Input{Files: []string{"internal/a/a.go", "internal/a/a_test.go", "web/app.js"},
		CI: CIState{Workflows: testWorkflow}, Coverage: CoverageResult{Available: true, Source: "x"}})
	if r.Blocking {
		t.Fatalf("a partial test change is a hint, not a block: %+v", r.Warnings)
	}
	if want := []string{"untested_sources"}; !reflect.DeepEqual(kinds(r), want) {
		t.Fatalf("kinds = %v", kinds(r))
	}
	if !reflect.DeepEqual(r.Warnings[0].Items, []string{"web/app.js"}) {
		t.Errorf("items = %v", r.Warnings[0].Items)
	}
}

func TestEvaluateUncoveredBlocks(t *testing.T) {
	r := Evaluate(Input{Files: []string{"internal/calc/calc.go", "internal/calc/calc_test.go"},
		CI: CIState{Workflows: testWorkflow},
		Coverage: CoverageResult{Available: true, Source: "artifact go-coverage", Uncovered: []UncoveredFile{
			{Path: "internal/calc/calc.go", Lines: []int{10, 11, 13}, Functions: []string{"Div"}},
		}}})
	if want := []string{"uncovered!"}; !reflect.DeepEqual(kinds(r), want) {
		t.Fatalf("kinds = %v", kinds(r))
	}
	if got := r.Warnings[0].Items[0]; got != "internal/calc/calc.go: functions Div; lines 10-11, 13" {
		t.Errorf("item = %q", got)
	}
}

func TestEvaluateNoCoverageSaysSo(t *testing.T) {
	r := Evaluate(Input{Files: []string{"a.go", "a_test.go"}, CI: CIState{Workflows: testWorkflow},
		Coverage: CoverageResult{Note: "CI uploaded no coverage artifact"}})
	if len(r.Warnings) != 1 || r.Warnings[0].Kind != KindNoCoverageData || r.Warnings[0].Blocking {
		t.Fatalf("got %+v", r.Warnings)
	}
	if !strings.Contains(r.Warnings[0].Message, "CI uploaded no coverage artifact") {
		t.Errorf("message = %q", r.Warnings[0].Message)
	}
}

func TestTaskTitleAndDescription(t *testing.T) {
	r := Evaluate(Input{Files: []string{"internal/calc/calc.go", "internal/calc/calc_test.go", "web/app.js"},
		CI: CIState{Workflows: map[string]string{}},
		Coverage: CoverageResult{Available: true, Source: "artifact go-coverage", Uncovered: []UncoveredFile{
			{Path: "internal/calc/calc.go", Lines: []int{10, 11}, Functions: []string{"Div"}},
		}}})
	g := GapTask{PRNumber: 12, PRURL: "https://github.com/o/r/pull/12", HeadSHA: "0123456789abcdef",
		MainSHA: "fedcba", Branch: "staypoint/task-1", SourceTaskID: "task-1", SourceTaskName: "Calc",
		CardURL: "/tasks/STA/core/task-1", Reason: "hotfix"}
	if got, want := TaskTitle(r, g), "Add tests for internal/calc/calc.go (Div) (merged untested in PR #12 `0123456`)"; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
	d := TaskDescription(r, g)
	for _, want := range []string{
		"PR: #12 https://github.com/o/r/pull/12", "`0123456789abcdef`", "Merge commit: `fedcba`",
		"task `task-1` Calc ([open card](/tasks/STA/core/task-1))", "Board's reason: hotfix",
		NoCIMessage, "- `web/app.js`", "internal/calc/calc.go: functions Div; lines 10-11",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("description missing %q:\n%s", want, d)
		}
	}
}

func TestTaskTitleTruncatesAndNoPR(t *testing.T) {
	r := Evaluate(Input{Files: []string{"a.go", "b.go", "c.go", "d.go", "e.go"}, CI: CIState{Workflows: testWorkflow}})
	got := TaskTitle(r, GapTask{HeadSHA: "abcdef0123"})
	if want := "Add tests for a.go, b.go, c.go +2 more (merged untested at `abcdef0`)"; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
}

func TestDedupeKey(t *testing.T) {
	if DedupeKey("/r", 7, "task-1") != DedupeKey("/r", 7, "task-2") {
		t.Error("same PR must dedupe across cards/tasks")
	}
	if DedupeKey("/r", 7, "t") == DedupeKey("/other", 7, "t") {
		t.Error("PR numbers are per repo")
	}
	if DedupeKey("/r", 0, "task-1") == DedupeKey("/r", 0, "task-2") {
		t.Error("without a PR, each task gets its own key")
	}
}
