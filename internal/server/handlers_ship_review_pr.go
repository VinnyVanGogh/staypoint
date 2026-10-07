package server

// STA-717: ship review merge modes. Approve in "open_pr" opens or updates a
// GitHub PR and stops there; in "pr_merge" it also waits for CI on the pinned
// head, and the Board merges through GitHub from the card.

import (
	gocontext "context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/bridge"
	"github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

func init() { shipreview.IsWorkRepo = shipReviewIsWorkRepo }

// shipReviewIsWorkRepo decides the Open PR default: the bridge path rules,
// or the router's path, remote and scan-repos checks. The router counts all
// of ~/Documents/dev/worktrees as work (right for account routing, wrong
// here: personal repos have worktrees there too), so under that dir only the
// git remote decides.
func shipReviewIsWorkRepo(path string) bool {
	if path == "" {
		return false
	}
	if bridge.IsWorkRepo(path) {
		return true
	}
	isWork, src, _ := router.IsWorkRepo(path)
	if !isWork {
		return false
	}
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(src, "prefix: ~/Documents/dev/work") {
		wt := filepath.Join(home, "Documents", "dev", "worktrees")
		if p := filepath.Clean(path); p == wt || strings.HasPrefix(p, wt+string(filepath.Separator)) {
			return remoteIsWork(path)
		}
	}
	return true
}

// remoteIsWork reports whether the repo's origin points at a Managed Solution
// GitHub org.
func remoteIsWork(path string) bool {
	cmd := gitexec.Command(gocontext.Background(), "config", "--get", "remote.origin.url")
	cmd.Dir = path
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	u := strings.ToLower(string(out))
	return strings.Contains(u, "managedsolution") || strings.Contains(u, "managed-solution") || strings.Contains(u, "mansol")
}

func writeJSONStatus(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// requireGHAuth resolves the repo's gh identity, answering 409 when the
// project's gh_config_dir override is unusable.
func (h *ShipReviewHandler) requireGHAuth(w http.ResponseWriter, task *context.Task) (shipreview.GHAuth, bool) {
	cfg, err := shipreview.GetProjectDevConfig(h.db, task.RepoPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load project config: "+err.Error())
		return shipreview.GHAuth{}, false
	}
	auth, err := shipreview.ResolveGHAuth(cfg, task.RepoPath)
	if err != nil {
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "gh_auth", "message": err.Error()})
		return auth, false
	}
	return auth, true
}

// approvePR is Approve for the PR modes: push the pinned head, open or reuse
// the PR. open_pr closes the card there; pr_merge leaves it pending with
// checks running.
func (h *ShipReviewHandler) approvePR(w http.ResponseWriter, r *http.Request, card *shipreview.Card, task *context.Task, cfg *shipreview.ProjectDevConfig, mode string, gate *gateOutcome) {
	auth, err := shipreview.ResolveGHAuth(cfg, task.RepoPath)
	if err != nil {
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "gh_auth", "message": err.Error()})
		return
	}
	pr, err := shipreview.OpenOrUpdatePR(r.Context(), auth, card, true)
	if errors.Is(err, shipreview.ErrHeadMoved) {
		newHead, _ := shipreview.CurrentBranchHEAD(r.Context(), task.RepoPath, card.Branch)
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "head_moved", "message": err.Error(), "new_head_sha": newHead})
		return
	}
	if err != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{"error": "github_error", "message": err.Error()})
		return
	}
	if err := shipreview.SetPROpened(h.db, card.ID, mode, pr, card.HeadSHA); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = context.AddWorkProduct(h.db, task.ID, "pull_request", pr.URL)

	if mode == shipreview.MergeModeOpenPR {
		// StayPoint never merges in this mode: the card closes with the PR link.
		if err := shipreview.MarkPRApproved(h.db, card.ID, card.HeadSHA); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		shipreview.StopDevServer(h.db, card)
		_ = context.MarkTaskDone(h.db, task.ID)
	}

	payload := map[string]any{
		"action": "approve", "task_id": task.ID, "branch": card.Branch, "head_sha": card.HeadSHA,
		"merge_mode": mode, "pr_number": pr.Number, "pr_url": pr.URL,
		"ip": r.RemoteAddr, "user_agent": r.UserAgent(),
	}
	_ = governance.LogEvent(h.db, task.ID, "board", governance.AuditBoardAction, nil, nil, payload)
	_ = governance.LogBoardEvent(h.db, "board", governance.AuditBoardAction, payload)

	event := "ship_review_pr_opened"
	if mode == shipreview.MergeModeOpenPR {
		event = "ship_review_approved"
	}
	h.hub.Publish(event, map[string]any{
		"task_id": task.ID, "approved_sha": card.HeadSHA, "merge_mode": mode,
		"pr_number": pr.Number, "pr_url": pr.URL,
	})

	refreshed, _ := shipreview.GetCard(h.db, task.ID)
	resp := map[string]any{"card": refreshed, "merge_mode": mode, "pr_number": pr.Number, "pr_url": pr.URL}
	// open_pr hands the PR to GitHub here; a bypass files its task now.
	h.addGateResult(resp, card, task, gate, pr.Number, pr.URL, "")
	writeJSON(w, resp)
}

