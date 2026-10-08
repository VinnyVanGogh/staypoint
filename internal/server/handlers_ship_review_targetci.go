package server

// task-2114d4aa: Approve and Merge PR refuse to land a card on a target
// branch whose CI is red, unless the Board sends merge_on_red_target. That
// override is written to board_audit_log before anything merges.

import (
	"net/http"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

// targetCIOverride is the override part of an Approve / Merge request body.
type targetCIOverride struct {
	MergeOnRedTarget       bool   `json:"merge_on_red_target"`
	MergeOnRedTargetReason string `json:"merge_on_red_target_reason"`
}

// readTargetCI reads the CI of the branch card merges into. A repo without a
// usable gh identity or target is unknown, which does not block.
func (h *ShipReviewHandler) readTargetCI(r *http.Request, card *shipreview.Card, task *context.Task, maxAge time.Duration) shipreview.TargetCI {
	unknown := func(branch, note string) shipreview.TargetCI {
		return shipreview.TargetCI{Branch: branch, State: shipreview.TargetCIUnknown, Note: note,
			CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	}
	target := card.TargetBranch
	if target == "" {
		t, err := shipreview.TaskTargetBranch(r.Context(), h.db, task.RepoPath, task.ID)
		if err != nil {
			return unknown("", "could not resolve the target branch: "+err.Error())
		}
		target = t
	}
	cfg, err := shipreview.GetProjectDevConfig(h.db, task.RepoPath)
	if err != nil {
		return unknown(target, "load project config: "+err.Error())
	}
	auth, err := shipreview.ResolveGHAuth(cfg, task.RepoPath)
	if err != nil {
		return unknown(target, "can't read CI for this repo ("+err.Error()+")")
	}
	return auth.TargetCIWithin(r.Context(), target, maxAge)
}

// TargetCIState handles GET /api/tasks/{id}/ship-review/target-ci: the CI of
// the card's target branch. ?fresh=1 skips the short display cache.
// Read-only.
func (h *ShipReviewHandler) TargetCIState(w http.ResponseWriter, r *http.Request) {
	card, task, ok := h.requireCard(w, r.PathValue("id"))
	if !ok {
		return
	}
	maxAge := shipreview.TargetCICacheTTL
	if r.URL.Query().Get("fresh") == "1" {
		maxAge = 0
	}
	writeJSON(w, map[string]any{"target_ci": h.readTargetCI(r, card, task, maxAge)})
}

// enforceTargetCI reads the target's CI fresh for a merge request and
// answers 409 "target_ci_red" when it is red and the Board did not override.
// An override must carry head_sha, the head the Board was shown, and its
// audit row is written first; if that fails nothing merges.
func (h *ShipReviewHandler) enforceTargetCI(w http.ResponseWriter, r *http.Request, card *shipreview.Card, task *context.Task, action, headSHA string, o targetCIOverride) bool {
	if o.MergeOnRedTarget && headSHA != card.HeadSHA {
		msg := "merge_on_red_target needs head_sha, the head the Board reviewed"
		if headSHA != "" {
			msg = "the card moved to " + card.HeadSHA + " since the Board chose Merge onto red target at " + headSHA + "; review the new head"
		}
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "override_head_mismatch", "message": msg, "head_sha": card.HeadSHA})
		return false
	}
	ci := h.readTargetCI(r, card, task, 0)
	if !ci.Blocking() {
		return true
	}
	failing := targetCIFailingNames(ci)
	if !o.MergeOnRedTarget {
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error":     "target_ci_red",
			"message":   ci.Branch + " is red (" + strings.Join(failing, ", ") + "). Fix " + ci.Branch + " first, or use Merge onto red " + ci.Branch + ".",
			"target_ci": ci,
		})
		return false
	}
	if err := governance.LogBoardEvent(h.db, "board", governance.AuditBoardAction, map[string]any{
		"action": "merge_on_red_target", "merge_action": action, "task_id": task.ID,
		"branch": card.Branch, "head_sha": card.HeadSHA, "pr_number": card.PRNumber, "pr_url": card.PRURL,
		"target_branch": ci.Branch, "target_head_sha": ci.HeadSHA, "failing": failing,
		"reason": o.MergeOnRedTargetReason, "ip": r.RemoteAddr, "user_agent": r.UserAgent(),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "audit log write failed; not merging: "+err.Error())
		return false
	}
	return true
}

// targetCIFailingNames names what is red: failed jobs, or the workflow when
// its jobs could not be read.
func targetCIFailingNames(ci shipreview.TargetCI) []string {
	var out []string
	for _, f := range ci.Failing {
		if len(f.FailedJobs) > 0 {
			out = append(out, f.FailedJobs...)
		} else {
			out = append(out, f.Workflow)
		}
	}
	return out
}
