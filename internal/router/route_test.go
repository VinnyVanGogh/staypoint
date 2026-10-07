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

// STA-856: for work that may write code, a locked work seat falls back to the
// personal Claude seat (same model), never Gemini; with both seats locked the
// route is all-locked so the run waits.
func TestResolveRoute_WorkRepoWorkLockedFallsBackToPersonalClaude(t *testing.T) {
	p := lockedPacer(map[PoolID]string{PoolWorkClaude: "5h limit"})
	for _, kind := range []string{"coding", "review", "qa", "", "bogus"} {
		d := ResolveRoute(kind, true, p, "", time.Now())
		s := mustChosen(t, d)
		if s.Family != FamilyClaude || s.Seat != SeatPersonal || s.PoolID != PoolPersonalClaude {
			t.Fatalf("kind %q: got %+v, want personal Claude", kind, s)
		}
		for _, c := range d.Candidates {
			if c.Family == FamilyGemini {
				t.Fatalf("kind %q: Gemini in work chain: %+v", kind, d.Candidates)
			}
		}
		if !strings.HasPrefix(d.Title(), "Fell back to Claude ") || !strings.HasSuffix(d.Title(), "personal seat: work seat locked (5h limit)") {
			t.Errorf("kind %q: title = %q", kind, d.Title())
		}
	}
}

func TestResolveRoute_WorkRepoBothSeatsLockedWaitsNeverGemini(t *testing.T) {
	p := lockedPacer(map[PoolID]string{PoolWorkClaude: "weekly limit", PoolPersonalClaude: "5h limit"})
	for _, kind := range []string{"coding", "review", "qa", "", "bogus"} {
		d := ResolveRoute(kind, true, p, "", time.Now())
		if !d.AllLocked() || !d.GeminiBarred {
			t.Fatalf("kind %q: want all-locked with Gemini barred, got %+v", kind, d)
		}
		if got, want := d.Title(), "Waiting for a Claude seat: work seat locked (weekly limit); personal seat locked (5h limit)"; got != want {
			t.Errorf("kind %q: title = %q, want %q", kind, got, want)
		}
		if !strings.Contains(d.Body(), GeminiCodeRule) {
			t.Errorf("kind %q: body %q should name the rule", kind, d.Body())
		}
	}
	// Model overrides do not reopen the Gemini pair either.
	d := ResolveRoute("coding", true, p, "opus", time.Now())
	if !d.AllLocked() {
		t.Fatalf("override: want all-locked, got %+v", d.Candidates)
	}
}

// Planning and architecture in a work repo may still run on Gemini (docs only;
// the harness guard reverts any code it writes).
func TestResolveRoute_WorkRepoPlanningMayUseGemini(t *testing.T) {
	for _, kind := range []string{"planning", "architecture"} {
		d := ResolveRoute(kind, true, openPacer(), "", time.Now())
		s := mustChosen(t, d)
		if s.Family != FamilyGemini || d.GeminiBarred {
			t.Errorf("%s: got %+v barred=%v, want Gemini first", kind, s, d.GeminiBarred)
		}
	}
}

func TestGeminiCodeForbidden(t *testing.T) {
	if !GeminiCodeForbidden(true) || GeminiCodeForbidden(false) {
		t.Fatal("rule must hold for work repos only")
	}
	for _, k := range []WorkKind{WorkKindCoding, WorkKindReview, WorkKindQA, "", "bogus"} {
		if GeminiAllowed(k, true) {
			t.Errorf("Gemini allowed for %q in a work repo", k)
		}
		if !GeminiAllowed(k, false) {
			t.Errorf("Gemini barred for %q in a personal repo", k)
		}
	}
	for _, k := range []WorkKind{WorkKindPlanning, WorkKindArchitecture} {
		if !GeminiAllowed(k, true) {
			t.Errorf("Gemini barred for %q in a work repo", k)
		}
	}
	if len(GeminiDocAllowlist()) == 0 {
		t.Error("allowlist empty")
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
	p := lockedPacer(map[PoolID]string{PoolPersonalClaude: "a", PoolGeminiNative: "b"})
	d := ResolveRoute("coding", false, p, "", time.Now())
	if !d.AllLocked() {
		t.Fatalf("expected all locked, got %+v", d)
	}
	if !strings.HasPrefix(d.Title(), "All providers locked") {
		t.Errorf("title = %q", d.Title())
	}
	if !strings.Contains(d.Body(), "personal seat locked (a)") || !strings.Contains(d.Body(), "Gemini locked (b)") {
		t.Errorf("body %q should list every lock reason", d.Body())
	}
}

func TestResolveRoute_ModelOverrideSonnet(t *testing.T) {
	d := ResolveRoute("qa", false, openPacer(), "sonnet", time.Now())
	s := mustChosen(t, d)
	if s.Family != FamilyClaude || s.Model != "sonnet" || s.PoolID != PoolPersonalClaude {
		t.Fatalf("override: got %+v, want personal Claude sonnet", s)
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
			for i, s := range chain {
				// STA-856: work chains that may write code end in personal-seat
				// fallbacks; the leading Claude slots stay on the repo's seat.
				if isWork && !GeminiAllowed(kind, true) && i > 0 && s.PoolID == PoolPersonalClaude {
					continue
				}
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
			if isWork && !GeminiAllowed(WorkKind(c.kind), true) {
				// STA-856: Gemini removed; work Claude then personal Claude, same model.
				if len(d.Candidates) != 2 ||
					d.Candidates[0].PoolID != PoolWorkClaude || d.Candidates[0].Seat != SeatWork ||
					d.Candidates[1].PoolID != PoolPersonalClaude || d.Candidates[1].Seat != SeatPersonal ||
					d.Candidates[0].Model != d.Candidates[1].Model {
					t.Errorf("%s work: want work then personal Claude, got %+v", c.kind, d.Candidates)
				}
				continue
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
