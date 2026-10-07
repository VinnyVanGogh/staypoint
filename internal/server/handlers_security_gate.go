package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

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
}

func NewSecurityGateHandler(db *sql.DB, hub *EventHub) *SecurityGateHandler {
	return &SecurityGateHandler{db: db, hub: hub}
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
		requests, err = listGateRequestsByStatus(h.db, status)
	}
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	if requests == nil {
		requests = []*security.GateRequest{}
	}
	writeJSON(w, map[string]any{"gate_requests": requests})
}

// CreateGateRequest handles POST /api/security/gate-requests
func (h *SecurityGateHandler) CreateGateRequest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cmdline string   `json:"cmdline"`
		Reasons []string `json:"reasons"`
		RunID   string   `json:"run_id"`
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
	gr, err := security.CreateGateRequest(h.db, req.Cmdline, req.Reasons, req.RunID)
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	h.hub.Publish("security_gate_request", map[string]any{
		"id":      gr.ID,
		"cmdline": gr.Cmdline,
		"reasons": gr.Reasons,
		"status":  gr.Status,
	})
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, gr)
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

// DecideGateRequest handles POST /api/security/gate-requests/{id}/decide
func (h *SecurityGateHandler) DecideGateRequest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Decision string `json:"decision"` // "approved" or "denied"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	approved := strings.ToLower(req.Decision) == "approved"
	if req.Decision != "approved" && req.Decision != "denied" {
		http.Error(w, `{"error":"decision must be approved or denied"}`, http.StatusBadRequest)
		return
	}
	var geminiScope *geminiapproval.Scope
	if cur, err := security.GetGateRequest(h.db, id); err == nil && cur != nil && cur.RunID == geminiapproval.RunID {
		scope, ok := geminiapproval.Parse(cur.Cmdline)
		if approved {
			// Hard no in work repos, even for the Board.
			if !ok {
				http.Error(w, `{"error":"invalid gemini-code request"}`, http.StatusForbidden)
				return
			}
			if err := geminiapproval.Validate(scope, isWorkRepoForGate); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusForbidden)
				return
			}
		}
		if ok {
			geminiScope = &scope
		}
	}
	gr, err := security.DecideGateRequest(h.db, id, approved)
	if err != nil {
		if strings.Contains(err.Error(), "not found or already decided") {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusConflict)
			return
		}
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}

	// Write to security_gate_audit_log (gate-scoped) and board_audit_log (Board action).
	pendingStatus := string(security.GateRequestPending)
	decidedStatus := string(gr.Status)
	if err := governance.LogGateEvent(h.db, gr.ID, "board", "security_gate_decided",
		&pendingStatus, &decidedStatus,
		map[string]any{"cmdline": gr.Cmdline, "decision": string(gr.Status), "run_id": gr.RunID},
	); err != nil {
		http.Error(w, `{"error":"audit log write failed"}`, http.StatusInternalServerError)
		return
	}
	if err := governance.LogBoardEvent(h.db, "board", governance.AuditBoardAction,
		map[string]string{"action": "decide_gate_request", "gate_id": id, "decision": req.Decision,
			"ip": r.RemoteAddr, "user_agent": r.UserAgent()}); err != nil {
		http.Error(w, `{"error":"board audit write failed"}`, http.StatusInternalServerError)
		return
	}
	h.hub.Publish("security_gate_decided", map[string]any{
		"id":       gr.ID,
		"decision": gr.Status,
	})
	if geminiScope != nil && geminiScope.TaskID != "" {
		// Wake the held task: an approval starts its run (consumed there),
		// a denial makes the daemon refuse it with a clear reason.
		orchestrator.GlobalDispatcher.Wake(geminiScope.TaskID, "gemini_code_"+string(gr.Status), "gemini_code:"+gr.ID)
	}
	writeJSON(w, gr)
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

// GetSecurityGateSettings handles GET /api/settings/security-gate
func (h *SecurityGateHandler) GetSecurityGateSettings(w http.ResponseWriter, r *http.Request) {
	val, err := getSettingKV(h.db, "gates.main_merge_approval")
	enabled := true // default on
	if err == nil && val == "false" {
		enabled = false
	}
	writeJSON(w, map[string]any{"main_merge_approval": enabled})
}

// UpdateSecurityGateSettings handles POST /api/settings/security-gate
func (h *SecurityGateHandler) UpdateSecurityGateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MainMergeApproval bool `json:"main_merge_approval"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	val := "true"
	if !req.MainMergeApproval {
		val = "false"
	}
	if err := setSettingKV(h.db, "gates.main_merge_approval", val); err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	action := "enabled"
	if !req.MainMergeApproval {
		action = "disabled"
	}
	if err := governance.LogBoardEvent(h.db, "board", governance.AuditBoardAction,
		map[string]string{"action": "update_security_gate_settings", "value": action,
			"ip": r.RemoteAddr, "user_agent": r.UserAgent()}); err != nil {
		http.Error(w, `{"error":"board audit write failed"}`, http.StatusInternalServerError)
		return
	}
	h.hub.Publish("security_gate_settings", map[string]any{"main_merge_approval": req.MainMergeApproval})
	writeJSON(w, map[string]any{"main_merge_approval": req.MainMergeApproval})
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

func listGateRequestsByStatus(db *sql.DB, status string) ([]*security.GateRequest, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if status == "all" {
		rows, err = db.Query(
			`SELECT id, cmdline, reasons_json, run_id, status, created_at, decided_at
			 FROM security_gate_requests ORDER BY created_at DESC LIMIT 100`)
	} else {
		rows, err = db.Query(
			`SELECT id, cmdline, reasons_json, run_id, status, created_at, decided_at
			 FROM security_gate_requests WHERE status = ? ORDER BY created_at DESC LIMIT 100`, status)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*security.GateRequest
	for rows.Next() {
		var (
			gr        security.GateRequest
			rj        string
			runID     sql.NullString
			createdAt string
			decidedAt sql.NullString
		)
		if err := rows.Scan(&gr.ID, &gr.Cmdline, &rj, &runID, &gr.Status, &createdAt, &decidedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(rj), &gr.Reasons)
		gr.RunID = runID.String
		if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
			gr.CreatedAt = t
		}
		if decidedAt.Valid {
			if t, err := time.Parse(time.RFC3339Nano, decidedAt.String); err == nil {
				gr.DecidedAt = &t
			}
		}
		out = append(out, &gr)
	}
	return out, rows.Err()
}

