package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/paperclip"
	"github.com/VinnyVanGogh/staypoint/internal/paperclipimport"
	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/VinnyVanGogh/staypoint/internal/telemetry"
	"github.com/VinnyVanGogh/staypoint/internal/telemetry/quota"
	_ "modernc.org/sqlite"
)

// Aggregator gathers multi-organization telemetry, task status, agent counts, and quota gauges.
type Aggregator struct {
	DB              *sql.DB
	TelemetryDBPath string
	PaperclipClient *paperclip.Client
	RateLimitsPath  string
	Now             func() time.Time
	// LiveRuns returns the tasks whose run is in flight, with when it
	// started (the daemon passes orchestrator.GlobalRunSlots.LiveRuns). Nil,
	// as in the CLI, which cannot see the daemon's runs, falls back to
	// "checkout_run_id is set".
	LiveRuns func() map[string]time.Time
}

// NewAggregator creates an Aggregator with sensible system defaults.
func NewAggregator(db *sql.DB, telemetryDBPath string, pclipClient *paperclip.Client) *Aggregator {
	home, _ := os.UserHomeDir()
	if telemetryDBPath == "" {
		telemetryDBPath = filepath.Join(home, ".config", "token-telemetry", "telemetry.db")
	}
	rateLimitsPath := filepath.Join(home, ".config", "rate-limits", "state.json")

	if pclipClient == nil {
		pclipClient = paperclip.NewClient("", "")
	}

	return &Aggregator{
		DB:              db,
		TelemetryDBPath: telemetryDBPath,
		PaperclipClient: pclipClient,
		RateLimitsPath:  rateLimitsPath,
		Now:             time.Now,
	}
}

// GatherOptions tunes Gather. The zero value is the default every surface
// uses: legacy tasks and archived Paperclip imports are left out of tasks,
// counts, projects and spend.
type GatherOptions struct {
	// IncludeHidden keeps legacy tasks and archived imports (the
	// include_legacy / include_archive switch).
	IncludeHidden bool
}

// Gather builds the complete FleetOverview across all organizations, hiding
// legacy tasks and archived imports.
func (a *Aggregator) Gather(ctx context.Context) (*FleetOverview, error) {
	return a.GatherWith(ctx, GatherOptions{})
}

// GatherWith is Gather with options.
func (a *Aggregator) GatherWith(ctx context.Context, opts GatherOptions) (*FleetOverview, error) {
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}

	overview := &FleetOverview{
		Timestamp:     now,
		Organizations: []OrgFleetSummary{},
		GlobalAgents: GlobalAgentMetrics{
			ByProvider:     make(map[string]int),
			ByOrganization: make(map[string]int),
			Items:          []AgentItem{},
		},
		ProviderQuotas: make(map[string]*ProviderQuotaGauge),
		Tasks:          []TaskItem{},
	}

	// 1. Gather Provider Quotas & 5-Hour Rolling Lockout Gauges
	a.gatherProviderQuotas(overview, now)

	// 2. Gather Organizations, Tasks, and Active Running Agents
	a.gatherOrgsAndTasks(ctx, overview, now, opts.IncludeHidden)

	// 3. Gather Token Telemetry and Cost Accounting
	a.gatherTokenTelemetry(overview)

	// 4. Calculate Individual Organization Rolling Quotas & Lockout Indicators
	a.populateOrgQuotas(overview, now)

	return overview, nil
}

