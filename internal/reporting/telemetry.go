package reporting

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/config"
	"github.com/VinnyVanGogh/staypoint/internal/telemetry"
	_ "modernc.org/sqlite"
)

type ModelStat struct {
	Name     string
	Turns    string
	TurnsPct string
	Value    string
	Tokens   string
}

type DeliverableItem struct {
	Name        string
	Impact      string
	Value       string
	ActualSpend string
	WorkProduct string
}

type WorkReportData struct {
	CompanyName                 string
	EngineerName                string
	WorkEmail                   string
	HourlyRate                  float64
	AuditPeriod                 string
	SubstantiatedValue          string
	APIListPriceEquivalentValue string
	ActualSpend                 string
	ActualSpendSub              string
	RatecardSource              string
	ROIMultiplier               string
	MonthlyRunRate              string
	AcceptedTurns               string
	MonthlyNetCost              string
	ExpectedROI                 string
	HoursSavedBreakEven         string
	TotalHoursSaved             string
	DirectCostMultiplier        string
	HasHourlyRate               bool
	HasCompanyName              bool
	HasEngineerName             bool
	DBPath                      string
	Deliverables                []DeliverableItem
}

type PersonalReportData struct {
	EngineerName    string
	PersonalEmail   string
	AuditPeriod     string
	TotalRequests   string
	DeliveredValue  string
	NetSurplus      string
	SubscriptionROI string
	// SubscriptionCost is the configured monthly plan price ("<amount>/mo").
	SubscriptionCost string
	TotalTokens      string
	ActiveDays       string
	Models           []ModelStat
	HasEngineerName  bool
}

type GeminiReportData struct {
	EngineerName    string
	AuditPeriod     string
	TotalTokens     string
	InputTokens     string
	OutputTokens    string
	TotalTurns      string
	CodeReviews     string
	UniqueRepos     string
	BrainSessions   string
	SubagentRuns    string
	BugsFound       string
	Models          []ModelStat
	HasEngineerName bool
}

type CombinedReportData struct {
	EngineerName          string
	AuditPeriod           string
	TotalValue            string
	TotalInvocations      string
	TotalTokens           string
	CombinedROI           string
	TotalSubscriptionCost string
	WorkTurns             string
	WorkValue             string
	WorkTokens            string
	PersonalTurns         string
	PersonalValue         string
	PersonalTokens        string
	GeminiTurns           string
	GeminiValue           string
	GeminiTokens          string
	GeminiReviews         string
	GeminiBrains          string
	HasEngineerName       bool
}

// NotMeasured stands in for any report figure the data cannot support. A
// report shows this rather than a placeholder number.
const NotMeasured = "not measured"

// daysPerMonth converts a day span into months for run rates and prorating
// monthly plan prices (365.25 / 12).
const daysPerMonth = 30.4375

// countBrainSessions returns the number of Antigravity brain sessions on
// disk, or 0 when the directory cannot be read.
func countBrainSessions() int {
	home, err := os.UserHomeDir()
	if err != nil {
		return 0
	}
	entries, err := os.ReadDir(filepath.Join(home, ".gemini", "antigravity-cli", "brain"))
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			count++
		}
	}
	return count
}

// usage aggregates the requests rows matching one filter.
type usage struct {
	count                 int64
	cost                  float64
	tokens, input, output int64
	activeDays            int64
	minTs, maxTs          string
}

// months is the span between the first and last request, counted in whole
// days inclusive, expressed in months. Zero when the span is unknown.
func (u usage) months() float64 {
	t1, err1 := time.Parse(time.RFC3339, u.minTs)
	t2, err2 := time.Parse(time.RFC3339, u.maxTs)
	if err1 != nil || err2 != nil {
		return 0
	}
	d1 := time.Date(t1.Year(), t1.Month(), t1.Day(), 0, 0, 0, 0, time.UTC)
	d2 := time.Date(t2.Year(), t2.Month(), t2.Day(), 0, 0, 0, 0, time.UTC)
	days := d2.Sub(d1).Hours()/24 + 1
	return days / daysPerMonth
}

