package reporting

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/config"
)

// legacyFigures are the literals the Boss Card used to print whatever the
// telemetry said. None of them may reach a rendered report again.
var legacyFigures = []string{
	"$2,881.71", "2881.71", "91.4x", "$1,827.57", "25,814", "9.1x to 15.0x",
	"14.4x net return", "102,502", "$7,574.00", "75.7x", "26.86 Billion",
	"38 days", "$14,259.29", "145,624", "39.7 Billion", "109.6x", "$130.00",
	"7.91 Billion", "53.08 Million", "$3,803.58", "Aug 10 to Sep 19, 2026",
	"Aug 11 to Sep 19, 2026", "Aug 1 to Sep 19, 2026", "July 7 to Sep 19, 2026",
	"Claude Sonnet 5", "Gemini 3.1 Pro (High)", "508 Sessions",
}

// seedHeadlineDB writes a telemetry DB with a model column so per-model
// breakdowns can be checked. Rows span 2026-08-10 to 2026-09-15.
func seedHeadlineDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "telemetry.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`
	CREATE TABLE requests (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts TEXT NOT NULL,
		account_email TEXT,
		model TEXT,
		model_family TEXT,
		cost_usd REAL,
		total_tokens INTEGER,
		input_tokens INTEGER,
		output_tokens INTEGER
	);
	INSERT INTO requests (ts, account_email, model, model_family, cost_usd, total_tokens, input_tokens, output_tokens) VALUES
		('2026-08-10T10:00:00.000Z', 'jane@acmework.com', 'claude-opus-5', 'claude', 50.0, 500000, 450000, 50000),
		('2026-08-15T12:00:00.000Z', 'jane@acmework.com', 'claude-opus-5', 'claude', 100.0, 1000000, 900000, 100000),
		('2026-08-20T14:00:00.000Z', 'jane.personal@example.com', 'claude-sonnet-5', 'claude', 250.0, 2500000, 2300000, 200000),
		('2026-09-01T09:00:00.000Z', 'jane.personal@example.com', 'gemini-3-flash', 'gemini', 20.0, 3000000, 2800000, 200000),
		('2026-09-15T16:00:00.000Z', 'jane@acmework.com', 'gemini-3-flash', 'gemini', 15.0, 2000000, 1900000, 100000);
	`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return dbPath
}

func headlineConfig(telemetryDB, staypointDB string) *config.Config {
	return &config.Config{
		TelemetryDBPath:         telemetryDB,
		DBPath:                  staypointDB,
		WorkEmail:               "jane@acmework.com",
		PersonalEmail:           "jane.personal@example.com",
		HourlyRate:              150,
		WorkSubscriptionUSD:     20,
		UpgradeSubscriptionUSD:  200,
		PersonalSubscriptionUSD: 100,
		GeminiSubscriptionUSD:   15,
	}
}

func TestHeadlineFiguresFromSeededRequests(t *testing.T) {
	dir := t.TempDir()
	cfg := headlineConfig(seedHeadlineDB(t), filepath.Join(dir, "missing-staypoint.db"))

	work, personal, gemini, combined, err := FetchTelemetryWithRange(cfg, DateRangeOptions{})
	if err != nil {
		t.Fatalf("FetchTelemetryWithRange: %v", err)
	}

	// Work: 3 rows, $165 over Aug 10..Sep 15 (37 days = 1.2156 months).
	checks := []struct{ name, got, want string }{
		{"work.AuditPeriod", work.AuditPeriod, "Aug 10 to Sep 15, 2026"},
		{"work.SubstantiatedValue", work.SubstantiatedValue, "$165.00"},
		{"work.APIListPriceEquivalentValue", work.APIListPriceEquivalentValue, "$165.00"},
		{"work.AcceptedTurns", work.AcceptedTurns, "3"},
		{"work.MonthlyRunRate", work.MonthlyRunRate, "$135.73/mo"},
		{"work.ROIMultiplier", work.ROIMultiplier, "6.8x"},
		{"work.MonthlyNetCost", work.MonthlyNetCost, "+$180.00 / mo"},
		{"work.ExpectedROI", work.ExpectedROI, "0.7x"},
		{"work.DirectCostMultiplier", work.DirectCostMultiplier, "0.8x net return on upgrade"},
		{"work.TotalHoursSaved", work.TotalHoursSaved, "~1.1 client billable hours saved"},
		{"work.HoursSavedBreakEven", work.HoursSavedBreakEven, "~1.2 billable client hours (@ $150/hr)"},

		// Personal: 2 rows, $270 over Aug 20..Sep 1 (13 days = 0.4271 months
		// of a $100/mo plan = $42.71).
		{"personal.AuditPeriod", personal.AuditPeriod, "Aug 20 to Sep 1, 2026"},
		{"personal.TotalRequests", personal.TotalRequests, "2"},
		{"personal.DeliveredValue", personal.DeliveredValue, "$270.00"},
		{"personal.TotalTokens", personal.TotalTokens, "5.50 Million"},
		{"personal.ActiveDays", personal.ActiveDays, "2 days"},
		{"personal.NetSurplus", personal.NetSurplus, "+$227.29"},
		{"personal.SubscriptionROI", personal.SubscriptionROI, "6.3x"},
		{"personal.SubscriptionCost", personal.SubscriptionCost, "$100.00/mo"},

		// Gemini: 2 rows, 5M tokens.
		{"gemini.TotalTurns", gemini.TotalTurns, "2"},
		{"gemini.TotalTokens", gemini.TotalTokens, "5.00 Million"},
		{"gemini.InputTokens", gemini.InputTokens, "4.70 Million"},
		{"gemini.OutputTokens", gemini.OutputTokens, "300,000"},

		// Combined: all 5 rows, $435 over Aug 10..Sep 15, against $135/mo
		// of configured plans (1.2156 months = $164.11).
		{"combined.AuditPeriod", combined.AuditPeriod, "Aug 10 to Sep 15, 2026"},
		{"combined.TotalInvocations", combined.TotalInvocations, "5"},
		{"combined.TotalTokens", combined.TotalTokens, "9.00 Million"},
		{"combined.TotalValue", combined.TotalValue, "$435.00"},
		{"combined.TotalSubscriptionCost", combined.TotalSubscriptionCost, "$135.00 / mo"},
		{"combined.CombinedROI", combined.CombinedROI, "2.7x"},
		{"combined.WorkTurns", combined.WorkTurns, "3"},
		{"combined.WorkValue", combined.WorkValue, "$165.00"},
		{"combined.WorkTokens", combined.WorkTokens, "3.50 Million"},
		{"combined.PersonalTurns", combined.PersonalTurns, "2"},
		{"combined.PersonalValue", combined.PersonalValue, "$270.00"},
		{"combined.PersonalTokens", combined.PersonalTokens, "5.50 Million"},
		{"combined.GeminiTurns", combined.GeminiTurns, "2"},
		{"combined.GeminiValue", combined.GeminiValue, "$35.00"},
		{"combined.GeminiTokens", combined.GeminiTokens, "5.00 Million"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}

	if len(personal.Models) != 2 {
		t.Fatalf("personal.Models = %+v, want 2 rows", personal.Models)
	}
	if m := personal.Models[0]; m.Name != "claude-sonnet-5" || m.Turns != "1" || m.TurnsPct != "50.0%" || m.Value != "$250.00" || m.Tokens != "2.50 Million" {
		t.Errorf("personal.Models[0] = %+v", m)
	}
	if len(gemini.Models) != 1 || gemini.Models[0].Name != "gemini-3-flash" || gemini.Models[0].Turns != "2" || gemini.Models[0].TurnsPct != "100.0%" {
		t.Errorf("gemini.Models = %+v", gemini.Models)
	}
}

func TestHeadlineFiguresRespectDateRange(t *testing.T) {
	cfg := headlineConfig(seedHeadlineDB(t), filepath.Join(t.TempDir(), "missing.db"))
	work, _, _, combined, err := FetchTelemetryWithRange(cfg, DateRangeOptions{Since: "2026-08-01", Until: "2026-08-31"})
	if err != nil {
		t.Fatalf("FetchTelemetryWithRange: %v", err)
	}
	if work.SubstantiatedValue != "$150.00" || work.AcceptedTurns != "2" || work.AuditPeriod != "Aug 10 to Aug 15, 2026" {
		t.Errorf("work = value %q turns %q period %q", work.SubstantiatedValue, work.AcceptedTurns, work.AuditPeriod)
	}
	if combined.TotalValue != "$400.00" || combined.GeminiTurns != NotMeasured {
		t.Errorf("combined = value %q gemini turns %q", combined.TotalValue, combined.GeminiTurns)
	}
}

// Without configured plan prices, nothing that divides by a plan price is
// invented: those figures read "not measured" while measured usage still shows.
func TestHeadlineFiguresWithoutSubscriptionConfig(t *testing.T) {
	cfg := headlineConfig(seedHeadlineDB(t), filepath.Join(t.TempDir(), "missing.db"))
	cfg.WorkSubscriptionUSD, cfg.UpgradeSubscriptionUSD = 0, 0
	cfg.PersonalSubscriptionUSD, cfg.GeminiSubscriptionUSD = 0, 0

	work, personal, _, combined, err := FetchTelemetryWithRange(cfg, DateRangeOptions{})
	if err != nil {
		t.Fatalf("FetchTelemetryWithRange: %v", err)
	}
	for name, got := range map[string]string{
		"work.ROIMultiplier":             work.ROIMultiplier,
		"work.MonthlyNetCost":            work.MonthlyNetCost,
		"work.ExpectedROI":               work.ExpectedROI,
		"work.DirectCostMultiplier":      work.DirectCostMultiplier,
		"work.HoursSavedBreakEven":       work.HoursSavedBreakEven,
		"personal.NetSurplus":            personal.NetSurplus,
		"personal.SubscriptionROI":       personal.SubscriptionROI,
		"personal.SubscriptionCost":      personal.SubscriptionCost,
		"combined.CombinedROI":           combined.CombinedROI,
		"combined.TotalSubscriptionCost": combined.TotalSubscriptionCost,
	} {
		if got != NotMeasured {
			t.Errorf("%s = %q, want %q", name, got, NotMeasured)
		}
	}
	if work.SubstantiatedValue != "$165.00" {
		t.Errorf("measured value should still show, got %q", work.SubstantiatedValue)
	}
}

// Empty DBs must not produce a single invented figure on any report.
func TestEmptyDBsInventNoFigures(t *testing.T) {
	dir := t.TempDir()
	cfg := headlineConfig(filepath.Join(dir, "missing-telemetry.db"), filepath.Join(dir, "missing-staypoint.db"))

	// An existing but empty telemetry DB behaves the same as a missing one.
	for _, mk := range []func() string{
		func() string { return filepath.Join(dir, "missing-telemetry.db") },
		func() string {
			p := filepath.Join(dir, "empty-telemetry.db")
			db, err := sql.Open("sqlite", p)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer db.Close()
			if _, err := db.Exec(`CREATE TABLE requests (ts TEXT, account_email TEXT, model TEXT, model_family TEXT, cost_usd REAL, total_tokens INTEGER, input_tokens INTEGER, output_tokens INTEGER)`); err != nil {
				t.Fatalf("schema: %v", err)
			}
			return p
		},
	} {
		cfg.TelemetryDBPath = mk()
		assertNoInventedFigures(t, cfg)
	}
}

func assertNoInventedFigures(t *testing.T, cfg *config.Config) {
	t.Helper()
	work, personal, gemini, combined, err := FetchTelemetryWithRange(cfg, DateRangeOptions{})
	if err != nil {
		t.Fatalf("FetchTelemetryWithRange: %v", err)
	}
	if len(personal.Models) != 0 || len(gemini.Models) != 0 || len(work.Deliverables) != 0 {
		t.Errorf("expected no model rows or deliverables, got %+v %+v %+v", personal.Models, gemini.Models, work.Deliverables)
	}
	for name, got := range map[string]string{
		"work.AuditPeriod":          work.AuditPeriod,
		"work.SubstantiatedValue":   work.SubstantiatedValue,
		"work.MonthlyRunRate":       work.MonthlyRunRate,
		"work.AcceptedTurns":        work.AcceptedTurns,
		"work.ExpectedROI":          work.ExpectedROI,
		"work.TotalHoursSaved":      work.TotalHoursSaved,
		"personal.TotalRequests":    personal.TotalRequests,
		"personal.DeliveredValue":   personal.DeliveredValue,
		"personal.TotalTokens":      personal.TotalTokens,
		"personal.ActiveDays":       personal.ActiveDays,
		"gemini.TotalTokens":        gemini.TotalTokens,
		"gemini.TotalTurns":         gemini.TotalTurns,
		"gemini.CodeReviews":        gemini.CodeReviews,
		"gemini.SubagentRuns":       gemini.SubagentRuns,
		"combined.TotalValue":       combined.TotalValue,
		"combined.TotalInvocations": combined.TotalInvocations,
		"combined.CombinedROI":      combined.CombinedROI,
		"combined.WorkValue":        combined.WorkValue,
		"combined.PersonalValue":    combined.PersonalValue,
		"combined.GeminiValue":      combined.GeminiValue,
	} {
		if got != NotMeasured {
			t.Errorf("%s = %q, want %q", name, got, NotMeasured)
		}
	}

	renders := map[string]func() (string, error){
		"work":     func() (string, error) { return generateWorkHTML(work) },
		"personal": func() (string, error) { return generatePersonalHTML(personal) },
		"gemini":   func() (string, error) { return generateGeminiHTML(gemini) },
		"combined": func() (string, error) { return generateCombinedHTML(combined) },
	}
	for name, render := range renders {
		html, err := render()
		if err != nil {
			t.Fatalf("%s render: %v", name, err)
		}
		for _, fake := range legacyFigures {
			if strings.Contains(html, fake) {
				t.Errorf("%s HTML contains hardcoded figure %q", name, fake)
			}
		}
	}
}

// Grep guard: telemetry.go computes figures; it must never again carry a
// dollar amount, a fixed denominator, or a padded "verified benchmark".
func TestTelemetryGoHasNoHardcodedFigures(t *testing.T) {
	src, err := os.ReadFile("telemetry.go")
	if err != nil {
		t.Fatalf("read telemetry.go: %v", err)
	}
	// Any dollar amount other than zero ("$0.00", "$0 marginal") is a figure
	// that belongs in data, not code.
	if m := regexp.MustCompile(`\$[0-9,.]*[1-9][0-9,.]*`).FindAll(src, -1); len(m) > 0 {
		t.Errorf("telemetry.go contains dollar literals: %q", m)
	}
	for _, lit := range append([]string{"3800.0", "130.0", "100.0", "/20.0", "/ 1.57", "180.0"}, legacyFigures...) {
		if strings.Contains(string(src), lit) {
			t.Errorf("telemetry.go contains hardcoded figure %q", lit)
		}
	}
}