// Checks handles GET /api/tasks/{id}/ship-review/checks: polls the card's PR
// and its CI checks from GitHub. Read-only.
func (h *ShipReviewHandler) Checks(w http.ResponseWriter, r *http.Request) {
	card, task, ok := h.requireCard(w, r.PathValue("id"))
	if !ok {
		return
	}
	if card.PRNumber <= 0 {
		writeError(w, http.StatusConflict, shipreview.ErrNoPR.Error())
		return
	}
	auth, ok := h.requireGHAuth(w, task)
	if !ok {
		return
	}
	res, err := shipreview.PollChecks(r.Context(), h.db, auth, card)
	if err != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{"error": "github_error", "message": err.Error()})
		return
	}
	writeJSON(w, map[string]any{
		"pr_number": card.PRNumber, "pr_url": card.PRURL, "head_sha": card.HeadSHA,
		"pr_head_sha": res.PR.HeadRefOid, "pr_state": res.PR.State,
		"head_moved": res.HeadMoved, "checks": res.Checks, "summary": res.Summary,
	})
}

// CheckFailures handles GET /api/tasks/{id}/ship-review/check-failures: the
// failing checks with their failing log lines, and the pre-filled Send back
// comment built from them.
func (h *ShipReviewHandler) CheckFailures(w http.ResponseWriter, r *http.Request) {
	card, task, ok := h.requireCard(w, r.PathValue("id"))
	if !ok {
		return
	}
	if card.PRNumber <= 0 {
		writeError(w, http.StatusConflict, shipreview.ErrNoPR.Error())
		return
	}
	auth, ok := h.requireGHAuth(w, task)
	if !ok {
		return
	}
	res, err := shipreview.PollChecks(r.Context(), h.db, auth, card)
	if err != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{"error": "github_error", "message": err.Error()})
		return
	}
	failures := shipreview.CheckFailures(r.Context(), auth, res.Checks)
	if failures == nil {
		failures = []shipreview.CheckFailure{}
	}
	var pending []shipreview.PRCheck
	for _, c := range shipreview.NotGreen(res.Checks) {
		if c.Bucket == "pending" {
			pending = append(pending, c)
		}
	}
	writeJSON(w, map[string]any{
		"failures": failures, "pending": pending, "head_moved": res.HeadMoved,
		"comment": shipreview.FailuresComment(card, failures, pending),
	})
}