// gatherProviderQuotas populates visual gauges for Gemini, Claude, and OpenAI.
func (a *Aggregator) gatherProviderQuotas(overview *FleetOverview, now time.Time) {
	// Initialize default gauges for the 3 major providers
	overview.ProviderQuotas["gemini"] = &ProviderQuotaGauge{
		Provider:             "gemini",
		DisplayName:          "Google Gemini",
		FiveHourRemainingPct: 100.0,
		FiveHourUsedPct:      0.0,
		WeeklyRemainingPct:   100.0,
		WeeklyUsedPct:        0.0,
		LockoutThresholdPct:  100.0,
		BurnRate5h:           1.50,
		BurnRateWeekly:       0.40,
		ProjectionStatus:     "on_track",
		ProjectionMessage:    "On Track: healthy 5-hour headroom",
		RunwayTurns:          66,
	}
	overview.ProviderQuotas["claude"] = &ProviderQuotaGauge{
		Provider:             "claude",
		DisplayName:          "Anthropic Claude",
		FiveHourRemainingPct: 100.0,
		FiveHourUsedPct:      0.0,
		WeeklyRemainingPct:   100.0,
		WeeklyUsedPct:        0.0,
		LockoutThresholdPct:  100.0,
		BurnRate5h:           5.62,
		BurnRateWeekly:       1.26,
		ProjectionStatus:     "on_track",
		ProjectionMessage:    "On Track: healthy 5-hour headroom",
		RunwayTurns:          18,
	}
	overview.ProviderQuotas["claude_work"] = &ProviderQuotaGauge{
		Provider:             "claude_work",
		DisplayName:          "Claude (Work)",
		FiveHourRemainingPct: 100.0,
		FiveHourUsedPct:      0.0,
		WeeklyRemainingPct:   100.0,
		WeeklyUsedPct:        0.0,
		LockoutThresholdPct:  100.0,
		BurnRate5h:           5.62,
		BurnRateWeekly:       1.26,
		ProjectionStatus:     "on_track",
		ProjectionMessage:    "On Track: healthy 5-hour headroom",
		RunwayTurns:          18,
	}
	overview.ProviderQuotas["claude_personal"] = &ProviderQuotaGauge{
		Provider:             "claude_personal",
		DisplayName:          "Claude (Personal)",
		FiveHourRemainingPct: 100.0,
		FiveHourUsedPct:      0.0,
		WeeklyRemainingPct:   100.0,
		WeeklyUsedPct:        0.0,
		LockoutThresholdPct:  100.0,
		BurnRate5h:           5.62,
		BurnRateWeekly:       1.26,
		ProjectionStatus:     "on_track",
		ProjectionMessage:    "On Track: healthy 5-hour headroom",
		RunwayTurns:          18,
	}
	overview.ProviderQuotas["openai"] = &ProviderQuotaGauge{
		Provider:             "openai",
		DisplayName:          "OpenAI / Codex",
		FiveHourRemainingPct: 100.0,
		FiveHourUsedPct:      0.0,
		WeeklyRemainingPct:   100.0,
		WeeklyUsedPct:        0.0,
		LockoutThresholdPct:  100.0,
		BurnRate5h:           4.00,
		BurnRateWeekly:       1.00,
		ProjectionStatus:     "on_track",
		ProjectionMessage:    "On Track: healthy 5-hour headroom",
		RunwayTurns:          25,
	}

	// 1a. Load from PacerState if available. Each seat's card comes from its
	// own pool; a window only counts as measured when the pool reported it.
	if pacerState, err := router.LoadPacerState(); err == nil && pacerState != nil {
		applyPoolToGauge(overview.ProviderQuotas["gemini"], pacerState.Pools[router.PoolGeminiNative])
		applyPoolToGauge(overview.ProviderQuotas["claude_work"], pacerState.Pools[router.PoolWorkClaude])
		applyPoolToGauge(overview.ProviderQuotas["claude_personal"], pacerState.Pools[router.PoolPersonalClaude])

		// Claude pool: prefer personal / 3p (backward-compat aggregate gauge)
		claudePool := pacerState.Pools[router.PoolPersonalClaude]
		if claudePool == nil || (!claudePool.FiveHour.Known && !claudePool.Weekly.Known) {
			claudePool = pacerState.Pools[router.PoolWorkClaude]
		}
		if claudePool == nil || (!claudePool.FiveHour.Known && !claudePool.Weekly.Known) {
			claudePool = pacerState.Pools[router.Pool3PClaude]
		}
		applyPoolToGauge(overview.ProviderQuotas["claude"], claudePool)
	}

	// 1b. Check SQLite quota_windows table directly. Each Claude seat writes
	// its own pool key (claude_personal, claude_work). The bare legacy
	// "claude" key is written by the personal seat for older readers; it only
	// backfills the personal card when no claude_personal rows exist. Rows
	// older than quota.StaleAfter are ignored: a seat whose fetch keeps
	// failing must read "no data", not its last good number.
	if a.DB != nil {
		a.applyQuotaWindowRows(overview, now)
	}

	// 1c. Overlay from state.json if present
	if a.RateLimitsPath != "" {
		applyRateLimitsState(a.RateLimitsPath, overview, now)
	}

	// 1d. Calculate Projection Status and Burn Pacing Indicators
	for _, g := range overview.ProviderQuotas {
		g.Measured = g.FiveHourMeasured || g.WeeklyMeasured || g.IsLocked
		if !g.Measured {
			// Nothing reported this provider/seat. The defaults above are
			// placeholders, not readings; never present them as "0% used".
			g.ProjectionStatus = "unknown"
			g.ProjectionMessage = "No data: quota not measured"
			g.RunwayTurns = 0
			continue
		}
		if g.IsLocked || (g.FiveHourMeasured && (g.FiveHourRemainingPct <= 0.0 || g.FiveHourUsedPct >= 100.0)) {
			g.IsLocked = true
			g.ProjectionStatus = "locked_out"
			if g.FiveHourResetsAt != nil && g.FiveHourResetsAt.After(now) {
				g.ProjectionMessage = fmt.Sprintf("Locked out: resets in %s", formatDuration(g.FiveHourResetsAt.Sub(now)))
			} else {
				g.ProjectionMessage = "Locked out: quota threshold exceeded (100% burn)"
			}
			g.RunwayTurns = 0
		} else if g.FiveHourRemainingPct < 15.0 {
			g.ProjectionStatus = "overpaced"
			g.ProjectionMessage = fmt.Sprintf("Critical Warning: only %.1f%% 5-hour quota remaining", g.FiveHourRemainingPct)
		} else if g.WeeklyRemainingPct < 20.0 && g.WeeklyUsedPct > 80.0 {
			g.ProjectionStatus = "overpaced"
			g.ProjectionMessage = "Overpaced: current burn on track to exhaust weekly quota before reset"
		} else {
			g.ProjectionStatus = "on_track"
			if g.RunwayTurns > 0 {
				g.ProjectionMessage = fmt.Sprintf("On Track: burn rate sustainable (%d runway turns)", g.RunwayTurns)
			} else {
				g.ProjectionMessage = "On Track: healthy 5-hour headroom"
			}
		}
	}
}

// applyPoolToGauge copies a pacer pool's readings onto a dashboard gauge.
func applyPoolToGauge(g *ProviderQuotaGauge, pool *router.QuotaPool) {
	if g == nil || pool == nil {
		return
	}
	if pool.FiveHour.Known {
		g.FiveHourUsedPct = pool.FiveHour.UsedPct
		g.FiveHourRemainingPct = pool.FiveHour.RemainingPct
		g.FiveHourMeasured = true
		if !pool.FiveHour.ResetsAt.IsZero() {
			t := pool.FiveHour.ResetsAt
			g.FiveHourResetsAt = &t
		}
	}
	if pool.Weekly.Known {
		g.WeeklyUsedPct = pool.Weekly.UsedPct
		g.WeeklyRemainingPct = pool.Weekly.RemainingPct
		g.WeeklyMeasured = true
		if !pool.Weekly.ResetsAt.IsZero() {
			t := pool.Weekly.ResetsAt
			g.WeeklyResetsAt = &t
		}
	}
	g.BurnRate5h = pool.BurnRate5h
	g.BurnRateWeekly = pool.BurnRateW
	g.IsLocked = pool.IsLocked
	g.LockoutReason = pool.LockoutReason
	if !pool.LockoutUntil.IsZero() {
		t := pool.LockoutUntil
		g.LockoutUntil = &t
	}
	g.RunwayTurns = pool.TurnsRunway
	if pool.LastUpdated.After(g.measuredAt) {
		g.measuredAt = pool.LastUpdated
	}
}

type quotaWindowRow struct {
	poolKey, winType string
	usedPct, remPct  float64
	locked           bool
	resetsAt         *time.Time
	updatedAt        time.Time
}

func parseQuotaTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// applyQuotaWindowRows overlays fresh quota_windows rows onto the gauges.
func (a *Aggregator) applyQuotaWindowRows(overview *FleetOverview, now time.Time) {
	rows, err := a.DB.Query(`
		SELECT pool_key, window_type, used_percent, remaining_pct, is_locked, resets_at, updated_at
		FROM quota_windows;
	`)
	if err != nil {
		return
	}
	defer rows.Close()
	var fresh []quotaWindowRow
	hasPersonal := false
	for rows.Next() {
		var r quotaWindowRow
		var isLockedInt int
		var resetsAt, updatedAt sql.NullString
		if err := rows.Scan(&r.poolKey, &r.winType, &r.usedPct, &r.remPct, &isLockedInt, &resetsAt, &updatedAt); err != nil {
			continue
		}
		r.poolKey = strings.ToLower(r.poolKey)
		r.locked = isLockedInt != 0
		if t, ok := parseQuotaTime(resetsAt.String); ok {
			r.resetsAt = &t
		}
		if t, ok := parseQuotaTime(updatedAt.String); ok {
			if now.Sub(t) > quota.StaleAfter {
				continue
			}
			r.updatedAt = t
		}
		if r.poolKey == quota.ProviderClaudePersonal {
			hasPersonal = true
		}
		fresh = append(fresh, r)
	}

	for _, r := range fresh {
		key := r.poolKey
		var gauges []*ProviderQuotaGauge
		switch {
		case strings.Contains(key, "gemini"):
			gauges = append(gauges, overview.ProviderQuotas["gemini"])
		case strings.Contains(key, "work") && strings.Contains(key, "claude"):
			gauges = append(gauges, overview.ProviderQuotas["claude_work"])
		case (strings.Contains(key, "personal") || strings.Contains(key, "3p")) && strings.Contains(key, "claude"):
			gauges = append(gauges, overview.ProviderQuotas["claude_personal"])
		case strings.Contains(key, "claude"):
			gauges = append(gauges, overview.ProviderQuotas["claude"])
			if !hasPersonal {
				gauges = append(gauges, overview.ProviderQuotas["claude_personal"])
			}
		case strings.Contains(key, "codex") || strings.Contains(key, "openai"):
			gauges = append(gauges, overview.ProviderQuotas["openai"])
		}
		for _, gauge := range gauges {
			if gauge == nil {
				continue
			}
			switch r.winType {
			case quota.WindowFiveHour:
				gauge.FiveHourUsedPct = r.usedPct
				gauge.FiveHourRemainingPct = r.remPct
				gauge.FiveHourResetsAt = r.resetsAt
				gauge.FiveHourMeasured = true
				gauge.IsLocked = r.locked
				if r.locked {
					gauge.LockoutReason = "5-hour quota locked"
					gauge.LockoutUntil = r.resetsAt
				} else {
					gauge.LockoutReason = ""
					gauge.LockoutUntil = nil
				}
			case quota.WindowWeekly:
				gauge.WeeklyUsedPct = r.usedPct
				gauge.WeeklyRemainingPct = r.remPct
				gauge.WeeklyResetsAt = r.resetsAt
				gauge.WeeklyMeasured = true
			default:
				continue
			}
			if r.updatedAt.After(gauge.measuredAt) {
				gauge.measuredAt = r.updatedAt
			}
		}
	}
}

// applyRateLimitsState overlays ~/.config/rate-limits/state.json (written by
// the external notifier/poller). Fields it omits stay unmeasured, an entry
// older than the gauge's live reading is skipped, and the generic "Claude"
// key (whichever seat's statusline ran last) only backfills the personal card
// when no seat-specific personal entry exists (STA-283).
func applyRateLimitsState(path string, overview *FleetOverview, now time.Time) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var stateData struct {
		Quotas map[string]struct {
			FiveHourRemaining *float64 `json:"five_hour_remaining"`
			FiveHourUsed      *float64 `json:"five_hour_used"`
			FiveHourResetsAt  float64  `json:"five_hour_resets_at"`
			WeeklyRemaining   *float64 `json:"weekly_remaining"`
			WeeklyUsed        *float64 `json:"weekly_used"`
			WeeklyResetsAt    float64  `json:"weekly_resets_at"`
			LastUpdated       string   `json:"last_updated"`
		} `json:"quotas"`
		Lockouts map[string]struct {
			Locked       bool   `json:"locked"`
			ResetsAt     int64  `json:"resets_at"`
			ResetTimeStr string `json:"reset_time_str"`
		} `json:"lockouts"`
	}
	if json.Unmarshal(data, &stateData) != nil {
		return
	}
	hasPersonal := false
	for name := range stateData.Quotas {
		lower := strings.ToLower(name)
		if strings.Contains(lower, "personal") && strings.Contains(lower, "claude") {
			hasPersonal = true
		}
	}
	gaugesFor := func(name string) []*ProviderQuotaGauge {
		lower := strings.ToLower(name)
		switch {
		case strings.Contains(lower, "gemini"):
			return []*ProviderQuotaGauge{overview.ProviderQuotas["gemini"]}
		case strings.Contains(lower, "work") && strings.Contains(lower, "claude"):
			return []*ProviderQuotaGauge{overview.ProviderQuotas["claude_work"]}
		case (strings.Contains(lower, "personal") || strings.Contains(lower, "3p")) && strings.Contains(lower, "claude"):
			return []*ProviderQuotaGauge{overview.ProviderQuotas["claude_personal"], overview.ProviderQuotas["claude"]}
		case strings.Contains(lower, "claude"):
			if hasPersonal {
				return nil
			}
			return []*ProviderQuotaGauge{overview.ProviderQuotas["claude"], overview.ProviderQuotas["claude_personal"]}
		case strings.Contains(lower, "openai") || strings.Contains(lower, "codex"):
			return []*ProviderQuotaGauge{overview.ProviderQuotas["openai"]}
		}
		return nil
	}

	for qName, qVal := range stateData.Quotas {
		updated, hasUpdated := parseQuotaTime(qVal.LastUpdated)
		for _, g := range gaugesFor(qName) {
			if g == nil {
				continue
			}
			if hasUpdated && !g.measuredAt.IsZero() && updated.Before(g.measuredAt) {
				continue
			}
			if qVal.FiveHourUsed != nil || qVal.FiveHourRemaining != nil {
				used, rem := pctPair(qVal.FiveHourUsed, qVal.FiveHourRemaining)
				g.FiveHourUsedPct, g.FiveHourRemainingPct, g.FiveHourMeasured = used, rem, true
				if qVal.FiveHourResetsAt > 0 {
					t := time.Unix(int64(qVal.FiveHourResetsAt), 0).UTC()
					g.FiveHourResetsAt = &t
				}
			}
			if qVal.WeeklyUsed != nil || qVal.WeeklyRemaining != nil {
				used, rem := pctPair(qVal.WeeklyUsed, qVal.WeeklyRemaining)
				g.WeeklyUsedPct, g.WeeklyRemainingPct, g.WeeklyMeasured = used, rem, true
				if qVal.WeeklyResetsAt > 0 {
					t := time.Unix(int64(qVal.WeeklyResetsAt), 0).UTC()
					g.WeeklyResetsAt = &t
				}
			}
			if hasUpdated && updated.After(g.measuredAt) {
				g.measuredAt = updated
			}
		}
	}
	for lName, lVal := range stateData.Lockouts {
		if !lVal.Locked || (lVal.ResetsAt > 0 && !time.Unix(lVal.ResetsAt, 0).After(now)) {
			continue // not locked, or the lockout already expired
		}
		for _, g := range gaugesFor(lName) {
			if g == nil {
				continue
			}
			g.IsLocked = true
			g.LockoutReason = "Rate limit lockout triggered"
			if lVal.ResetsAt > 0 {
				t := time.Unix(lVal.ResetsAt, 0).UTC()
				g.LockoutUntil = &t
			}
		}
	}
}

// pctPair fills in whichever of used/remaining is missing from the other.
func pctPair(used, remaining *float64) (float64, float64) {
	switch {
	case used != nil && remaining != nil:
		return *used, *remaining
	case used != nil:
		return *used, math.Max(0, 100-*used)
	default:
		return math.Max(0, 100-*remaining), *remaining
	}
}

