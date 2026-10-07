package router

import (
	"strings"
	"testing"
	"time"
)

func openPacer() *PacerState {
	return &PacerState{Pools: map[PoolID]*QuotaPool{}}
}

func lockedPacer(reasons map[PoolID]string) *PacerState {
	p := openPacer()
	for id, reason := range reasons {
		p.Pools[id] = &QuotaPool{ID: id, IsLocked: true, LockoutReason: reason}
	}
	return p
}

func mustChosen(t *testing.T, d KindRoute) RouteSlot {
	t.Helper()
	s, ok := d.Chosen()
	if !ok {
		t.Fatalf("expected a chosen slot, got all locked: %+v", d)
	}
	return s
}

func TestResolveRoute_PersonalCodingRunsPersonalClaudeOpus(t *testing.T) {
	d := ResolveRoute("coding", false, openPacer(), "", time.Now())
	s := mustChosen(t, d)
	if s.Family != FamilyClaude || s.Model != "opus" || s.PoolID != PoolPersonalClaude || s.Seat != SeatPersonal {
		t.Fatalf("personal coding: got %+v, want personal Claude opus", s)
	}
	if got, want := d.Title(), "Ran on Claude Opus · personal seat"; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
}

func TestResolveRoute_WorkCodingRunsWorkClaudeOpus(t *testing.T) {
	d := ResolveRoute("coding", true, openPacer(), "", time.Now())
	s := mustChosen(t, d)
	if s.Family != FamilyClaude || s.Model != "opus" || s.PoolID != PoolWorkClaude || s.Seat != SeatWork {
		t.Fatalf("work coding: got %+v, want work Claude opus", s)
	}
	if got, want := d.Title(), "Ran on Claude Opus · work seat"; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
}

// The STA-772 repro: personal seat locked must not push a work repo to Gemini.
func TestResolveRoute_WorkRepoPersonalLockedStillRunsWorkClaude(t *testing.T) {
	p := lockedPacer(map[PoolID]string{PoolPersonalClaude: "weekly limit"})
	d := ResolveRoute("coding", true, p, "", time.Now())
	s := mustChosen(t, d)
	if s.PoolID != PoolWorkClaude || s.Family != FamilyClaude {
		t.Fatalf("got %+v, want work Claude", s)
	}
	if len(d.Skipped) != 0 {
		t.Errorf("no slot should be skipped, got %+v", d.Skipped)
	}
}

func TestResolveRoute_WorkRepoWorkLockedFallsBackToGeminiWithReason(t *testing.T) {
	p := lockedPacer(map[PoolID]string{PoolWorkClaude: "weekly limit"})
	d := ResolveRoute("coding", true, p, "", time.Now())
	s := mustChosen(t, d)
	if s.Family != FamilyGemini || s.Model != "gemini-3.1-pro-high" {
		t.Fatalf("got %+v, want Gemini 3.1 Pro", s)
	}
	if got, want := d.Title(), "Fell back to Gemini 3.1 Pro: work seat locked (weekly limit)"; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
}

func TestResolveRoute_PersonalRepoNeverUsesWorkSeat(t *testing.T) {
	p := lockedPacer(map[PoolID]string{PoolPersonalClaude: ""})
	d := ResolveRoute("coding", false, p, "", time.Now())
	for _, c := range d.Candidates {
		if c.PoolID == PoolWorkClaude {
			t.Fatalf("personal repo must never route to the work seat: %+v", d.Candidates)
		}
	}
	s := mustChosen(t, d)
	if s.Family != FamilyGemini {
		t.Fatalf("got %+v, want Gemini fallback", s)
	}
	if !strings.Contains(d.Title(), "personal seat locked (locked)") {
		t.Errorf("title %q should name the locked personal seat", d.Title())
	}
}

func TestResolveRoute_EmptyOrUnknownKindIsClaudeFirst(t *testing.T) {
	for _, kind := range []string{"", "bogus", "  CODING "} {
		d := ResolveRoute(kind, false, openPacer(), "", time.Now())
		s := mustChosen(t, d)
		if s.Family != FamilyClaude || s.Model != "opus" {
			t.Errorf("kind %q: got %+v, want Claude opus first", kind, s)
		}
		if d.Kind != WorkKindCoding {
			t.Errorf("kind %q normalized to %q, want coding", kind, d.Kind)
		}
	}
}

