package server_test

// STA-734: the merge test gate, through the HTTP API against real git repos.
// The fake gh wraps the STA-717 fake (PRs, checks, merge) and adds CI runs,
// run artifacts and artifact download, so a coverage report can be served.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

const fakeGHCoverageScript = `#!/bin/sh
D="$FAKE_GH_DIR"
case "$1 $2" in
"run list")
  printf '%s\n' "$*" >> "$D/calls"
  if [ -f "$D/runs.json" ]; then cat "$D/runs.json"; else echo "[]"; fi ;;
"api repos/{owner}/{repo}/actions/runs/"*)
  printf '%s\n' "$*" >> "$D/calls"
  if [ -f "$D/artifacts.json" ]; then cat "$D/artifacts.json"; else echo '{"artifacts":[]}'; fi ;;
"run download")
  printf '%s\n' "$*" >> "$D/calls"
  dir=""; while [ $# -gt 0 ]; do [ "$1" = "-D" ] && dir="$2"; shift; done
  mkdir -p "$dir" && cp -R "$D/artifact/." "$dir/" ;;
*) exec "$(dirname "$0")/gh-pr" "$@" ;;
esac
`

// installCoverageGH installs the STA-717 fake as gh-pr and this wrapper as gh.
func installCoverageGH(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh-pr"), []byte(fakeGHScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeGHCoverageScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_GH_DIR", state)
	return state
}

const ciWorkflow = "name: CI\non: [push, pull_request]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n      - run: go test -coverprofile=coverage.out ./...\n"

const calcSrc = `package calc

import "errors"

func Add(a, b int) int {
	return a + b
}

func Div(a, b int) (int, error) {
	if b == 0 {
		return 0, errors.New("div by zero")
	}
	return a / b, nil
}
`

// commitOnTaskBranch adds files to the task branch, pushes it and re-pins
// the card to the new head.
func commitOnTaskBranch(t *testing.T, client *http.Client, baseURL, token, taskID, repoDir string, files map[string]string) string {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	branch := "staypoint/" + taskID
	run("checkout", branch)
	for p, body := range files {
		full := filepath.Join(repoDir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", ".")
	run("commit", "-m", "task: change")
	run("push", "origin", branch)
	run("checkout", "main")
	upsert, _ := json.Marshal(map[string]any{"test_steps": []string{"1. Open /"}})
	resp, rb := shipDoReq(t, client, token, "PUT", baseURL+"/api/tasks/"+taskID+"/ship-review", upsert)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("re-pin card: %d %s", resp.StatusCode, rb)
	}
	return gitOut(t, repoDir, "rev-parse", branch)
}

type gateReport struct {
	HeadSHA    string   `json:"head_sha"`
	ExemptOnly bool     `json:"exempt_only"`
	Sources    []string `json:"sources"`
	Tests      []string `json:"tests"`
	Blocking   bool     `json:"blocking"`
	Missing    []string `json:"missing"`
	Warnings   []struct {
		Kind     string   `json:"kind"`
		Blocking bool     `json:"blocking"`
		Message  string   `json:"message"`
		Items    []string `json:"items"`
	} `json:"warnings"`
	Coverage struct {
		Available bool   `json:"available"`
		Note      string `json:"note"`
		Source    string `json:"source"`
		Uncovered []struct {
			Path      string   `json:"path"`
			Lines     []int    `json:"lines"`
			Functions []string `json:"functions"`
		} `json:"uncovered"`
	} `json:"coverage"`
}

type gateTask struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Created bool   `json:"created"`
}

type gateResp struct {
	Error              string     `json:"error"`
	Message            string     `json:"message"`
	Missing            []string   `json:"missing"`
	TestCoverage       gateReport `json:"test_coverage"`
	Report             gateReport `json:"report"`
	MergedWithoutTests bool       `json:"merged_without_tests"`
	TestTask           *gateTask  `json:"test_task"`
	TestTaskError      string     `json:"test_task_error"`
	MainSHA            string     `json:"main_sha"`
}

func decodeGate(t *testing.T, rb []byte) gateResp {
	t.Helper()
	var g gateResp
	if err := json.Unmarshal(rb, &g); err != nil {
		t.Fatalf("decode %s: %v", rb, err)
	}
	return g
}