// gatherOrgsAndTasks consolidates organizations, tasks, and agent sessions across StayPoint and Paperclip.
func (a *Aggregator) gatherOrgsAndTasks(ctx context.Context, overview *FleetOverview, now time.Time, includeHidden bool) {
	orgMap := make(map[string]*OrgFleetSummary)

	ensureOrg := func(name, id, prefix string) *OrgFleetSummary {
		if name == "" {
			name = "StayPoint"
		}
		if s, ok := orgMap[name]; ok {
			if id != "" && s.ID == "" {
				s.ID = id
			}
			if prefix != "" && s.IssuePrefix == "" {
				s.IssuePrefix = prefix
			}
			return s
		}
		if prefix == "" {
			prefix = derivePrefix(name)
		}
		s := &OrgFleetSummary{
			ID:                     id,
			Name:                   name,
			IssuePrefix:            prefix,
			ActiveAgentsByProvider: make(map[string]int),
			Tasks:                  []TaskItem{},
			Agents:                 []AgentItem{},
		}
		orgMap[name] = s
		return s
	}

	// Paperclip issues are added after local tasks, so a local task (an
	// imported copy, or a task that shares a title) wins over its frozen
	// Paperclip record.
	var paperclipItems []TaskItem
	localIDs, localTitles := map[string]bool{}, map[string]bool{}

	// live reports whether a task's run is in flight, and since when.
	var liveRuns map[string]time.Time
	if a.LiveRuns != nil {
		liveRuns = a.LiveRuns()
	}
	live := func(taskID, checkoutRunID string) (string, bool) {
		if checkoutRunID == "" {
			return "", false
		}
		if a.LiveRuns == nil {
			return "", true
		}
		since, ok := liveRuns[taskID]
		if !ok {
			return "", false
		}
		if since.IsZero() {
			return "", true
		}
		return since.UTC().Format(time.RFC3339Nano), true
	}

	// 2a. Query Paperclip companies if client is usable.
	// Projects, issues, and agents are fetched in parallel per company to avoid
	// sequential HTTP round-trips that previously added ≈3–4 s to every boot.
	if a.PaperclipClient != nil {
		if companies, err := a.PaperclipClient.ListCompanies(ctx); err == nil && len(companies) > 0 {
			type companyFetch struct {
				company paperclip.CompanyResponse
				projs   []paperclip.ProjectResponse
				issues  []paperclip.IssueResponse
				agents  []paperclip.AgentResponse
			}
			fetched := make([]companyFetch, len(companies))
			var wg sync.WaitGroup
			for i, c := range companies {
				fetched[i].company = c
				wg.Add(3)
				go func(i int, cid string) {
					defer wg.Done()
					projs, _ := a.PaperclipClient.ListProjects(ctx, cid)
					fetched[i].projs = projs
				}(i, c.ID)
				go func(i int, cid string) {
					defer wg.Done()
					issues, _ := a.PaperclipClient.ListActiveIssues(ctx, cid)
					fetched[i].issues = issues
				}(i, c.ID)
				go func(i int, cid string) {
					defer wg.Done()
					agents, _ := a.PaperclipClient.ListAgents(ctx, cid)
					fetched[i].agents = agents
				}(i, c.ID)
			}
			wg.Wait()

			for _, cf := range fetched {
				c := cf.company
				// The import mapped each company to an organization already
				// in use ("RuneLite Plugins" -> "RuneLite"); list it under
				// that one name, not both (task-3387cad2).
				orgName := paperclipimport.OrganizationFor(c)
				orgSummary := ensureOrg(orgName, c.ID, c.IssuePrefix)

				projMap := make(map[string]string)
				for _, p := range cf.projs {
					projMap[p.ID] = p.Name
					orgSummary.Projects = append(orgSummary.Projects, p.Name)
				}

				for _, iss := range cf.issues {
					projectName := ""
					if iss.ProjectID != "" {
						if pName, ok := projMap[iss.ProjectID]; ok {
							projectName = pName
						}
					}
					tItem := TaskItem{
						ID:              iss.ID,
						Identifier:      iss.Identifier,
						Title:           iss.Title,
						Description:     iss.Description,
						Organization:    orgName,
						Project:         projectName,
						AssigneeAgentID: iss.AssigneeAgentID,
						CheckoutAgentID: iss.CheckoutAgentID,
						Priority:        iss.Priority,
						ParentID:        iss.ParentID,
						UpdatedAt:       now,
						// Paperclip is frozen (read-only record), so its live
						// issues are legacy: lists hide them behind "Show
						// archive & legacy" like imported legacy tasks.
						Origin: "legacy",
					}
					// Paperclip is frozen: none of its issues has a run in
					// flight, whatever status it was left in.
					switch iss.Status {
					case "in_progress", "running":
						tItem.Status, tItem.ExecutionStage = "active", "in_progress"
					case "blocked":
						tItem.Status, tItem.ExecutionStage = "blocked", "blocked"
						tItem.IsBlocked = true
					case "stopped", "paused", "cancelled":
						tItem.Status, tItem.ExecutionStage = "stopped", "cancelled"
					case "done", "closed":
						tItem.Status, tItem.ExecutionStage = "done", "done"
					case "error", "failed":
						tItem.Status = "errored"
					case "backlog":
						tItem.Status, tItem.ExecutionStage = "active", "backlog"
					case "in_review":
						tItem.Status, tItem.ExecutionStage = "active", "in_review"
					default:
						tItem.Status, tItem.ExecutionStage = "active", "todo"
					}
					paperclipItems = append(paperclipItems, tItem)
				}

				for _, ag := range cf.agents {
					model := extractConfigString(ag.RuntimeConfig, "model", "defaultModel", "default_model")
					if model == "" {
						model = extractConfigString(ag.AdapterConfig, "model", "defaultModel", "default_model")
					}
					if model == "" {
						model = ag.Model
					}
					provider := ResolveAgentProvider(ag.AdapterType, ag.AdapterConfig, ag.RuntimeConfig, model, ag.Name, ag.Role, ag.Title)

					status := ag.Status
					if status == "" {
						status = "active"
					}

					hb := now
					if ag.LastHeartbeatAt != "" {
						if t, err := time.Parse(time.RFC3339Nano, ag.LastHeartbeatAt); err == nil {
							hb = t
						} else if t, err := time.Parse(time.RFC3339, ag.LastHeartbeatAt); err == nil {
							hb = t
						}
					}

					quota := getProviderQuotaGauge(overview.ProviderQuotas, provider, c.Name)

					aItem := AgentItem{
						ID:            ag.ID,
						Name:          ag.Name,
						Role:          ag.Role,
						Organization:  c.Name,
						Provider:      provider,
						Model:         model,
						Status:        status,
						LastHeartbeat: hb,
						Quota:         quota,
					}
					orgSummary.ActiveAgents++
					orgSummary.ActiveAgentsByProvider[provider]++
					overview.GlobalAgents.Total++
					overview.GlobalAgents.ActiveRunning++
					overview.GlobalAgents.ByProvider[provider]++
					overview.GlobalAgents.ByOrganization[c.Name]++
					orgSummary.Agents = append(orgSummary.Agents, aItem)
					overview.GlobalAgents.Items = append(overview.GlobalAgents.Items, aItem)
				}
			}
		}
	}

	// 2b. Query StayPoint local database tasks
	if a.DB != nil {
		localComments := make(map[string][]string)
		if cRows, err := a.DB.Query(`SELECT task_id, message FROM task_comments ORDER BY created_at ASC;`); err == nil {
			defer cRows.Close()
			for cRows.Next() {
				var tid, msg string
				if err := cRows.Scan(&tid, &msg); err == nil && msg != "" {
					localComments[tid] = append(localComments[tid], msg)
				}
			}
		}

		localDocs := make(map[string]string)
		if dRows, err := a.DB.Query(`SELECT task_id, content FROM task_documents WHERE doc_key = 'description' ORDER BY version DESC;`); err == nil {
			defer dRows.Close()
			for dRows.Next() {
				var tid, content string
				if err := dRows.Scan(&tid, &content); err == nil && content != "" {
					if _, exists := localDocs[tid]; !exists {
						localDocs[tid] = content
					}
				}
			}
		}

		// Legacy tasks and archived imports are left out of every count,
		// project list and spend total unless includeHidden.
		tRows, err := a.DB.Query(`
			SELECT id, name, COALESCE(organization, ''), COALESCE(project, ''),
			       status, execution_stage, is_blocked, COALESCE(block_reason, ''),
			       spent_usd, spent_tokens, updated_at, COALESCE(parent_id, ''),
			       COALESCE(checkout_agent_id, ''), COALESCE(origin, 'native'),
			       COALESCE(checkout_run_id, '')
			FROM tasks
			WHERE status != 'soft_deleted' AND ` + meshContext.VisibleTasksSQL("", includeHidden) + `;
		`)
		if err == nil {
			defer tRows.Close()
			for tRows.Next() {
				var id, name, org, proj, st, stage, bReason, upAt, parentID, checkoutAgentID, origin, checkoutRunID string
				var isBlockedInt int
				var spentUSD float64
				var spentTokens int64
				if err := tRows.Scan(&id, &name, &org, &proj, &st, &stage, &isBlockedInt, &bReason, &spentUSD, &spentTokens, &upAt, &parentID, &checkoutAgentID, &origin, &checkoutRunID); err == nil {
					if org == "" {
						org = "StayPoint"
					}
					orgSummary := ensureOrg(org, "", "")

					if proj != "" && proj != "(No Project)" {
						foundProj := false
						for _, ep := range orgSummary.Projects {
							if ep == proj {
								foundProj = true
								break
							}
						}
						if !foundProj {
							orgSummary.Projects = append(orgSummary.Projects, proj)
						}
					}

					var parsedUp time.Time
					if t, err := time.Parse(time.RFC3339Nano, upAt); err == nil {
						parsedUp = t
					} else {
						parsedUp = now
					}

					// "running" means a run is in flight, never just stage
					// in_progress: a stopped or crashed run leaves that behind.
					runStart, running := live(id, checkoutRunID)
					taskStatus := "active"
					switch {
					case isBlockedInt != 0 || stage == "blocked":
						taskStatus = "blocked"
					case running:
						taskStatus = "running"
					case stage == "error" || stage == "failed" || st == "error":
						taskStatus = "errored"
					case st == "done" || stage == "done":
						taskStatus = "done"
					case st == "stopped" || st == "cancelled" || stage == "cancelled":
						taskStatus = "stopped"
					}

					orgSummary.SpentUSD += spentUSD
					orgSummary.SpentTokens += spentTokens

					desc := localDocs[id]
					cmts := localComments[id]
					if desc == "" && len(cmts) > 0 {
						desc = cmts[0]
					}

					item := TaskItem{
						ID:              id,
						Identifier:      fmt.Sprintf("%s-%s", orgSummary.IssuePrefix, shortID(id)),
						Title:           name,
						Description:     desc,
						Comments:        cmts,
						Organization:    org,
						Project:         proj,
						ParentID:        parentID,
						Status:          taskStatus,
						ExecutionStage:  stage,
						SpentUSD:        spentUSD,
						SpentTokens:     spentTokens,
						IsBlocked:       isBlockedInt != 0,
						BlockReason:     normalizeBlockReason(bReason),
						UpdatedAt:       parsedUp,
						CheckoutAgentID: checkoutAgentID,
						Origin:          origin,
						Running:         running,
						RunStartedAt:    runStart,
					}
					orgSummary.Tasks = append(orgSummary.Tasks, item)
					overview.Tasks = append(overview.Tasks, item)
					localIDs[id] = true
					localTitles[name] = true
				}
			}
		}

		// 2c. Query local agent sessions
		sRows, err := a.DB.Query(`
			SELECT id, agent_type, status, repo_path, last_heartbeat_at
			FROM agent_sessions
			WHERE status = 'active';
		`)
		if err == nil {
			defer sRows.Close()
			for sRows.Next() {
				var id, agType, st, repoPath, hbStr string
				if err := sRows.Scan(&id, &agType, &st, &repoPath, &hbStr); err == nil {
					provider := normalizeProvider(agType)
					org := "StayPoint"
					if strings.Contains(repoPath, "mansol") || strings.Contains(repoPath, "managed") {
						org = "Managed Solution"
					}
					orgSummary := ensureOrg(org, "", "")

					// Check deduplication
					found := false
					for _, ex := range orgSummary.Agents {
						if ex.ID == id {
							found = true
							break
						}
					}
					if !found {
						var hb time.Time
						if t, err := time.Parse(time.RFC3339Nano, hbStr); err == nil {
							hb = t
						} else {
							hb = now
						}
						quota := getProviderQuotaGauge(overview.ProviderQuotas, provider, org)
						aItem := AgentItem{
							ID:            id,
							Name:          fmt.Sprintf("%s Session (%s)", titleCase(provider), shortID(id)),
							Role:          "Local Agent",
							Organization:  org,
							Provider:      provider,
							Status:        st,
							LastHeartbeat: hb,
							Quota:         quota,
						}
						orgSummary.ActiveAgents++
						orgSummary.ActiveAgentsByProvider[provider]++
						overview.GlobalAgents.Total++
						overview.GlobalAgents.ActiveRunning++
						overview.GlobalAgents.ByProvider[provider]++
						overview.GlobalAgents.ByOrganization[org]++
						orgSummary.Agents = append(orgSummary.Agents, aItem)
						overview.GlobalAgents.Items = append(overview.GlobalAgents.Items, aItem)
					}
				}
			}
		}
	}

	for _, item := range paperclipItems {
		if localIDs[item.ID] || localTitles[item.Title] {
			continue
		}
		orgSummary := ensureOrg(item.Organization, "", "")
		orgSummary.Tasks = append(orgSummary.Tasks, item)
		overview.Tasks = append(overview.Tasks, item)
	}

	// Count once every task is in. Frozen Paperclip issues are legacy, left
	// out of every count unless includeHidden, like local legacy tasks.
	for _, s := range orgMap {
		for _, item := range s.Tasks {
			if item.Origin == "legacy" && !includeHidden {
				continue
			}
			s.TaskCounts.add(item)
			overview.GlobalTasks.add(item)
		}
	}

	// Always ensure primary organizations exist even if empty
	ensureOrg("StayPoint", "601f782a-4293-4e4c-bc02-bcb6ff0a52ca", "STA")
	ensureOrg("Managed Solution", "5c9023d0-383d-4d45-967e-79b8c667b266", "MAN")

	for _, s := range orgMap {
		overview.Organizations = append(overview.Organizations, *s)
	}
	// orgMap order is random; sort so org cards hold their position between
	// refreshes instead of reshuffling on every poll (STA-283).
	sort.Slice(overview.Organizations, func(i, j int) bool {
		return overview.Organizations[i].Name < overview.Organizations[j].Name
	})
}

