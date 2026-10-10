package server_test

// STA-717: ship review merge modes, driven through the HTTP API against a
// real git repo with a bare origin and a fake gh on PATH. The fake keeps its
// PR state in files and reads the PR head live from origin, so pushes and
// head moves behave like GitHub; `pr merge` refuses a stale
// --match-head-commit and lands a real merge commit on origin/main.

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

const fakeGHScript = `#!/bin/sh
D="$FAKE_GH_DIR"
printf '%s|GH_CONFIG_DIR=%s|GH_TOKEN=%s\n' "$(printf '%s' "$*" | tr '\n' ' ')" "${GH_CONFIG_DIR:-}" "${GH_TOKEN:-}" >> "$D/calls"
head_sha() { git ls-remote origin "refs/heads/$1" | cut -f1; }
pr_json() {
  # $1 = number, $2 = url, $3 = branch
  state=OPEN; [ -f "$D/state" ] && state=$(cat "$D/state")
  mc=null; [ -f "$D/merge_sha" ] && mc="{\"oid\":\"$(cat "$D/merge_sha")\"}"
  printf '{"number":%s,"url":"%s","state":"%s","headRefOid":"%s","headRefName":"%s","baseRefName":"main","mergeCommit":%s}' \
    "$1" "$2" "$state" "$(head_sha "$3")" "$3" "$mc"
}
case "$1 $2" in
"repo view") echo main ;;
"pr list")
  if [ -f "$D/pr_list_raw" ]; then cat "$D/pr_list_raw"; rm "$D/pr_list_raw"; exit 0; fi
  if [ -f "$D/pr" ]; then set -- $(cat "$D/pr"); echo "[$(pr_json "$1" "$2" "$3")]"; else echo "[]"; fi ;;
"pr create")
  br=""; while [ $# -gt 0 ]; do [ "$1" = "--head" ] && br="$2"; shift; done
  echo "7 https://github.com/o/r/pull/7 $br" > "$D/pr"
  echo "https://github.com/o/r/pull/7" ;;
"pr view")
  set -- $(cat "$D/pr"); pr_json "$1" "$2" "$3" ;;
"pr checks")
  if [ -f "$D/checks.json" ]; then cat "$D/checks.json"; else echo "no checks reported on the 'x' branch" >&2; exit 1; fi
  [ -f "$D/checks_exit" ] && exit $(cat "$D/checks_exit")
  exit 0 ;;
"pr merge")
  if [ -f "$D/merge_refuse" ]; then cat "$D/merge_refuse" >&2; exit 1; fi
  want=""; n="$3"; shift 2; while [ $# -gt 0 ]; do [ "$1" = "--match-head-commit" ] && want="$2"; shift; done
  set -- $(cat "$D/pr"); cur=$(head_sha "$3")
  if [ "$want" != "$cur" ]; then echo "GraphQL: Head branch was modified. Review and try the merge again. (mergePullRequest)" >&2; exit 1; fi
  git fetch -q origin main || exit 1
  m=$(git commit-tree "$cur^{tree}" -p origin/main -p "$cur" -m "Merge pull request #$n") || exit 1
  git push -q origin "$m:refs/heads/main" || exit 1
  echo "$m" > "$D/merge_sha"; echo MERGED > "$D/state" ;;
"run view") cat "$D/runlog" 2>/dev/null ;;
*) echo "fake gh: unexpected $*" >&2; exit 2 ;;
esac
`

// installFakeGH puts the fake gh first on PATH and returns its state dir.
func installFakeGH(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeGHScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_GH_DIR", state)
	return state
}

