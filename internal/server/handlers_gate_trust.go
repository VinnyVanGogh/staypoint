package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/gates"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
	"github.com/VinnyVanGogh/staypoint/internal/security"
)

// "Trust this task until…" (task-6c1ed91f): creating, revoking and showing a
// task trust, the tev1 overnight mode, and the deferral of deletes outside
// the worktree. The approval itself is in createOrAutoApprove.

// deferDue skips every under-trust request whose deferral deadline passed:
// the waiting hook sees "deferred" and tells the agent to move on. The
// request stays pending in the Deferred queue for the Board.
func (h *SecurityGateHandler) deferDue() {
	now := h.clock()
	tx, err := h.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	ids, err := security.DeferDue(tx, now)
	if err != nil || len(ids) == 0 {
		return
	}
	pending := string(security.GateRequestPending)
	for _, id := range ids {
		if err := governance.LogGateEventTx(tx, id, "system", "security_gate_deferred", &pending, &pending,
			map[string]any{"message": "not approved in time: skipped, not run; queued for the Board (deciding it later does not replay it)"}); err != nil {
			slog.Warn("gate defer: audit failed", slog.String("gate_id", id), slog.String("error", err.Error()))
			return
		}
	}
	if err := tx.Commit(); err != nil {
		return
	}
	for _, id := range ids {
		h.hub.Publish("security_gate_deferred", map[string]any{"id": id})
	}
}

// hookView is a request as the waiting hook sees it: a deferred request
// reports "deferred" for good, even after the Board decides it, so a hook
// still polling (an old binary) never runs a command it already skipped.
func hookView(gr *security.GateRequest) any {
	if gr.DeferredAt == nil {
		return gr
	}
	v := *gr
	v.Status = "deferred"
	return struct {
		*security.GateRequest
		BoardStatus security.GateRequestStatus `json:"board_status"`
	}{&v, gr.Status}
}

func (h *SecurityGateHandler) tev1Advisor() gates.RequestAdvisor {
	if h.tev1 != nil {
		return h.tev1
	}
	return h.advisor
}

// runTev1 lets the local tev1 model decide a trusted task's request. It
// approves only on "approve" with p >= the trust's threshold; anything else
// (deny, low confidence, error, timeout, no model) denies the request and
// parks the whole task until the Board looks.
func (h *SecurityGateHandler) runTev1(gr *security.GateRequest, scripts []security.ScriptRef, trust *gates.Rule) {
	timeout := h.advisoryTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	a := gates.Advice{Advisor: gates.AdvisorTogether}
	var err error
	start := time.Now()
	if adv := h.tev1Advisor(); adv == nil {
		err = fmt.Errorf("no tev1 advisor configured")
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		a, err = adv.Advise(ctx, gr, scripts)
		cancel()
		a.Advisor = adv.Name()
	}
	a.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		a.Recommendation, a.Error = "", err.Error()
	}
	// Fail closed (#243-2): an error, or a probability that is not a
	// probability, never approves, whatever the advisor recommended.
	validP := !math.IsNaN(a.Probability) && !math.IsInf(a.Probability, 0) && a.Probability >= 0 && a.Probability <= 1
	approve := err == nil && validP && a.Recommendation == gates.RecApprove && a.Probability >= trust.Tev1Threshold
	why := ""
	switch {
	case err != nil:
		why = "tev1 error: " + err.Error()
	case !validP:
		why = fmt.Sprintf("tev1 probability %v is outside [0,1]", a.Probability)
	case a.Recommendation != gates.RecApprove:
		why = fmt.Sprintf("tev1 said %q", a.Recommendation)
	case !approve:
		why = fmt.Sprintf("tev1 confidence %.2f below threshold %.2f", a.Probability, trust.Tev1Threshold)
	}
	if lerr := gates.LogAdvice(h.db, gr.ID, a); lerr != nil {
		slog.Warn("tev1: advice log failed", slog.String("gate_id", gr.ID), slog.String("error", lerr.Error()))
	}
	if err := h.decideTev1(gr, trust, approve, a, why); err != nil {
		slog.Warn("tev1: decision failed", slog.String("gate_id", gr.ID), slog.String("error", err.Error()))
	}
}

