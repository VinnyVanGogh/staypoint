package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/config"
	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/fleet"
	"github.com/VinnyVanGogh/staypoint/internal/reporting"
)

const fleetOverviewCacheTTL = 10 * time.Second

type fleetOverviewCache struct {
	mu        sync.Mutex
	body      []byte
	expiresAt time.Time
}

type TelemetryHandler struct {
	db              *sql.DB
	hub             *EventHub
	fleetAgg        *fleet.Aggregator
	telemetryDBPath string
	overviewCache   fleetOverviewCache
	// overviewCacheAll caches the include_legacy=1 overview separately so the
	// default (hidden) and full bodies never overwrite each other.
	overviewCacheAll fleetOverviewCache
}

func NewTelemetryHandler(db *sql.DB, hub *EventHub, telemetryDBPath string) *TelemetryHandler {
	agg := fleet.NewAggregator(db, telemetryDBPath, nil)
	// Running counts are runs in flight in this daemon (task-3387cad2).
	agg.LiveRuns = func() map[string]time.Time { return liveRuns() }
	return &TelemetryHandler{
		db:              db,
		hub:             hub,
		fleetAgg:        agg,
		telemetryDBPath: telemetryDBPath,
	}
}

type QuotaWindowRecord struct {
	PoolKey      string    `json:"pool_key"`
	WindowType   string    `json:"window_type"`
	UsedPercent  float64   `json:"used_percent"`
	RemainingPct float64   `json:"remaining_pct"`
	IsLocked     bool      `json:"is_locked"`
	ResetsAt     *string   `json:"resets_at,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type QuotaFetchRecord struct {
	Provider      string  `json:"provider"`
	LastAttemptAt string  `json:"last_attempt_at"`
	NextAttemptAt string  `json:"next_attempt_at"`
	LastSuccessAt *string `json:"last_success_at,omitempty"`
	LastStatus    string  `json:"last_status"`
}

type TaskSpendSummary struct {
	TotalSpentUSD    float64 `json:"total_spent_usd"`
	TotalSpentTokens int64   `json:"total_spent_tokens"`
	TotalTurns       int     `json:"total_turns"`
	ActiveTasks      int     `json:"active_tasks"`
	DoneTasks        int     `json:"done_tasks"`
}

// GetTelemetry handles GET /api/telemetry
func (h *TelemetryHandler) GetTelemetry(w http.ResponseWriter, r *http.Request) {
	// 1. Quota windows
	var windows []QuotaWindowRecord
	qRows, err := h.db.QueryContext(r.Context(), `
		SELECT pool_key, window_type, used_percent, remaining_pct, is_locked, resets_at, updated_at
		FROM quota_windows
		ORDER BY pool_key, window_type;
	`)
	if err == nil {
		defer qRows.Close()
		for qRows.Next() {
			var wRec QuotaWindowRecord
			var isLockedInt int
			var resetsAt sql.NullString
			var updatedAtStr string
			if err := qRows.Scan(&wRec.PoolKey, &wRec.WindowType, &wRec.UsedPercent, &wRec.RemainingPct, &isLockedInt, &resetsAt, &updatedAtStr); err == nil {
				wRec.IsLocked = isLockedInt != 0
				if resetsAt.Valid {
					wRec.ResetsAt = &resetsAt.String
				}
				if t, err := time.Parse(time.RFC3339Nano, updatedAtStr); err == nil {
					wRec.UpdatedAt = t
				}
				windows = append(windows, wRec)
			}
		}
	}

	// 2. Quota fetch states
	var fetchStates []QuotaFetchRecord
	fRows, err := h.db.QueryContext(r.Context(), `
		SELECT provider, last_attempt_at, next_attempt_at, last_success_at, last_status
		FROM quota_fetch_state;
	`)
	if err == nil {
		defer fRows.Close()
		for fRows.Next() {
			var fRec QuotaFetchRecord
			var lastSuccess sql.NullString
			if err := fRows.Scan(&fRec.Provider, &fRec.LastAttemptAt, &fRec.NextAttemptAt, &lastSuccess, &fRec.LastStatus); err == nil {
				if lastSuccess.Valid {
					fRec.LastSuccessAt = &lastSuccess.String
				}
				fetchStates = append(fetchStates, fRec)
			}
		}
	}

	// 3. Task spend summary (legacy tasks and archived imports hidden unless
	// include_legacy / include_archive).
	includeHidden := includeHiddenTasks(r)
	var spend TaskSpendSummary
	row := h.db.QueryRowContext(r.Context(), `
		SELECT
			COALESCE(SUM(spent_usd), 0),
			COALESCE(SUM(spent_tokens), 0),
			COALESCE(SUM(spent_turns), 0),
			COALESCE(SUM(CASE WHEN status = 'active' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'done' THEN 1 ELSE 0 END), 0)
		FROM tasks
		WHERE `+meshContext.VisibleTasksSQL("", includeHidden)+`;
	`)
	_ = row.Scan(&spend.TotalSpentUSD, &spend.TotalSpentTokens, &spend.TotalTurns, &spend.ActiveTasks, &spend.DoneTasks)

	w.Header().Set("Content-Type", "application/json")
	respMap := map[string]any{
		"timestamp":    time.Now().UTC(),
		"quota_pools":  windows,
		"fetch_states": fetchStates,
		"task_spend":   spend,
	}

	if h.fleetAgg != nil {
		if fleetOverview, err := h.fleetAgg.GatherWith(r.Context(), fleet.GatherOptions{IncludeHidden: includeHidden}); err == nil {
			respMap["fleet"] = fleetOverview
		}
	}

	_ = json.NewEncoder(w).Encode(respMap)
}

// GetFleetOverview handles GET /api/fleet/overview.
// Legacy tasks and archived imports are left out of tasks, counts, projects
// and spend unless include_legacy / include_archive is set.
// Results are cached for fleetOverviewCacheTTL (10 s) to avoid the ≈3–4 s
// Paperclip API round-trip on every page load.
func (h *TelemetryHandler) GetFleetOverview(w http.ResponseWriter, r *http.Request) {
	if h.fleetAgg == nil {
		http.Error(w, `{"error":"fleet aggregator unavailable"}`, http.StatusServiceUnavailable)
		return
	}

	includeHidden := includeHiddenTasks(r)
	cache := &h.overviewCache
	if includeHidden {
		cache = &h.overviewCacheAll
	}

	cache.mu.Lock()
	if time.Now().Before(cache.expiresAt) && len(cache.body) > 0 {
		body := cache.body
		cache.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Fleet-Cache", "hit")
		_, _ = w.Write(body)
		return
	}
	cache.mu.Unlock()

	t0 := time.Now()
	overview, err := h.fleetAgg.GatherWith(r.Context(), fleet.GatherOptions{IncludeHidden: includeHidden})
	elapsed := time.Since(t0)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	body, err := json.Marshal(overview)
	if err != nil {
		http.Error(w, `{"error":"marshal failed"}`, http.StatusInternalServerError)
		return
	}

	cache.mu.Lock()
	cache.body = body
	cache.expiresAt = time.Now().Add(fleetOverviewCacheTTL)
	cache.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Fleet-Cache", "miss")
	w.Header().Set("X-Fleet-Gather-Ms", fmt.Sprintf("%d", elapsed.Milliseconds()))
	_, _ = w.Write(body)
}

// paperclipAPIBase returns the Paperclip local server base URL.
func paperclipAPIBase() string {
	if u := os.Getenv("PAPERCLIP_API_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://127.0.0.1:3100"
}

// proxyPaperclip forwards a GET request to the Paperclip API and pipes the response back.
func proxyPaperclip(w http.ResponseWriter, r *http.Request, path string) {
	target := paperclipAPIBase() + path
	req, err := http.NewRequestWithContext(r.Context(), "GET", target, nil)
	if err != nil {
		http.Error(w, `{"error":"proxy request build failed"}`, http.StatusInternalServerError)
		return
	}
	if key := os.Getenv("PAPERCLIP_API_KEY"); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, `{"error":"paperclip unreachable"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// GetFleetTask proxies GET /api/fleet/tasks/{id} to the Paperclip issue detail API.
// This lets the web UI show full task body and metadata for fleet (Paperclip) tasks.
func (h *TelemetryHandler) GetFleetTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, `{"error":"missing task id"}`, http.StatusBadRequest)
		return
	}
	proxyPaperclip(w, r, "/api/issues/"+id)
}