func TestResolveRoute_FiveHourExhaustedCountsAsLocked(t *testing.T) {
	p := openPacer()
	p.Pools[PoolPersonalClaude] = &QuotaPool{FiveHour: QuotaWindow{RemainingPct: 0, ResetsAt: time.Now().Add(time.Hour)}}
	d := ResolveRoute("coding", false, p, "", time.Now())
	s := mustChosen(t, d)
	if s.Family != FamilyGemini {
		t.Fatalf("got %+v, want Gemini fallback when 5h window is spent", s)
	}
	if !strings.Contains(d.Title(), "5h limit") {
		t.Errorf("title %q should give the 5h reason", d.Title())
	}
}

func TestResolveRoute_AllLocked(t *testing.T) {
	p := lockedPacer(map[PoolID]string{PoolWorkClaude: "a", PoolGeminiNative: "b"})
	d := ResolveRoute("coding", true, p, "", time.Now())
	if !d.AllLocked() {
		t.Fatalf("expected all locked, got %+v", d)
	}
	if !strings.HasPrefix(d.Title(), "All providers locked") {
		t.Errorf("title = %q", d.Title())
	}
	if !strings.Contains(d.Body(), "work seat locked (a)") || !strings.Contains(d.Body(), "Gemini locked (b)") {
		t.Errorf("body %q should list every lock reason", d.Body())
	}
}

func TestResolveRoute_ModelOverrideSonnet(t *testing.T) {
	d := ResolveRoute("qa", true, openPacer(), "sonnet", time.Now())
	s := mustChosen(t, d)
	if s.Family != FamilyClaude || s.Model != "sonnet" || s.PoolID != PoolWorkClaude {
		t.Fatalf("override: got %+v, want work Claude sonnet", s)
	}
	if len(d.Candidates) < 2 || d.Candidates[1].Model != "gemini-3.8-flash-high" {
		t.Errorf("override fallback should pair sonnet with Gemini 3.8 Flash, got %+v", d.Candidates)
	}
	if !strings.Contains(d.Body(), "model override: sonnet") {
		t.Errorf("body %q should mention the override", d.Body())
	}
}

func TestResolveRoute_InvalidModelOverrideIgnored(t *testing.T) {
	d := ResolveRoute("coding", false, openPacer(), "gpt-9", time.Now())
	if d.ModelOverride != "" {
		t.Errorf("invalid override kept: %q", d.ModelOverride)
	}
	if s := mustChosen(t, d); s.Model != "opus" {
		t.Errorf("got %+v, want default opus", s)
	}
}

