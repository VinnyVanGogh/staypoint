package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/gates"
	"github.com/VinnyVanGogh/staypoint/internal/geminiapproval"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/VinnyVanGogh/staypoint/internal/security"
)

// SecurityGateHandler handles Board-approval gate requests for Red-tier commands.
type SecurityGateHandler struct {
	db  *sql.DB
	hub *EventHub

	// STA-868: per-request advisor, batch reviewer and context resolver.
	advisor  gates.RequestAdvisor
	reviewer gates.Reviewer
	resolver *gates.Resolver
	// advisoryTimeout bounds one advisory call (default 20s).
	advisoryTimeout time.Duration
}

func NewSecurityGateHandler(db *sql.DB, hub *EventHub) *SecurityGateHandler {
	return &SecurityGateHandler{db: db, hub: hub}
}

func (h *SecurityGateHandler) res() *gates.Resolver {
	if h.resolver != nil {
		if h.resolver.DB == nil {
			h.resolver.DB = h.db
		}
		return h.resolver
	}
	return &gates.Resolver{DB: h.db}
}

// gateRequestView is a gate request plus its latest advisory per advisor.
type gateRequestView struct {
	*security.GateRequest
	Advice map[string]gates.Advice `json:"advice,omitempty"`
}

func (h *SecurityGateHandler) withAdvice(reqs []*security.GateRequest) []gateRequestView {
	ids := make([]string, 0, len(reqs))
	for _, r := range reqs {
		ids = append(ids, r.ID)
	}
	adv, _ := gates.LatestAdvice(h.db, ids)
	out := make([]gateRequestView, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, gateRequestView{GateRequest: r, Advice: adv[r.ID]})
	}
	return out
}

// ListGateRequests handles GET /api/security/gate-requests[?status=pending]
func (h *SecurityGateHandler) ListGateRequests(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	var (
		requests []*security.GateRequest
		err      error
	)
	if status == "" || status == "pending" {
		requests, err = security.ListPendingGateRequests(h.db)
	} else {
		requests, err = security.ListGateRequests(h.db, status)
	}
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"gate_requests":   h.withAdvice(requests),
		"advisor_enabled": h.advisor != nil && gates.AdvisorEnabled(h.db),
	})
}