func (u usage) period() string {
	if p := formatPeriod(u.minTs, u.maxTs); p != "" {
		return p
	}
	return NotMeasured
}

// queryUsage sums requests rows matching filter (a SQL boolean over the
// requests table, "1=1" for all) within the date bounds.
func queryUsage(conn *sql.DB, filter string, filterArgs []any, since, until string) usage {
	var u usage
	var minTs, maxTs sql.NullString
	args := append(append([]any{}, filterArgs...), since, since, until, until)
	err := conn.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(cost_usd), 0), COALESCE(SUM(total_tokens), 0),
		       COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		       COUNT(DISTINCT substr(ts, 1, 10)), MIN(ts), MAX(ts)
		FROM requests
		WHERE `+filter+`
		  AND (ts >= ? OR ? = '')
		  AND (ts <= ? OR ? = '')`, args...).Scan(
		&u.count, &u.cost, &u.tokens, &u.input, &u.output, &u.activeDays, &minTs, &maxTs)
	if err != nil {
		return usage{}
	}
	u.minTs, u.maxTs = minTs.String, maxTs.String
	return u
}

// queryModels breaks the matching requests down per model, busiest first.
func queryModels(conn *sql.DB, filter string, filterArgs []any, since, until string, total int64) []ModelStat {
	if total == 0 {
		return nil
	}
	args := append(append([]any{}, filterArgs...), since, since, until, until)
	rows, err := conn.Query(`
		SELECT COALESCE(NULLIF(model, ''), 'unknown'), COUNT(*), COALESCE(SUM(cost_usd), 0), COALESCE(SUM(total_tokens), 0)
		FROM requests
		WHERE `+filter+`
		  AND (ts >= ? OR ? = '')
		  AND (ts <= ? OR ? = '')
		GROUP BY 1
		ORDER BY 2 DESC, 1
		LIMIT 5`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []ModelStat
	for rows.Next() {
		var name string
		var n, tokens int64
		var cost float64
		if rows.Scan(&name, &n, &cost, &tokens) != nil {
			continue
		}
		out = append(out, ModelStat{
			Name:     name,
			Turns:    formatInt(n),
			TurnsPct: fmt.Sprintf("%.1f%%", float64(n)*100/float64(total)),
			Value:    formatUSD(cost),
			Tokens:   formatTokens(tokens),
		})
	}
	return out
}

// ratio formats num/den as a multiplier, or NotMeasured when den is not positive.
func ratio(num, den float64, suffix string) string {
	if den <= 0 {
		return NotMeasured
	}
	return fmt.Sprintf("%.1fx%s", num/den, suffix)
}

func formatUSD(v float64) string {
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	cents := int64(v*100 + 0.5)
	return fmt.Sprintf("%s$%s.%02d", sign, formatInt(cents/100), cents%100)
}

func formatMonthlyUSD(v float64) string {
	if v <= 0 {
		return NotMeasured
	}
	return formatUSD(v) + "/mo"
}

// DateRangeOptions specifies optional time bounding for executive reports.
type DateRangeOptions struct {
	Since string
	Until string
}

func FetchTelemetry(cfg *config.Config) (
	work WorkReportData,
	personal PersonalReportData,
	gemini GeminiReportData,
	combined CombinedReportData,
) {
	w, p, g, c, _ := FetchTelemetryWithRange(cfg, DateRangeOptions{})
	return w, p, g, c
}

func FetchTelemetryWithRange(cfg *config.Config, rangeOpts DateRangeOptions) (
	work WorkReportData,
	personal PersonalReportData,
	gemini GeminiReportData,
	combined CombinedReportData,
	err error,
) {
	sinceBound, err := parseDateBound(rangeOpts.Since, false)
	if err != nil {
		return work, personal, gemini, combined, fmt.Errorf("invalid --since date '%s': %w", rangeOpts.Since, err)
	}
	untilBound, err := parseDateBound(rangeOpts.Until, true)
	if err != nil {
		return work, personal, gemini, combined, fmt.Errorf("invalid --until date '%s': %w", rangeOpts.Until, err)
	}

	ratecardSrc := telemetry.RateCardSource()
	hasEngineerName := strings.TrimSpace(cfg.EngineerName) != ""

	brainSessions := NotMeasured
	if n := countBrainSessions(); n > 0 {
		brainSessions = formatInt(int64(n))
	}

	// 1. Start every figure at "not measured"; only data replaces it.
	work = WorkReportData{
		CompanyName:                 cfg.CompanyName,
		EngineerName:                cfg.EngineerName,
		WorkEmail:                   cfg.WorkEmail,
		HourlyRate:                  cfg.HourlyRate,
		AuditPeriod:                 NotMeasured,
		SubstantiatedValue:          NotMeasured,
		APIListPriceEquivalentValue: NotMeasured,
		ActualSpend:                 formatUSD(0),
		ActualSpendSub:              "$0 marginal for flat subscriptions",
		RatecardSource:              ratecardSrc,
		ROIMultiplier:               NotMeasured,
		MonthlyRunRate:              NotMeasured,
		AcceptedTurns:               NotMeasured,
		MonthlyNetCost:              NotMeasured,
		ExpectedROI:                 NotMeasured,
		DirectCostMultiplier:        NotMeasured,
		HasHourlyRate:               cfg.HourlyRate > 0,
		HasCompanyName:              strings.TrimSpace(cfg.CompanyName) != "",
		HasEngineerName:             hasEngineerName,
		DBPath:                      cfg.DBPath,
		// Deliverables come only from audited tasks and their recorded work
		// products (queryStaypointTasks); with none, the report shows none.
	}
	if cfg.HourlyRate > 0 {
		work.HoursSavedBreakEven = NotMeasured
		work.TotalHoursSaved = NotMeasured
	}

	// The upgrade's net monthly cost needs both plan prices configured.
	upgradeNet := 0.0
	if cfg.UpgradeSubscriptionUSD > 0 && cfg.WorkSubscriptionUSD > 0 {
		upgradeNet = cfg.UpgradeSubscriptionUSD - cfg.WorkSubscriptionUSD
	}
	if upgradeNet > 0 {
		work.MonthlyNetCost = "+" + formatUSD(upgradeNet) + " / mo"
		if cfg.HourlyRate > 0 {
			work.HoursSavedBreakEven = fmt.Sprintf("~%.1f billable client hours (@ $%.0f/hr)", upgradeNet/cfg.HourlyRate, cfg.HourlyRate)
		}
	}

	// Query StayPoint local DB for audited deliverables and work products
	queryStaypointTasks(&work, cfg, sinceBound, untilBound)

	personal = PersonalReportData{
		EngineerName:     cfg.EngineerName,
		PersonalEmail:    cfg.PersonalEmail,
		AuditPeriod:      NotMeasured,
		TotalRequests:    NotMeasured,
		DeliveredValue:   NotMeasured,
		NetSurplus:       NotMeasured,
		SubscriptionROI:  NotMeasured,
		SubscriptionCost: formatMonthlyUSD(cfg.PersonalSubscriptionUSD),
		TotalTokens:      NotMeasured,
		ActiveDays:       NotMeasured,
		HasEngineerName:  hasEngineerName,
	}

	gemini = GeminiReportData{
		EngineerName:    cfg.EngineerName,
		AuditPeriod:     NotMeasured,
		TotalTokens:     NotMeasured,
		InputTokens:     NotMeasured,
		OutputTokens:    NotMeasured,
		TotalTurns:      NotMeasured,
		CodeReviews:     NotMeasured,
		UniqueRepos:     NotMeasured,
		BrainSessions:   brainSessions,
		SubagentRuns:    NotMeasured,
		BugsFound:       NotMeasured,
		HasEngineerName: hasEngineerName,
	}

	totalSubscription := cfg.WorkSubscriptionUSD + cfg.PersonalSubscriptionUSD + cfg.GeminiSubscriptionUSD
	combined = CombinedReportData{
		EngineerName:          cfg.EngineerName,
		AuditPeriod:           NotMeasured,
		TotalValue:            NotMeasured,
		TotalInvocations:      NotMeasured,
		TotalTokens:           NotMeasured,
		CombinedROI:           NotMeasured,
		TotalSubscriptionCost: NotMeasured,
		WorkTurns:             NotMeasured,
		WorkValue:             NotMeasured,
		WorkTokens:            NotMeasured,
		PersonalTurns:         NotMeasured,
		PersonalValue:         NotMeasured,
		PersonalTokens:        NotMeasured,
		GeminiTurns:           NotMeasured,
		GeminiValue:           NotMeasured,
		GeminiTokens:          NotMeasured,
		GeminiReviews:         NotMeasured,
		GeminiBrains:          NotMeasured,
		HasEngineerName:       hasEngineerName,
	}
	if totalSubscription > 0 {
		combined.TotalSubscriptionCost = formatUSD(totalSubscription) + " / mo"
	}
	if brainSessions != NotMeasured {
		combined.GeminiBrains = brainSessions + " Sessions"
	}

	// 2. Query telemetry.db; cost_usd is the ratecard valuation recorded at ingest.
	dbPath := cfg.TelemetryDBPath
	if dbPath == "" {
		home, _ := os.UserHomeDir()
		dbPath = filepath.Join(home, ".config", "token-telemetry", "telemetry.db")
	}

	if _, statErr := os.Stat(dbPath); statErr != nil {
		return work, personal, gemini, combined, nil
	}

	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(3000)", dbPath)
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return work, personal, gemini, combined, nil
	}
	defer conn.Close()

	// Check global date range match if range specified
	if sinceBound != "" || untilBound != "" {
		var matchedRows int64
		_ = conn.QueryRow(`
			SELECT COUNT(*) FROM requests
			WHERE (ts >= ? OR ? = '') AND (ts <= ? OR ? = '')`,
			sinceBound, sinceBound, untilBound, untilBound).Scan(&matchedRows)
		if matchedRows == 0 {
			var globalMin, globalMax sql.NullString
			_ = conn.QueryRow(`SELECT MIN(ts), MAX(ts) FROM requests`).Scan(&globalMin, &globalMax)
			return work, personal, gemini, combined, fmt.Errorf("no telemetry records found between %s and %s (database spans %s to %s)",
				rangeOpts.Since, rangeOpts.Until, formatBoundDate(globalMin.String), formatBoundDate(globalMax.String))
		}
	}

	// Work
	w := queryUsage(conn, "account_email = ?", []any{cfg.WorkEmail}, sinceBound, untilBound)
	if w.count > 0 {
		months := w.months()
		work.AuditPeriod = w.period()
		work.SubstantiatedValue = formatUSD(w.cost)
		work.APIListPriceEquivalentValue = work.SubstantiatedValue
		work.AcceptedTurns = formatInt(w.count)
		work.ROIMultiplier = ratio(w.cost, cfg.WorkSubscriptionUSD*months, "")
		if cfg.HourlyRate > 0 {
			work.TotalHoursSaved = fmt.Sprintf("~%.1f client billable hours saved", w.cost/cfg.HourlyRate)
		}
		if months > 0 {
			runRate := w.cost / months
			work.MonthlyRunRate = formatUSD(runRate) + "/mo"
			work.ExpectedROI = ratio(runRate, cfg.UpgradeSubscriptionUSD, "")
			work.DirectCostMultiplier = ratio(runRate, upgradeNet, " net return on upgrade")
		}
		combined.WorkTurns = work.AcceptedTurns
		combined.WorkValue = work.SubstantiatedValue
		combined.WorkTokens = formatTokens(w.tokens)
	}

	// Personal
	p := queryUsage(conn, "account_email = ?", []any{cfg.PersonalEmail}, sinceBound, untilBound)
	if p.count > 0 {
		personal.AuditPeriod = p.period()
		personal.TotalRequests = formatInt(p.count)
		personal.DeliveredValue = formatUSD(p.cost)
		personal.TotalTokens = formatTokens(p.tokens)
		personal.ActiveDays = formatInt(p.activeDays) + " days"
		if p.activeDays == 1 {
			personal.ActiveDays = "1 day"
		}
		personal.Models = queryModels(conn, "account_email = ?", []any{cfg.PersonalEmail}, sinceBound, untilBound, p.count)
		if planCost := cfg.PersonalSubscriptionUSD * p.months(); planCost > 0 {
			surplus := p.cost - planCost
			personal.NetSurplus = formatUSD(surplus)
			if surplus >= 0 {
				personal.NetSurplus = "+" + personal.NetSurplus
			}
			personal.SubscriptionROI = ratio(p.cost, planCost, "")
		}
		combined.PersonalTurns = personal.TotalRequests
		combined.PersonalValue = personal.DeliveredValue
		combined.PersonalTokens = personal.TotalTokens
	}

	// Gemini
	g := queryUsage(conn, "model_family = 'gemini'", nil, sinceBound, untilBound)
	if g.count > 0 {
		gemini.AuditPeriod = g.period()
		gemini.TotalTokens = formatTokens(g.tokens)
		gemini.InputTokens = formatTokens(g.input)
		gemini.OutputTokens = formatTokens(g.output)
		gemini.TotalTurns = formatInt(g.count)
		gemini.Models = queryModels(conn, "model_family = 'gemini'", nil, sinceBound, untilBound, g.count)
		combined.GeminiTurns = gemini.TotalTurns
		combined.GeminiValue = formatUSD(g.cost)
		combined.GeminiTokens = gemini.TotalTokens
	}

	// Reviews
	var totalReviews, uniqueRepos, bugsFound int64
	err = conn.QueryRow(`
		SELECT COUNT(*), COUNT(DISTINCT repo), COALESCE(SUM(CASE WHEN severity > 0 THEN 1 ELSE 0 END), 0)
		FROM review_outcomes`).Scan(&totalReviews, &uniqueRepos, &bugsFound)
	if err == nil && totalReviews > 0 {
		gemini.CodeReviews = formatInt(totalReviews)
		gemini.UniqueRepos = formatInt(uniqueRepos)
		gemini.BugsFound = formatInt(bugsFound)
		combined.GeminiReviews = fmt.Sprintf("%s Reviews (%s Repos)", formatInt(totalReviews), formatInt(uniqueRepos))
	}

	// Subagents
	var subagentCount int64
	err = conn.QueryRow(`SELECT COUNT(*) FROM subagent_runs`).Scan(&subagentCount)
	if err == nil && subagentCount > 0 {
		gemini.SubagentRuns = formatInt(subagentCount)
	}

	// All accounts
	all := queryUsage(conn, "1=1", nil, sinceBound, untilBound)
	if all.count > 0 {
		combined.AuditPeriod = all.period()
		combined.TotalInvocations = formatInt(all.count)
		combined.TotalTokens = formatTokens(all.tokens)
		combined.TotalValue = formatUSD(all.cost)
		combined.CombinedROI = ratio(all.cost, totalSubscription*all.months(), "")
	}

	return work, personal, gemini, combined, nil
}

func formatInt(n int64) string {
	in := fmt.Sprintf("%d", n)
	var out []rune
	l := len(in)
	for i, r := range in {
		if i > 0 && (l-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, r)
	}
	return string(out)
}

func formatTokens(tokens int64) string {
	if tokens >= 1_000_000_000 {
		return fmt.Sprintf("%.2f Billion", float64(tokens)/1_000_000_000.0)
	}
	if tokens >= 1_000_000 {
		return fmt.Sprintf("%.2f Million", float64(tokens)/1_000_000.0)
	}
	return formatInt(tokens)
}

func formatPeriod(start, end string) string {
	t1, err1 := time.Parse(time.RFC3339, start)
	t2, err2 := time.Parse(time.RFC3339, end)
	if err1 == nil && err2 == nil {
		return fmt.Sprintf("%s to %s", t1.Format("Jan 2"), t2.Format("Jan 2, 2006"))
	}
	return ""
}

func formatBoundDate(s string) string {
	if s == "" {
		return "unknown"
	}
	t, err := time.Parse(time.RFC3339, s)
	if err == nil {
		return t.Format("Jan 2, 2006")
	}
	return s
}

func parseDateBound(s string, isEnd bool) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}

	// Relative day shorthand: 7d, 14d, 30d, 90d
	if strings.HasSuffix(s, "d") {
		var days int
		if _, err := fmt.Sscanf(s, "%dd", &days); err == nil && days > 0 {
			t := time.Now().UTC().AddDate(0, 0, -days)
			if isEnd {
				t = time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, time.UTC)
			} else {
				t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
			}
			return t.Format(time.RFC3339), nil
		}
	}

	formats := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
		"2006/01/02",
		"01/02/2006",
		"Jan 2, 2006",
	}

	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			if f == "2006-01-02" || f == "2006/01/02" || f == "01/02/2006" || f == "Jan 2, 2006" {
				if isEnd {
					t = time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, time.UTC)
				} else {
					t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
				}
			}
			return t.Format(time.RFC3339), nil
		}
	}

	return "", fmt.Errorf("unrecognized date format (supported: YYYY-MM-DD or 7d/30d)")
}

func queryStaypointTasks(work *WorkReportData, cfg *config.Config, sinceBound, untilBound string) {
	dbPath := cfg.DBPath
	if dbPath == "" {
		home, _ := os.UserHomeDir()
		dbPath = filepath.Join(home, ".staypoint", "staypoint.db")
	}
	if _, err := os.Stat(dbPath); err != nil {
		return
	}
	conn, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(3000)", dbPath))
	if err != nil {
		return
	}
	defer conn.Close()

	// Query tasks with their associated work products
	rows, err := conn.Query(`
		SELECT t.id, t.name, COALESCE(t.project, ''), t.spent_usd, t.spent_turns, t.spent_tokens,
		       COALESCE(wp.product_type, ''), COALESCE(wp.reference, '')
		FROM tasks t
		LEFT JOIN task_work_products wp ON wp.task_id = t.id
		WHERE t.status != 'soft_deleted'
		  AND (t.created_at >= ? OR ? = '')
		  AND (t.created_at <= ? OR ? = '')
		ORDER BY t.spent_usd DESC, t.updated_at DESC
		LIMIT 15
	`, sinceBound, sinceBound, untilBound, untilBound)
	if err != nil {
		return
	}
	defer rows.Close()

	var deliverables []DeliverableItem
	var totalUSD float64
	seenTasks := make(map[string]bool)

	for rows.Next() {
		var taskID, name, proj, prodType, prodRef string
		var spentUSD float64
		var spentTurns, spentTokens int64
		if err := rows.Scan(&taskID, &name, &proj, &spentUSD, &spentTurns, &spentTokens, &prodType, &prodRef); err == nil {
			if !seenTasks[taskID] {
				seenTasks[taskID] = true
				totalUSD += spentUSD

				impact := proj
				if impact == "" {
					if spentTurns > 0 || spentTokens > 0 {
						impact = fmt.Sprintf("%s turns • %s tokens", formatInt(spentTurns), formatTokens(spentTokens))
					} else {
						impact = "Audited engineering activity"
					}
				}
				valStr := formatUSD(spentUSD) + " value"
				wpStr := ""
				if prodType != "" && prodRef != "" {
					wpStr = fmt.Sprintf("%s: %s", prodType, prodRef)
				}

				deliverables = append(deliverables, DeliverableItem{
					Name:        name,
					Impact:      impact,
					Value:       valStr,
					ActualSpend: "$0.00 ($0 marginal)",
					WorkProduct: wpStr,
				})
			}
		}
	}

	// Headline figures come from telemetry alone; tasks only list deliverables.
	if len(deliverables) > 0 && totalUSD > 0 {
		work.Deliverables = deliverables
	}
}