func ghCalls(t *testing.T, state string) []string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(state, "calls"))
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func hasCall(calls []string, prefix string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func writeState(t *testing.T, state, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(state, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setMergeMode(t *testing.T, database *sql.DB, repoDir, mode, ghConfigDir string) {
	t.Helper()
	cfg, err := shipreview.GetProjectDevConfig(database, repoDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MergeMode = mode
	cfg.GHConfigDir = ghConfigDir
	if err := shipreview.UpsertProjectDevConfig(database, cfg); err != nil {
		t.Fatal(err)
	}
}

func notWork(t *testing.T) {
	t.Helper()
	prev := shipreview.IsWorkRepo
	shipreview.IsWorkRepo = func(string) bool { return false }
	t.Cleanup(func() { shipreview.IsWorkRepo = prev })
}

type prCardResp struct {
	Status          string               `json:"status"`
	HeadSHA         string               `json:"head_sha"`
	MainSHA         string               `json:"main_sha"`
	MergeMode       string               `json:"merge_mode"`
	PRNumber        int                  `json:"pr_number"`
	PRURL           string               `json:"pr_url"`
	PRChecksSHA     string               `json:"pr_checks_sha"`
	PRChecksSummary string               `json:"pr_checks_summary"`
	PRChecks        []shipreview.PRCheck `json:"pr_checks"`
	CIFixRequested  bool                 `json:"ci_fix_requested"`
	BranchDeleted   bool                 `json:"branch_deleted"`
	EffectiveMode   string               `json:"effective_merge_mode"`
}

func getPRCard(t *testing.T, client *http.Client, baseURL, token, taskID string) prCardResp {
	t.Helper()
	resp, rb := shipDoReq(t, client, token, "GET", baseURL+"/api/tasks/"+taskID+"/ship-review", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET card: %d %s", resp.StatusCode, rb)
	}
	var c prCardResp
	if err := json.Unmarshal(rb, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// prApprove runs Approve with a passing Board passkey.
func prApprove(t *testing.T, client *http.Client, baseURL, token, boardToken, taskID string) (int, []byte) {
	t.Helper()
	resp, rb := shipDoReq(t, client, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/approve", nil, boardToken, "", "mock-assertion")
	return resp.StatusCode, rb
}

// prMerge posts Merge with the card's current head as the head the Board saw,
// unless body sets head_sha itself.
func prMerge(t *testing.T, client *http.Client, baseURL, token, boardToken, taskID string, body string) (int, []byte) {
	t.Helper()
	req := map[string]any{}
	if body != "" {
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := req["head_sha"]; !ok {
		req["head_sha"] = getPRCard(t, client, baseURL, token, taskID).HeadSHA
	}
	b, _ := json.Marshal(req)
	resp, rb := shipDoReq(t, client, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/merge", b, boardToken, "", "mock-assertion")
	return resp.StatusCode, rb
}

const greenChecks = `[{"name":"Build & Test","state":"SUCCESS","bucket":"pass","link":"https://github.com/o/r/actions/runs/11/job/21","startedAt":"2026-10-05T10:00:00Z","completedAt":"2026-10-05T10:03:20Z","workflow":"CI"},
{"name":"Release","state":"SKIPPED","bucket":"skipping","link":"","startedAt":"0001-01-01T00:00:00Z","completedAt":"0001-01-01T00:00:00Z","workflow":"CI"}]`

const redChecks = `[{"name":"Build & Test","state":"FAILURE","bucket":"fail","link":"https://github.com/o/r/actions/runs/11/job/21","startedAt":"2026-10-05T10:00:00Z","completedAt":"2026-10-05T10:03:20Z","workflow":"CI"},
{"name":"Playwright UI Specs","state":"IN_PROGRESS","bucket":"pending","link":"https://github.com/o/r/actions/runs/11/job/22","startedAt":"2026-10-05T10:00:00Z","completedAt":"0001-01-01T00:00:00Z","workflow":"CI"}]`

// Open PR mode: Approve pushes the pinned head and opens a PR against the
// default branch; StayPoint never merges and the card closes with the link.
func TestShipReviewPR_OpenPRModeOpensPRAndNeverMerges(t *testing.T) {
	notWork(t)
	state := installFakeGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	setMergeMode(t, database, repoDir, shipreview.MergeModeOpenPR, "")
	bare := gitOut(t, repoDir, "remote", "get-url", "origin")
	mainBefore := gitOut(t, bare, "rev-parse", "main")
	head := gitOut(t, repoDir, "rev-parse", "staypoint/"+taskID)

	code, rb := prApprove(t, client, baseURL, token, boardToken, taskID)
	if code != http.StatusOK {
		t.Fatalf("Approve: %d %s", code, rb)
	}
	calls := ghCalls(t, state)
	if !hasCall(calls, "pr create --head staypoint/"+taskID+" --base main") {
		t.Fatalf("want gh pr create against main, calls: %v", calls)
	}
	if hasCall(calls, "pr merge") {
		t.Fatalf("open_pr must never merge, calls: %v", calls)
	}
	if got := gitOut(t, bare, "rev-parse", "main"); got != mainBefore {
		t.Errorf("origin main moved %s -> %s; open_pr must not merge", mainBefore, got)
	}
	if got := gitOut(t, bare, "rev-parse", "staypoint/"+taskID); got != head {
		t.Errorf("origin task branch = %s, want pinned %s", got, head)
	}
	c := getPRCard(t, client, baseURL, token, taskID)
	if c.Status != "approved" || c.MainSHA != "" || c.PRNumber != 7 || c.PRURL != "https://github.com/o/r/pull/7" || c.MergeMode != "open_pr" {
		t.Errorf("card after open_pr approve = %+v", c)
	}
	var wp int
	_ = database.QueryRow(`SELECT COUNT(*) FROM task_work_products WHERE task_id = ? AND product_type = 'pull_request' AND reference = ?`, taskID, c.PRURL).Scan(&wp)
	if wp != 1 {
		t.Errorf("pull_request work product rows = %d, want 1", wp)
	}
	audit := lastBoardAudit(t, database, "approve")
	if audit["merge_mode"] != "open_pr" || audit["pr_url"] != c.PRURL {
		t.Errorf("approve audit = %v", audit)
	}
}

// An open PR for the branch is reused, not duplicated.
func TestShipReviewPR_ReusesOpenPR(t *testing.T) {
	notWork(t)
	state := installFakeGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	setMergeMode(t, database, repoDir, shipreview.MergeModePRMerge, "")
	writeState(t, state, "pr", "42 https://github.com/o/r/pull/42 staypoint/"+taskID)

	code, rb := prApprove(t, client, baseURL, token, boardToken, taskID)
	if code != http.StatusOK {
		t.Fatalf("Approve: %d %s", code, rb)
	}
	if calls := ghCalls(t, state); hasCall(calls, "pr create") {
		t.Fatalf("existing PR must be reused, calls: %v", calls)
	}
	c := getPRCard(t, client, baseURL, token, taskID)
	if c.Status != "pending" || c.PRNumber != 42 || c.PRChecksSHA != c.HeadSHA || c.MergeMode != "pr_merge" {
		t.Errorf("card after pr_merge approve = %+v", c)
	}
}

// The checks poll reports per-check state and duration, and the summary
// moves running -> failed / passed with GitHub.
func TestShipReviewPR_ChecksPoll(t *testing.T) {
	notWork(t)
	state := installFakeGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	setMergeMode(t, database, repoDir, shipreview.MergeModePRMerge, "")
	if code, rb := prApprove(t, client, baseURL, token, boardToken, taskID); code != http.StatusOK {
		t.Fatalf("Approve: %d %s", code, rb)
	}

	poll := func() map[string]any {
		t.Helper()
		resp, rb := shipDoReq(t, client, token, "GET", baseURL+"/api/tasks/"+taskID+"/ship-review/checks", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("checks: %d %s", resp.StatusCode, rb)
		}
		var m map[string]any
		_ = json.Unmarshal(rb, &m)
		return m
	}

	// No checks reported yet.
	if m := poll(); m["summary"] != "none" {
		t.Errorf("summary with no checks = %v", m["summary"])
	}

	writeState(t, state, "checks.json", `[{"name":"Build & Test","state":"IN_PROGRESS","bucket":"pending","link":"https://github.com/o/r/actions/runs/11/job/21","startedAt":"2026-10-05T10:00:00Z","completedAt":"0001-01-01T00:00:00Z","workflow":"CI"},
{"name":"Lint","state":"SUCCESS","bucket":"pass","link":"https://github.com/o/r/actions/runs/11/job/23","startedAt":"2026-10-05T10:00:00Z","completedAt":"2026-10-05T10:00:42Z","workflow":"CI"}]`)
	writeState(t, state, "checks_exit", "8") // gh: checks pending
	m := poll()
	if m["summary"] != "running" {
		t.Fatalf("summary while pending = %v (%v)", m["summary"], m)
	}
	checks, _ := m["checks"].([]any)
	if len(checks) != 2 {
		t.Fatalf("checks = %v", m["checks"])
	}
	lint := checks[1].(map[string]any)
	if lint["name"] != "Lint" || lint["bucket"] != "pass" || lint["duration_sec"] != float64(42) || lint["link"] != "https://github.com/o/r/actions/runs/11/job/23" {
		t.Errorf("lint check = %v", lint)
	}
	if c := getPRCard(t, client, baseURL, token, taskID); c.PRChecksSummary != "running" || len(c.PRChecks) != 2 {
		t.Errorf("card checks not recorded: %+v", c)
	}

	writeState(t, state, "checks.json", redChecks)
	writeState(t, state, "checks_exit", "1")
	if m := poll(); m["summary"] != "failed" {
		t.Errorf("summary with a failure = %v", m["summary"])
	}
	writeState(t, state, "checks.json", greenChecks)
	writeState(t, state, "checks_exit", "0")
	if m := poll(); m["summary"] != "passed" {
		t.Errorf("summary all green = %v", m["summary"])
	}
}

// All green: Merge goes through gh pr merge pinned with --match-head-commit,
// lands on origin main, closes the card and deletes the branch.
func TestShipReviewPR_MergeWithMatchHeadCommit(t *testing.T) {
	notWork(t)
	state := installFakeGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	setMergeMode(t, database, repoDir, shipreview.MergeModePRMerge, "")
	head := gitOut(t, repoDir, "rev-parse", "staypoint/"+taskID)
	bare := gitOut(t, repoDir, "remote", "get-url", "origin")
	if code, rb := prApprove(t, client, baseURL, token, boardToken, taskID); code != http.StatusOK {
		t.Fatalf("Approve: %d %s", code, rb)
	}
	writeState(t, state, "checks.json", greenChecks)

	code, rb := prMerge(t, client, baseURL, token, boardToken, taskID, "")
	if code != http.StatusOK {
		t.Fatalf("Merge: %d %s", code, rb)
	}
	if !hasCall(ghCalls(t, state), "pr merge 7 --merge --match-head-commit "+head) {
		t.Fatalf("want pr merge pinned to %s, calls: %v", head, ghCalls(t, state))
	}
	mergeSHA := strings.TrimSpace(readFile(t, filepath.Join(state, "merge_sha")))
	if got := gitOut(t, bare, "rev-parse", "main"); got != mergeSHA {
		t.Errorf("origin main = %s, want merge commit %s", got, mergeSHA)
	}
	c := getPRCard(t, client, baseURL, token, taskID)
	if c.Status != "approved" || c.MainSHA != mergeSHA || !c.BranchDeleted {
		t.Errorf("card after merge = %+v", c)
	}
	if out := gitOut(t, bare, "branch", "--list", "staypoint/"+taskID); out != "" {
		t.Errorf("task branch still on origin after merge: %q", out)
	}
	audit := lastBoardAudit(t, database, "merge_pr")
	if audit["override"] != false || audit["main_sha"] != mergeSHA {
		t.Errorf("merge_pr audit = %v", audit)
	}
}

// Failing or pending checks block Merge without an override.
func TestShipReviewPR_MergeBlockedWhenChecksNotGreen(t *testing.T) {
	notWork(t)
	state := installFakeGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	setMergeMode(t, database, repoDir, shipreview.MergeModePRMerge, "")
	if code, rb := prApprove(t, client, baseURL, token, boardToken, taskID); code != http.StatusOK {
		t.Fatalf("Approve: %d %s", code, rb)
	}
	writeState(t, state, "checks.json", redChecks)
	writeState(t, state, "checks_exit", "1")

	code, rb := prMerge(t, client, baseURL, token, boardToken, taskID, "")
	if code != http.StatusConflict || !strings.Contains(string(rb), `"checks_not_green"`) {
		t.Fatalf("Merge on red checks: %d %s", code, rb)
	}
	var body struct {
		NotGreen []shipreview.PRCheck `json:"not_green"`
	}
	_ = json.Unmarshal(rb, &body)
	if len(body.NotGreen) != 2 {
		t.Errorf("not_green = %+v, want the failing and the pending check", body.NotGreen)
	}
	if hasCall(ghCalls(t, state), "pr merge") {
		t.Fatal("gh pr merge ran despite red checks")
	}
	if c := getPRCard(t, client, baseURL, token, taskID); c.Status != "pending" {
		t.Errorf("card status = %s, want pending", c.Status)
	}
}

// Merge anyway merges on red checks and records the override, with the
// checks it overrode, in board_audit_log.
func TestShipReviewPR_MergeAnywayIsAudited(t *testing.T) {
	notWork(t)
	state := installFakeGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	setMergeMode(t, database, repoDir, shipreview.MergeModePRMerge, "")
	head := gitOut(t, repoDir, "rev-parse", "staypoint/"+taskID)
	if code, rb := prApprove(t, client, baseURL, token, boardToken, taskID); code != http.StatusOK {
		t.Fatalf("Approve: %d %s", code, rb)
	}
	writeState(t, state, "checks.json", redChecks)
	writeState(t, state, "checks_exit", "1")

	code, rb := prMerge(t, client, baseURL, token, boardToken, taskID, `{"override":true,"override_reason":"flaky e2e"}`)
	if code != http.StatusOK {
		t.Fatalf("Merge anyway: %d %s", code, rb)
	}
	audit := lastBoardAudit(t, database, "merge_override")
	names, _ := audit["not_green_checks"].([]any)
	if audit["head_sha"] != head || audit["reason"] != "flaky e2e" || audit["pr_number"] != float64(7) || len(names) != 2 ||
		names[0] != "Build & Test (fail)" || names[1] != "Playwright UI Specs (pending)" {
		t.Errorf("merge_override audit = %v", audit)
	}
	if merged := lastBoardAudit(t, database, "merge_pr"); merged["override"] != true {
		t.Errorf("merge_pr audit override = %v", merged["override"])
	}
}

// If the branch moves after the checks started, Merge is blocked, the
// checks are dropped, and gh pr merge never runs.
func TestShipReviewPR_HeadMovedBlocksMerge(t *testing.T) {
	notWork(t)
	state := installFakeGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	setMergeMode(t, database, repoDir, shipreview.MergeModePRMerge, "")
	if code, rb := prApprove(t, client, baseURL, token, boardToken, taskID); code != http.StatusOK {
		t.Fatalf("Approve: %d %s", code, rb)
	}
	writeState(t, state, "checks.json", greenChecks)

	// Someone pushes another commit to the PR branch.
	branch := "staypoint/" + taskID
	gitOut(t, repoDir, "checkout", branch)
	if err := os.WriteFile(filepath.Join(repoDir, "late.txt"), []byte("late\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repoDir, "add", ".")
	gitOut(t, repoDir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-m", "late push")
	gitOut(t, repoDir, "push", "origin", branch)
	newHead := gitOut(t, repoDir, "rev-parse", "HEAD")
	gitOut(t, repoDir, "checkout", "main")

	code, rb := prMerge(t, client, baseURL, token, boardToken, taskID, `{"override":true}`)
	if code != http.StatusConflict || !strings.Contains(string(rb), `"head_moved"`) || !strings.Contains(string(rb), newHead) {
		t.Fatalf("Merge after head moved: %d %s", code, rb)
	}
	if hasCall(ghCalls(t, state), "pr merge") {
		t.Fatal("gh pr merge ran after the head moved")
	}
	c := getPRCard(t, client, baseURL, token, taskID)
	if c.Status != "pending" || c.PRChecksSHA != "" || len(c.PRChecks) != 0 {
		t.Errorf("card after head moved = %+v; checks must be dropped", c)
	}
	// And Merge stays refused until the new head is pushed and checked.
	if code, rb := prMerge(t, client, baseURL, token, boardToken, taskID, ""); code != http.StatusConflict || !strings.Contains(string(rb), "not_pushed") {
		t.Errorf("second Merge: %d %s", code, rb)
	}
}

// GitHub's refusal (branch protection, required reviews) comes back verbatim.
func TestShipReviewPR_GitHubRefusalShownVerbatim(t *testing.T) {
	notWork(t)
	state := installFakeGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	setMergeMode(t, database, repoDir, shipreview.MergeModePRMerge, "")
	if code, rb := prApprove(t, client, baseURL, token, boardToken, taskID); code != http.StatusOK {
		t.Fatalf("Approve: %d %s", code, rb)
	}
	writeState(t, state, "checks.json", greenChecks)
	reason := "GraphQL: At least 1 approving review is required by reviewers with write access. (mergePullRequest)"
	writeState(t, state, "merge_refuse", reason+"\n")

	code, rb := prMerge(t, client, baseURL, token, boardToken, taskID, "")
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("Merge refused: %d %s", code, rb)
	}
	var body map[string]string
	_ = json.Unmarshal(rb, &body)
	if body["error"] != "github_refused" || body["message"] != reason {
		t.Errorf("refusal body = %v, want message %q", body, reason)
	}
	if c := getPRCard(t, client, baseURL, token, taskID); c.Status != "pending" {
		t.Errorf("card status = %s, want pending", c.Status)
	}
}

// Work repos default to Open PR and use the repo's normal gh auth (Board,
// 2026-10-05); gh_config_dir is an optional override that wins over tokens.
func TestShipReviewPR_WorkRepoAuth(t *testing.T) {
	state := installFakeGH(t)
	daemonDir := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", daemonDir)
	t.Setenv("GH_TOKEN", "daemon-token")
	prev := shipreview.IsWorkRepo
	shipreview.IsWorkRepo = func(string) bool { return true }
	t.Cleanup(func() { shipreview.IsWorkRepo = prev })

	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	if c := getPRCard(t, client, baseURL, token, taskID); c.EffectiveMode != "open_pr" {
		t.Fatalf("work repo default mode = %q, want open_pr", c.EffectiveMode)
	}
	if code, rb := prApprove(t, client, baseURL, token, boardToken, taskID); code != http.StatusOK {
		t.Fatalf("Approve with the normal gh auth: %d %s", code, rb)
	}
	for _, c := range ghCalls(t, state) {
		if !strings.HasSuffix(c, "|GH_CONFIG_DIR="+daemonDir+"|GH_TOKEN=daemon-token") {
			t.Errorf("gh call did not use the normal gh auth: %q", c)
		}
	}

	// With an override the dir wins and the daemon's token is dropped.
	_ = os.Remove(filepath.Join(state, "calls"))
	_ = os.Remove(filepath.Join(state, "pr"))
	_, _ = database.Exec(`UPDATE ship_review_cards SET status = 'pending' WHERE task_id = ?`, taskID)
	override := t.TempDir()
	setMergeMode(t, database, repoDir, "", override)
	if code, rb := prApprove(t, client, baseURL, token, boardToken, taskID); code != http.StatusOK {
		t.Fatalf("Approve with gh_config_dir: %d %s", code, rb)
	}
	calls := ghCalls(t, state)
	if len(calls) == 0 {
		t.Fatal("no gh calls")
	}
	for _, c := range calls {
		if !strings.HasSuffix(c, "|GH_CONFIG_DIR="+override+"|GH_TOKEN=") {
			t.Errorf("gh call not under the override: %q", c)
		}
	}
}

// Merge is pinned to the head the Board saw: if the agent re-pins the card
// (and re-pushes the PR) between render and click, Merge is refused.
func TestShipReviewPR_MergePinnedToHeadBoardSaw(t *testing.T) {
	notWork(t)
	state := installFakeGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	setMergeMode(t, database, repoDir, shipreview.MergeModePRMerge, "")
	if code, rb := prApprove(t, client, baseURL, token, boardToken, taskID); code != http.StatusOK {
		t.Fatalf("Approve: %d %s", code, rb)
	}
	seen := getPRCard(t, client, baseURL, token, taskID).HeadSHA

	// Send failures, then the agent resubmits B, which is re-pushed to the PR.
	sb, _ := json.Marshal(map[string]any{"comment": "CI red", "ci_failures": true})
	if resp, rb := shipDoReq(t, client, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/send-back", sb, boardToken, "", "mock-assertion"); resp.StatusCode != http.StatusOK {
		t.Fatalf("send-back: %d %s", resp.StatusCode, rb)
	}
	branch := "staypoint/" + taskID
	gitOut(t, repoDir, "checkout", branch)
	if err := os.WriteFile(filepath.Join(repoDir, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repoDir, "add", ".")
	gitOut(t, repoDir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-m", "B")
	headB := gitOut(t, repoDir, "rev-parse", "HEAD")
	gitOut(t, repoDir, "checkout", "main")
	up, _ := json.Marshal(map[string]any{"test_steps": []string{"1. ok"}})
	if resp, rb := shipDoReq(t, client, token, "PUT", baseURL+"/api/tasks/"+taskID+"/ship-review", up); resp.StatusCode != http.StatusCreated {
		t.Fatalf("resubmit: %d %s", resp.StatusCode, rb)
	}
	writeState(t, state, "checks.json", greenChecks)

	// The Board, still looking at the old head, clicks Merge anyway.
	code, rb := prMerge(t, client, baseURL, token, boardToken, taskID, `{"head_sha":"`+seen+`","override":true}`)
	if code != http.StatusConflict || !strings.Contains(string(rb), `"head_moved"`) || !strings.Contains(string(rb), headB) {
		t.Fatalf("Merge on a stale view: %d %s", code, rb)
	}
	if hasCall(ghCalls(t, state), "pr merge") {
		t.Fatal("gh pr merge ran for a head the Board never saw")
	}
	// Without head_sha the request is refused outright.
	resp, rb := shipDoReq(t, client, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/merge", []byte(`{}`), boardToken, "", "mock-assertion")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Merge without head_sha: %d %s", resp.StatusCode, rb)
	}
}

// A PR merged on GitHub outside StayPoint (by hand, auto-merge, merge queue)
// is recorded by Merge instead of leaving the card stuck pending.
func TestShipReviewPR_MergedOutsideIsFinalized(t *testing.T) {
	notWork(t)
	state := installFakeGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	setMergeMode(t, database, repoDir, shipreview.MergeModePRMerge, "")
	head := gitOut(t, repoDir, "rev-parse", "staypoint/"+taskID)
	if code, rb := prApprove(t, client, baseURL, token, boardToken, taskID); code != http.StatusOK {
		t.Fatalf("Approve: %d %s", code, rb)
	}
	writeState(t, state, "checks.json", redChecks)

	// Someone merges the PR on GitHub.
	gh := exec.Command(filepath.Join(filepath.SplitList(os.Getenv("PATH"))[0], "gh"), "pr", "merge", "7", "--merge", "--match-head-commit", head)
	gh.Dir = repoDir
	if out, err := gh.CombinedOutput(); err != nil {
		t.Fatalf("outside merge: %v %s", err, out)
	}
	mergeSHA := strings.TrimSpace(readFile(t, filepath.Join(state, "merge_sha")))

	code, rb := prMerge(t, client, baseURL, token, boardToken, taskID, "")
	if code != http.StatusOK {
		t.Fatalf("Merge after an outside merge: %d %s", code, rb)
	}
	if n := strings.Count(strings.Join(ghCalls(t, state), "\n"), "pr merge"); n != 1 {
		t.Errorf("pr merge ran %d times, want only the outside one", n)
	}
	c := getPRCard(t, client, baseURL, token, taskID)
	if c.Status != "approved" || c.MainSHA != mergeSHA {
		t.Errorf("card after outside merge = %+v, want approved at %s", c, mergeSHA)
	}
}

// Send failures to agent: the pre-filled comment names the failing checks,
// their failing log lines and links; the agent's resubmitted card is pushed
// to the same PR and its checks re-run on the new head.
func TestShipReviewPR_SendFailuresThenRepin(t *testing.T) {
	notWork(t)
	state := installFakeGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	setMergeMode(t, database, repoDir, shipreview.MergeModePRMerge, "")
	if code, rb := prApprove(t, client, baseURL, token, boardToken, taskID); code != http.StatusOK {
		t.Fatalf("Approve: %d %s", code, rb)
	}
	writeState(t, state, "checks.json", redChecks)
	writeState(t, state, "checks_exit", "1")
	writeState(t, state, "runlog", "Build & Test\tRun go test\t2026-10-05T10:03:01.1234567Z ok  \tpkg/a\t0.2s\n"+
		"Build & Test\tRun go test\t2026-10-05T10:03:02.1234567Z --- FAIL: TestMergeMode (0.01s)\n"+
		"Build & Test\tRun go test\t2026-10-05T10:03:02.2234567Z     pr_test.go:42: want open_pr, got direct\n"+
		"Build & Test\tRun go test\t2026-10-05T10:03:03.1234567Z FAIL\tgithub.com/x/y/internal/shipreview\t0.4s\n")

	resp, rb := shipDoReq(t, client, token, "GET", baseURL+"/api/tasks/"+taskID+"/ship-review/check-failures", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("check-failures: %d %s", resp.StatusCode, rb)
	}
	var cf struct {
		Comment  string                    `json:"comment"`
		Failures []shipreview.CheckFailure `json:"failures"`
	}
	_ = json.Unmarshal(rb, &cf)
	if !hasCall(ghCalls(t, state), "run view 11 --job 21 --log-failed") {
		t.Errorf("failing job log not read, calls: %v", ghCalls(t, state))
	}
	for _, want := range []string{"Build & Test", "--- FAIL: TestMergeMode (0.01s)", "FAIL\tgithub.com/x/y/internal/shipreview\t0.4s",
		"https://github.com/o/r/actions/runs/11/job/21", "Playwright UI Specs"} {
		if !strings.Contains(cf.Comment, want) {
			t.Errorf("pre-filled comment missing %q:\n%s", want, cf.Comment)
		}
	}
	if strings.Contains(cf.Comment, "ok  \tpkg/a") {
		t.Errorf("passing log line leaked into the comment:\n%s", cf.Comment)
	}

	sb, _ := json.Marshal(map[string]any{"comment": cf.Comment, "ci_failures": true})
	resp, rb = shipDoReq(t, client, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/send-back", sb, boardToken, "", "mock-assertion")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send-back: %d %s", resp.StatusCode, rb)
	}
	if c := getPRCard(t, client, baseURL, token, taskID); c.Status != "sent_back" || !c.CIFixRequested {
		t.Fatalf("card after send failures = %+v", c)
	}

	// The agent fixes it on the task branch and resubmits.
	branch := "staypoint/" + taskID
	gitOut(t, repoDir, "checkout", branch)
	if err := os.WriteFile(filepath.Join(repoDir, "fix.txt"), []byte("fix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repoDir, "add", ".")
	gitOut(t, repoDir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-m", "fix CI")
	newHead := gitOut(t, repoDir, "rev-parse", "HEAD")
	gitOut(t, repoDir, "checkout", "main")
	up, _ := json.Marshal(map[string]any{"test_steps": []string{"1. CI is green"}})
	resp, rb = shipDoReq(t, client, token, "PUT", baseURL+"/api/tasks/"+taskID+"/ship-review", up)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("resubmit: %d %s", resp.StatusCode, rb)
	}

	c := getPRCard(t, client, baseURL, token, taskID)
	if c.Status != "pending" || c.HeadSHA != newHead || c.PRNumber != 7 || c.PRChecksSHA != newHead || c.MergeMode != "pr_merge" {
		t.Errorf("resubmitted card = %+v, want PR #7 re-pinned to %s", c, newHead)
	}
	bare := gitOut(t, repoDir, "remote", "get-url", "origin")
	if got := gitOut(t, bare, "rev-parse", branch); got != newHead {
		t.Errorf("origin branch = %s, want the fix %s pushed", got, newHead)
	}
	if n := strings.Count(strings.Join(ghCalls(t, state), "\n"), "pr create"); n != 1 {
		t.Errorf("pr create ran %d times, want 1 (PR reused)", n)
	}
}

// The merge mode is a Board-gated dev config field, validated and audited.
func TestShipReviewPR_DevConfigMergeMode(t *testing.T) {
	notWork(t)
	database, baseURL, token, boardToken, _, repoDir, client := shipApproveServer(t)
	put := func(body map[string]any, opts ...string) (int, []byte) {
		b, _ := json.Marshal(body)
		resp, rb := shipDoReq(t, client, token, "PUT", baseURL+"/api/project-dev-configs", b, opts...)
		return resp.StatusCode, rb
	}
	if code, rb := put(map[string]any{"repo_path": repoDir, "merge_mode": "pr_merge"}); code == http.StatusOK {
		t.Fatalf("merge_mode saved without a Board session: %s", rb)
	}
	if code, rb := put(map[string]any{"repo_path": repoDir, "merge_mode": "yolo"}, boardToken, "", "mock-assertion"); code != http.StatusBadRequest {
		t.Fatalf("invalid merge_mode: %d %s", code, rb)
	}
	if code, rb := put(map[string]any{"repo_path": repoDir, "gh_config_dir": "relative/gh"}, boardToken, "", "mock-assertion"); code != http.StatusBadRequest {
		t.Fatalf("relative gh_config_dir: %d %s", code, rb)
	}
	if code, rb := put(map[string]any{"repo_path": repoDir, "merge_mode": "pr_merge"}, boardToken, "", "mock-assertion"); code != http.StatusOK {
		t.Fatalf("save merge_mode: %d %s", code, rb)
	}
	cfg, _ := shipreview.GetProjectDevConfig(database, repoDir)
	if cfg.MergeMode != "pr_merge" {
		t.Errorf("merge_mode = %q", cfg.MergeMode)
	}
	var payload string
	_ = database.QueryRow(`SELECT payload FROM board_audit_log WHERE event_type = 'dev_config_change' ORDER BY id DESC LIMIT 1`).Scan(&payload)
	if !strings.Contains(payload, `"new_merge_mode":"pr_merge"`) {
		t.Errorf("dev_config_change audit = %s", payload)
	}
	resp, rb := shipDoReq(t, client, token, "GET", baseURL+"/api/project-dev-configs", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(rb), `"effective_merge_mode":"pr_merge"`) {
		t.Errorf("list configs: %d %s", resp.StatusCode, rb)
	}
}

// A fork's open PR from a same-named branch is not this task's PR: a new
// PR is created for the task branch instead.
func TestShipReviewPR_IgnoresForkPRWithSameBranch(t *testing.T) {
	notWork(t)
	state := installFakeGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	setMergeMode(t, database, repoDir, shipreview.MergeModePRMerge, "")
	head := gitOut(t, repoDir, "rev-parse", "staypoint/"+taskID)
	writeState(t, state, "pr_list_raw", `[{"number":99,"url":"https://github.com/o/r/pull/99","state":"OPEN","headRefOid":"`+head+
		`","headRefName":"staypoint/`+taskID+`","baseRefName":"main","isCrossRepository":true,"mergeCommit":null}]`)

	if code, rb := prApprove(t, client, baseURL, token, boardToken, taskID); code != http.StatusOK {
		t.Fatalf("Approve: %d %s", code, rb)
	}
	if !hasCall(ghCalls(t, state), "pr create") {
		t.Fatalf("fork PR was reused, calls: %v", ghCalls(t, state))
	}
	if c := getPRCard(t, client, baseURL, token, taskID); c.PRNumber != 7 {
		t.Errorf("card PR = #%d, want the task's own #7", c.PRNumber)
	}
}

// Merge re-runs Approve's migration gate: a fix re-pushed after "Send
// failures to agent" that adds a migration cannot be merged until the
// migration is marked applied (or explicitly overridden).
func TestShipReviewPR_MergeRechecksMigrations(t *testing.T) {
	notWork(t)
	state := installFakeGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	setMergeMode(t, database, repoDir, shipreview.MergeModePRMerge, "")
	if code, rb := prApprove(t, client, baseURL, token, boardToken, taskID); code != http.StatusOK {
		t.Fatalf("Approve: %d %s", code, rb)
	}
	sb, _ := json.Marshal(map[string]any{"comment": "CI red", "ci_failures": true})
	if resp, rb := shipDoReq(t, client, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/send-back", sb, boardToken, "", "mock-assertion"); resp.StatusCode != http.StatusOK {
		t.Fatalf("send-back: %d %s", resp.StatusCode, rb)
	}
	branch := "staypoint/" + taskID
	gitOut(t, repoDir, "checkout", branch)
	if err := os.MkdirAll(filepath.Join(repoDir, "supabase", "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "supabase", "migrations", "001_x.sql"), []byte("select 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repoDir, "add", ".")
	gitOut(t, repoDir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-m", "fix with migration")
	gitOut(t, repoDir, "checkout", "main")
	up, _ := json.Marshal(map[string]any{"test_steps": []string{"1. ok"}})
	if resp, rb := shipDoReq(t, client, token, "PUT", baseURL+"/api/tasks/"+taskID+"/ship-review", up); resp.StatusCode != http.StatusCreated {
		t.Fatalf("resubmit: %d %s", resp.StatusCode, rb)
	}
	writeState(t, state, "checks.json", greenChecks)
	// The migration diff is taken against the task's checkpoint ref.
	gitOut(t, repoDir, "update-ref", "refs/staypoint/checkpoints/latest", "main")

	code, rb := prMerge(t, client, baseURL, token, boardToken, taskID, "")
	if code != http.StatusConflict || !strings.Contains(string(rb), "unverified_migrations") || !strings.Contains(string(rb), "001_x.sql") {
		t.Fatalf("Merge with an unverified migration: %d %s", code, rb)
	}
	if hasCall(ghCalls(t, state), "pr merge") {
		t.Fatal("gh pr merge ran with an unverified migration")
	}
}

// task-a5c42165: "Open PR" on a direct-mode card opens a PR so CI runs on
// the commit, and never merges; the card then follows the pr_merge flow.
func TestShipReviewPR_OpenPRForCIOnDirectCard(t *testing.T) {
	notWork(t)
	state := installFakeGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	setMergeMode(t, database, repoDir, shipreview.MergeModeDirect, "")
	bare := gitOut(t, repoDir, "remote", "get-url", "origin")
	mainBefore := gitOut(t, bare, "rev-parse", "main")
	head := gitOut(t, repoDir, "rev-parse", "staypoint/"+taskID)

	body, _ := json.Marshal(map[string]any{"head_sha": head, "open_pr_for_ci": true})
	resp, rb := shipDoReq(t, client, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/approve", body, boardToken, "", "mock-assertion")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Approve open_pr_for_ci: %d %s", resp.StatusCode, rb)
	}
	calls := ghCalls(t, state)
	if !hasCall(calls, "pr create --head staypoint/"+taskID) || hasCall(calls, "pr merge") {
		t.Fatalf("want a PR opened and nothing merged, calls: %v", calls)
	}
	if got := gitOut(t, bare, "rev-parse", "main"); got != mainBefore {
		t.Errorf("origin main moved %s -> %s; Open PR must not merge", mainBefore, got)
	}
	c := getPRCard(t, client, baseURL, token, taskID)
	if c.Status != "pending" || c.PRNumber != 7 || c.MergeMode != "pr_merge" || c.EffectiveMode != "pr_merge" {
		t.Errorf("card after Open PR = %+v", c)
	}

	// The card now has a PR: open_pr_for_ci no longer applies.
	resp, rb = shipDoReq(t, client, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/approve", body, boardToken, "", "mock-assertion")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second open_pr_for_ci: %d %s, want 409", resp.StatusCode, rb)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// task-a5c42165: Approve on a branch whose head is already in the target
// (merged earlier through its own PR, branch tip now the merge commit) says
// so and closes the card, instead of a 502 from `gh pr create` ("No commits
// between"), and never rewinds the merged branch on origin.
func TestShipReviewPR_ApproveAlreadyMerged(t *testing.T) {
	notWork(t)
	state := installFakeGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	setMergeMode(t, database, repoDir, shipreview.MergeModePRMerge, "")
	bare := gitOut(t, repoDir, "remote", "get-url", "origin")
	branch := "staypoint/" + taskID
	head := gitOut(t, repoDir, "rev-parse", branch)

	// PR #477 merged the branch into main on GitHub; the branch tip there
	// moved on to the merge commit.
	if out, err := exec.Command("git", "-C", repoDir, "push", "-q", "origin", head+":refs/heads/"+branch).CombinedOutput(); err != nil {
		t.Fatalf("push branch: %v %s", err, out)
	}
	m := gitOut(t, bare, "-c", "user.name=t", "-c", "user.email=t@t", "commit-tree", head+"^{tree}", "-p", "main", "-p", head, "-m", "Merge pull request #477")
	gitOut(t, bare, "update-ref", "refs/heads/main", m)
	gitOut(t, bare, "update-ref", "refs/heads/"+branch, m)
	writeState(t, state, "pr", "477 https://github.com/o/r/pull/477 "+branch)
	writeState(t, state, "state", "MERGED")
	writeState(t, state, "merge_sha", m)

	code, rb := prApprove(t, client, baseURL, token, boardToken, taskID)
	if code != http.StatusOK {
		t.Fatalf("Approve on an already merged branch: %d %s", code, rb)
	}
	var resp struct {
		AlreadyMerged bool   `json:"already_merged"`
		Target        string `json:"target"`
		MainSHA       string `json:"main_sha"`
		PRNumber      int    `json:"pr_number"`
		Message       string `json:"message"`
		NextStep      string `json:"next_step"`
	}
	if err := json.Unmarshal(rb, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.AlreadyMerged || resp.Target != "main" || resp.MainSHA != m || resp.PRNumber != 477 ||
		resp.Message != "Already merged into main (PR #477)" || resp.NextStep == "" {
		t.Errorf("approve response = %+v", resp)
	}
	calls := ghCalls(t, state)
	if hasCall(calls, "pr create") || hasCall(calls, "pr merge") {
		t.Fatalf("already merged: nothing to create or merge, calls: %v", calls)
	}
	if got := gitOut(t, bare, "rev-parse", branch); got != m {
		t.Errorf("origin %s = %s, want merge commit %s left alone", branch, got, m)
	}
	c := getPRCard(t, client, baseURL, token, taskID)
	if c.Status != "approved" || c.MainSHA != m {
		t.Errorf("card after already-merged approve = %+v", c)
	}
}