func (h *SecurityGateHandler) decideTev1(gr *security.GateRequest, trust *gates.Rule, approve bool, a gates.Advice, why string) error {
	now := h.clock()
	by := gates.DecidedByTev1(trust.ID)
	tx, err := h.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	decided, err := security.DecideGateRequestTx(tx, gr.ID, approve, by)
	if err != nil {
		return nil // the Board decided first
	}
	pending, status := string(security.GateRequestPending), string(decided.Status)
	event := "security_gate_tev1_approved"
	msg := fmt.Sprintf("tev1 approved (p=%.2f ≥ %.2f)", a.Probability, trust.Tev1Threshold)
	reason := "tev1 denied: " + truncateRunes(gr.Cmdline, 160)
	if !approve {
		event, msg = "security_gate_tev1_denied", "not run; task parked for the Board: "+why
	}
	if err := governance.LogGateEventTx(tx, gr.ID, by, event, &pending, &status, map[string]any{
		"message": msg, "rule_id": trust.ID, "task_id": gr.TaskID, "cmdline": gr.Cmdline,
		"probability": a.Probability, "threshold": trust.Tev1Threshold, "advice_reason": a.Reason, "error": a.Error,
	}); err != nil {
		return err
	}
	if err := gates.RecordFinalDecision(tx, gr.ID, status, by, now); err != nil {
		return err
	}
	if approve {
		if err := gates.RecordHit(tx, trust.ID, now); err != nil {
			return err
		}
	} else if gr.TaskID != "" {
		if _, err := tx.Exec(`UPDATE tasks SET is_blocked = 1, block_reason = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`,
			reason, gr.TaskID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if !approve && gr.TaskID != "" {
		// Hold the run at its next tool call; other tasks keep running.
		if err := orchestrator.GlobalRunControl.SetPause(gr.TaskID, true); err != nil {
			slog.Warn("tev1: pause failed", slog.String("task_id", gr.TaskID), slog.String("error", err.Error()))
		}
		h.hub.Publish("task_parked", map[string]any{"task_id": gr.TaskID, "reason": reason})
	}
	h.hub.Publish("security_gate_decided", map[string]any{"id": gr.ID, "decision": decided.Status, "rule_id": trust.ID, "tev1": true})
	return nil
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// trustView is one trust with what it decided, for the task page and the
// Gates page's morning summary.
type trustView struct {
	*gates.Rule
	Active        bool                    `json:"active"`
	TaskName      string                  `json:"task_name,omitempty"`
	AutoApproved  []*security.GateRequest `json:"auto_approved"`
	Tev1Approved  []*security.GateRequest `json:"tev1_approved"`
	Tev1Denied    []*security.GateRequest `json:"tev1_denied"`
	AutoCount     int                     `json:"auto_approved_count"`
	HeldCount     int                     `json:"held_count"`
	DeferredCount int                     `json:"deferred_count"`
}

func (h *SecurityGateHandler) buildTrustView(r *gates.Rule, now time.Time) (*trustView, error) {
	reqs, err := gates.TrustRequests(h.db, []int64{r.ID})
	if err != nil {
		return nil, err
	}
	v := &trustView{Rule: r, Active: r.Active(now), AutoApproved: []*security.GateRequest{},
		Tev1Approved: []*security.GateRequest{}, Tev1Denied: []*security.GateRequest{}}
	for _, gr := range reqs {
		switch {
		case gr.DecidedBy == gates.DecidedByTrust(r.ID):
			v.AutoApproved = append(v.AutoApproved, gr)
		case gr.Status == security.GateRequestApproved:
			v.Tev1Approved = append(v.Tev1Approved, gr)
		default:
			v.Tev1Denied = append(v.Tev1Denied, gr)
		}
	}
	v.AutoCount = len(v.AutoApproved) + len(v.Tev1Approved)
	held, err := gates.TaskHeldRequests(h.db, r.ScopeValue)
	if err != nil {
		return nil, err
	}
	for _, gr := range held {
		if gr.DeferredAt != nil {
			v.DeferredCount++
		} else {
			v.HeldCount++
		}
	}
	_ = h.db.QueryRow(`SELECT COALESCE(name,'') FROM tasks WHERE id = ?`, r.ScopeValue).Scan(&v.TaskName)
	return v, nil
}

// GetTaskTrust handles GET /api/tasks/{id}/trust: the task's trust (active or
// the latest), what it approved newest first, and what is still held.
func (h *SecurityGateHandler) GetTaskTrust(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	now := h.clock()
	h.deferDue()
	if _, err := gates.ActiveTrust(h.db, taskID, now); err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	trusts, err := gates.TrustsForTask(h.db, taskID)
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	resp := map[string]any{
		"task_id": taskID, "active": nil, "latest": nil,
		"presets": gates.TrustPresets, "min_minutes": gates.MinTrustMinutes, "max_minutes": gates.MaxTrustMinutes,
		"defer_minutes": gates.TrustDeferMinutes(h.db), "tev1_threshold": gates.Tev1Threshold(h.db),
		"tev1_warning": gates.Tev1Warning, "tev1_configured": h.tev1Advisor() != nil,
	}
	if len(trusts) > 0 {
		v, err := h.buildTrustView(trusts[0], now)
		if err != nil {
			http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
			return
		}
		resp["latest"] = v
		if v.Active {
			resp["active"] = v
		}
	}
	held, err := gates.TaskHeldRequests(h.db, taskID)
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	resp["held"] = h.withAdvice(held)
	writeJSON(w, resp)
}

// CreateTaskTrust handles POST /api/tasks/{id}/trust (Board + Touch ID, no
// grace): {"preset":"1h"|"4h"|"overnight"|"custom","minutes":N,"tev1":bool,
// "tev1_ack":bool}. Unknown fields are refused, so a client cannot send an
// expiry, a scope, a pattern or exclusions; the server derives them.
func (h *SecurityGateHandler) CreateTaskTrust(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	var spec gates.TrustSpec
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, "invalid body: "+err.Error()), http.StatusBadRequest)
		return
	}
	now := h.clock()
	if err := gates.CanTrust(h.db, taskID); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusConflict)
		return
	}
	draft, err := gates.NewTrust(taskID, spec, gates.Tev1Threshold(h.db), now)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusUnprocessableEntity)
		return
	}
	tx, err := h.db.Begin()
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	rule, err := gates.InsertTrust(tx, draft, now)
	if err == nil {
		err = governance.LogBoardEventTx(tx, "board", governance.AuditBoardAction, map[string]string{
			"action": "create_task_trust", "rule_id": fmt.Sprint(rule.ID), "task_id": taskID,
			"expires_at": rule.ExpiresAt.UTC().Format(time.RFC3339), "tev1": fmt.Sprint(rule.Tev1),
			"tev1_threshold": fmt.Sprint(rule.Tev1Threshold), "ip": r.RemoteAddr, "user_agent": r.UserAgent()})
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, "trust write failed: "+err.Error()), http.StatusInternalServerError)
		return
	}
	h.hub.Publish("security_gate_rules", map[string]any{"id": rule.ID, "action": "created", "task_id": taskID, "trust": true})
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, rule)
}

