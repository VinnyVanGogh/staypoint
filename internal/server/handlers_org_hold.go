package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/governance"
)

// Org hold (2026-10-07): a Board-only switch per organization. While it is
// on, the dispatcher claims nothing in that organization (see
// governance.OrgNotHeldSQL). Reads are open to the session token so the UI
// and agents can see the HELD badge; writes are Board-only (WrapBoardAction:
// Board session + passkey), so an agent can neither place nor lift a hold.

// GetOrgHolds handles GET /api/settings/org-hold.
func (h *SecurityGateHandler) GetOrgHolds(w http.ResponseWriter, r *http.Request) {
	held, err := governance.HeldOrgs(h.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	if held == nil {
		held = []string{}
	}
	writeJSON(w, map[string]any{"held": held})
}

// UpdateOrgHold handles POST /api/settings/org-hold
// {"organization": "...", "held": bool}. Board-only.
func (h *SecurityGateHandler) UpdateOrgHold(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Organization string `json:"organization"`
		Held         *bool  `json:"held"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Organization) == "" || req.Held == nil {
		writeError(w, http.StatusBadRequest, "organization and held are required")
		return
	}
	org := strings.TrimSpace(req.Organization)
	if err := governance.SetOrgHold(h.db, org, *req.Held); err != nil {
		writeError(w, http.StatusInternalServerError, "db error")
		return
	}
	if err := governance.LogBoardEvent(h.db, "board", governance.AuditBoardAction,
		map[string]string{"action": "update_org_hold", "organization": org,
			"held": strconv.FormatBool(*req.Held), "ip": r.RemoteAddr, "user_agent": r.UserAgent()}); err != nil {
		writeError(w, http.StatusInternalServerError, "board audit write failed")
		return
	}
	if h.hub != nil {
		h.hub.Publish("org_hold", map[string]any{"organization": org, "held": *req.Held})
	}
	writeJSON(w, map[string]any{"organization": org, "held": *req.Held})
}
