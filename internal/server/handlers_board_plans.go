package server

import (
	"bytes"
	gocontext "context"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/alerts"
	"github.com/VinnyVanGogh/staypoint/internal/boardplan"
	"github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/security"
	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

// Board action plans (task-e1b24d66). A Board session proposes a batch of
// Board actions; the Board reviews them on /board/plans/<id>, unticks what it
// does not want and signs the selected rows with one passkey assertion. The
// daemon then runs each row through the same route the per-task button uses
// (the real mux, so WrapBoardAction and every in-handler check still apply),
// carrying an in-process marker in place of a per-row assertion.
//
// Who may propose: the web UI (Board session cookie) or the Board's own
// terminal (the CLI / MCP tool, which send the board token and refuse inside
// a daemon run). The agent token alone cannot propose, and nothing executes
// without a fresh assertion bound to the exact selection.

// planRowKey is the context key of a signed plan row. Only the plan executor
// sets it, on requests it builds in process.
type planRowKey struct{}

type planRow struct {
	planID string
	index  int
}

// planRowAuthorized reports whether r is a row of a signed plan being run.
func planRowAuthorized(r *http.Request) bool {
	row, _ := r.Context().Value(planRowKey{}).(*planRow)
	return row != nil && row.planID != ""
}

// BoardPlansHandler serves /api/board/plans.
type BoardPlansHandler struct {
	db       *sql.DB
	hub      *EventHub
	secMid   *SecurityMiddleware
	webAuthn *WebAuthnHandler
	routes   http.Handler // the daemon's mux: rows run through the real routes
}

// NewBoardPlansHandler creates a BoardPlansHandler.
func NewBoardPlansHandler(db *sql.DB, hub *EventHub, secMid *SecurityMiddleware, webAuthn *WebAuthnHandler, routes http.Handler) *BoardPlansHandler {
	return &BoardPlansHandler{db: db, hub: hub, secMid: secMid, webAuthn: webAuthn, routes: routes}
}

// proposerKind says which Board credential proposed: the web UI's Board
// session or the Board terminal's board token. "" means neither.
func (h *BoardPlansHandler) proposerKind(r *http.Request) string {
	if h.secMid.IsBoardSession(r) {
		return "web"
	}
	bt := r.Header.Get("X-Board-Token")
	if bt != "" && h.secMid.boardToken != "" &&
		subtle.ConstantTimeCompare([]byte(bt), []byte(h.secMid.boardToken)) == 1 {
		return "cli"
	}
	return ""
}

// Propose handles POST /api/board/plans: {proposer, actions:[...]}. It stores
// a pending plan and changes nothing else.
func (h *BoardPlansHandler) Propose(w http.ResponseWriter, r *http.Request) {
	kind := h.proposerKind(r)
	if kind == "" {
		writeBoardError(w, "board_session_required",
			"forbidden: only the Board (web UI session or the Board terminal's board token) may propose a plan")
		return
	}
	// The CLI and MCP tool refuse inside a daemon run; a run that still
	// reaches here (it would need the board token) is refused as well.
	if r.Header.Get("X-StayPoint-Task-ID") != "" {
		writeBoardError(w, "agent_run_refused", "forbidden: a daemon agent run cannot propose Board action plans")
		return
	}
	var req struct {
		Proposer string             `json:"proposer"`
		Actions  []boardplan.Action `json:"actions"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	proposer := strings.TrimSpace(req.Proposer)
	if proposer == "" {
		proposer = kind
	}
	if len(proposer) > 200 {
		proposer = proposer[:200]
	}
	plan, err := boardplan.Create(h.db, proposer, kind, req.Actions)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	_, _ = alerts.Record(h.db, alerts.Alert{
		Kind:      "board_plan",
		Severity:  alerts.SeverityInfo,
		Title:     fmt.Sprintf("Board action plan to sign: %d actions", len(plan.Actions)),
		Message:   fmt.Sprintf("Proposed by %s. Review and sign at /board/plans/%s", plan.Proposer, plan.ID),
		DedupeKey: "board_plan:" + plan.ID,
	})
	h.hub.Publish("board_plan_proposed", map[string]any{"id": plan.ID, "actions": len(plan.Actions), "proposer": plan.Proposer})
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]any{"plan": plan, "url": "/board/plans/" + plan.ID})
}

// List handles GET /api/board/plans?status=pending.
func (h *BoardPlansHandler) List(w http.ResponseWriter, r *http.Request) {
	plans, err := boardplan.List(h.db, r.URL.Query().Get("status"), 50)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"plans": plans})
}

// planRowView is one action with what the Board needs to judge it.
type planRowView struct {
	Index  int              `json:"index"`
	Action boardplan.Action `json:"action"`
	Task   *struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Stage string `json:"stage"`
	} `json:"task,omitempty"`
	Card *struct {
		Status     string `json:"status"`
		HeadSHA    string `json:"head_sha"`
		Branch     string `json:"branch"`
		Target     string `json:"target_branch,omitempty"`
		PRNumber   int    `json:"pr_number,omitempty"`
		PRURL      string `json:"pr_url,omitempty"`
		HeadMoved  bool   `json:"head_moved"`
		ViaPRMerge bool   `json:"via_pr_merge"`
	} `json:"card,omitempty"`
	Gate *struct {
		Cmdline string `json:"cmdline"`
		Status  string `json:"status"`
		TaskID  string `json:"task_id,omitempty"`
	} `json:"gate,omitempty"`
	Problem string `json:"problem,omitempty"`
}

// Get handles GET /api/board/plans/{id}: the plan plus, per row, the task,
// the Ship Review head it would merge and the gate command it would decide.
func (h *BoardPlansHandler) Get(w http.ResponseWriter, r *http.Request) {
	plan, err := boardplan.Get(h.db, r.PathValue("id"))
	if errors.Is(err, boardplan.ErrNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rows := make([]planRowView, len(plan.Actions))
	for i, a := range plan.Actions {
		rows[i] = h.describe(i, a)
	}
	writeJSON(w, map[string]any{"plan": plan, "rows": rows})
}

func (h *BoardPlansHandler) describe(i int, a boardplan.Action) planRowView {
	v := planRowView{Index: i, Action: a}
	if a.TaskID != "" {
		if t, err := context.GetTask(h.db, a.TaskID); err == nil {
			stage := t.ExecutionStage
			if stage == "" {
				stage = t.Status
			}
			v.Task = &struct {
				ID    string `json:"id"`
				Name  string `json:"name"`
				Stage string `json:"stage"`
			}{t.ID, t.Name, stage}
		} else {
			v.Problem = "task not found"
		}
	}
	if a.Action == boardplan.ApproveMerge {
		card, err := shipreview.GetCard(h.db, a.TaskID)
		if err != nil {
			v.Problem = "no Ship Review card: " + err.Error()
		} else {
			v.Card = &struct {
				Status     string `json:"status"`
				HeadSHA    string `json:"head_sha"`
				Branch     string `json:"branch"`
				Target     string `json:"target_branch,omitempty"`
				PRNumber   int    `json:"pr_number,omitempty"`
				PRURL      string `json:"pr_url,omitempty"`
				HeadMoved  bool   `json:"head_moved"`
				ViaPRMerge bool   `json:"via_pr_merge"`
			}{card.Status, card.HeadSHA, card.Branch, card.TargetBranch, card.PRNumber, card.PRURL,
				card.HeadSHA != a.ExpectedHeadSHA, cardMergesViaPR(card)}
			if card.HeadSHA != a.ExpectedHeadSHA {
				v.Problem = "the card's head moved since the plan was proposed; this row will be refused"
			}
		}
	}
	if a.GateID != "" {
		if gr, err := security.GetGateRequest(h.db, a.GateID); err == nil && gr != nil {
			v.Gate = &struct {
				Cmdline string `json:"cmdline"`
				Status  string `json:"status"`
				TaskID  string `json:"task_id,omitempty"`
			}{gr.Cmdline, string(gr.Status), gr.TaskID}
		} else {
			v.Problem = "gate request not found"
		}
	}
	return v
}

// cardMergesViaPR mirrors the task page: a pending pr_merge card whose head
// is on the PR shows "Merge PR" instead of "Approve".
func cardMergesViaPR(c *shipreview.Card) bool {
	return c.Status == shipreview.StatusPending && c.MergeMode == shipreview.MergeModePRMerge &&
		c.PRNumber > 0 && c.PRChecksSHA != "" && c.PRChecksSHA == c.HeadSHA
}

// selectionRequest is the body of challenge and execute.
type selectionRequest struct {
	Selected []int `json:"selected"`
}

// loadSelection reads the plan and the selection, both as stored now.
func (h *BoardPlansHandler) loadSelection(w http.ResponseWriter, r *http.Request) (*boardplan.Plan, []int, []byte, bool) {
	plan, err := boardplan.Get(h.db, r.PathValue("id"))
	if errors.Is(err, boardplan.ErrNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return nil, nil, nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, nil, nil, false
	}
	if plan.Status != boardplan.StatusPending {
		writeError(w, http.StatusConflict, "plan is "+plan.Status+", not pending")
		return nil, nil, nil, false
	}
	var req selectionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return nil, nil, nil, false
	}
	sel, err := boardplan.NormalizeSelection(req.Selected, len(plan.Actions))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return nil, nil, nil, false
	}
	return plan, sel, boardplan.SelectionHash(plan, sel), true
}

// Challenge handles POST /api/board/plans/{id}/challenge (Board session):
// {selected:[...]} -> WebAuthn request options whose challenge ends in the
// selection hash. The session token comes back in X-WebAuthn-Session.
func (h *BoardPlansHandler) Challenge(w http.ResponseWriter, r *http.Request) {
	plan, _, selHash, ok := h.loadSelection(w, r)
	if !ok {
		return
	}
	options, token, err := h.webAuthn.PlanChallenge(plan.ID, selHash)
	if errors.Is(err, errNoPasskey) {
		writeError(w, http.StatusPreconditionFailed, "no credentials registered")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("X-WebAuthn-Session", token)
	w.Header().Set("X-Board-Plan-Selection", hex.EncodeToString(selHash))
	writeJSON(w, options)
}

// Execute handles POST /api/board/plans/{id}/execute (Board session):
// {selected:[...]} with X-WebAuthn-Session and X-WebAuthn-Assertion from a
// plan challenge for exactly this plan and selection. Runs the selected rows
// in order; a failing row does not stop the rest.
func (h *BoardPlansHandler) Execute(w http.ResponseWriter, r *http.Request) {
	plan, sel, selHash, ok := h.loadSelection(w, r)
	if !ok {
		return
	}
	token, assertion := r.Header.Get("X-WebAuthn-Session"), r.Header.Get("X-WebAuthn-Assertion")
	if token == "" || assertion == "" {
		writeBoardError(w, "board_passkey_assertion_required", "forbidden: signing a Board action plan requires a passkey assertion")
		return
	}
	signer, err := h.webAuthn.VerifyPlanAssertion(token, assertion, plan.ID, selHash)
	if err != nil {
		slog.Warn("board plan: passkey assertion rejected", slog.String("plan", plan.ID), slog.String("error", err.Error()))
		_ = governance.LogBoardEvent(h.db, "board", governance.AuditPasskeyEvent,
			map[string]any{"action": "board_plan_sign_refused", "plan_id": plan.ID, "error": err.Error(), "ip": r.RemoteAddr})
		writeBoardError(w, "board_passkey_assertion_invalid", "forbidden: "+err.Error())
		return
	}
	if err := boardplan.Claim(h.db, plan.ID, plan.ContentHash, sel, hex.EncodeToString(selHash), signer); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	results := make([]boardplan.Result, 0, len(sel))
	for _, i := range sel {
		res := h.runRow(r, plan, i)
		results = append(results, res)
		a := plan.Actions[i]
		_ = governance.LogBoardEvent(h.db, "board", governance.AuditBoardAction, map[string]any{
			"action": "board_plan_row", "plan_id": plan.ID, "row": i, "plan_action": a.Action,
			"task_id": a.TaskID, "gate_id": a.GateID, "expected_head_sha": a.ExpectedHeadSHA,
			"signer": signer, "selection_hash": hex.EncodeToString(selHash), "proposer": plan.Proposer,
			"status": res.Status, "http_status": res.HTTP, "error": res.Error,
			"ip": r.RemoteAddr, "user_agent": r.UserAgent(),
		})
	}
	if err := boardplan.Finish(h.db, plan.ID, results); err != nil {
		slog.Warn("board plan: record results", slog.String("plan", plan.ID), slog.String("error", err.Error()))
	}
	done, _ := boardplan.Get(h.db, plan.ID)
	h.hub.Publish("board_plan_executed", map[string]any{"id": plan.ID})
	writeJSON(w, map[string]any{"plan": done, "results": results})
}

// Discard handles POST /api/board/plans/{id}/discard (Board session).
func (h *BoardPlansHandler) Discard(w http.ResponseWriter, r *http.Request) {
	err := boardplan.Discard(h.db, r.PathValue("id"))
	switch {
	case errors.Is(err, boardplan.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, boardplan.ErrNotPending):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, map[string]string{"status": "discarded"})
	}
}

// planCall is one request a row makes, in order.
type planCall struct {
	path string
	body any
}

// rowCalls maps a row to the requests its per-task button sends.
func (h *BoardPlansHandler) rowCalls(a boardplan.Action) ([]planCall, error) {
	task := "/api/tasks/" + url.PathEscape(a.TaskID)
	stopFirst := func() []planCall {
		t, err := context.GetTask(h.db, a.TaskID)
		if err != nil {
			return nil
		}
		stage := t.ExecutionStage
		if stage == "" {
			stage = t.Status
		}
		if stage == "in_progress" || stage == "paused" {
			return []planCall{{task + "/run-control", map[string]string{"action": "stop"}}}
		}
		return nil
	}
	switch a.Action {
	case boardplan.ApproveMerge:
		card, err := shipreview.GetCard(h.db, a.TaskID)
		if err != nil {
			return nil, fmt.Errorf("no Ship Review card: %w", err)
		}
		if card.HeadSHA != a.ExpectedHeadSHA {
			return nil, fmt.Errorf("head moved: the card is at %s, the plan expected %s", card.HeadSHA, a.ExpectedHeadSHA)
		}
		if cardMergesViaPR(card) {
			return []planCall{{task + "/ship-review/merge", map[string]string{"head_sha": a.ExpectedHeadSHA}}}, nil
		}
		return []planCall{{task + "/ship-review/approve", map[string]string{"head_sha": a.ExpectedHeadSHA}}}, nil
	case boardplan.SendBack:
		return []planCall{{task + "/ship-review/send-back", map[string]string{"comment": a.Text}}}, nil
	case boardplan.MarkDone:
		body := map[string]any{"board": true}
		if note := strings.TrimSpace(a.Text); note != "" {
			body["note"] = note
		}
		return append(stopFirst(), planCall{task + "/done", body}), nil
	case boardplan.RunNow:
		return []planCall{{task + "/stage", map[string]string{"stage": "in_progress"}}}, nil
	case boardplan.Cancel:
		return append(stopFirst(), planCall{task + "/stage", map[string]string{"stage": "cancelled"}}), nil
	case boardplan.GateApprove, boardplan.GateDeny:
		decision := "approved"
		if a.Action == boardplan.GateDeny {
			decision = "denied"
		}
		return []planCall{{"/api/security/gate-requests/" + url.PathEscape(a.GateID) + "/decide", map[string]string{"decision": decision}}}, nil
	case boardplan.Unblock:
		return []planCall{{task + "/unblock", nil}}, nil
	}
	return nil, fmt.Errorf("unknown action %q", a.Action)
}

// runRow runs one row. A row stops at its first failing request.
func (h *BoardPlansHandler) runRow(r *http.Request, plan *boardplan.Plan, i int) (res boardplan.Result) {
	res.Index = i
	defer func() {
		if p := recover(); p != nil {
			res.Status, res.Error = "failed", fmt.Sprintf("internal error: %v", p)
		}
	}()
	calls, err := h.rowCalls(plan.Actions[i])
	if err != nil {
		res.Status, res.Error = "failed", err.Error()
		return res
	}
	// Detached from the Board's request so closing the tab cannot abort a
	// merge halfway; the row marker is what stands in for the assertion.
	ctx := gocontext.WithValue(gocontext.WithoutCancel(r.Context()), planRowKey{}, &planRow{planID: plan.ID, index: i})
	for _, c := range calls {
		var body []byte
		if c.body != nil {
			body, _ = json.Marshal(c.body)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.path, bytes.NewReader(body))
		if err != nil {
			res.Status, res.Error = "failed", err.Error()
			return res
		}
		req.Host, req.RemoteAddr = r.Host, r.RemoteAddr
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "staypoint-board-plan/"+plan.ID)
		for _, ck := range r.Cookies() {
			req.AddCookie(ck)
		}
		rec := &boardGateRefusal{header: http.Header{}, status: http.StatusOK}
		h.routes.ServeHTTP(rec, req)
		res.HTTP = rec.status
		if rec.status < 200 || rec.status > 299 {
			res.Status, res.Error = "failed", responseError(rec.status, rec.body.Bytes())
			return res
		}
	}
	res.Status = "ok"
	return res
}

// responseError pulls a readable message out of an error response.
func responseError(status int, body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		for _, k := range []string{"message", "error"} {
			if s, ok := m[k].(string); ok && s != "" {
				if k == "message" {
					if code, ok := m["error"].(string); ok && code != "" {
						return code + ": " + s
					}
				}
				return s
			}
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	if s == "" {
		s = http.StatusText(status)
	}
	return fmt.Sprintf("HTTP %d: %s", status, s)
}