// CreateGateRequest handles POST /api/security/gate-requests
func (h *SecurityGateHandler) CreateGateRequest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cmdline string   `json:"cmdline"`
		Reasons []string `json:"reasons"`
		RunID   string   `json:"run_id"`
		TaskID  string   `json:"task_id"`
		CWD     string   `json:"cwd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Cmdline == "" {
		http.Error(w, `{"error":"cmdline required"}`, http.StatusBadRequest)
		return
	}
	if req.RunID == geminiapproval.RunID {
		// Board Touch ID for Gemini code: personal repos only. A work-repo
		// request is refused outright, so it can never be approved.
		scope, ok := geminiapproval.Parse(req.Cmdline)
		if !ok {
			http.Error(w, `{"error":"invalid gemini-code request"}`, http.StatusBadRequest)
			return
		}
		if err := geminiapproval.Validate(scope, isWorkRepoForGate); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusForbidden)
			return
		}
		gr, err := geminiapproval.Request(h.db, scope, isWorkRepoForGate)
		if err != nil {
			http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
			return
		}
		h.hub.Publish("security_gate_request", map[string]any{
			"id": gr.ID, "cmdline": gr.Cmdline, "reasons": gr.Reasons, "status": gr.Status,
		})
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, gr)
		return
	}
	in := security.GateRequestInput{Cmdline: req.Cmdline, Reasons: req.Reasons, RunID: req.RunID, TaskID: req.TaskID, CWD: req.CWD}
	var scripts []security.ScriptRef
	if !gates.SpecialRunIDs[req.RunID] {
		scripts = h.res().Resolve(&in)
	}

	gr, rule, err := h.createOrAutoApprove(in)
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	if rule != nil {
		h.hub.Publish("security_gate_decided", map[string]any{"id": gr.ID, "decision": gr.Status, "rule_id": rule.ID})
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, gr)
		return
	}
	h.hub.Publish("security_gate_request", map[string]any{
		"id":      gr.ID,
		"cmdline": gr.Cmdline,
		"reasons": gr.Reasons,
		"status":  gr.Status,
	})
	h.startAdvisory(gr, scripts)
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, gr)
}

// createOrAutoApprove stores the request: approved by the first matching
// Board allow rule (with its audit row and hit count, in one transaction),
// or pending.
func (h *SecurityGateHandler) createOrAutoApprove(in security.GateRequestInput) (*security.GateRequest, *gates.Rule, error) {
	now := time.Now()
	tx, err := h.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	rule, err := gates.MatchRule(tx, in, now)
	if err != nil {
		return nil, nil, err
	}
	if rule == nil {
		gr, err := security.InsertGateRequest(tx, in, security.GateRequestPending, "")
		if err != nil {
			return nil, nil, err
		}
		return gr, nil, tx.Commit()
	}
	by := fmt.Sprintf("rule:%d", rule.ID)
	gr, err := security.InsertGateRequest(tx, in, security.GateRequestApproved, by)
	if err != nil {
		return nil, nil, err
	}
	pending, approved := string(security.GateRequestPending), string(security.GateRequestApproved)
	if err := governance.LogGateEventTx(tx, gr.ID, by, "security_gate_auto_approved", &pending, &approved,
		map[string]any{"message": fmt.Sprintf("auto-approved by rule #%d", rule.ID), "rule_id": rule.ID,
			"cmdline": gr.Cmdline, "run_id": gr.RunID, "task_id": gr.TaskID, "scope": rule.Scope, "scope_value": rule.ScopeValue}); err != nil {
		return nil, nil, err
	}
	if err := gates.RecordHit(tx, rule.ID, now); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return gr, rule, nil
}

// startAdvisory asks the Together advisor about a new pending request in the
// background. It never blocks the request and never changes its state.
func (h *SecurityGateHandler) startAdvisory(gr *security.GateRequest, scripts []security.ScriptRef) {
	if h.advisor == nil || gates.SpecialRunIDs[gr.RunID] || !gates.AdvisorEnabled(h.db) {
		return
	}
	timeout := h.advisoryTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	snapshot := *gr
	go gates.RunAdvisory(h.db, h.advisor, &snapshot, scripts, timeout, func(a gates.Advice) {
		h.hub.Publish("security_gate_advice", map[string]any{"id": snapshot.ID, "advice": a})
	})
}

// GetGateRequest handles GET /api/security/gate-requests/{id}
// Supports long-poll: if ?wait=true and status is still pending, blocks up to 30s.
func (h *SecurityGateHandler) GetGateRequest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	wait := r.URL.Query().Get("wait") == "true"

	gr, err := security.GetGateRequest(h.db, id)
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	if gr == nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if !wait || gr.Status != security.GateRequestPending {
		writeJSON(w, gr)
		return
	}

	// Long-poll: check every 500ms for up to 29s (client should retry on 200+pending).
	ctx, cancel := context.WithTimeout(r.Context(), 29*time.Second)
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Return current state; hook will retry.
			writeJSON(w, gr)
			return
		case <-ticker.C:
			fresh, err := security.GetGateRequest(h.db, id)
			if err != nil || fresh == nil {
				writeJSON(w, gr)
				return
			}
			gr = fresh
			if gr.Status != security.GateRequestPending {
				writeJSON(w, gr)
				return
			}
		}
	}
}

// decideError is a decision failure with the HTTP status it maps to.
type decideError struct {
	status int
	msg    string
}

func (e *decideError) Error() string { return e.msg }

// decideOne decides one request: the status change, the gate audit row, the
// Board audit row, the decision-log stamp and an optional allow rule commit
// in one transaction, so an audit failure leaves the request pending and the
// caller gets an honest error (STA-868).
func (h *SecurityGateHandler) decideOne(r *http.Request, id, decisionStr string, remember *gates.RuleSpec) (*security.GateRequest, *gates.Rule, *geminiapproval.Scope, error) {
	approved := decisionStr == "approved"
	if decisionStr != "approved" && decisionStr != "denied" {
		return nil, nil, nil, &decideError{http.StatusBadRequest, "decision must be approved or denied"}
	}
	cur, err := security.GetGateRequest(h.db, id)
	if err != nil {
		return nil, nil, nil, &decideError{http.StatusInternalServerError, "db error"}
	}
	if cur == nil {
		return nil, nil, nil, &decideError{http.StatusConflict, fmt.Sprintf("gate request %s not found or already decided", id)}
	}
	var geminiScope *geminiapproval.Scope
	if cur.RunID == geminiapproval.RunID {
		scope, ok := geminiapproval.Parse(cur.Cmdline)
		if approved {
			// Hard no in work repos, even for the Board.
			if !ok {
				return nil, nil, nil, &decideError{http.StatusForbidden, "invalid gemini-code request"}
			}
			if err := geminiapproval.Validate(scope, isWorkRepoForGate); err != nil {
				return nil, nil, nil, &decideError{http.StatusForbidden, err.Error()}
			}
		}
		if ok {
			geminiScope = &scope
		}
	}
	var newRule *gates.Rule
	var ruleDraft *gates.Rule
	if remember != nil {
		if !approved {
			return nil, nil, nil, &decideError{http.StatusBadRequest, "only an approval can be remembered"}
		}
		draft, err := gates.RuleFromRequest(cur, *remember, h.res().CurrentScripts(cur), time.Now())
		if err != nil {
			return nil, nil, nil, &decideError{http.StatusUnprocessableEntity, err.Error()}
		}
		ruleDraft = &draft
	}

	tx, err := h.db.Begin()
	if err != nil {
		return nil, nil, nil, &decideError{http.StatusInternalServerError, "db error"}
	}
	defer tx.Rollback()
	gr, err := security.DecideGateRequestTx(tx, id, approved, "board")
	if err != nil {
		if strings.Contains(err.Error(), "not found or already decided") {
			return nil, nil, nil, &decideError{http.StatusConflict, err.Error()}
		}
		return nil, nil, nil, &decideError{http.StatusInternalServerError, "db error"}
	}
	pendingStatus := string(security.GateRequestPending)
	decidedStatus := string(gr.Status)
	payload := map[string]any{"cmdline": gr.Cmdline, "decision": string(gr.Status), "run_id": gr.RunID}
	if ruleDraft != nil {
		if newRule, err = gates.InsertRule(tx, *ruleDraft); err != nil {
			return nil, nil, nil, &decideError{http.StatusInternalServerError, "rule write failed"}
		}
		payload["rule_id"] = newRule.ID
	}
	if err := governance.LogGateEventTx(tx, gr.ID, "board", "security_gate_decided", &pendingStatus, &decidedStatus, payload); err != nil {
		return nil, nil, nil, &decideError{http.StatusInternalServerError, "audit log write failed: " + err.Error()}
	}
	boardPayload := map[string]string{"action": "decide_gate_request", "gate_id": id, "decision": decisionStr,
		"ip": r.RemoteAddr, "user_agent": r.UserAgent()}
	if newRule != nil {
		boardPayload["rule_id"] = fmt.Sprint(newRule.ID)
		boardPayload["rule_scope"] = newRule.Scope + ":" + newRule.ScopeValue
	}
	if err := governance.LogBoardEventTx(tx, "board", governance.AuditBoardAction, boardPayload); err != nil {
		return nil, nil, nil, &decideError{http.StatusInternalServerError, "board audit write failed: " + err.Error()}
	}
	if err := gates.RecordFinalDecision(tx, gr.ID, decidedStatus, "board", time.Now()); err != nil {
		return nil, nil, nil, &decideError{http.StatusInternalServerError, "decision log write failed: " + err.Error()}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, nil, &decideError{http.StatusInternalServerError, "commit failed: " + err.Error()}
	}
	return gr, newRule, geminiScope, nil
}

func (h *SecurityGateHandler) afterDecide(gr *security.GateRequest, rule *gates.Rule, geminiScope *geminiapproval.Scope) {
	h.hub.Publish("security_gate_decided", map[string]any{
		"id":       gr.ID,
		"decision": gr.Status,
	})
	if rule != nil {
		h.hub.Publish("security_gate_rules", map[string]any{"id": rule.ID, "action": "created"})
	}
	if geminiScope != nil && geminiScope.TaskID != "" {
		// Wake the held task: an approval starts its run (consumed there),
		// a denial makes the daemon refuse it with a clear reason.
		orchestrator.GlobalDispatcher.Wake(geminiScope.TaskID, "gemini_code_"+string(gr.Status), "gemini_code:"+gr.ID)
	}
}

// DecideGateRequest handles POST /api/security/gate-requests/{id}/decide.
// Body: {"decision":"approved"|"denied", "remember":{scope, match_kind, pattern, expires_in_minutes}}.
func (h *SecurityGateHandler) DecideGateRequest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Decision string          `json:"decision"` // "approved" or "denied"
		Remember *gates.RuleSpec `json:"remember"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	gr, rule, scope, err := h.decideOne(r, id, req.Decision, req.Remember)
	if err != nil {
		de, _ := err.(*decideError)
		if de == nil {
			de = &decideError{http.StatusInternalServerError, err.Error()}
		}
		http.Error(w, fmt.Sprintf(`{"error":%q}`, de.msg), de.status)
		return
	}
	h.afterDecide(gr, rule, scope)
	if rule != nil {
		writeJSON(w, map[string]any{"gate_request": gr, "rule": rule, "id": gr.ID, "status": gr.Status})
		return
	}
	writeJSON(w, gr)
}

