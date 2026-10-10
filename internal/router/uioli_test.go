package router

import (
	"context"
	"encoding/json"
	"github.com/VinnyVanGogh/staypoint/internal/telemetry/quota"
	"os"
	"strings"
	"testing"
	"time"
)

var uioliNow = time.Date(2026, 9, 29, 9, 0, 0, 0, time.Local)

func uioliState(personalWeeklyLeft float64, resetIn time.Duration, geminiWeeklyLeft float64) *PacerState {
	return &PacerState{Pools: map[PoolID]*QuotaPool{
		PoolGeminiNative: {
			TurnsRunway: 100,
			FiveHour:    QuotaWindow{RemainingPct: 90, Known: true},
			Weekly:      QuotaWindow{RemainingPct: geminiWeeklyLeft, Known: true},
		},
		PoolPersonalClaude: {
			TurnsRunway: 80,
			FiveHour:    QuotaWindow{RemainingPct: 100, Known: true},
			Weekly: QuotaWindow{
				UsedPct: 100 - personalWeeklyLeft, RemainingPct: personalWeeklyLeft,
				ResetsAt: uioliNow.Add(resetIn), Known: true,
			},
		},
	}}
}

func TestUIOLIPressure(t *testing.T) {
	cases := []struct {
		name    string
		left    float64
		resetIn time.Duration
		cfg     UIOLIConfig
		active  bool
	}{
		{"end of window, unspent quota", 38, 7 * time.Hour, UIOLIConfig{}, true},
		{"mid-week, plenty left is inactive", 38, 4 * 24 * time.Hour, UIOLIConfig{}, false},
		{"near reset, crumbs still expire", 5, 3 * time.Hour, UIOLIConfig{}, true},    // floor 3%
		{"near reset, only dust left", 2, 3 * time.Hour, UIOLIConfig{}, false},        // floor 3%
		{"start of window, crumbs ignored", 10, 20 * time.Hour, UIOLIConfig{}, false}, // floor 12.5%
		{"work seat tonight: 15% left, 7h to reset", 15, 7 * time.Hour, UIOLIConfig{}, true},
		{"floor is inclusive", 7.5, 12 * time.Hour, UIOLIConfig{MinPctPerHour: 0.5}, true}, // floor 7.5%
		{"just under the floor", 7.4, 12 * time.Hour, UIOLIConfig{MinPctPerHour: 0.5}, false},
		{"disabled by config", 38, 7 * time.Hour, UIOLIConfig{Disabled: true}, false},
		{"custom window widens trigger", 38, 30 * time.Hour, UIOLIConfig{WindowHours: 48}, true},
		{"low burn-down rate below threshold", 20, 23 * time.Hour, UIOLIConfig{}, false}, // 0.87%/h < 1
		{"already past reset", 38, -time.Hour, UIOLIConfig{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := uioliState(tc.left, tc.resetIn, 60).Pools[PoolPersonalClaude]
			got := p.UIOLIPressure(uioliNow, tc.cfg)
			if got.Active != tc.active {
				t.Fatalf("Active=%v want %v (%+v)", got.Active, tc.active, got)
			}
		})
	}

	t.Run("unknown weekly window never triggers", func(t *testing.T) {
		p := uioliState(38, 7*time.Hour, 60).Pools[PoolPersonalClaude]
		p.Weekly.Known = false
		if p.UIOLIPressure(uioliNow, UIOLIConfig{}).Active {
			t.Fatal("unknown data must not drive routing pressure")
		}
	})

	t.Run("burn-down rate is remaining over hours", func(t *testing.T) {
		p := uioliState(40, 8*time.Hour, 60).Pools[PoolPersonalClaude]
		if r := p.UIOLIPressure(uioliNow, UIOLIConfig{}).PctPerHourNeed; r != 5 {
			t.Fatalf("PctPerHourNeed=%v want 5", r)
		}
	})
}