func approveBody(t *testing.T, client *http.Client, baseURL, token, boardToken, taskID, body string) (int, gateResp) {
	t.Helper()
	var b []byte
	if body != "" {
		b = []byte(body)
	}
	resp, rb := shipDoReq(t, client, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/approve", b, boardToken, "", "mock-assertion")
	return resp.StatusCode, decodeGate(t, rb)
}

func getTestCoverage(t *testing.T, client *http.Client, baseURL, token, taskID string) gateResp {
	t.Helper()
	resp, rb := shipDoReq(t, client, token, "GET", baseURL+"/api/tasks/"+taskID+"/ship-review/test-coverage", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET test-coverage: %d %s", resp.StatusCode, rb)
	}
	return decodeGate(t, rb)
}

func kindsOf(r gateReport) []string {
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

func taskRow(t *testing.T, database *sql.DB, id string) (name, stage, status, project string) {
	t.Helper()
	if err := database.QueryRow(`SELECT name, execution_stage, status, COALESCE(project,'') FROM tasks WHERE id = ?`, id).Scan(&name, &stage, &status, &project); err != nil {
		t.Fatalf("task %s: %v", id, err)
	}
	return
}

// A docs/config-only change (the default fixture's task.txt) is exempt:
// Approve merges with no bypass and files nothing.
func TestTestGate_ExemptChangeMergesClean(t *testing.T) {
	notWork(t)
	installCoverageGH(t)
	_, baseURL, token, boardToken, taskID, _, client := shipApproveServer(t)

	cov := getTestCoverage(t, client, baseURL, token, taskID)
	if !cov.Report.ExemptOnly || cov.Report.Blocking || len(cov.Report.Warnings) != 0 {
		t.Fatalf("exempt change report = %+v", cov.Report)
	}
	code, g := approveBody(t, client, baseURL, token, boardToken, taskID, "")
	if code != http.StatusOK {
		t.Fatalf("Approve: %d %+v", code, g)
	}
	if g.MergedWithoutTests || g.TestTask != nil {
		t.Errorf("clean merge must not bypass or file a task: %+v", g)
	}
}

// A source change with no tests and no CI blocks Approve with 409 untested,
// naming both gaps, and nothing merges.
func TestTestGate_UntestedChangeBlocksApprove(t *testing.T) {
	notWork(t)
	installCoverageGH(t)
	_, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	commitOnTaskBranch(t, client, baseURL, token, taskID, repoDir, map[string]string{"calc/calc.go": calcSrc})
	bare := gitOut(t, repoDir, "remote", "get-url", "origin")
	mainBefore := gitOut(t, bare, "rev-parse", "main")

	cov := getTestCoverage(t, client, baseURL, token, taskID)
	if got, want := strings.Join(kindsOf(cov.Report), ","), "no_ci!,no_tests!,no_coverage_data"; got != want {
		t.Fatalf("warnings = %s, want %s (%+v)", got, want, cov.Report)
	}
	if cov.Report.Warnings[0].Message != "This repo has no CI. Nothing checked this change automatically." {
		t.Errorf("no-CI message = %q", cov.Report.Warnings[0].Message)
	}
	if strings.Join(cov.Report.Warnings[1].Items, ",") != "calc/calc.go" {
		t.Errorf("no-tests items = %v", cov.Report.Warnings[1].Items)
	}
	if !strings.Contains(cov.Report.Warnings[2].Message, "no CI run for this commit") {
		t.Errorf("no-coverage message = %q", cov.Report.Warnings[2].Message)
	}

	code, g := approveBody(t, client, baseURL, token, boardToken, taskID, "")
	if code != http.StatusConflict || g.Error != "untested" {
		t.Fatalf("Approve: %d %+v, want 409 untested", code, g)
	}
	if strings.Join(g.Missing, "|") != "no CI|no tests changed or added" {
		t.Errorf("missing = %v", g.Missing)
	}
	if got := gitOut(t, bare, "rev-parse", "main"); got != mainBefore {
		t.Errorf("origin main moved on a refused Approve")
	}
	if got := gitOut(t, repoDir, "rev-parse", "main"); got == gitOut(t, repoDir, "rev-parse", "staypoint/"+taskID) {
		t.Errorf("local main was merged on a refused Approve")
	}
}

// "Merge without tests" merges, audits the bypass with what was missing,
// and files one backlog task in the same project linked to the card.
func TestTestGate_BypassAuditsAndFilesBacklogTask(t *testing.T) {
	notWork(t)
	installCoverageGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	head := commitOnTaskBranch(t, client, baseURL, token, taskID, repoDir, map[string]string{"calc/calc.go": calcSrc})

	code, g := approveBody(t, client, baseURL, token, boardToken, taskID, `{"merge_without_tests":true,"merge_without_tests_reason":"hotfix","head_sha":"`+head+`"}`)
	if code != http.StatusOK {
		t.Fatalf("Approve with bypass: %d %+v", code, g)
	}
	if !g.MergedWithoutTests || g.MainSHA == "" {
		t.Fatalf("response = %+v", g)
	}
	audit := lastBoardAudit(t, database, "merge_without_tests")
	if audit["task_id"] != taskID || audit["head_sha"] != head || audit["reason"] != "hotfix" || audit["merge_action"] != "approve" {
		t.Errorf("audit = %v", audit)
	}
	if missing, _ := audit["missing"].([]any); len(missing) != 2 {
		t.Errorf("audit missing = %v", audit["missing"])
	}

	if g.TestTask == nil || g.TestTask.ID == "" || !g.TestTask.Created {
		t.Fatalf("want a created test task, got %+v (err %q)", g.TestTask, g.TestTaskError)
	}
	name, stage, status, project := taskRow(t, database, g.TestTask.ID)
	if want := "Add tests for calc/calc.go (merged untested at `" + head[:7] + "`)"; name != want {
		t.Errorf("task name = %q, want %q", name, want)
	}
	if stage != "backlog" || status != "active" || project != "ship-review-test" {
		t.Errorf("task stage/status/project = %s/%s/%s, want backlog/active/ship-review-test", stage, status, project)
	}
	if !strings.Contains(g.TestTask.URL, "/tasks/STA/ship-review-test/"+g.TestTask.ID) {
		t.Errorf("task url = %q", g.TestTask.URL)
	}
	var desc string
	_ = database.QueryRow(`SELECT content FROM task_documents WHERE task_id = ? AND doc_key = 'description' ORDER BY version DESC LIMIT 1`, g.TestTask.ID).Scan(&desc)
	for _, want := range []string{"Merge without tests", head, "task `" + taskID + "`", "/tasks/STA/ship-review-test/" + taskID,
		"Board's reason: hotfix", "This repo has no CI.", "- `calc/calc.go`", "No coverage data"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description missing %q:\n%s", want, desc)
		}
	}

	// The card reports the filed task afterwards.
	cov := getTestCoverage(t, client, baseURL, token, taskID)
	if cov.TestTask == nil || cov.TestTask.ID != g.TestTask.ID {
		t.Errorf("test-coverage after merge: test_task = %+v", cov.TestTask)
	}
}

// Tests changed and a CI workflow that runs them: Approve needs no bypass.
func TestTestGate_TestedChangeWithCIMergesClean(t *testing.T) {
	notWork(t)
	installCoverageGH(t)
	_, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	commitOnTaskBranch(t, client, baseURL, token, taskID, repoDir, map[string]string{
		"calc/calc.go":             calcSrc,
		"calc/calc_test.go":        "package calc\n",
		".github/workflows/ci.yml": ciWorkflow,
	})
	code, g := approveBody(t, client, baseURL, token, boardToken, taskID, "")
	if code != http.StatusOK {
		t.Fatalf("Approve: %d %+v", code, g)
	}
	if g.TestCoverage.Blocking || g.TestTask != nil {
		t.Errorf("tested change should pass the gate: %+v", g)
	}
}

// A coverage artifact from CI marks changed lines and functions no test runs;
// that blocks until bypassed.
func TestTestGate_CoverageArtifactShowsUncoveredCode(t *testing.T) {
	notWork(t)
	state := installCoverageGH(t)
	_, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	head := commitOnTaskBranch(t, client, baseURL, token, taskID, repoDir, map[string]string{
		"calc/calc.go":             calcSrc,
		"calc/calc_test.go":        "package calc\n",
		".github/workflows/ci.yml": ciWorkflow,
	})
	writeState(t, state, "runs.json", `[{"databaseId":901,"workflowName":"CI","status":"completed","conclusion":"success"}]`)
	writeState(t, state, "artifacts.json", `{"artifacts":[{"name":"go-coverage","size_in_bytes":300,"expired":false},{"name":"playwright-report","size_in_bytes":300,"expired":false}]}`)
	if err := os.MkdirAll(filepath.Join(state, "artifact"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeState(t, state, "artifact/coverage.out", "mode: set\n"+
		"example.com/app/calc/calc.go:5.24,7.2 1 1\n"+
		"example.com/app/calc/calc.go:9.33,10.12 1 0\n"+
		"example.com/app/calc/calc.go:10.12,12.3 1 0\n"+
		"example.com/app/calc/calc.go:13.2,13.16 1 0\n")

	cov := getTestCoverage(t, client, baseURL, token, taskID)
	r := cov.Report
	if !r.Coverage.Available || r.Coverage.Source != "CI artifact go-coverage" {
		t.Fatalf("coverage = %+v", r.Coverage)
	}
	if len(r.Coverage.Uncovered) != 1 || r.Coverage.Uncovered[0].Path != "calc/calc.go" ||
		strings.Join(r.Coverage.Uncovered[0].Functions, ",") != "Div" {
		t.Fatalf("uncovered = %+v", r.Coverage.Uncovered)
	}
	if got := kindsOf(r); strings.Join(got, ",") != "uncovered!" {
		t.Fatalf("warnings = %v", got)
	}
	if calls := ghCalls(t, state); hasCall(calls, "run download 901 -n playwright-report") {
		t.Errorf("non-coverage artifact downloaded: %v", calls)
	}

	code, g := approveBody(t, client, baseURL, token, boardToken, taskID, "")
	if code != http.StatusConflict || strings.Join(g.Missing, ",") != "changed code with 0% coverage" {
		t.Fatalf("Approve: %d %+v", code, g)
	}
	code, g = approveBody(t, client, baseURL, token, boardToken, taskID, `{"merge_without_tests":true,"head_sha":"`+head+`"}`)
	if code != http.StatusOK || g.TestTask == nil {
		t.Fatalf("bypass: %d %+v", code, g)
	}
	if !strings.HasPrefix(g.TestTask.Name, "Add tests for calc/calc.go (Div) (merged untested at") {
		t.Errorf("task name = %q", g.TestTask.Name)
	}
}

// The project's own exempt globs are saved through the Board-gated config
// API (audited) and make matching changes exempt.
func TestTestGate_ProjectExemptGlobs(t *testing.T) {
	notWork(t)
	installCoverageGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	commitOnTaskBranch(t, client, baseURL, token, taskID, repoDir, map[string]string{"scripts/release.sh": "#!/bin/sh\necho hi\n"})

	if cov := getTestCoverage(t, client, baseURL, token, taskID); !cov.Report.Blocking {
		t.Fatalf("scripts/ change should block before the project exempts it: %+v", cov.Report)
	}
	body, _ := json.Marshal(map[string]any{"repo_path": repoDir, "test_exempt_globs": []string{"scripts/**", " "}})
	resp, rb := shipDoReq(t, client, token, "PUT", baseURL+"/api/project-dev-configs", body, boardToken, "", "mock-assertion")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT config: %d %s", resp.StatusCode, rb)
	}
	var saved struct {
		RepoPath        string   `json:"repo_path"`
		TestExemptGlobs []string `json:"test_exempt_globs"`
	}
	_ = json.Unmarshal(rb, &saved)
	if saved.RepoPath != repoDir {
		t.Errorf("config response lost its shape: %s", rb)
	}
	if got, _ := shipreview.GetTestExemptGlobs(database, repoDir); strings.Join(got, ",") != "scripts/**" {
		t.Errorf("stored globs = %v", got)
	}
	var payload string
	_ = database.QueryRow(`SELECT payload FROM board_audit_log WHERE event_type = 'dev_config_change' ORDER BY id DESC LIMIT 1`).Scan(&payload)
	if !strings.Contains(payload, `"new_test_exempt_globs":["scripts/**"`) {
		t.Errorf("dev_config_change audit = %s", payload)
	}

	if cov := getTestCoverage(t, client, baseURL, token, taskID); !cov.Report.ExemptOnly || cov.Report.Blocking {
		t.Fatalf("after exempting scripts/**: %+v", cov.Report)
	}
	if code, g := approveBody(t, client, baseURL, token, boardToken, taskID, ""); code != http.StatusOK {
		t.Fatalf("Approve: %d %+v", code, g)
	}
}

// pr_merge: a PR whose head has no check runs or statuses is "no CI" at
// Merge. Bypassing (plus overriding the missing checks) merges through
// GitHub and files the task against the PR, once: a resubmitted card for
// the same PR reuses it.
func TestTestGate_PRMergeNoChecksBypassAndDedupe(t *testing.T) {
	notWork(t)
	state := installCoverageGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	setMergeMode(t, database, repoDir, shipreview.MergeModePRMerge, "")
	head := commitOnTaskBranch(t, client, baseURL, token, taskID, repoDir, map[string]string{
		"calc/calc.go":             calcSrc,
		"calc/calc_test.go":        "package calc\n",
		".github/workflows/ci.yml": ciWorkflow,
	})

	// pr_merge Approve only opens the PR: no gate there.
	if code, rb := prApprove(t, client, baseURL, token, boardToken, taskID); code != http.StatusOK {
		t.Fatalf("Approve (open PR): %d %s", code, rb)
	}
	// Poll checks: GitHub reports none.
	if resp, rb := shipDoReq(t, client, token, "GET", baseURL+"/api/tasks/"+taskID+"/ship-review/checks", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("checks: %d %s", resp.StatusCode, rb)
	}
	code, rb := prMerge(t, client, baseURL, token, boardToken, taskID, "")
	g := decodeGate(t, rb)
	if code != http.StatusConflict || g.Error != "untested" || strings.Join(g.Missing, ",") != "no CI checks on PR #7" {
		t.Fatalf("Merge: %d %s", code, rb)
	}
	if hasCall(ghCalls(t, state), "pr merge") {
		t.Fatal("gh pr merge ran despite the test gate")
	}

	code, rb = prMerge(t, client, baseURL, token, boardToken, taskID, `{"merge_without_tests":true,"override":true,"head_sha":"`+head+`"}`)
	g = decodeGate(t, rb)
	if code != http.StatusOK || g.TestTask == nil || !g.TestTask.Created {
		t.Fatalf("Merge with bypass: %d %s", code, rb)
	}
	if !strings.Contains(g.TestTask.Name, "(merged untested in PR #7 `") {
		t.Errorf("task name = %q", g.TestTask.Name)
	}
	var wp int
	_ = database.QueryRow(`SELECT COUNT(*) FROM task_work_products WHERE task_id = ? AND product_type = 'pull_request' AND reference = 'https://github.com/o/r/pull/7'`, g.TestTask.ID).Scan(&wp)
	if wp != 1 {
		t.Errorf("test task pull_request work products = %d, want 1", wp)
	}
	if audit := lastBoardAudit(t, database, "merge_without_tests"); audit["merge_action"] != "merge_pr" || audit["pr_number"] != float64(7) {
		t.Errorf("audit = %v", audit)
	}

	// Dedupe: a second bypass for the same PR returns the same task.
	var n int
	_ = database.QueryRow(`SELECT COUNT(*) FROM ship_review_test_tasks WHERE pr_number = 7`).Scan(&n)
	if n != 1 {
		t.Fatalf("dedupe rows = %d", n)
	}
	existing, claimed, err := shipreview.ClaimTestGapTask(database, repoDir+"#pr:7", "task-other", "card-other", 7, "abc")
	if err != nil || claimed || existing != g.TestTask.ID {
		t.Errorf("second claim for PR #7 = (%q, %v, %v), want existing %q", existing, claimed, err, g.TestTask.ID)
	}
	var tasks int
	_ = database.QueryRow(`SELECT COUNT(*) FROM tasks WHERE name LIKE 'Add tests for %'`).Scan(&tasks)
	if tasks != 1 {
		t.Errorf("Add-tests tasks = %d, want 1", tasks)
	}
}

// Resubmitting an open_pr card for the same PR and bypassing again reuses
// the PR's task instead of filing a second one.
func TestTestGate_OpenPRResubmitDedupesTask(t *testing.T) {
	notWork(t)
	installCoverageGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	setMergeMode(t, database, repoDir, shipreview.MergeModeOpenPR, "")
	head1 := commitOnTaskBranch(t, client, baseURL, token, taskID, repoDir, map[string]string{"calc/calc.go": calcSrc})

	code, first := approveBody(t, client, baseURL, token, boardToken, taskID, `{"merge_without_tests":true,"head_sha":"`+head1+`"}`)
	if code != http.StatusOK || first.TestTask == nil || !first.TestTask.Created {
		t.Fatalf("first bypass: %d %+v", code, first)
	}
	head2 := commitOnTaskBranch(t, client, baseURL, token, taskID, repoDir, map[string]string{"calc/more.go": "package calc\n\nfunc More() {}\n"})
	code, second := approveBody(t, client, baseURL, token, boardToken, taskID, `{"merge_without_tests":true,"head_sha":"`+head2+`"}`)
	if code != http.StatusOK || second.TestTask == nil {
		t.Fatalf("second bypass: %d %+v", code, second)
	}
	if second.TestTask.Created || second.TestTask.ID != first.TestTask.ID {
		t.Errorf("same PR must reuse task %s, got %+v", first.TestTask.ID, second.TestTask)
	}
	var tasks int
	_ = database.QueryRow(`SELECT COUNT(*) FROM tasks WHERE name LIKE 'Add tests for %'`).Scan(&tasks)
	if tasks != 1 {
		t.Errorf("Add-tests tasks = %d, want 1", tasks)
	}
}

// A bypass is pinned to the head the Board reviewed: without head_sha, or
// with a head the card has since moved past, nothing merges.
func TestTestGate_BypassPinnedToReviewedHead(t *testing.T) {
	notWork(t)
	installCoverageGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	old := commitOnTaskBranch(t, client, baseURL, token, taskID, repoDir, map[string]string{"calc/calc.go": calcSrc})
	cur := commitOnTaskBranch(t, client, baseURL, token, taskID, repoDir, map[string]string{"calc/more.go": "package calc\n"})
	bare := gitOut(t, repoDir, "remote", "get-url", "origin")
	mainBefore := gitOut(t, bare, "rev-parse", "main")

	// No head: the gate refuses the bypass. A stale head: Approve's own
	// head pin (STA-717) refuses it before the gate runs.
	for name, tc := range map[string]struct{ body, wantErr string }{
		"no head":    {`{"merge_without_tests":true}`, "bypass_head_mismatch"},
		"stale head": {`{"merge_without_tests":true,"head_sha":"` + old + `"}`, "head_moved"},
	} {
		code, g := approveBody(t, client, baseURL, token, boardToken, taskID, tc.body)
		if code != http.StatusConflict || g.Error != tc.wantErr {
			t.Errorf("%s: %d %+v, want 409 %s", name, code, g, tc.wantErr)
		}
	}
	if got := gitOut(t, bare, "rev-parse", "main"); got != mainBefore {
		t.Fatal("a mismatched bypass merged")
	}
	var n int
	_ = database.QueryRow(`SELECT COUNT(*) FROM board_audit_log WHERE payload LIKE '%merge_without_tests%'`).Scan(&n)
	if n != 0 {
		t.Errorf("a refused bypass wrote %d audit rows", n)
	}
	if code, g := approveBody(t, client, baseURL, token, boardToken, taskID, `{"merge_without_tests":true,"head_sha":"`+cur+`"}`); code != http.StatusOK {
		t.Fatalf("bypass at the current head: %d %+v", code, g)
	}
}
