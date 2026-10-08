package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/gates"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/security"
)

// "Trust this organization until…" (task-33692ffb): creating, revoking and
// listing org trusts. It sits next to org hold in Settings. Reads are open to
// the session token like org hold's; creating needs the Board session and a
// passkey (WrapBoardAction), revoking the Board session only, like a task
// trust. The approval itself is in createOrAutoApprove.

// orgTrustView is one org trust with what it approved and what still waits.
type orgTrustView struct {
	*gates.Rule
	Active        bool                    `json:"active"`
	AutoApproved  []*security.GateRequest `json:"auto_approved"`
	AutoCount     int                     `json:"auto_approved_count"`
	HeldCount     int                     `json:"held_count"`
	DeferredCount int                     `json:"deferred_count"`
}

func (h *SecurityGateHandler) buildOrgTrustView(r *gates.Rule, now time.Time) (*orgTrustView, error) {
	reqs, err := gates.TrustRequests(h.db, []int64{r.ID})
	if err != nil {
		return nil, err
	}
	v := &orgTrustView{Rule: r, Active: r.Active(now), AutoApproved: []*security.GateRequest{}}
	for _, gr := range reqs {
		if gr.Status == security.GateRequestApproved {
			v.AutoApproved = append(v.AutoApproved, gr)
		}
	}
	v.AutoCount = len(v.AutoApproved)
	held, err := gates.OrgHeldRequests(h.db, r.ScopeValue)
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
	return v, nil
}

// ListOrgTrusts handles GET /api/settings/org-trust: the org trusts active
// now, and the limits the picker offers.
func (h *SecurityGateHandler) ListOrgTrusts(w http.ResponseWriter, r *http.Request) {
	now := h.clock()
	rules, err := gates.ActiveOrgTrusts(h.db, now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	out := []*orgTrustView{}
	for _, ru := range rules {
		v, err := h.buildOrgTrustView(ru, now)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db error")
			return
		}
		out = append(out, v)
	}
	writeJSON(w, map[string]any{
		"trusts": out, "presets": gates.TrustPresets, "min_minutes": gates.MinTrustMinutes,
		"max_minutes": gates.MaxOrgTrustMinutes, "tev1_threshold": gates.Tev1Threshold(h.db),
		"tev1_warning": gates.Tev1Warning, "tev1_configured": h.tev1Advisor() != nil,
	})
}

// CreateOrgTrust handles POST /api/settings/org-trust (Board + Touch ID):
// {"org":"...","preset":"1h"|"4h"|"overnight"|"custom","minutes":N,
// "tev1":bool,"tev1_ack":bool}. Unknown fields are refused, so a client
// cannot send an expiry, a pattern or exclusions; the server derives them.
func (h *SecurityGateHandler) CreateOrgTrust(w http.ResponseWriter, r *http.Request) {
	var spec gates.OrgTrustSpec
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	now := h.clock()
	draft, err := gates.NewOrgTrust(spec, gates.Tev1Threshold(h.db), now)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if ok, err := gates.OrgExists(h.db, draft.ScopeValue); err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	} else if !ok {
		writeError(w, http.StatusNotFound, "no task belongs to organization "+draft.ScopeValue)
		return
	}
	tx, err := h.db.Begin()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	defer tx.Rollback()
	rule, err := gates.InsertOrgTrust(tx, draft, now)
	if err == nil {
		err = governance.LogBoardEventTx(tx, "board", governance.AuditBoardAction, map[string]string{
			"action": "create_org_trust", "rule_id": fmt.Sprint(rule.ID), "organization": rule.ScopeValue,
			"expires_at": rule.ExpiresAt.UTC().Format(time.RFC3339), "tev1": fmt.Sprint(rule.Tev1),
			"tev1_threshold": fmt.Sprint(rule.Tev1Threshold), "ip": r.RemoteAddr, "user_agent": r.UserAgent()})
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "trust write failed: "+err.Error())
		return
	}
	h.hub.Publish("security_gate_rules", map[string]any{"id": rule.ID, "action": "created", "organization": rule.ScopeValue, "trust": true})
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, rule)
}

// RevokeOrgTrust handles POST /api/settings/org-trust/revoke {"org":"..."}
// (Board session). Revoking only removes power, so it does not wait for
// Touch ID; it ends the trust at once.
func (h *SecurityGateHandler) RevokeOrgTrust(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Org string `json:"org"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || gates.NormalizeOrg(req.Org) == "" {
		writeError(w, http.StatusBadRequest, "org required")
		return
	}
	org := gates.NormalizeOrg(req.Org)
	now := h.clock()
	trusts, err := gates.OrgTrustsFor(h.db, org)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	tx, err := h.db.Begin()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
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
			writeError(w, http.StatusInternalServerError, "db error")
			return
		}
		if ok {
			ended = append(ended, t.ID)
		}
	}
	if len(ended) == 0 {
		writeError(w, http.StatusNotFound, "no trust to revoke")
		return
	}
	if err := governance.LogBoardEventTx(tx, "board", governance.AuditBoardAction, map[string]string{
		"action": "revoke_org_trust", "organization": org, "rule_ids": fmt.Sprint(ended), "ip": r.RemoteAddr}); err != nil {
		writeError(w, http.StatusInternalServerError, "board audit write failed")
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	h.hub.Publish("security_gate_rules", map[string]any{"action": "revoked", "organization": org, "trust": true})
	writeJSON(w, map[string]any{"organization": org, "revoked": ended})
}