func TestUIOLIRoutingPrefersPersonalClaudeOverGemini(t *testing.T) {
	ctx := context.Background()
	repo := "/Users/vincevasile/Documents/dev/personal-app"

	// Gemini has far more headroom, but the router never picks agy
	// (GeminiCodeForbidden): even with UIOLI off it is personal Claude.
	st := uioliState(38, 7*time.Hour, 80)
	base, _ := Route(ctx, repo, st, RouteOptions{Now: uioliNow, UIOLI: UIOLIConfig{Disabled: true}})
	if base.Tool != "claude" || base.Target != TargetClaudePersonal {
		t.Fatalf("baseline must be personal Claude, got %s (%s)", base.Tool, base.Reason)
	}

	dec, _ := Route(ctx, repo, st, RouteOptions{Now: uioliNow})
	if dec.Tool != "claude" || dec.Target != TargetClaudePersonal {
		t.Fatalf("UIOLI should route to personal Claude, got %s/%s (%s)", dec.Tool, dec.Target, dec.Reason)
	}
	if !strings.Contains(dec.Reason, "use-it-or-lose-it") {
		t.Errorf("reason should explain UIOLI: %s", dec.Reason)
	}

	hp, _ := Route(ctx, repo, st, RouteOptions{Now: uioliNow, HighPriority: true})
	if hp.Model != UIOLIHighPriorityModel {
		t.Errorf("high-priority UIOLI model=%s want %s", hp.Model, UIOLIHighPriorityModel)
	}
	if lo, _ := Route(ctx, repo, st, RouteOptions{Now: uioliNow}); lo.Model != UIOLIHighPriorityModel {
		t.Errorf("every run in the window steers by default, got %s", lo.Model)
	}
	hpOnly := UIOLIConfig{HighPriorityOnly: true}
	if lo, _ := Route(ctx, repo, st, RouteOptions{Now: uioliNow, UIOLI: hpOnly}); lo.Model == UIOLIHighPriorityModel {
		t.Errorf("high_priority_only: normal-priority task must not get the top-tier model")
	}
	if hi, _ := Route(ctx, repo, st, RouteOptions{Now: uioliNow, UIOLI: hpOnly, HighPriority: true}); hi.Model != UIOLIHighPriorityModel {
		t.Errorf("high_priority_only: high-priority task model=%s want %s", hi.Model, UIOLIHighPriorityModel)
	}
	pinned, _ := Route(ctx, repo, st, RouteOptions{Now: uioliNow, HighPriority: true, PreferredModel: "sonnet"})
	if pinned.Model != "sonnet" {
		t.Errorf("explicit model must win, got %s", pinned.Model)
	}

	// Mid-week: no UIOLI, still Claude (the router never picks agy).
	mid := uioliState(38, 4*24*time.Hour, 80)
	if d, _ := Route(ctx, repo, mid, RouteOptions{Now: uioliNow}); d.Tool != "claude" || strings.Contains(d.Reason, "use-it-or-lose-it") {
		t.Errorf("mid-week must be plain Claude without UIOLI, got %s (%s)", d.Tool, d.Reason)
	}

	// A configured agy preference no longer picks agy; a locked Claude pool
	// waits instead of falling back to Gemini.
	if d, _ := Route(ctx, repo, st, RouteOptions{Now: uioliNow, PreferredPersonalTool: "agy"}); d.Tool != "claude" {
		t.Errorf("agy preference must not pick agy, got %s", d.Tool)
	}
	locked := uioliState(38, 7*time.Hour, 80)
	locked.Pools[PoolPersonalClaude].IsLocked = true
	if d, _ := Route(ctx, repo, locked, RouteOptions{Now: uioliNow}); d.Tool != "claude" || !d.Waiting || strings.Contains(d.Reason, "use-it-or-lose-it") {
		t.Errorf("locked Claude must wait, got %s waiting=%v (%s)", d.Tool, d.Waiting, d.Reason)
	}
}

func TestUnreportedWeeklyWindowIsNotZeroPercent(t *testing.T) {
	// A provider row set that carries only the 5h window.
	now := time.Now()
	p := &QuotaPool{Weekly: QuotaWindow{RemainingPct: 100}, FiveHour: QuotaWindow{RemainingPct: 100}}
	applyQuotaRows(p, []quota.Row{{WindowType: quota.WindowFiveHour, UsedPct: 40, UpdatedAt: now}}, now)
	if p.Weekly.Known {
		t.Fatal("weekly window must stay unknown")
	}
	if got := p.Weekly.FormatPct(true, 0); got != "n/a" {
		t.Errorf("unknown weekly renders %q, want n/a", got)
	}
	if got := p.FiveHour.FormatPct(true, 0); got != "60%" {
		t.Errorf("5h remaining=%q want 60%%", got)
	}

	applyQuotaRows(p, []quota.Row{{WindowType: quota.WindowWeekly, UsedPct: 63, UpdatedAt: now}}, now)
	if !p.Weekly.Known || p.Weekly.RemainingPct != 37 || p.Weekly.FormatPct(false, 0) != "63%" {
		t.Errorf("derived weekly wrong: %+v", p.Weekly)
	}
}