// RevokeTaskTrust handles POST /api/tasks/{id}/trust/revoke (Board session).
// Revoking only removes power, so it does not wait for Touch ID.
func (h *SecurityGateHandler) RevokeTaskTrust(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	now := h.clock()
	trusts, err := gates.TrustsForTask(h.db, taskID)
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	tx, err := h.db.Begin()
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	var ended []int64
	for _, t := range trusts {
		if t.DeletedAt != nil {
			continue
		}
		ok, err := gates.EndTrust(tx, t.ID, "revoked by the Board", now)
		if err != nil {
			http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
			return
		}
		if ok {
			ended = append(ended, t.ID)
		}
	}
	if len(ended) == 0 {
		http.Error(w, `{"error":"no trust to revoke"}`, http.StatusNotFound)
		return
	}
	if err := governance.LogBoardEventTx(tx, "board", governance.AuditBoardAction, map[string]string{
		"action": "revoke_task_trust", "task_id": taskID, "rule_ids": fmt.Sprint(ended), "ip": r.RemoteAddr}); err != nil {
		http.Error(w, `{"error":"board audit write failed"}`, http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	h.hub.Publish("security_gate_rules", map[string]any{"action": "revoked", "task_id": taskID, "trust": true})
	writeJSON(w, map[string]any{"task_id": taskID, "revoked": ended})
}

// ListTrusts handles GET /api/security/trusts: trusts live now or ended in
// the last 24h, each with what it approved and what tev1 decided (the
// morning summary).
func (h *SecurityGateHandler) ListTrusts(w http.ResponseWriter, r *http.Request) {
	now := h.clock()
	_ = gates.SweepTrusts(h.db, now)
	rules, err := gates.RecentTrusts(h.db, now.Add(-24*time.Hour))
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	out := []*trustView{}
	for _, ru := range rules {
		v, err := h.buildTrustView(ru, now)
		if err != nil {
			http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
			return
		}
		out = append(out, v)
	}
	writeJSON(w, map[string]any{"trusts": out, "tev1_warning": gates.Tev1Warning})
}
