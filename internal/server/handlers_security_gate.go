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

	// tev1 decides requests of tasks trusted in tev1 mode (default: advisor).
	tev1 gates.RequestAdvisor
	// now is the clock (tests).
	now func() time.Time
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
	h.deferDue()
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
		// Scripts are the hook's snapshots of the scripts the command runs;
		// Pinned says the hook will run exactly those bytes (STA-868).
		Scripts []gates.HookScript `json:"scripts"`
		Pinned  bool               `json:"pinned"`
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
		if req.Scripts != nil {
			in.Scripts = gates.ScriptsFromHook(req.Scripts, req.Pinned)
		}
		scripts = h.res().Resolve(&in)
	}

	out, err := h.createOrAutoApprove(in)
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	gr := out.gr
	if out.rule != nil {
		h.hub.Publish("security_gate_decided", map[string]any{"id": gr.ID, "decision": gr.Status, "rule_id": out.rule.ID})
		if out.rule.IsTrust() {
			// Trust approvals still get an advisor rating for the Board's review.
			h.startAdvisory(gr, scripts)
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, gr)
		return
	}
	h.hub.Publish("security_gate_request", map[string]any{
		"id":      gr.ID,
		"cmdline": gr.Cmdline,
		"reasons": gr.Reasons,
		"status":  gr.Status,
		"held":    out.held,
	})
	if out.tev1 != nil {
		go h.runTev1(gr, scripts, out.tev1)
	} else {
		h.startAdvisory(gr, scripts)
	}
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, gr)
}

func (h *SecurityGateHandler) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// gateCreateOutcome is how a new request was stored.
type gateCreateOutcome struct {
	gr *security.GateRequest
	// rule approved it (an allow rule or a task trust).
	rule *gates.Rule
	// held says why a request of a trusted task still waits for the Board.
	held string
	// tev1 is the trust whose tev1 mode decides this request.
	tev1 *gates.Rule
}

