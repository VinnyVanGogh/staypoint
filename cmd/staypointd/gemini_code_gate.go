package main

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/geminiapproval"
	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/VinnyVanGogh/staypoint/internal/workorgs"
)

// Board addition (2026-10-06): provider=gemini on a code kind in a PERSONAL
// repo runs only after a Board Touch ID approval of a gate request
// "Gemini code: <task> in <repo>". One approval covers one run. Work repos
// never get here: router.NeedsGeminiCodeApproval is false there and the
// route keeps Gemini out (GeminiCodeForbidden), approval rows or not.

// geminiCodeGateResult is the gate's verdict for one wake.
type geminiCodeGateResult struct {
	// Hold means the run must not start (approval pending, denied, or the
	// approval state could not be read: fail closed).
	Hold bool
	// ApprovalID is the gate request consumed by this run when approved.
	ApprovalID string
}

// geminiCodeGate checks (and, on approval, consumes) the Board Touch ID
// approval for one run of taskID. publish, when set, announces a newly filed
// request to the Board UI. Swappable in tests.
var geminiCodeGate = func(conn *sql.DB, taskID, repoRoot, runID string, publish func(string, any)) geminiCodeGateResult {
	var repoPath, workKind, provider, model, blockReason, org string
	if err := conn.QueryRowContext(context.Background(),
		`SELECT COALESCE(repo_path,''), COALESCE(work_kind,''), COALESCE(provider,''), COALESCE(model_override,''), COALESCE(block_reason,''), COALESCE(organization,'')
		 FROM tasks WHERE id=?`, taskID,
	).Scan(&repoPath, &workKind, &provider, &model, &blockReason, &org); err != nil {
		return geminiCodeGateResult{} // no task row: the run path reports it
	}
	if !taskDirIsGit(repoPath, taskID) {
		// Non-git dir (STA-864): an approval cannot apply (nothing to revert
		// to), so none is asked for; the route keeps Gemini off code work.
		return geminiCodeGateResult{}
	}
	if repoPath == "" {
		repoPath = repoRoot
	}
	isWork := workorgs.IsWork(org) || isWorkRepoPath(repoPath)
	if !router.NeedsGeminiCodeApproval(workKind, isWork, router.ChoiceFromStored(provider, model)) {
		return geminiCodeGateResult{}
	}
	if strings.HasPrefix(blockReason, geminiapproval.DeniedReason) {
		// Refused until the Board changes the task's provider (which clears
		// this hold) and so asks again.
		return geminiCodeGateResult{Hold: true}
	}

	grant, err := geminiapproval.LatestTaskGrant(conn, taskID, repoPath, isWorkRepoPath)
	if err != nil {
		slog.Warn("gemini-code gate: approval state unreadable; holding the run",
			slog.String("task", taskID), slog.Any("error", err))
		holdTask(conn, taskID, geminiapproval.WaitingReason+" (approval state unreadable: "+err.Error()+")")
		return geminiCodeGateResult{Hold: true}
	}
	switch grant.State {
	case geminiapproval.StateApproved:
		if err := geminiapproval.Consume(conn, grant.GateID, taskID, runID); err != nil {
			slog.Warn("gemini-code gate: approval not consumable; holding the run",
				slog.String("task", taskID), slog.Any("error", err))
			return geminiCodeGateResult{Hold: true}
		}
		releaseHold(conn, taskID)
		_ = meshContext.LogActivity(conn, taskID, "gemini_code_approval_used",
			"Board Touch ID approval "+grant.GateID+" used by run "+runID+" (one run)")
		return geminiCodeGateResult{ApprovalID: grant.GateID}
	case geminiapproval.StatePending:
		holdTask(conn, taskID, geminiapproval.WaitingReason+" (gate request "+grant.GateID+")")
		return geminiCodeGateResult{Hold: true}
	case geminiapproval.StateDenied:
		_ = geminiapproval.Consume(conn, grant.GateID, taskID, "refused:"+runID)
		holdTask(conn, taskID, geminiapproval.DeniedReason+" (gate request "+grant.GateID+"): run refused. Set the provider again to ask for a new approval, or choose claude.")
		_ = meshContext.LogActivity(conn, taskID, "gemini_code_denied", "Board denied Gemini code (gate request "+grant.GateID+"); run refused")
		return geminiCodeGateResult{Hold: true}
	}

	gr, err := geminiapproval.Request(conn, geminiapproval.Scope{TaskID: taskID, Repo: repoPath}, isWorkRepoPath)
	if err != nil {
		slog.Warn("gemini-code gate: could not file the approval request; holding the run",
			slog.String("task", taskID), slog.Any("error", err))
		holdTask(conn, taskID, geminiapproval.WaitingReason+" (request failed: "+err.Error()+")")
		return geminiCodeGateResult{Hold: true}
	}
	holdTask(conn, taskID, geminiapproval.WaitingReason+" (gate request "+gr.ID+")")
	_ = meshContext.LogActivity(conn, taskID, "gemini_code_approval_requested", "Filed gate request "+gr.ID+": "+geminiapproval.Scope{TaskID: taskID, Repo: repoPath}.Title())
	if publish != nil {
		publish("security_gate_request", map[string]any{
			"id": gr.ID, "cmdline": gr.Cmdline, "reasons": gr.Reasons, "status": gr.Status,
		})
	}
	return geminiCodeGateResult{Hold: true}
}

// isWorkRepoPath is router.IsWorkRepo as a predicate; an error counts as work
// (fail closed).
func isWorkRepoPath(p string) bool {
	ok, _, err := router.IsWorkRepo(p)
	return err != nil || ok
}

// holdTask marks the task as waiting on the Board with a clear reason.
func holdTask(conn *sql.DB, taskID, reason string) {
	_, _ = conn.Exec(`UPDATE tasks SET is_blocked = 1, block_reason = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, reason, taskID)
}

// releaseHold clears a Gemini-code hold (and only that).
func releaseHold(conn *sql.DB, taskID string) {
	_, _ = conn.Exec(`UPDATE tasks SET is_blocked = 0, block_reason = '' WHERE id = ? AND block_reason LIKE ?`,
		taskID, geminiapproval.WaitingReason+"%")
}