// gatherTokenTelemetry calculates input/output tokens, cost USD, and breakdowns by model and org.
func (a *Aggregator) gatherTokenTelemetry(overview *FleetOverview) {
	// Attempt connection to telemetry.db (read-only)
	if a.TelemetryDBPath != "" {
		if _, err := os.Stat(a.TelemetryDBPath); err == nil {
			dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(3000)", a.TelemetryDBPath)
			if tConn, err := sql.Open("sqlite", dsn); err == nil {
				defer tConn.Close()

				// Global totals
				var inTok, outTok, crTok, ccTok, totTok int64
				var costUSD sql.NullFloat64
				row := tConn.QueryRow(`
					SELECT
						COALESCE(SUM(input_tokens), 0),
						COALESCE(SUM(output_tokens), 0),
						COALESCE(SUM(cache_read_tokens), 0),
						COALESCE(SUM(cache_creation_tokens), 0),
						COALESCE(SUM(total_tokens), 0),
						SUM(cost_usd)
					FROM requests;
				`)
				if err := row.Scan(&inTok, &outTok, &crTok, &ccTok, &totTok, &costUSD); err == nil {
					overview.TokenTelemetry.InputTokens = inTok
					overview.TokenTelemetry.OutputTokens = outTok
					overview.TokenTelemetry.CacheReadTokens = crTok
					overview.TokenTelemetry.CacheCreationTokens = ccTok
					overview.TokenTelemetry.TotalTokens = totTok
					if costUSD.Valid {
						overview.TokenTelemetry.TotalCostUSD = costUSD.Float64
					}
				}

				// Model breakdown
				mRows, err := tConn.Query(`
					SELECT
						COALESCE(model, 'unknown'),
						COALESCE(model_family, 'other'),
						COALESCE(SUM(input_tokens), 0),
						COALESCE(SUM(output_tokens), 0),
						COALESCE(SUM(cache_read_tokens), 0),
						COALESCE(SUM(cache_creation_tokens), 0),
						COALESCE(SUM(total_tokens), 0),
						COALESCE(SUM(cost_usd), 0.0)
					FROM requests
					GROUP BY model
					ORDER BY SUM(total_tokens) DESC;
				`)
				if err == nil {
					defer mRows.Close()
					var totalCalculatedCost float64
					var breakdowns []ModelSpendBreakdown

					for mRows.Next() {
						var mName, mFam string
						var mIn, mOut, mCR, mCC, mTot int64
						var mCost float64
						if err := mRows.Scan(&mName, &mFam, &mIn, &mOut, &mCR, &mCC, &mTot, &mCost); err == nil {
							// If stored cost_usd is 0/null, compute on the fly using the ratecard
							// (preferred) then fall back to the legacy family estimate.
							if mCost <= 0.0001 {
								if micros, ok := telemetry.ComputeCostMicros(mName, telemetry.Usage{
									Input:           mIn,
									Output:          mOut,
									CacheRead:       mCR,
									CacheCreation5m: mCC,
								}); ok {
									mCost = float64(micros) / 1_000_000.0
								} else {
									mCost = telemetry.EstimateModelCost(mName, mIn, mOut, mCR, mCC)
								}
							}
							totalCalculatedCost += mCost
							breakdowns = append(breakdowns, ModelSpendBreakdown{
								Model:        mName,
								Family:       mFam,
								InputTokens:  mIn,
								OutputTokens: mOut,
								TotalTokens:  mTot,
								CostUSD:      mCost,
							})
						}
					}

					if overview.TokenTelemetry.TotalCostUSD <= 0.0001 && totalCalculatedCost > 0 {
						overview.TokenTelemetry.TotalCostUSD = totalCalculatedCost
					}

					// Calculate percentages
					for i := range breakdowns {
						if overview.TokenTelemetry.TotalCostUSD > 0 {
							breakdowns[i].Percentage = math.Round((breakdowns[i].CostUSD/overview.TokenTelemetry.TotalCostUSD)*1000) / 10
						}
					}
					overview.ModelSpend = breakdowns
				}
			}
		}
	}

	// Spend breakdown across organizations
	var totalOrgCost float64
	for _, org := range overview.Organizations {
		totalOrgCost += org.SpentUSD
	}
	if totalOrgCost <= 0.01 && overview.TokenTelemetry.TotalCostUSD > 0 {
		totalOrgCost = overview.TokenTelemetry.TotalCostUSD
	}

	for _, org := range overview.Organizations {
		cost := org.SpentUSD
		// Default estimation for demo/display if no direct per-task spend recorded yet
		if cost <= 0.0 && totalOrgCost > 0 {
			switch org.IssuePrefix {
			case "STA":
				cost = totalOrgCost * 0.55
			case "MAN":
				cost = totalOrgCost * 0.35
			default:
				cost = totalOrgCost * 0.10
			}
		}
		pct := 0.0
		if totalOrgCost > 0 {
			pct = math.Round((cost/totalOrgCost)*1000) / 10
		}
		tokens := org.SpentTokens
		if tokens == 0 && overview.TokenTelemetry.TotalTokens > 0 {
			tokens = int64(float64(overview.TokenTelemetry.TotalTokens) * (pct / 100.0))
		}
		overview.OrgSpend = append(overview.OrgSpend, OrgSpendBreakdown{
			Organization: org.Name,
			CostUSD:      cost,
			TotalTokens:  tokens,
			InputTokens:  int64(float64(tokens) * 0.4),
			OutputTokens: int64(float64(tokens) * 0.6),
			Percentage:   pct,
			ActiveTasks:  org.TaskCounts.Running + org.TaskCounts.Active,
			ActiveAgents: org.ActiveAgents,
		})
	}
}