// GetReport handles GET /api/report?type={work|personal|gemini|combined}&format={pdf|html}
// It renders the requested PDF report via chromedp and streams it as application/pdf,
// or returns the HTML directly if format=html is requested for interactive in-app preview.
func (h *TelemetryHandler) GetReport(w http.ResponseWriter, r *http.Request) {
	reportType := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("type")))
	if reportType == "" {
		reportType = "work"
	}

	allowed := map[string]bool{"work": true, "personal": true, "gemini": true, "combined": true, "boss": true, "fleet": true}
	if !allowed[reportType] {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": fmt.Sprintf("unknown report type %q; supported: work, personal, gemini, combined", reportType),
		})
		return
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		cfg = config.DefaultConfig()
	}
	if cfg.TelemetryDBPath == "" && h.telemetryDBPath != "" {
		cfg.TelemetryDBPath = h.telemetryDBPath
	}

	var rangeOpts []reporting.DateRangeOptions
	startParam := strings.TrimSpace(r.URL.Query().Get("start"))
	endParam := strings.TrimSpace(r.URL.Query().Get("end"))
	if startParam != "" || endParam != "" {
		rangeOpts = append(rangeOpts, reporting.DateRangeOptions{
			Since: startParam,
			Until: endParam,
		})
	}

	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "html" {
		htmlContent, err := reporting.GenerateReportHTML(reportType, cfg, rangeOpts...)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(htmlContent))
		return
	}

	tmpFile, err := os.CreateTemp("", "staypoint-report-*.pdf")
	if err != nil {
		http.Error(w, `{"error":"failed to create temp file"}`, http.StatusInternalServerError)
		return
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpPath)

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	if err := reporting.RenderReport(ctx, reportType, cfg, tmpPath, rangeOpts...); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	pdfData, err := os.ReadFile(tmpPath)
	if err != nil {
		http.Error(w, `{"error":"failed to read generated PDF"}`, http.StatusInternalServerError)
		return
	}

	filename := reporting.DefaultReportFilename(reportType, cfg)
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(pdfData)))
	_, _ = w.Write(pdfData)
}

