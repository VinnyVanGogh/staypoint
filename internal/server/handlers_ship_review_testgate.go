package server

// STA-734: merge test gate. Every Board action that lands a card on main
// (direct Approve, open_pr Approve, pr_merge Merge) first checks the change
// for CI, test changes and coverage. A blocking warning refuses the merge
// unless the Board sends merge_without_tests; that bypass is written to
// board_audit_log before anything merges, and once the merge stands a backlog
// "Add tests" task is filed, one per PR.

import (
	gocontext "context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
	"github.com/VinnyVanGogh/staypoint/internal/taskref"
	"github.com/VinnyVanGogh/staypoint/internal/testgate"
)

// testGateTimeout bounds one gate evaluation, CI artifact download included.
var testGateTimeout = 2 * time.Minute

// testGateBypass is the bypass part of an Approve / Merge request body.
type testGateBypass struct {
	MergeWithoutTests       bool   `json:"merge_without_tests"`
	MergeWithoutTestsReason string `json:"merge_without_tests_reason"`
}

// gateOutcome is what the gate decided for one merge request.
type gateOutcome struct {
	report   *testgate.Report
	bypassed bool
	reason   string
}

// gateDeps builds the gate's inputs for a task's repo: the project's exempt
// globs and, when the repo has a usable gh identity, the CI coverage reader.
func (h *ShipReviewHandler) gateDeps(task *context.Task) (shipreview.GateDeps, error) {
	extra, err := shipreview.GetTestExemptGlobs(h.db, task.RepoPath)
	if err != nil {
		return shipreview.GateDeps{}, err
	}
	deps := shipreview.GateDeps{ExtraExempt: extra}
	cfg, err := shipreview.GetProjectDevConfig(h.db, task.RepoPath)
	if err != nil {
		return deps, err
	}
	auth, err := shipreview.ResolveGHAuth(cfg, task.RepoPath)
	if err != nil {
		deps.CoverageUnavailable = "can't read CI artifacts for this repo (" + err.Error() + ")"
		return deps, nil
	}
	deps.Coverage = auth.CICoverage
	return deps, nil
}

func (h *ShipReviewHandler) evaluateTestGate(ctx gocontext.Context, card *shipreview.Card, task *context.Task) (*testgate.Report, error) {
	deps, err := h.gateDeps(task)
	if err != nil {
		return nil, err
	}
	ctx, cancel := gocontext.WithTimeout(ctx, testGateTimeout)
	defer cancel()
	return shipreview.EvaluateTestGate(ctx, h.db, card, task.RepoPath, deps)
}

// testTaskView is the "Add tests" task shown on the card.
type testTaskView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Created bool   `json:"created"`
}

// taskOrgProject reads a task's organization and project. context.GetTask
// does not fill them in, so they are read here directly.
func (h *ShipReviewHandler) taskOrgProject(t *context.Task) (string, string) {
	org, project := t.Organization, t.Project
	if org == "" && project == "" {
		_ = h.db.QueryRow(`SELECT COALESCE(organization,''), COALESCE(project,'') FROM tasks WHERE id = ?`, t.ID).Scan(&org, &project)
	}
	return org, project
}

func (h *ShipReviewHandler) taskPagePath(t *context.Task) string {
	if t.Identifier != "" {
		return taskref.Path(t.Identifier, t.Slug)
	}
	org, project := h.taskOrgProject(t)
	if org == "" {
		org = "STA"
	}
	if project == "" {
		project = "default"
	}
	return "/tasks/" + url.PathEscape(org) + "/" + url.PathEscape(project) + "/" + url.PathEscape(t.ID)
}

func (h *ShipReviewHandler) testTaskView(id string, created bool) *testTaskView {
	if id == "" {
		return nil
	}
	v := &testTaskView{ID: id, Created: created}
	if t, err := context.GetTask(h.db, id); err == nil {
		v.Name = t.Name
		v.URL = h.taskPagePath(t)
	}
	return v
}

// TestCoverage handles GET /api/tasks/{id}/ship-review/test-coverage: the
// card's "Test coverage" report for its current head. Read-only.
func (h *ShipReviewHandler) TestCoverage(w http.ResponseWriter, r *http.Request) {
	card, task, ok := h.requireCard(w, r.PathValue("id"))
	if !ok {
		return
	}
	resp := map[string]any{"head_sha": card.HeadSHA}
	if id, _ := shipreview.TestGapTaskFor(h.db, task.ID); id != "" {
		resp["test_task"] = h.testTaskView(id, false)
	}
	// A closed card shows the report it was decided on; it is not re-run.
	if card.Status != shipreview.StatusPending {
		prev, _ := shipreview.GetTestGate(h.db, card.ID)
		resp["report"] = prev
		writeJSON(w, resp)
		return
	}
	report, err := h.evaluateTestGate(r.Context(), card, task)
	if err != nil {
		writeJSONStatus(w, gitErrorStatus(err), map[string]any{
			"error": "test_gate_error", "message": "could not check test coverage: " + err.Error(), "head_sha": card.HeadSHA,
		})
		return
	}
	resp["report"] = report
	writeJSON(w, resp)
}

