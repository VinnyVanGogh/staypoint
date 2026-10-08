package server_test

// task-2114d4aa: Approve and Merge PR refuse a red target branch unless the
// Board overrides, driven through the HTTP API with the fake gh from
// handlers_ship_review_pr_test.go answering `gh run list`.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

// redMainRuns: the newest push run on main failed; an older one passed.
const redMainRuns = `[
{"databaseId":901,"workflowName":"CI/CD Pipeline","headSha":"aaaaaaa1","status":"completed","conclusion":"failure","url":"https://github.com/o/r/actions/runs/901","createdAt":"2026-10-08T10:00:00Z"},
{"databaseId":900,"workflowName":"CI/CD Pipeline","headSha":"aaaaaaa0","status":"completed","conclusion":"success","url":"https://github.com/o/r/actions/runs/900","createdAt":"2026-10-08T09:00:00Z"}]`

const redMainJobs = `{"jobs":[{"name":"Build & Test (ubuntu-latest, 1.25)","conclusion":"success"},{"name":"Playwright UI Specs","conclusion":"failure"},{"name":"Release Binaries","conclusion":"skipped"}]}`

const greenMainRuns = `[
{"databaseId":902,"workflowName":"CI/CD Pipeline","headSha":"aaaaaaa2","status":"completed","conclusion":"success","url":"https://github.com/o/r/actions/runs/902","createdAt":"2026-10-08T11:00:00Z"}]`

type targetCIResp struct {
	Error    string              `json:"error"`
	TargetCI shipreview.TargetCI `json:"target_ci"`
}

func decodeTargetCI(t *testing.T, rb []byte) targetCIResp {
	t.Helper()
	var r targetCIResp
	if err := json.Unmarshal(rb, &r); err != nil {
		t.Fatalf("decode %s: %v", rb, err)
	}
	return r
}

func approveWith(t *testing.T, client *http.Client, baseURL, token, boardToken, taskID string, body map[string]any) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, rb := shipDoReq(t, client, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/approve", b, boardToken, "", "mock-assertion")
	return resp.StatusCode, rb
}

// The card's Target CI row: red names the failing job, green after a fix.
func TestShipReviewTargetCI_StateEndpoint(t *testing.T) {
	notWork(t)
	state := installFakeGH(t)
	database, baseURL, token, _, taskID, repoDir, client := shipApproveServer(t)
	setMergeMode(t, database, repoDir, shipreview.MergeModeOpenPR, "")
	writeState(t, state, "runs.json", redMainRuns)
	writeState(t, state, "jobs.json", redMainJobs)

	resp, rb := shipDoReq(t, client, token, "GET", baseURL+"/api/tasks/"+taskID+"/ship-review/target-ci?fresh=1", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET target-ci: %d %s", resp.StatusCode, rb)
	}
	ci := decodeTargetCI(t, rb).TargetCI
	if ci.State != shipreview.TargetCIRed || ci.Branch != "main" || len(ci.Failing) != 1 ||
		strings.Join(ci.Failing[0].FailedJobs, ",") != "Playwright UI Specs" || ci.Failing[0].RunID != 901 {
		t.Fatalf("target_ci = %+v", ci)
	}
	if !hasCall(ghCalls(t, state), "run list --branch main --event push") {
		t.Errorf("want gh run list on main, calls: %v", ghCalls(t, state))
	}

	writeState(t, state, "runs.json", greenMainRuns)
	// Without fresh=1 the display cache answers; fresh=1 reads GitHub again.
	_, rb = shipDoReq(t, client, token, "GET", baseURL+"/api/tasks/"+taskID+"/ship-review/target-ci", nil)
	if got := decodeTargetCI(t, rb).TargetCI.State; got != shipreview.TargetCIRed {
		t.Errorf("cached read = %s, want the cached red", got)
	}
	_, rb = shipDoReq(t, client, token, "GET", baseURL+"/api/tasks/"+taskID+"/ship-review/target-ci?fresh=1", nil)
	if got := decodeTargetCI(t, rb).TargetCI.State; got != shipreview.TargetCIGreen {
		t.Errorf("fresh read = %s, want green", got)
	}
}