// createOrAutoApprove stores the request: approved by the first matching
// Board allow rule or by the task's trust or, failing that, its
// organization's trust (with its audit row and hit count, in one
// transaction), or pending. Under trust, merges/pushes to protected
// branches stay pending, deletes outside the worktree stay pending with a
// deferral deadline, and tev1 mode leaves the request to runTev1. Under any
// trust the Board's unattended-run rules (AnalyzeBoardRules) also stay
// pending. Every held request gets the deferral deadline, so the run moves on
// and the Board decides it in the morning (task-9d94997c). A trust lookup
// error fails the request (fail closed).
func (h *SecurityGateHandler) createOrAutoApprove(in security.GateRequestInput) (*gateCreateOutcome, error) {
	now := h.clock()
	// Trust lookup and the exclusion analysis run git and read the
	// filesystem: do them before the transaction holds the only connection.
	var (
		trust     *gates.Rule
		facts     security.TrustFacts
		deferMins int
		boardRule string
	)
	if !gates.SpecialRunIDs[in.RunID] && in.TaskID != "" {
		var err error
		if trust, err = gates.ActiveTrust(h.db, in.TaskID, now); err != nil {
			return nil, err
		}
		if trust == nil {
			if trust, err = gates.ActiveOrgTrust(h.db, in.TaskID, now); err != nil {
				return nil, err
			}
			if trust != nil && !orgTrustCoversRepo(h.db, in.TaskID) {
				trust = nil
			}
		}
		if trust != nil {
			// The Board's unattended-run rules hold under any trust.
			boardRule = security.AnalyzeBoardRules(in.Cmdline, in.Scripts)
			if boardRule == "" && isFileEditRequest(in.Cmdline) {
				// The hook sends an edit only when it is outside the worktree
				// or to a protected path: never approved by a trust.
				boardRule = "file edit outside the task worktree (Board rule: edits stay in the worktree)"
			}
			facts = security.AnalyzeForTrust(in.Cmdline, gates.TrustContextFor(h.db, in.TaskID, in.CWD, in.Scripts))
			deferMins = gates.TrustDeferMinutes(h.db)
		}
	}

	tx, err := h.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rule, err := gates.MatchRule(tx, in, now)
	if err != nil {
		return nil, err
	}
	if rule == nil && trust != nil {
		// Revoked between the lookup and now: it approves nothing.
		if cur, err := gates.GetRule(tx, trust.ID); err != nil {
			return nil, err
		} else if cur == nil || !cur.Active(now) {
			trust = nil
		}
	}
	pending, approved := string(security.GateRequestPending), string(security.GateRequestApproved)
	if rule == nil && trust != nil && !facts.Protected && boardRule == "" && !facts.DeleteOutside && !trust.Tev1 {
		rule = trust
	}
	if rule == nil {
		gr, err := security.InsertGateRequest(tx, in, security.GateRequestPending, "")
		if err != nil {
			return nil, err
		}
		out := &gateCreateOutcome{gr: gr}
		if trust != nil {
			event, payload := "", map[string]any{"rule_id": trust.ID, "cmdline": gr.Cmdline, "task_id": gr.TaskID}
			deferHeld := func() error {
				at := now.Add(time.Duration(deferMins) * time.Minute)
				if err := security.SetDeferAt(tx, gr.ID, at); err != nil {
					return err
				}
				gr.DeferAt = &at
				payload["defer_at"] = at.UTC().Format(time.RFC3339)
				return nil
			}
			switch {
			case facts.Protected:
				if err := deferHeld(); err != nil {
					return nil, err
				}
				event, out.held = "security_gate_trust_held", "protected: "+facts.ProtectedWhy
				payload["message"] = fmt.Sprintf("held for the Board under trust (waits %d min, then is skipped): %s", deferMins, facts.ProtectedWhy)
			case boardRule != "":
				if err := deferHeld(); err != nil {
					return nil, err
				}
				event, out.held = "security_gate_trust_held", "board rule: "+boardRule
				payload["message"] = fmt.Sprintf("held for the Board under trust (waits %d min, then is skipped): %s", deferMins, boardRule)
			case facts.DeleteOutside:
				if err := deferHeld(); err != nil {
					return nil, err
				}
				event, out.held = "security_gate_trust_deferring", "delete outside worktree: "+facts.DeleteWhy
				payload["message"] = fmt.Sprintf("delete outside the worktree waits %d min, then is skipped: %s", deferMins, facts.DeleteWhy)
			default:
				event, out.tev1 = "security_gate_tev1_asked", trust
				payload["message"] = fmt.Sprintf("tev1 decides (threshold %.2f)", trust.Tev1Threshold)
			}
			if err := governance.LogGateEventTx(tx, gr.ID, gates.DecidedByTrust(trust.ID), event, nil, &pending, payload); err != nil {
				return nil, err
			}
		}
		return out, tx.Commit()
	}
	by := fmt.Sprintf("rule:%d", rule.ID)
	gr, err := security.InsertGateRequest(tx, in, security.GateRequestApproved, by)
	if err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("auto-approved by rule #%d", rule.ID)
	if rule.IsTrust() {
		msg = fmt.Sprintf("auto-approved by trust #%d (%s)", rule.ID, gates.TrustLabel(rule))
	}
	if err := governance.LogGateEventTx(tx, gr.ID, by, "security_gate_auto_approved", &pending, &approved,
		map[string]any{"message": msg, "rule_id": rule.ID, "trust": rule.IsTrust(),
			"cmdline": gr.Cmdline, "run_id": gr.RunID, "task_id": gr.TaskID, "scope": rule.Scope, "scope_value": rule.ScopeValue}); err != nil {
		return nil, err
	}
	if err := gates.RecordHit(tx, rule.ID, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &gateCreateOutcome{gr: gr, rule: rule}, nil
}

// isFileEditRequest reports a hook file-edit request ("Write <path>").
func isFileEditRequest(cmdline string) bool {
	for _, t := range []string{"Write ", "Edit ", "MultiEdit ", "NotebookEdit "} {
		if strings.HasPrefix(cmdline, t) {
			return true
		}
	}
	return false
}

// orgTrustCoversRepo reports whether an org trust may cover taskID: only a
// task with a repo that is not a work repo. Anyone can create a task in an
// organization pointing at any repo, so a work repo (or an unreadable or
// missing repo) is never covered (fail closed).
func orgTrustCoversRepo(db *sql.DB, taskID string) bool {
	var repo string
	if err := db.QueryRow(`SELECT COALESCE(repo_path,'') FROM tasks WHERE id = ?`, taskID).Scan(&repo); err != nil {
		return false
	}
	if strings.TrimSpace(repo) == "" {
		return false
	}
	return !repoIsWork(repo)
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

	h.deferDue()
	gr, err := security.GetGateRequest(h.db, id)
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	if gr == nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if !wait || gr.Status != security.GateRequestPending || gr.DeferredAt != nil {
		writeJSON(w, hookView(gr))
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
			writeJSON(w, hookView(gr))
			return
		case <-ticker.C:
			if gr.DeferAt != nil {
				h.deferDue()
			}
			fresh, err := security.GetGateRequest(h.db, id)
			if err != nil || fresh == nil {
				writeJSON(w, hookView(gr))
				return
			}
			gr = fresh
			if gr.Status != security.GateRequestPending || gr.DeferredAt != nil {
				writeJSON(w, hookView(gr))
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
		draft, err := gates.RuleFromRequest(cur, *remember, time.Now())
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
	if cur.DeferredAt != nil {
		// The hook gave up at the deadline: the command is not replayed. The
		// task is told on its thread; an approval also wakes it (afterDecide)
		// so the agent performs the action itself (task-9d94997c).
		payload["deferred"] = true
		payload["message"] = "deferred request decided; the command is not replayed, the task is told to perform it"
		if cur.TaskID != "" {
			msg := fmt.Sprintf("Board rejected held action %s: %s; do not perform it.", gr.ID, gr.Cmdline)
			if approved {
				msg = fmt.Sprintf("Board approved held action %s: %s; perform it now.", gr.ID, gr.Cmdline)
				if ruleDraft == nil {
					// Let the identical re-run through: a task-scoped exact
					// rule for 24h (MatchRule runs before any trust).
					draft, derr := gates.RuleFromRequest(cur, gates.RuleSpec{Scope: gates.ScopeTask, MatchKind: gates.MatchExact,
						ExpiresInMinutes: 24 * 60, Note: "approved deferred request " + gr.ID}, time.Now())
					if derr == nil {
						if newRule, err = gates.InsertRule(tx, draft); err != nil {
							return nil, nil, nil, &decideError{http.StatusInternalServerError, "rule write failed"}
						}
						payload["rule_id"] = newRule.ID
						msg += fmt.Sprintf(" Allow rule #%d lets exactly this command through for this task for 24h.", newRule.ID)
					} else {
						msg += " (No allow rule: " + derr.Error() + "; the re-run waits for the Board again.)"
					}
				}
			}
			if _, err := tx.Exec(`INSERT INTO task_comments (task_id, author, message) VALUES (?, 'board', ?)`, cur.TaskID, msg); err != nil {
				return nil, nil, nil, &decideError{http.StatusInternalServerError, "task comment write failed"}
			}
		}
	}
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
	if gr.DeferredAt != nil && gr.TaskID != "" && gr.Status == security.GateRequestApproved {
		orchestrator.GlobalDispatcher.Wake(gr.TaskID, "gate_approved_deferred", "gate_approved:"+gr.ID)
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
	draft, err := gates.RuleFromRequest(gr, req.RuleSpec, time.Now())
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
			scripts = gates.AdviceScripts(gr.Scripts)
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
		"trust_defer_minutes":   gates.TrustDeferMinutes(h.db),
		"tev1_threshold":        gates.Tev1Threshold(h.db),
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
		MainMergeApproval   *bool    `json:"main_merge_approval"`
		AdvisorEnabled      *bool    `json:"advisor_enabled"`
		PasskeyGraceMinutes *int     `json:"passkey_grace_minutes"`
		TrustDeferMinutes   *int     `json:"trust_defer_minutes"`
		Tev1Threshold       *float64 `json:"tev1_threshold"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	if req.PasskeyGraceMinutes != nil && (*req.PasskeyGraceMinutes < 0 || *req.PasskeyGraceMinutes > gates.MaxGraceMinutes) {
		http.Error(w, `{"error":"passkey_grace_minutes must be 0 (off) to 5"}`, http.StatusBadRequest)
		return
	}
	if req.TrustDeferMinutes != nil && (*req.TrustDeferMinutes < 1 || *req.TrustDeferMinutes > gates.MaxTrustDeferMinutes) {
		http.Error(w, `{"error":"trust_defer_minutes must be 1 to 120"}`, http.StatusBadRequest)
		return
	}
	if req.Tev1Threshold != nil && (*req.Tev1Threshold < gates.MinTev1Threshold || *req.Tev1Threshold > gates.MaxTev1Threshold) {
		http.Error(w, `{"error":"tev1_threshold must be 0.5 to 0.99"}`, http.StatusBadRequest)
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
	if err == nil && req.TrustDeferMinutes != nil {
		err = set(gates.SettingTrustDeferMinutes, strconv.Itoa(*req.TrustDeferMinutes))
	}
	if err == nil && req.Tev1Threshold != nil {
		err = set(gates.SettingTev1Threshold, strconv.FormatFloat(*req.Tev1Threshold, 'f', 2, 64))
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