// MergePR handles POST /api/tasks/{id}/ship-review/merge (Board action): merge
// the card's PR through GitHub, pinned to the reviewed head. With
// {"override": true} it merges even when checks are not green, and the
// override is written to board_audit_log first.
func (h *ShipReviewHandler) MergePR(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	card, task, ok := h.requireCard(w, taskID)
	if !ok {
		return
	}
	if card.Status != shipreview.StatusPending {
		writeError(w, http.StatusConflict, "card is not pending")
		return
	}
	if card.MergeMode != shipreview.MergeModePRMerge || card.PRNumber <= 0 {
		writeError(w, http.StatusConflict, "card has no PR waiting to merge; Approve first")
		return
	}
	if card.PRChecksSHA != card.HeadSHA {
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "not_pushed",
			"message": "this head has not been pushed to the PR yet; Approve to push it and run checks"})
		return
	}
	var req struct {
		// HeadSHA is the head the Board was shown. The agent can replace the
		// card (and re-push the PR) while the Board looks at it, so the merge
		// is pinned to what the Board saw, not to the latest card.
		HeadSHA                 string `json:"head_sha"`
		Override                bool   `json:"override"`
		OverrideReason          string `json:"override_reason"`
		MigrationOverrideReason string `json:"migration_override_reason"`
		testGateBypass
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.HeadSHA == "" {
		writeError(w, http.StatusBadRequest, "head_sha (the head shown on the card) is required")
		return
	}
	if req.HeadSHA != card.HeadSHA {
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "head_moved",
			"message":      "the card was re-pinned to a new head since you opened it; review the new head before merging",
			"new_head_sha": card.HeadSHA})
		return
	}

	// STA-774: the head being merged is re-verified against the recorded
	// task base, as at Approve.
	if !h.verifyCardBase(w, r, card, task) {
		return
	}

	// The head being merged may be a re-push the Board has not Approved (the
	// fix after "Send failures to agent"), so the migration gate Approve
	// applies runs again here, fail-closed.
	if req.MigrationOverrideReason == "" {
		ctx, cancel := gitRequestContext(r)
		unverified, uErr := unverifiedMigrations(ctx, h.db, task)
		cancel()
		if uErr != nil {
			writeError(w, gitErrorStatus(uErr), "could not check migration verification status: "+uErr.Error())
			return
		}
		if len(unverified) > 0 {
			writeJSONStatus(w, http.StatusConflict, map[string]any{
				"error":                 "unverified_migrations",
				"message":               "one or more migration files have not been verified; mark them applied or supply migration_override_reason",
				"unverified_migrations": unverified,
			})
			return
		}
	} else {
		logPayload, _ := json.Marshal(map[string]string{"override_reason": req.MigrationOverrideReason})
		_ = context.LogActivity(h.db, taskID, "migration_override", string(logPayload))
	}

	auth, ok := h.requireGHAuth(w, task)
	if !ok {
		return
	}

	// STA-734: the test gate, after the head and CI facts above are settled.
	gate, ok := h.enforceTestGate(w, r, card, task, "merge_pr", req.HeadSHA, req.testGateBypass)
	if !ok {
		return
	}

	onOverride := func(overridden []shipreview.PRCheck) error {
		names := make([]string, 0, len(overridden))
		for _, c := range overridden {
			names = append(names, c.Name+" ("+c.Bucket+")")
		}
		return governance.LogBoardEvent(h.db, "board", governance.AuditBoardAction, map[string]any{
			"action": "merge_override", "task_id": taskID, "branch": card.Branch,
			"head_sha": card.HeadSHA, "pr_number": card.PRNumber, "pr_url": card.PRURL,
			"not_green_checks": names, "reason": req.OverrideReason,
			"ip": r.RemoteAddr, "user_agent": r.UserAgent(),
		})
	}

	res, err := shipreview.MergePR(r.Context(), h.db, auth, card, req.Override, onOverride)
	switch {
	case errors.Is(err, shipreview.ErrHeadMoved):
		// The checks no longer describe what would merge: drop them so the
		// card cannot offer Merge on them again.
		_ = shipreview.SetPRChecks(h.db, card.ID, "", []shipreview.PRCheck{})
		newHead, _ := shipreview.CurrentBranchHEAD(r.Context(), task.RepoPath, card.Branch)
		h.hub.Publish("ship_review_checks_reset", map[string]any{"task_id": taskID})
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "head_moved",
			"message":      "the branch moved after the checks started; merge blocked. Re-pin the card to the new head and its checks re-run.",
			"new_head_sha": newHead})
		return
	case errors.Is(err, shipreview.ErrChecksNotGreen):
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "checks_not_green",
			"message": err.Error(), "checks": res.Checks, "not_green": shipreview.NotGreen(res.Checks)})
		return
	case errors.Is(err, shipreview.ErrGitHubRefused):
		_ = governance.LogBoardEvent(h.db, "board", governance.AuditBoardAction, map[string]any{
			"action": "merge_pr_refused", "task_id": taskID, "pr_number": card.PRNumber,
			"head_sha": card.HeadSHA, "error": err.Error(),
		})
		h.hub.Publish("ship_review_merge_refused", map[string]any{"task_id": taskID})
		writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{"error": "github_refused", "message": unwrapRefusal(err)})
		return
	case err != nil:
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{"error": "github_error", "message": err.Error()})
		return
	}

	shipreview.StopDevServer(h.db, card)
	_ = context.AddWorkProduct(h.db, taskID, "commit", res.MainSHA)
	_ = context.MarkTaskDone(h.db, taskID)

	// The merge commit lives on GitHub; fetch it so cleanup can check that
	// every copy of the branch is contained in it before deleting.
	authCtx := auth.WithAuth(r.Context())
	deleteErr := ""
	if err := shipreview.FetchOrigin(authCtx, auth); err != nil {
		deleteErr = "fetch merge commit: " + err.Error()
		_ = shipreview.SetBranchCleanup(h.db, card.ID, false, deleteErr)
	} else {
		deleteErr = h.cleanupMergedBranch(authCtx, card, task, res.MainSHA)
	}

	payload := branchAuditPayload(r, "merge_pr", taskID, card.Branch, res.MainSHA, deleteErr)
	payload["pr_number"] = card.PRNumber
	payload["head_sha"] = card.HeadSHA
	payload["override"] = res.Overridden != nil
	_ = governance.LogEvent(h.db, taskID, "board", governance.AuditBoardAction, nil, nil, payload)
	_ = governance.LogBoardEvent(h.db, "board", governance.AuditBoardAction, payload)

	h.hub.Publish("ship_review_approved", map[string]any{
		"task_id": taskID, "approved_sha": card.HeadSHA, "main_sha": res.MainSHA,
		"pr_number": card.PRNumber, "branch_deleted": deleteErr == "",
	})

	refreshed, _ := shipreview.GetCard(h.db, taskID)
	resp := map[string]any{"card": refreshed, "main_sha": res.MainSHA, "branch_deleted": deleteErr == "", "pr_url": card.PRURL}
	h.addGateResult(resp, card, task, gate, card.PRNumber, card.PRURL, res.MainSHA)
	if deleteErr != "" {
		resp["branch_delete_error"] = deleteErr
		resp["warning"] = "merged; branch delete failed: " + deleteErr
	}
	writeJSON(w, resp)
}

// unwrapRefusal returns GitHub's own words from a wrapped ErrGitHubRefused.
func unwrapRefusal(err error) string {
	prefix := shipreview.ErrGitHubRefused.Error() + ": "
	msg := err.Error()
	if len(msg) > len(prefix) && msg[:len(prefix)] == prefix {
		return msg[len(prefix):]
	}
	return msg
}