// enforceTestGate runs the gate for a merge request. It answers 409
// "untested" when a blocking warning stands and the Board did not bypass,
// and fails closed ("test_gate_error") when the change can't be read. On a
// bypass the audit row is written first; if that fails nothing merges.
//
// headSHA is the request's head_sha, the head the Board was shown. A bypass
// must carry it: an agent resubmit can swap the card under an open tab, and a
// bypass must never carry over to code the Board has not looked at.
func (h *ShipReviewHandler) enforceTestGate(w http.ResponseWriter, r *http.Request, card *shipreview.Card, task *context.Task, action, headSHA string, bypass testGateBypass) (*gateOutcome, bool) {
	if bypass.MergeWithoutTests && headSHA != card.HeadSHA {
		msg := "merge_without_tests needs head_sha, the head the Board reviewed"
		if headSHA != "" {
			msg = "the card moved to " + card.HeadSHA + " since the Board chose Merge without tests at " + headSHA + "; review the new head"
		}
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "bypass_head_mismatch", "message": msg, "head_sha": card.HeadSHA})
		return nil, false
	}
	report, err := h.evaluateTestGate(r.Context(), card, task)
	if err != nil {
		if !bypass.MergeWithoutTests {
			writeJSONStatus(w, gitErrorStatus(err), map[string]any{
				"error":   "test_gate_error",
				"message": "could not check test coverage: " + err.Error() + ". Retry, or use Merge without tests.",
			})
			return nil, false
		}
		// Bypassing a gate that could not run: record that, file the task
		// from what little is known.
		report = &testgate.Report{HeadSHA: card.HeadSHA, Blocking: true,
			Missing:  []string{"test coverage unknown: " + err.Error()},
			Warnings: []testgate.Warning{{Kind: testgate.KindNoCI, Blocking: true, Message: "The test gate could not run: " + err.Error()}}}
	}
	if !report.Blocking {
		return &gateOutcome{report: report}, true
	}
	if !bypass.MergeWithoutTests {
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error":         "untested",
			"message":       "this change is not tested: " + strings.Join(report.Missing, "; ") + ". Add tests, or use Merge without tests.",
			"missing":       report.Missing,
			"test_coverage": report,
		})
		return nil, false
	}
	if err := governance.LogBoardEvent(h.db, "board", governance.AuditBoardAction, map[string]any{
		"action": "merge_without_tests", "merge_action": action, "task_id": task.ID,
		"branch": card.Branch, "head_sha": card.HeadSHA, "pr_number": card.PRNumber, "pr_url": card.PRURL,
		"missing": report.Missing, "untested_sources": report.UntestedSources,
		"reason": bypass.MergeWithoutTestsReason, "ip": r.RemoteAddr, "user_agent": r.UserAgent(),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "audit log write failed; not merging: "+err.Error())
		return nil, false
	}
	return &gateOutcome{report: report, bypassed: true, reason: bypass.MergeWithoutTestsReason}, true
}

// fileTestGapTask files (or finds) the backlog "Add tests" task after a
// bypassed merge stood. One task per PR; per ship review task without a PR.
func (h *ShipReviewHandler) fileTestGapTask(card *shipreview.Card, task *context.Task, g *gateOutcome, prNumber int, prURL, mainSHA string) (*testTaskView, error) {
	if g == nil || !g.bypassed {
		return nil, nil
	}
	key := testgate.DedupeKey(task.RepoPath, prNumber, task.ID)
	existing, claimed, err := shipreview.ClaimTestGapTask(h.db, key, task.ID, card.ID, prNumber, card.HeadSHA)
	if err != nil {
		return nil, err
	}
	if !claimed {
		if existing == "" {
			return nil, fmt.Errorf("another request is filing the tests task for %s", key)
		}
		return h.testTaskView(existing, false), nil
	}
	info := testgate.GapTask{
		PRNumber: prNumber, PRURL: prURL, HeadSHA: card.HeadSHA, MainSHA: mainSHA, Branch: card.Branch,
		SourceTaskID: task.ID, SourceTaskName: task.Name, CardURL: h.taskPagePath(task), Reason: g.reason,
	}
	org, project := h.taskOrgProject(task)
	created, err := context.CreateTaskWithOptions(h.db, context.TaskCreateOptions{
		Name:           testgate.TaskTitle(g.report, info),
		RepoPath:       task.RepoPath,
		GitBranch:      "main",
		AccountRole:    task.AccountRole,
		Organization:   org,
		Project:        project,
		Description:    testgate.TaskDescription(g.report, info),
		ExecutionStage: governance.StageBacklog,
	})
	if err != nil {
		_ = shipreview.ReleaseTestGapClaim(h.db, key)
		return nil, err
	}
	if err := shipreview.SetTestGapTaskID(h.db, key, created.ID); err != nil {
		return nil, err
	}
	if prURL != "" {
		_ = context.AddWorkProduct(h.db, created.ID, "pull_request", prURL)
	}
	// Activity rows, not comments: a comment would wake the (done) task's agent.
	_ = context.LogActivity(h.db, created.ID, "test_gap_filed", fmt.Sprintf(`{"source_task_id":%q,"card_id":%q,"pr_number":%d,"head_sha":%q}`, task.ID, card.ID, prNumber, card.HeadSHA))
	_ = context.LogActivity(h.db, task.ID, "merged_without_tests", fmt.Sprintf(`{"test_task_id":%q,"pr_number":%d,"head_sha":%q}`, created.ID, prNumber, card.HeadSHA))
	h.hub.Publish("ship_review_test_task_created", map[string]any{"task_id": task.ID, "test_task_id": created.ID, "pr_number": prNumber})
	return h.testTaskView(created.ID, true), nil
}

// addGateResult puts the gate's outcome and any filed task on a merge response.
func (h *ShipReviewHandler) addGateResult(resp map[string]any, card *shipreview.Card, task *context.Task, g *gateOutcome, prNumber int, prURL, mainSHA string) {
	if g == nil {
		return
	}
	resp["test_coverage"] = g.report
	resp["merged_without_tests"] = g.bypassed
	tt, err := h.fileTestGapTask(card, task, g, prNumber, prURL, mainSHA)
	if err != nil {
		resp["test_task_error"] = err.Error()
		return
	}
	if tt != nil {
		resp["test_task"] = tt
	}
}