// DecideBatch handles POST /api/security/gate-requests/decide-batch:
// {"ids":[...], "decision":"approved"|"denied"}. One Board authorization
// covers the batch; each request is decided in its own transaction with its
// own audit rows, and failures are reported per id.
func (h *SecurityGateHandler) DecideBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs      []string `json:"ids"`
		Decision string   `json:"decision"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.IDs) == 0 {
		http.Error(w, `{"error":"ids and decision required"}`, http.StatusBadRequest)
		return
	}
	if req.Decision != "approved" && req.Decision != "denied" {
		http.Error(w, `{"error":"decision must be approved or denied"}`, http.StatusBadRequest)
		return
	}
	if len(req.IDs) > 200 {
		http.Error(w, `{"error":"at most 200 requests per batch"}`, http.StatusBadRequest)
		return
	}
	type result struct {
		ID     string `json:"id"`
		OK     bool   `json:"ok"`
		Status string `json:"status,omitempty"`
		Error  string `json:"error,omitempty"`
	}
	results := make([]result, 0, len(req.IDs))
	okCount := 0
	seen := map[string]bool{}
	for _, id := range req.IDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		gr, rule, scope, err := h.decideOne(r, id, req.Decision, nil)
		if err != nil {
			results = append(results, result{ID: id, Error: err.Error()})
			continue
		}
		h.afterDecide(gr, rule, scope)
		okCount++
		results = append(results, result{ID: id, OK: true, Status: string(gr.Status)})
	}
	writeJSON(w, map[string]any{"results": results, "decided": okCount, "failed": len(results) - okCount})
}

// ListRules handles GET /api/security/gate-rules[?include_deleted=1].
func (h *SecurityGateHandler) ListRules(w http.ResponseWriter, r *http.Request) {
	rules, err := gates.ListRules(h.db, r.URL.Query().Get("include_deleted") == "1")
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"rules": rules})
}

// CreateRule handles POST /api/security/gate-rules (Board):
// {"gate_id":..., "scope":..., "match_kind":..., "pattern":..., "expires_in_minutes":...}
// remembers an existing request as an allow rule.
func (h *SecurityGateHandler) CreateRule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GateID string `json:"gate_id"`
		gates.RuleSpec
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.GateID == "" {
		http.Error(w, `{"error":"gate_id required"}`, http.StatusBadRequest)
		return
	}
	gr, err := security.GetGateRequest(h.db, req.GateID)
	if err != nil || gr == nil {
		http.Error(w, `{"error":"gate request not found"}`, http.StatusNotFound)
		return
	}
	draft, err := gates.RuleFromRequest(gr, req.RuleSpec, h.res().CurrentScripts(gr), time.Now())
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
	rule, err := gates.InsertRule(tx, draft)
	if err == nil {
		err = governance.LogBoardEventTx(tx, "board", governance.AuditBoardAction, map[string]string{
			"action": "create_gate_rule", "rule_id": fmt.Sprint(rule.ID), "gate_id": gr.ID,
			"scope": rule.Scope + ":" + rule.ScopeValue, "pattern": rule.Pattern, "ip": r.RemoteAddr})
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, "rule write failed: "+err.Error()), http.StatusInternalServerError)
		return
	}
	h.hub.Publish("security_gate_rules", map[string]any{"id": rule.ID, "action": "created"})
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, rule)
}

// DeleteRule handles DELETE /api/security/gate-rules/{id} (Board).
func (h *SecurityGateHandler) DeleteRule(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, `{"error":"bad rule id"}`, http.StatusBadRequest)
		return
	}
	tx, err := h.db.Begin()
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if err := gates.DeleteRule(tx, id, time.Now()); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, `{"error":"rule not found"}`, http.StatusNotFound)
			return
		}
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	if err := governance.LogBoardEventTx(tx, "board", governance.AuditBoardAction, map[string]string{
		"action": "delete_gate_rule", "rule_id": fmt.Sprint(id), "ip": r.RemoteAddr}); err != nil {
		http.Error(w, `{"error":"board audit write failed"}`, http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	h.hub.Publish("security_gate_rules", map[string]any{"id": id, "action": "deleted"})
	writeJSON(w, map[string]any{"id": id, "deleted": true})
}

// Stats handles GET /api/security/gate-stats: advisor agreement with the
// Board and rule totals.
func (h *SecurityGateHandler) Stats(w http.ResponseWriter, r *http.Request) {
	adv, err := gates.Stats(h.db)
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	rules, _ := gates.ListRules(h.db, false)
	hits := 0
	for _, ru := range rules {
		hits += ru.HitCount
	}
	writeJSON(w, map[string]any{"advisors": adv, "active_rules": len(rules), "rule_hits": hits,
		"advisor_enabled": h.advisor != nil && gates.AdvisorEnabled(h.db)})
}

// ReviewPending handles POST /api/security/gate-requests/review: a Gemini
// review of the pending requests ({"ids":[...]} narrows it). Advisory only.
func (h *SecurityGateHandler) ReviewPending(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	pending, err := security.ListPendingGateRequests(h.db)
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	want := map[string]bool{}
	for _, id := range req.IDs {
		want[id] = true
	}
	var items []gates.ReviewItem
	for _, gr := range pending {
		if len(want) > 0 && !want[gr.ID] {
			continue
		}
		var scripts []security.ScriptRef
		if !gates.SpecialRunIDs[gr.RunID] {
			scripts = h.res().CurrentScripts(gr)
		}
		items = append(items, gates.ReviewItem{Request: gr, Scripts: scripts})
		if len(items) == 50 {
			break
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	res, err := gates.RunReview(ctx, h.db, h.reviewer, items)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, res)
}

// isWorkRepoForGate classifies a repo for Gemini-code approvals; an error
// counts as work (fail closed).
func isWorkRepoForGate(p string) bool {
	ok, _, err := router.IsWorkRepo(p)
	return err != nil || ok
}

// ListGateAuditLog handles GET /api/security/gate-requests/{id}/audit-log
func (h *SecurityGateHandler) ListGateAuditLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	entries, err := governance.ListGateAuditLog(h.db, id)
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []map[string]any{}
	}
	writeJSON(w, map[string]any{"audit_log": entries})
}

func (h *SecurityGateHandler) gateSettings() map[string]any {
	val, err := getSettingKV(h.db, "gates.main_merge_approval")
	enabled := true // default on
	if err == nil && val == "false" {
		enabled = false
	}
	return map[string]any{
		"main_merge_approval":   enabled,
		"advisor_enabled":       gates.AdvisorEnabled(h.db),
		"advisor_configured":    h.advisor != nil,
		"passkey_grace_minutes": gates.GraceMinutes(h.db),
	}
}

// GetSecurityGateSettings handles GET /api/settings/security-gate
func (h *SecurityGateHandler) GetSecurityGateSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, h.gateSettings())
}

// UpdateSecurityGateSettings handles POST /api/settings/security-gate. Each
// field is optional; only the ones sent change.
func (h *SecurityGateHandler) UpdateSecurityGateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MainMergeApproval   *bool `json:"main_merge_approval"`
		AdvisorEnabled      *bool `json:"advisor_enabled"`
		PasskeyGraceMinutes *int  `json:"passkey_grace_minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	if req.PasskeyGraceMinutes != nil && (*req.PasskeyGraceMinutes < 0 || *req.PasskeyGraceMinutes > gates.MaxGraceMinutes) {
		http.Error(w, `{"error":"passkey_grace_minutes must be 0 (off) to 5"}`, http.StatusBadRequest)
		return
	}
	tx, err := h.db.Begin()
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	changes := map[string]string{"action": "update_security_gate_settings", "ip": r.RemoteAddr, "user_agent": r.UserAgent()}
	set := func(key, val string) error {
		changes[key] = val
		_, err := tx.Exec(
			`INSERT INTO settings_kv (key, value, updated_at) VALUES (?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, val)
		return err
	}
	boolStr := func(b bool) string {
		if b {
			return "true"
		}
		return "false"
	}
	if req.MainMergeApproval != nil {
		err = set("gates.main_merge_approval", boolStr(*req.MainMergeApproval))
		action := "enabled"
		if !*req.MainMergeApproval {
			action = "disabled"
		}
		changes["value"] = action
	}
	if err == nil && req.AdvisorEnabled != nil {
		err = set(gates.SettingAdvisorEnabled, boolStr(*req.AdvisorEnabled))
	}
	if err == nil && req.PasskeyGraceMinutes != nil {
		err = set(gates.SettingPasskeyGraceMinutes, strconv.Itoa(*req.PasskeyGraceMinutes))
	}
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	if err := governance.LogBoardEventTx(tx, "board", governance.AuditBoardAction, changes); err != nil {
		http.Error(w, `{"error":"board audit write failed"}`, http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	out := h.gateSettings()
	h.hub.Publish("security_gate_settings", out)
	writeJSON(w, out)
}

// helpers

func getSettingKV(db *sql.DB, key string) (string, error) {
	var val string
	err := db.QueryRow(`SELECT value FROM settings_kv WHERE key = ?`, key).Scan(&val)
	return val, err
}

func setSettingKV(db *sql.DB, key, value string) error {
	_, err := db.Exec(
		`INSERT INTO settings_kv (key, value, updated_at) VALUES (?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value,
	)
	return err
}