func TestFormatReset(t *testing.T) {
	now := time.Date(2026, 9, 29, 9, 0, 0, 0, time.Local)
	cases := map[string]time.Time{
		"today 4:00 PM":    time.Date(2026, 9, 29, 16, 0, 0, 0, time.Local),
		"tomorrow 9:00 AM": time.Date(2026, 9, 30, 9, 0, 0, 0, time.Local),
		"Fri 4:00 PM":      time.Date(2026, 10, 2, 16, 0, 0, 0, time.Local),
	}
	for want, at := range cases {
		if got := FormatReset(at, now); got != want {
			t.Errorf("FormatReset=%q want %q", got, want)
		}
	}
	if FormatReset(time.Time{}, now) != "" {
		t.Error("zero time should render empty")
	}
}

// Reset times are stored in UTC. A 02:10Z reset seen at 4:37 PM PDT is
// 7:10 PM the same day, not "2:10am" (task-036279f2).
func TestFormatResetConvertsUTCToLocal(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	now := time.Date(2026, 10, 9, 16, 37, 0, 0, la)
	reset := time.Date(2026, 10, 10, 2, 10, 0, 0, time.UTC)
	if got := FormatReset(reset, now); got != "today 7:10 PM" {
		t.Errorf("FormatReset=%q want %q", got, "today 7:10 PM")
	}
}

func TestReadSamplesTailBackfillsMissingWeekly(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "samples-*.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Write a sample with weekly quota resetting in the future
	futureReset := float64(time.Now().Add(6 * time.Hour).Unix())
	sample := rawSampleJSON{
		AccountEmail:   "stylesbyvinny@gmail.com",
		Timestamp:      time.Now().Add(-1 * time.Hour).Format(time.RFC3339),
		FiveHourPct:    20,
		FiveHourResets: float64(time.Now().Add(2 * time.Hour).Unix()),
		SevenDayPct:    63,
		SevenDayResets: futureReset,
	}
	sampleData, _ := json.Marshal(sample)
	tmpFile.Write(append(sampleData, '\n'))
	tmpFile.Sync()

	state := &PacerState{Pools: map[PoolID]*QuotaPool{
		PoolPersonalClaude: {
			ID:          PoolPersonalClaude,
			Name:        "Claude (Personal)",
			LastUpdated: time.Now(), // Newer than sample, simulating state.json 5h update
			FiveHour:    QuotaWindow{Known: true, UsedPct: 30, RemainingPct: 70},
			Weekly:      QuotaWindow{Known: false}, // state.json lacked weekly
		},
		PoolWorkClaude: {
			ID:       PoolWorkClaude,
			Name:     "Claude (Work)",
			Weekly:   QuotaWindow{Known: true},
			FiveHour: QuotaWindow{Known: true},
		},
	}}

	readSamplesTail(tmpFile.Name(), state)

	p := state.Pools[PoolPersonalClaude]
	if !p.Weekly.Known {
		t.Fatal("expected Weekly.Known to be true after backfill")
	}
	if p.Weekly.UsedPct != 63 || p.Weekly.RemainingPct != 37 {
		t.Fatalf("expected 63%% used, 37%% remaining, got used=%v rem=%v", p.Weekly.UsedPct, p.Weekly.RemainingPct)
	}
	if p.Weekly.ResetsAt.Unix() != int64(futureReset) {
		t.Fatalf("expected ResetsAt %v, got %v", int64(futureReset), p.Weekly.ResetsAt.Unix())
	}
}