func TestNormalizeModelOverride(t *testing.T) {
	cases := map[string]string{"": "", "opus": "opus", " Opus ": "opus", "sonnet": "sonnet", "haiku": "", "gemini": ""}
	for in, want := range cases {
		if got := NormalizeModelOverride(in); got != want {
			t.Errorf("NormalizeModelOverride(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestChainsForRepo_MapsClaudeSlotsToSeat(t *testing.T) {
	for _, isWork := range []bool{false, true} {
		want := PoolPersonalClaude
		if isWork {
			want = PoolWorkClaude
		}
		for kind, chain := range ChainsForRepo(isWork) {
			for _, s := range chain {
				if strings.HasPrefix(s.Provider, "claude-") && s.Provider != "claude-cloud" && s.PoolID != want {
					t.Errorf("isWork=%v kind=%s slot %s pool=%s, want %s", isWork, kind, s.Provider, s.PoolID, want)
				}
			}
		}
	}
}

func TestPoolLockReason(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name   string
		pool   *QuotaPool
		locked bool
		reason string
	}{
		{"nil", nil, false, ""},
		{"hard lock with reason", &QuotaPool{IsLocked: true, LockoutReason: "rate limited"}, true, "rate limited"},
		{"hard lock no reason", &QuotaPool{IsLocked: true}, true, "locked"},
		{"5h spent", &QuotaPool{FiveHour: QuotaWindow{RemainingPct: 0, ResetsAt: now.Add(time.Hour)}}, true, "5h limit reached"},
		{"weekly spent", &QuotaPool{FiveHour: QuotaWindow{RemainingPct: 50}, Weekly: QuotaWindow{RemainingPct: 0, Known: true, ResetsAt: now.Add(48 * time.Hour)}}, true, "weekly limit reached"},
		{"weekly unreported", &QuotaPool{FiveHour: QuotaWindow{RemainingPct: 50}, Weekly: QuotaWindow{UsedPct: 50, ResetsAt: now.Add(48 * time.Hour)}}, false, ""},
		{"5h spent but reset passed", &QuotaPool{FiveHour: QuotaWindow{RemainingPct: 0, ResetsAt: now.Add(-time.Hour)}}, false, ""},
		{"healthy", &QuotaPool{FiveHour: QuotaWindow{RemainingPct: 80, ResetsAt: now.Add(time.Hour)}}, false, ""},
	}
	for _, c := range cases {
		locked, reason := PoolLockReason(c.pool, now)
		if locked != c.locked || reason != c.reason {
			t.Errorf("%s: got (%v,%q), want (%v,%q)", c.name, locked, reason, c.locked, c.reason)
		}
	}
}

// Every kind in the routing table is honoured per repo seat: coding and review
// run Claude Opus first; architecture and planning run Gemini first with a
// Claude fallback on the repo's seat; qa runs Gemini Flash first.
func TestResolveRoute_EveryKindFirstAndFallback(t *testing.T) {
	cases := []struct {
		kind                 string
		firstFamily, firstM  string
		secondFamily, second string
	}{
		{"coding", FamilyClaude, "opus", FamilyGemini, "gemini-3.1-pro-high"},
		{"review", FamilyClaude, "opus", FamilyGemini, "gemini-3.1-pro-high"},
		{"architecture", FamilyGemini, "gemini-3.1-pro-high", FamilyClaude, "opus"},
		{"planning", FamilyGemini, "gemini-3.8-flash-high", FamilyClaude, "sonnet"},
		{"qa", FamilyGemini, "gemini-3.8-flash-high", FamilyClaude, "sonnet"},
	}
	for _, isWork := range []bool{false, true} {
		wantPool, wantSeat := PoolPersonalClaude, SeatPersonal
		if isWork {
			wantPool, wantSeat = PoolWorkClaude, SeatWork
		}
		for _, c := range cases {
			d := ResolveRoute(c.kind, isWork, openPacer(), "", time.Now())
			if d.Kind != WorkKind(c.kind) {
				t.Errorf("%s: kind normalised to %q", c.kind, d.Kind)
			}
			if len(d.Candidates) != 2 {
				t.Fatalf("%s work=%v: want 2 candidates, got %+v", c.kind, isWork, d.Candidates)
			}
			first, second := d.Candidates[0], d.Candidates[1]
			if first.Family != c.firstFamily || first.Model != c.firstM {
				t.Errorf("%s work=%v: first = %+v, want %s %s", c.kind, isWork, first, c.firstFamily, c.firstM)
			}
			if second.Family != c.secondFamily || second.Model != c.second {
				t.Errorf("%s work=%v: second = %+v, want %s %s", c.kind, isWork, second, c.secondFamily, c.second)
			}
			for _, s := range d.Candidates {
				if s.Family == FamilyClaude && (s.PoolID != wantPool || s.Seat != wantSeat) {
					t.Errorf("%s work=%v: Claude slot on %s/%s, want %s/%s", c.kind, isWork, s.PoolID, s.Seat, wantPool, wantSeat)
				}
			}
		}
	}
}

// Gemini-first kinds fall back to the repo's Claude seat when Gemini is locked.
func TestResolveRoute_PlanningGeminiLockedFallsBackToClaude(t *testing.T) {
	p := lockedPacer(map[PoolID]string{PoolGeminiNative: "5h limit reached"})
	d := ResolveRoute("planning", true, p, "", time.Now())
	s := mustChosen(t, d)
	if s.Family != FamilyClaude || s.Model != "sonnet" || s.Seat != SeatWork {
		t.Fatalf("got %+v, want work Claude sonnet", s)
	}
	if got, want := d.Title(), "Fell back to Claude Sonnet · work seat: Gemini locked (5h limit reached)"; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
}

func TestResolveRoute_ReviewIsOpus(t *testing.T) {
	d := ResolveRoute("review", false, openPacer(), "", time.Now())
	if got, want := d.Title(), "Ran on Claude Opus · personal seat"; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
	if got, want := d.Body(), "Kind of work: review"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}