// Approve onto a red main is refused before any PR opens, even when the
// display cache last saw main green; the override needs the head the Board
// saw and is audited with what was failing.
func TestShipReviewTargetCI_ApproveRefusesRedTarget(t *testing.T) {
	notWork(t)
	state := installFakeGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	setMergeMode(t, database, repoDir, shipreview.MergeModeOpenPR, "")
	head := gitOut(t, repoDir, "rev-parse", "staypoint/"+taskID)

	writeState(t, state, "runs.json", greenMainRuns)
	shipDoReq(t, client, token, "GET", baseURL+"/api/tasks/"+taskID+"/ship-review/target-ci", nil) // caches green
	writeState(t, state, "runs.json", redMainRuns)
	writeState(t, state, "jobs.json", redMainJobs)

	code, rb := approveWith(t, client, baseURL, token, boardToken, taskID, map[string]any{"head_sha": head})
	if code != http.StatusConflict || decodeTargetCI(t, rb).Error != "target_ci_red" {
		t.Fatalf("Approve on red main: %d %s", code, rb)
	}
	if !strings.Contains(string(rb), "Playwright UI Specs") {
		t.Errorf("refusal should name the failing job: %s", rb)
	}
	if hasCall(ghCalls(t, state), "pr create") {
		t.Fatal("PR opened despite red main")
	}

	// An override without the reviewed head is refused too.
	code, rb = approveWith(t, client, baseURL, token, boardToken, taskID, map[string]any{"merge_on_red_target": true})
	if code != http.StatusConflict || !strings.Contains(string(rb), "override_head_mismatch") {
		t.Fatalf("override without head_sha: %d %s", code, rb)
	}
	// Another head is caught earlier, by Approve's head_moved check.
	code, rb = approveWith(t, client, baseURL, token, boardToken, taskID, map[string]any{"merge_on_red_target": true, "head_sha": "deadbeef"})
	if code != http.StatusConflict || !strings.Contains(string(rb), "head_moved") {
		t.Fatalf("override for another head: %d %s", code, rb)
	}
	if hasCall(ghCalls(t, state), "pr create") {
		t.Fatal("PR opened on a refused override")
	}

	code, rb = approveWith(t, client, baseURL, token, boardToken, taskID,
		map[string]any{"merge_on_red_target": true, "merge_on_red_target_reason": "fix is this PR", "head_sha": head})
	if code != http.StatusOK {
		t.Fatalf("Approve with override: %d %s", code, rb)
	}
	audit := lastBoardAudit(t, database, "merge_on_red_target")
	failing, _ := audit["failing"].([]any)
	if audit["merge_action"] != "approve" || audit["head_sha"] != head || audit["target_branch"] != "main" ||
		audit["reason"] != "fix is this PR" || len(failing) != 1 || failing[0] != "Playwright UI Specs" {
		t.Errorf("merge_on_red_target audit = %v", audit)
	}
}

// pr_merge: Approve only opens the PR, so it is not gated; Merge PR is.
func TestShipReviewTargetCI_MergePRRefusesRedTarget(t *testing.T) {
	notWork(t)
	state := installFakeGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	setMergeMode(t, database, repoDir, shipreview.MergeModePRMerge, "")
	writeState(t, state, "runs.json", redMainRuns)
	writeState(t, state, "jobs.json", redMainJobs)

	if code, rb := prApprove(t, client, baseURL, token, boardToken, taskID); code != http.StatusOK {
		t.Fatalf("pr_merge Approve should not be gated by main's CI: %d %s", code, rb)
	}
	writeState(t, state, "checks.json", greenChecks)

	code, rb := prMerge(t, client, baseURL, token, boardToken, taskID, "")
	if code != http.StatusConflict || decodeTargetCI(t, rb).Error != "target_ci_red" {
		t.Fatalf("Merge PR on red main: %d %s", code, rb)
	}
	if hasCall(ghCalls(t, state), "pr merge") {
		t.Fatal("gh pr merge ran onto red main")
	}

	code, rb = prMerge(t, client, baseURL, token, boardToken, taskID, `{"merge_on_red_target":true,"merge_on_red_target_reason":"hotfix"}`)
	if code != http.StatusOK {
		t.Fatalf("Merge PR with override: %d %s", code, rb)
	}
	if audit := lastBoardAudit(t, database, "merge_on_red_target"); audit["merge_action"] != "merge_pr" || audit["reason"] != "hotfix" {
		t.Errorf("merge_on_red_target audit = %v", audit)
	}
}

// A main CI that can't be read (no runs, gh error) does not block: branch
// protection's required checks are the hard gate on GitHub.
func TestShipReviewTargetCI_UnknownDoesNotBlock(t *testing.T) {
	notWork(t)
	state := installFakeGH(t)
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	setMergeMode(t, database, repoDir, shipreview.MergeModeOpenPR, "")
	writeState(t, state, "runs.json", "not json")

	code, rb := prApprove(t, client, baseURL, token, boardToken, taskID)
	if code != http.StatusOK {
		t.Fatalf("Approve with unreadable main CI: %d %s", code, rb)
	}
	if !hasCall(ghCalls(t, state), "pr create") {
		t.Error("want the PR opened")
	}
}