// uioliWorkState: work seat in its burn-down window, personal seat mid-week
// (not in its window), Gemini with plenty of headroom.
func uioliWorkState(workLeft float64, resetIn time.Duration) *PacerState {
	st := uioliState(60, 4*24*time.Hour, 80)
	st.Pools[PoolWorkClaude] = &QuotaPool{
		ID:          PoolWorkClaude,
		TurnsRunway: 50,
		FiveHour:    QuotaWindow{RemainingPct: 100, Known: true},
		Weekly: QuotaWindow{
			UsedPct: 100 - workLeft, RemainingPct: workLeft,
			ResetsAt: uioliNow.Add(resetIn), Known: true,
		},
	}
	return st
}

func TestUIOLIWorkSeatInteractiveRoute(t *testing.T) {
	ctx := context.Background()
	repo := "/Users/vincevasile/Documents/dev/mansol-apps-server/github_repo-prod"
	st := uioliWorkState(15, 7*time.Hour)

	d, _ := Route(ctx, repo, st, RouteOptions{Now: uioliNow})
	if d.Target != TargetLocalClaudeWork || d.Tool != "claude" || d.Model != UIOLIHighPriorityModel {
		t.Fatalf("work seat in window: want Fable on local work seat, got %s/%s/%s (%s)", d.Target, d.Tool, d.Model, d.Reason)
	}
	if !strings.Contains(d.Reason, "UIOLI active: pool work-claude") {
		t.Errorf("reason must carry the UIOLI note: %s", d.Reason)
	}

	if d, _ := Route(ctx, repo, st, RouteOptions{Now: uioliNow, PreferredModel: "sonnet"}); d.Model != "sonnet" {
		t.Errorf("explicit model must win in a work repo, got %s", d.Model)
	}
	if d, _ := Route(ctx, repo, st, RouteOptions{Now: uioliNow, UIOLI: UIOLIConfig{Disabled: true}}); d.Model != "claude-opus-5" || strings.Contains(d.Reason, "UIOLI") {
		t.Errorf("disabled: want plain opus, got %s (%s)", d.Model, d.Reason)
	}
	if d, _ := Route(ctx, repo, uioliWorkState(15, 4*24*time.Hour), RouteOptions{Now: uioliNow}); d.Model != "claude-opus-5" {
		t.Errorf("mid-week work seat must not steer, got %s", d.Model)
	}

	locked := uioliWorkState(15, 7*time.Hour)
	locked.Pools[PoolWorkClaude].IsLocked = true
	d, _ = Route(ctx, repo, locked, RouteOptions{Now: uioliNow})
	if d.Target != TargetClaudePersonal || d.Model == UIOLIHighPriorityModel || strings.Contains(d.Reason, "UIOLI") {
		t.Errorf("locked work seat: want personal fallback without UIOLI, got %s/%s (%s)", d.Target, d.Model, d.Reason)
	}

	// Personal repo is unaffected by the work seat's window.
	if d, _ := Route(ctx, "/Users/vincevasile/Documents/dev/personal-app", st, RouteOptions{Now: uioliNow}); d.Model == UIOLIHighPriorityModel || d.Target != TargetClaudePersonal {
		t.Errorf("personal repo must not steer off the work seat's window, got %s/%s", d.Target, d.Model)
	}
}

func TestUIOLIDaemonRouteSteersWorkSeatToFable(t *testing.T) {
	cfg := &UIOLIConfig{}
	st := uioliWorkState(15, 7*time.Hour)

	for _, kind := range []string{"coding", "qa", "docs", "planning", "review", "architecture"} {
		r := ResolveRouteChoice(kind, true, st, RouteChoice{UIOLI: cfg}, uioliNow)
		s, ok := r.Chosen()
		if !ok || s.Family != FamilyClaude || s.Seat != SeatWork || s.Model != UIOLIHighPriorityModel {
			t.Errorf("%s: want Fable on the work seat, got %+v ok=%v", kind, s, ok)
		}
		if r.Candidates[0].Family == FamilyGemini {
			t.Errorf("%s: Gemini must never be chosen under UIOLI", kind)
		}
		if !strings.Contains(r.UIOLINote, "pool work-claude") || !strings.Contains(r.Body(), r.UIOLINote) {
			t.Errorf("%s: note %q must be on the route row body %q", kind, r.UIOLINote, r.Body())
		}
		if r.Title() != "Ran on Claude Fable · work seat" {
			t.Errorf("%s: title=%q", kind, r.Title())
		}
		for _, c := range r.Candidates[1:] {
			if c.Model == UIOLIHighPriorityModel {
				t.Errorf("%s: only the steered slot runs Fable, got %+v", kind, c)
			}
		}
	}

	// Coding chain keeps its personal fallback on opus.
	r := ResolveRouteChoice("coding", true, st, RouteChoice{UIOLI: cfg}, uioliNow)
	if len(r.Candidates) != 2 || r.Candidates[1].Seat != SeatPersonal || r.Candidates[1].Model != "opus" {
		t.Errorf("personal fallback changed: %+v", r.Candidates)
	}
}

