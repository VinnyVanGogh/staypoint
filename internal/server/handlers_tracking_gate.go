package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/trackgate"
)

// STA-854: per-company tracking-gate toggle. Reads are open to the session
// token; writes are Board-only (WrapBoardAction in server.go) so an agent
// cannot switch off the gate that is blocking it. STA-816's gates page will
// call these.

type trackingGateCompany struct {
	Company         string     `json:"company"`
	Enabled         bool       `json:"enabled"`
	Source          string     `json:"source"` // "default" or "board"
	OverrideUntil   *time.Time `json:"override_until,omitempty"`
	OverrideRequest string     `json:"override_request,omitempty"`
}

// GetTrackingGateSettings handles GET /api/settings/tracking-gate.
func (h *SecurityGateHandler) GetTrackingGateSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := trackgate.ListSettings(h.db)
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	names := []string{trackgate.CompanyManagedSolution, trackgate.CompanyPersonal}
	seen := map[string]bool{}
	for _, n := range names {
		seen[strings.ToLower(n)] = true
	}
	for k := range settings {
		if !seen[k] {
			names = append(names, k)
			seen[k] = true
		}
	}
	out := make([]trackingGateCompany, 0, len(names))
	now := time.Now()
	for _, n := range names {
		on, err := trackgate.Enabled(h.db, n)
		if err != nil {
			http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
			return
		}
		c := trackingGateCompany{Company: n, Enabled: on, Source: "default"}
		if _, ok := settings[strings.ToLower(n)]; ok {
			c.Source = "board"
		}
		if ov, err := trackgate.ActiveOverride(h.db, n, now); err == nil && ov != nil {
			exp := ov.ExpiresAt
			c.OverrideUntil = &exp
			c.OverrideRequest = ov.GateRequestID
		}
		out = append(out, c)
	}
	writeJSON(w, map[string]any{"companies": out})
}

// UpdateTrackingGateSettings handles POST /api/settings/tracking-gate
// {"company": "...", "enabled": bool}. Board-only.
func (h *SecurityGateHandler) UpdateTrackingGateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Company string `json:"company"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Company) == "" || req.Enabled == nil {
		http.Error(w, `{"error":"company and enabled are required"}`, http.StatusBadRequest)
		return
	}
	company := strings.TrimSpace(req.Company)
	if err := trackgate.SetEnabled(h.db, company, *req.Enabled); err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	if err := governance.LogBoardEvent(h.db, "board", governance.AuditBoardAction,
		map[string]string{"action": "update_tracking_gate", "company": company,
			"enabled": strconv.FormatBool(*req.Enabled), "ip": r.RemoteAddr, "user_agent": r.UserAgent()}); err != nil {
		http.Error(w, `{"error":"board audit write failed"}`, http.StatusInternalServerError)
		return
	}
	h.hub.Publish("tracking_gate_settings", map[string]any{"company": company, "enabled": *req.Enabled})
	writeJSON(w, map[string]any{"company": company, "enabled": *req.Enabled})
}