// PostFleetTaskComment proxies POST /api/fleet/tasks/{id}/comments to Paperclip.
func (h *TelemetryHandler) PostFleetTaskComment(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, `{"error":"missing task id"}`, http.StatusBadRequest)
		return
	}
	target := paperclipAPIBase() + "/api/issues/" + id + "/comments"
	req, err := http.NewRequestWithContext(r.Context(), "POST", target, r.Body)
	if err != nil {
		http.Error(w, `{"error":"proxy request build failed"}`, http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if key := os.Getenv("PAPERCLIP_API_KEY"); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, `{"error":"paperclip unreachable"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// GetFleetTaskComments proxies GET /api/fleet/tasks/{id}/comments to Paperclip.
func (h *TelemetryHandler) GetFleetTaskComments(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, `{"error":"missing task id"}`, http.StatusBadRequest)
		return
	}
	// Paperclip returns a JSON array; wrap it for consistent client-side handling.
	target := paperclipAPIBase() + "/api/issues/" + id + "/comments"
	req, err := http.NewRequestWithContext(r.Context(), "GET", target, nil)
	if err != nil {
		http.Error(w, `{"comments":[]}`, http.StatusOK)
		return
	}
	if key := os.Getenv("PAPERCLIP_API_KEY"); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"comments":[]}`))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"comments":[]}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]json.RawMessage{"comments": raw})
}