func normalizeBlockReason(reason string) string {
	lower := strings.ToLower(strings.TrimSpace(reason))
	if lower == "blocked via tui" || lower == "via tui" || lower == "" {
		return ""
	}
	return reason
}

func detectProviderStr(s string) string {
	lower := strings.ToLower(strings.TrimSpace(s))
	if lower == "" || lower == "other" || lower == "unknown" {
		return ""
	}
	switch {
	case strings.Contains(lower, "gemini") || strings.Contains(lower, "google"):
		return "gemini"
	case strings.Contains(lower, "claude") || strings.Contains(lower, "anthropic") || strings.Contains(lower, "fable"):
		return "claude"
	case strings.Contains(lower, "codex") || strings.Contains(lower, "openai") || strings.Contains(lower, "gpt") || strings.Contains(lower, "o1") || strings.Contains(lower, "o3"):
		return "openai"
	default:
		if strings.HasSuffix(lower, "_local") {
			trimmed := strings.TrimSuffix(lower, "_local")
			if trimmed != "" && trimmed != "other" && trimmed != "unknown" {
				return trimmed
			}
		}
		return ""
	}
}

func extractConfigString(m map[string]interface{}, keys ...string) string {
	if m == nil {
		return ""
	}
	for _, k := range keys {
		if val, ok := m[k]; ok && val != nil {
			if s, ok := val.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// ResolveAgentProvider determines the provider strictly for an agent, never returning "other".
// It inspects runtime config, adapter type (gemini_local, claude_local, codex_local),
// adapter config, model, name, title, and role.
func ResolveAgentProvider(adapterType string, adapterConfig, runtimeConfig map[string]interface{}, model, name, role, title string) string {
	// 1. Inspect runtime config
	if rcProv := extractConfigString(runtimeConfig, "provider", "model", "defaultModel", "default_model", "model_family", "modelFamily", "adapter", "adapter_type"); rcProv != "" {
		if p := detectProviderStr(rcProv); p != "" {
			return p
		}
		if p := extractConfigString(runtimeConfig, "provider"); p != "" && !strings.EqualFold(p, "other") && !strings.EqualFold(p, "unknown") {
			return strings.ToLower(strings.TrimSpace(p))
		}
	}

	// 2. Inspect adapter type
	if adapterType != "" {
		if p := detectProviderStr(adapterType); p != "" {
			return p
		}
		lower := strings.ToLower(strings.TrimSpace(adapterType))
		trimmed := strings.TrimSuffix(lower, "_local")
		if trimmed != "" && trimmed != "other" && trimmed != "unknown" {
			return trimmed
		}
	}

	// 3. Inspect adapter config
	if acProv := extractConfigString(adapterConfig, "provider", "model", "defaultModel", "default_model", "model_family", "modelFamily"); acProv != "" {
		if p := detectProviderStr(acProv); p != "" {
			return p
		}
		if p := extractConfigString(adapterConfig, "provider"); p != "" && !strings.EqualFold(p, "other") && !strings.EqualFold(p, "unknown") {
			return strings.ToLower(strings.TrimSpace(p))
		}
	}

	// 4. Inspect model
	if model != "" {
		if p := detectProviderStr(model); p != "" {
			return p
		}
	}

	// 5. Inspect name, title, and role
	if p := detectProviderStr(name + " " + title + " " + role); p != "" {
		return p
	}

	// Default fallback: Never return "other"
	return "gemini"
}

func getProviderQuotaGauge(quotas map[string]*ProviderQuotaGauge, provider string, orgName ...string) *ProviderQuotaGauge {
	if quotas == nil || provider == "" {
		return nil
	}
	p := strings.ToLower(provider)
	org := ""
	if len(orgName) > 0 {
		org = strings.ToLower(orgName[0])
	}
	isWorkOrg := strings.Contains(org, "managed") || strings.Contains(org, "mansol")

	if strings.Contains(p, "claude") || strings.Contains(p, "anthropic") || strings.Contains(p, "fable") {
		if p == "claude_personal" {
			if q, ok := quotas["claude_personal"]; ok && q != nil {
				return q
			}
		}
		if p == "claude_work" {
			if q, ok := quotas["claude_work"]; ok && q != nil {
				return q
			}
		}
		if isWorkOrg {
			if q, ok := quotas["claude_work"]; ok && q != nil {
				return q
			}
			if q, ok := quotas["claude"]; ok && q != nil {
				return q
			}
			if q, ok := quotas["claude_personal"]; ok && q != nil {
				return q
			}
		} else {
			if q, ok := quotas["claude_personal"]; ok && q != nil {
				return q
			}
			if q, ok := quotas["claude"]; ok && q != nil {
				return q
			}
			if q, ok := quotas["claude_work"]; ok && q != nil {
				return q
			}
		}
	}
	if q, ok := quotas[p]; ok && q != nil {
		return q
	}
	if strings.Contains(p, "gemini") || strings.Contains(p, "google") {
		if q, ok := quotas["gemini"]; ok && q != nil {
			return q
		}
	}
	if strings.Contains(p, "openai") || strings.Contains(p, "codex") || strings.Contains(p, "gpt") {
		if q, ok := quotas["openai"]; ok && q != nil {
			return q
		}
	}
	return quotas["gemini"]
}

func normalizeProvider(s string) string {
	detected := detectProviderStr(s)
	if detected != "" {
		return detected
	}
	lower := strings.ToLower(strings.TrimSpace(s))
	if lower == "" || lower == "other" || lower == "unknown" {
		return "gemini"
	}
	if strings.HasSuffix(lower, "_local") {
		trimmed := strings.TrimSuffix(lower, "_local")
		if trimmed != "" && trimmed != "other" && trimmed != "unknown" {
			return trimmed
		}
	}
	return "gemini"
}

func derivePrefix(name string) string {
	parts := strings.Fields(name)
	if len(parts) >= 2 {
		return strings.ToUpper(string(parts[0][0]) + string(parts[1][0]))
	}
	if len(name) >= 3 {
		return strings.ToUpper(name[:3])
	}
	return "ORG"
}

func shortID(id string) string {
	if len(id) > 6 {
		return id[:6]
	}
	return id
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0m"
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// populateOrgQuotas computes the 5-hour rolling quota and lockout gauges for each individual organization.
func (a *Aggregator) populateOrgQuotas(overview *FleetOverview, now time.Time) {
	for i := range overview.Organizations {
		org := &overview.Organizations[i]
		org.ProviderQuotas = make(map[string]*ProviderQuotaGauge)
		isManagedSol := strings.Contains(strings.ToLower(org.Name), "managed")

		for key, fleetGauge := range overview.ProviderQuotas {
			if fleetGauge == nil {
				continue
			}

			org5hUsed := fleetGauge.FiveHourUsedPct
			org5hRemaining := fleetGauge.FiveHourRemainingPct
			orgWeeklyUsed := fleetGauge.WeeklyUsedPct
			orgWeeklyRemaining := fleetGauge.WeeklyRemainingPct
			burn5h := fleetGauge.BurnRate5h
			burnWeekly := fleetGauge.BurnRateWeekly
			isLocked := fleetGauge.IsLocked
			measured5h, measuredWk := fleetGauge.FiveHourMeasured, fleetGauge.WeeklyMeasured

			// Organization-specific quota pool prioritization:
			// Managed Solution prioritizes claude_work and does not burn personal quota.
			// Personal orgs (StayPoint, etc.) prioritize claude_personal and do not burn work quota.
			if isManagedSol {
				if key == "claude_personal" {
					org5hUsed = 0.0
					org5hRemaining = 100.0
					orgWeeklyUsed = 0.0
					orgWeeklyRemaining = 100.0
					burn5h = 0.0
					burnWeekly = 0.0
					isLocked = false
				} else if key == "claude" {
					if workGauge, ok := overview.ProviderQuotas["claude_work"]; ok && workGauge != nil {
						org5hUsed = workGauge.FiveHourUsedPct
						org5hRemaining = workGauge.FiveHourRemainingPct
						orgWeeklyUsed = workGauge.WeeklyUsedPct
						orgWeeklyRemaining = workGauge.WeeklyRemainingPct
						burn5h = workGauge.BurnRate5h
						burnWeekly = workGauge.BurnRateWeekly
						isLocked = workGauge.IsLocked
						measured5h, measuredWk = workGauge.FiveHourMeasured, workGauge.WeeklyMeasured
					}
				}
			} else {
				if key == "claude_work" {
					org5hUsed = 0.0
					org5hRemaining = 100.0
					orgWeeklyUsed = 0.0
					orgWeeklyRemaining = 100.0
					burn5h = 0.0
					burnWeekly = 0.0
					isLocked = false
				} else if key == "claude" {
					if persGauge, ok := overview.ProviderQuotas["claude_personal"]; ok && persGauge != nil {
						org5hUsed = persGauge.FiveHourUsedPct
						org5hRemaining = persGauge.FiveHourRemainingPct
						orgWeeklyUsed = persGauge.WeeklyUsedPct
						orgWeeklyRemaining = persGauge.WeeklyRemainingPct
						burn5h = persGauge.BurnRate5h
						burnWeekly = persGauge.BurnRateWeekly
						isLocked = persGauge.IsLocked
						measured5h, measuredWk = persGauge.FiveHourMeasured, persGauge.WeeklyMeasured
					}
				}
			}

			orgGauge := &ProviderQuotaGauge{
				Provider:             fleetGauge.Provider,
				DisplayName:          fleetGauge.DisplayName,
				FiveHourUsedPct:      org5hUsed,
				FiveHourRemainingPct: org5hRemaining,
				FiveHourResetsAt:     fleetGauge.FiveHourResetsAt,
				WeeklyUsedPct:        orgWeeklyUsed,
				WeeklyRemainingPct:   orgWeeklyRemaining,
				WeeklyResetsAt:       fleetGauge.WeeklyResetsAt,
				BurnRate5h:           burn5h,
				BurnRateWeekly:       burnWeekly,
				LockoutThresholdPct:  fleetGauge.LockoutThresholdPct,
				IsLocked:             isLocked,
				LockoutReason:        fleetGauge.LockoutReason,
				LockoutUntil:         fleetGauge.LockoutUntil,
				RunwayTurns:          fleetGauge.RunwayTurns,
				FiveHourMeasured:     measured5h,
				WeeklyMeasured:       measuredWk,
			}
			orgGauge.Measured = measured5h || measuredWk || isLocked
			if !orgGauge.Measured {
				orgGauge.ProjectionStatus = "unknown"
				orgGauge.ProjectionMessage = "No data: quota not measured"
				orgGauge.RunwayTurns = 0
				org.ProviderQuotas[key] = orgGauge
				continue
			}

			if orgGauge.IsLocked {
				// Managed Solution is an enterprise work org: check claude_work first before locking claude/personal
				if isManagedSol && (key == "claude_personal" || key == "claude") {
					workGauge := overview.ProviderQuotas["claude_work"]
					if workGauge != nil && !workGauge.IsLocked {
						// Work quota has headroom; do not lock org out on Claude
						orgGauge.IsLocked = false
						orgGauge.ProjectionStatus = "on_track"
						orgGauge.ProjectionMessage = "Claude Work prioritized (healthy headroom)"
						org.ProviderQuotas[key] = orgGauge
						continue
					}
				}
				// Personal org (StayPoint, etc.): check claude_personal before locking on claude_work/claude
				if !isManagedSol && (key == "claude_work" || key == "claude") {
					persGauge := overview.ProviderQuotas["claude_personal"]
					if persGauge != nil && !persGauge.IsLocked {
						// Personal quota has headroom; do not lock org out on Claude
						orgGauge.IsLocked = false
						orgGauge.ProjectionStatus = "on_track"
						orgGauge.ProjectionMessage = "Claude Personal prioritized (healthy headroom)"
						org.ProviderQuotas[key] = orgGauge
						continue
					}
				}
				orgGauge.ProjectionStatus = "locked_out"
				if fleetGauge.FiveHourResetsAt != nil && fleetGauge.FiveHourResetsAt.After(now) {
					orgGauge.ProjectionMessage = fmt.Sprintf("Locked out: resets in %s", formatDuration(fleetGauge.FiveHourResetsAt.Sub(now)))
				} else {
					orgGauge.ProjectionMessage = "Locked out: quota limit reached"
				}
			} else if org5hRemaining < 20.0 {
				orgGauge.ProjectionStatus = "overpaced"
				orgGauge.ProjectionMessage = fmt.Sprintf("High usage: %.1f%% used", org5hUsed)
			} else {
				orgGauge.ProjectionStatus = "on_track"
				if isManagedSol && (key == "claude_work" || key == "claude") {
					orgGauge.ProjectionMessage = "Claude Work prioritized (healthy headroom)"
				} else if !isManagedSol && (key == "claude_personal" || key == "claude") {
					orgGauge.ProjectionMessage = "Claude Personal prioritized (healthy headroom)"
				} else if isManagedSol && key == "claude_personal" {
					orgGauge.ProjectionMessage = "Personal quota retained"
				} else if !isManagedSol && key == "claude_work" {
					orgGauge.ProjectionMessage = "Enterprise seat available"
				} else {
					orgGauge.ProjectionMessage = fmt.Sprintf("Healthy headroom: %.1f%% remaining", org5hRemaining)
				}
			}

			org.ProviderQuotas[key] = orgGauge
		}
	}
}