func TestUIOLIDaemonRouteLeavesOtherRoutesAlone(t *testing.T) {
	cfg := &UIOLIConfig{}
	st := uioliWorkState(15, 7*time.Hour)
	chosen := func(r KindRoute) RouteSlot { s, _ := r.Chosen(); return s }

	if r := ResolveRouteChoice("coding", false, st, RouteChoice{UIOLI: cfg}, uioliNow); chosen(r).Model != "opus" || r.UIOLINote != "" {
		t.Errorf("personal repo (personal seat mid-week) must be unaffected, got %+v note=%q", chosen(r), r.UIOLINote)
	}
	if r := ResolveRouteChoice("coding", true, st, RouteChoice{}, uioliNow); chosen(r).Model != "opus" || r.UIOLINote != "" {
		t.Errorf("nil UIOLI config must leave the route alone, got %+v", chosen(r))
	}
	if r := ResolveRouteChoice("coding", true, st, RouteChoice{UIOLI: &UIOLIConfig{Disabled: true}}, uioliNow); chosen(r).Model != "opus" {
		t.Errorf("disabled must not steer, got %+v", chosen(r))
	}
	if r := ResolveRouteChoice("coding", true, st, RouteChoice{Model: "sonnet", UIOLI: cfg}, uioliNow); chosen(r).Model != "sonnet" || r.UIOLINote != "" {
		t.Errorf("explicit model must win, got %+v", chosen(r))
	}
	if r := ResolveRouteChoice("docs", true, st, RouteChoice{Provider: ProviderGemini, UIOLI: cfg}, uioliNow); chosen(r).Family != FamilyGemini || r.UIOLINote != "" {
		t.Errorf("Board's provider=gemini on a docs task stays, got %+v", chosen(r))
	}

	hpOnly := &UIOLIConfig{HighPriorityOnly: true}
	if r := ResolveRouteChoice("coding", true, st, RouteChoice{UIOLI: hpOnly}, uioliNow); chosen(r).Model != "opus" {
		t.Errorf("high_priority_only, normal task: want opus, got %+v", chosen(r))
	}
	if r := ResolveRouteChoice("coding", true, st, RouteChoice{UIOLI: hpOnly, HighPriority: true}, uioliNow); chosen(r).Model != UIOLIHighPriorityModel {
		t.Errorf("high_priority_only, high task: want Fable, got %+v", chosen(r))
	}

	// Locked work seat: hard lock or a spent 5h window. Never steered to;
	// the personal fallback runs its normal model.
	hard := uioliWorkState(15, 7*time.Hour)
	hard.Pools[PoolWorkClaude].IsLocked = true
	fiveH := uioliWorkState(15, 7*time.Hour)
	fiveH.Pools[PoolWorkClaude].FiveHour = QuotaWindow{RemainingPct: 0, ResetsAt: uioliNow.Add(time.Hour), Known: true}
	for name, p := range map[string]*PacerState{"hard lock": hard, "5h spent": fiveH} {
		r := ResolveRouteChoice("coding", true, p, RouteChoice{UIOLI: cfg}, uioliNow)
		if s := chosen(r); s.Seat != SeatPersonal || s.Model != "opus" || r.UIOLINote != "" {
			t.Errorf("%s: want personal opus fallback without UIOLI, got %+v note=%q", name, s, r.UIOLINote)
		}
	}
}

func TestIsHighPriority(t *testing.T) {
	for p, want := range map[string]bool{"high": true, "Urgent": true, "critical": true, "medium": false, "": false, "low": false} {
		if IsHighPriority(p) != want {
			t.Errorf("IsHighPriority(%q) != %v", p, want)
		}
	}
}
