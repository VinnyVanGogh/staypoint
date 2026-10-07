package router

import (
	"errors"
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

// Board rule (GeminiCodeForbidden, all repos): code kinds never get Gemini. A
// locked work seat falls back to the personal Claude seat (same model); with
// both seats locked the route is all-locked so the run waits.
var codeKinds = []string{"coding", "qa", "", "bogus"}
var nonCodeKinds = []string{"planning", "architecture", "review", "docs"}

func TestResolveRoute_WorkRepoWorkLockedFallsBackToPersonalClaude(t *testing.T) {
	p := lockedPacer(map[PoolID]string{PoolWorkClaude: "5h limit"})
	for _, kind := range codeKinds {
		d := ResolveRoute(kind, true, p, "", time.Now())
		s := mustChosen(t, d)
		if s.Family != FamilyClaude || s.Seat != SeatPersonal || s.PoolID != PoolPersonalClaude {
			t.Fatalf("kind %q: got %+v, want personal Claude", kind, s)
		}
		if d.HasGemini() {
			t.Fatalf("kind %q: Gemini in code chain: %+v", kind, d.Candidates)
		}
		if got, want := d.Title(), "Fell back to Claude Opus · personal seat: work seat locked (5h limit)"; got != want {
			t.Errorf("kind %q: title = %q, want %q", kind, got, want)
		}
	}
}

func TestResolveRoute_CodeKindsAllSeatsLockedWaitNeverGemini(t *testing.T) {
	for _, isWork := range []bool{false, true} {
		p := lockedPacer(map[PoolID]string{PoolWorkClaude: "weekly limit", PoolPersonalClaude: "5h limit"})
		want := "Waiting for a Claude seat: personal seat locked (5h limit)"
		if isWork {
			want = "Waiting for a Claude seat: work seat locked (weekly limit); personal seat locked (5h limit)"
		}
		for _, kind := range codeKinds {
			d := ResolveRoute(kind, isWork, p, "", time.Now())
			if !d.AllLocked() || !d.GeminiBarred || d.HasGemini() {
				t.Fatalf("work=%v kind %q: want all-locked with Gemini barred, got %+v", isWork, kind, d)
			}
			if got := d.Title(); got != want {
				t.Errorf("work=%v kind %q: title = %q, want %q", isWork, kind, got, want)
			}
			if !strings.Contains(d.Body(), GeminiCodeRule) {
				t.Errorf("kind %q: body %q should name the rule", kind, d.Body())
			}
		}
		// Model overrides do not reopen the Gemini pair either.
		for _, m := range []string{"opus", "sonnet"} {
			if d := ResolveRoute("coding", isWork, p, m, time.Now()); !d.AllLocked() || d.HasGemini() {
				t.Fatalf("work=%v override %s: want all-locked Claude only, got %+v", isWork, m, d)
			}
		}
	}
}

// Non-code kinds run Gemini first in every repo; the harness guard reverts any
// code a Gemini turn writes.
func TestResolveRoute_NonCodeKindsGeminiFirstEveryRepo(t *testing.T) {
	for _, isWork := range []bool{false, true} {
		for _, kind := range nonCodeKinds {
			d := ResolveRoute(kind, isWork, openPacer(), "", time.Now())
			s := mustChosen(t, d)
			if s.Family != FamilyGemini || d.GeminiBarred || s.ChosenByBoard {
				t.Errorf("work=%v %s: got %+v barred=%v, want automatic Gemini first", isWork, kind, s, d.GeminiBarred)
			}
			if strings.Contains(d.Title(), "chosen by Board") {
				t.Errorf("work=%v %s: automatic route labelled as Board choice: %q", isWork, kind, d.Title())
			}
		}
	}
}

func TestGeminiCodeForbidden(t *testing.T) {
	if !GeminiCodeForbidden(true) || !GeminiCodeForbidden(false) {
		t.Fatal("rule must hold in every repo")
	}
	for _, isWork := range []bool{false, true} {
		for _, k := range []WorkKind{WorkKindCoding, WorkKindQA, "", "bogus"} {
			if GeminiAllowed(k, isWork) || !KindMayWriteCode(k) {
				t.Errorf("work=%v: Gemini auto-allowed for code kind %q", isWork, k)
			}
			// Board choice on a code kind: personal repos only (Touch ID per run).
			if GeminiChoiceAllowed(k, isWork) == isWork {
				t.Errorf("work=%v: GeminiChoiceAllowed(%q) wrong", isWork, k)
			}
		}
		for _, k := range []WorkKind{WorkKindPlanning, WorkKindArchitecture, WorkKindReview, WorkKindDocs} {
			if !GeminiAllowed(k, isWork) || KindMayWriteCode(k) || !GeminiChoiceAllowed(k, isWork) {
				t.Errorf("work=%v: Gemini barred for non-code kind %q", isWork, k)
			}
		}
	}
	if GeminiCodeApprovalAllowed(true) || !GeminiCodeApprovalAllowed(false) {
		t.Error("Touch ID code approval must be personal-repo only")
	}
	if !GeminiReviewAdvisory() {
		t.Error("Gemini review must be advisory")
	}
	if len(GeminiDocAllowlist()) == 0 {
		t.Error("allowlist empty")
	}
}

func TestResolveRoute_PersonalRepoNeverUsesWorkSeat(t *testing.T) {
	p := lockedPacer(map[PoolID]string{PoolPersonalClaude: ""})
	d := ResolveRoute("coding", false, p, "", time.Now())
	for _, c := range append(d.Candidates, func() []RouteSlot {
		var out []RouteSlot
		for _, l := range d.Locked {
			out = append(out, l.Slot)
		}
		return out
	}()...) {
		if c.PoolID == PoolWorkClaude || c.Family == FamilyGemini {
			t.Fatalf("personal coding must stay on the personal Claude seat: %+v", d)
		}
	}
	if !d.AllLocked() {
		t.Fatalf("personal seat locked: want waiting, got %+v", d.Candidates)
	}
	if got, want := d.Title(), "Waiting for a Claude seat: personal seat locked (locked)"; got != want {
		t.Errorf("title = %q, want %q", got, want)
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
	if !d.AllLocked() {
		t.Fatalf("got %+v, want waiting when the 5h window is spent", d.Candidates)
	}
	if !strings.Contains(d.Title(), "5h limit") {
		t.Errorf("title %q should give the 5h reason", d.Title())
	}
}

func TestResolveRoute_AllLocked(t *testing.T) {
	p := lockedPacer(map[PoolID]string{PoolPersonalClaude: "a", PoolGeminiNative: "b"})
	d := ResolveRoute("planning", false, p, "", time.Now())
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
	// Non-code kind: the override pins Claude first, Gemini pair as fallback.
	d := ResolveRoute("planning", false, openPacer(), "sonnet", time.Now())
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
	// Code kind: Claude only.
	d = ResolveRoute("qa", false, openPacer(), "sonnet", time.Now())
	if len(d.Candidates) != 1 || d.Candidates[0].Model != "sonnet" || d.HasGemini() {
		t.Errorf("qa override: want Claude sonnet only, got %+v", d.Candidates)
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
			seenWork := false
			for _, s := range chain {
				if !isLocalClaudeSlot(s) {
					if slotFamily(s) == FamilyGemini && !GeminiAllowed(kind, isWork) {
						t.Errorf("isWork=%v kind=%s: Gemini in a code chain", isWork, kind)
					}
					continue
				}
				// Work chains end in personal-seat fallbacks after the work slots.
				if isWork && seenWork && s.PoolID == PoolPersonalClaude {
					continue
				}
				if s.PoolID != want {
					t.Errorf("isWork=%v kind=%s slot %s pool=%s, want %s", isWork, kind, s.Provider, s.PoolID, want)
				}
				seenWork = true
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

// Every kind in the routing table is honoured per repo seat: code kinds
// (coding, qa) run Claude Opus only; non-code kinds run Gemini first with a
// Claude fallback on the repo's seat (work repos then the personal seat).
func TestResolveRoute_EveryKindFirstAndFallback(t *testing.T) {
	cases := []struct {
		kind   string
		gemini string // "" = Claude only
		claude string
	}{
		{"coding", "", "opus"},
		{"qa", "", "opus"},
		{"review", "gemini-3.1-pro-high", "opus"},
		{"architecture", "gemini-3.1-pro-high", "opus"},
		{"planning", "gemini-3.8-flash-high", "sonnet"},
		{"docs", "gemini-3.8-flash-high", "sonnet"},
	}
	for _, isWork := range []bool{false, true} {
		for _, c := range cases {
			d := ResolveRoute(c.kind, isWork, openPacer(), "", time.Now())
			if d.Kind != WorkKind(c.kind) {
				t.Errorf("%s: kind normalised to %q", c.kind, d.Kind)
			}
			var want []RouteSlot
			if c.gemini != "" {
				want = append(want, RouteSlot{Family: FamilyGemini, Model: c.gemini, PoolID: PoolGeminiNative})
			}
			if isWork {
				want = append(want, RouteSlot{Family: FamilyClaude, Model: c.claude, PoolID: PoolWorkClaude, Seat: SeatWork})
			}
			want = append(want, RouteSlot{Family: FamilyClaude, Model: c.claude, PoolID: PoolPersonalClaude, Seat: SeatPersonal})
			if len(d.Candidates) != len(want) {
				t.Fatalf("%s work=%v: got %+v, want %+v", c.kind, isWork, d.Candidates, want)
			}
			for i := range want {
				if d.Candidates[i] != want[i] {
					t.Errorf("%s work=%v slot %d: got %+v, want %+v", c.kind, isWork, i, d.Candidates[i], want[i])
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

func TestResolveRoute_ReviewIsGeminiAdvisory(t *testing.T) {
	d := ResolveRoute("review", false, openPacer(), "", time.Now())
	if got, want := d.Title(), "Ran on Gemini 3.1 Pro"; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
	if got, want := d.Body(), "Kind of work: review"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// --- STA-838: the Board's explicit provider choice ---

func TestResolveRouteChoice_GeminiChosenOnNonCodeKind(t *testing.T) {
	for _, isWork := range []bool{false, true} {
		d := ResolveRouteChoice("docs", isWork, openPacer(), RouteChoice{Provider: ProviderGemini}, time.Now())
		s := mustChosen(t, d)
		if s.Family != FamilyGemini || !s.ChosenByBoard || s.Model != "gemini-3.1-pro-high" {
			t.Fatalf("work=%v: got %+v, want Board-chosen Gemini 3.1 Pro", isWork, s)
		}
		if got, want := d.Title(), "Ran on Gemini 3.1 Pro · chosen by Board"; got != want {
			t.Errorf("title = %q, want %q", got, want)
		}
		if !strings.Contains(d.Body(), "provider: gemini (chosen by Board)") {
			t.Errorf("body %q should say the Board chose Gemini", d.Body())
		}
		last := d.Candidates[len(d.Candidates)-1]
		if last.Family != FamilyClaude || last.Model != "opus" || last.Seat != SeatPersonal {
			t.Errorf("work=%v: Claude fallback = %+v, want personal Claude opus", isWork, last)
		}
	}
	d := ResolveRouteChoice("planning", false, openPacer(), RouteChoice{Provider: ProviderGemini, Model: "flash"}, time.Now())
	if s := mustChosen(t, d); s.Model != "gemini-3.8-flash-high" || d.Candidates[1].Model != "sonnet" {
		t.Errorf("flash choice: got %+v", d.Candidates)
	}
}

func TestResolveRouteChoice_GeminiChosenOnCodeKindStaysClaude(t *testing.T) {
	for _, isWork := range []bool{false, true} {
		for _, kind := range codeKinds {
			d := ResolveRouteChoice(kind, isWork, openPacer(), RouteChoice{Provider: ProviderGemini}, time.Now())
			if d.HasGemini() || !d.GeminiBarred {
				t.Fatalf("work=%v kind %q: stored gemini choice reopened Gemini: %+v", isWork, kind, d)
			}
			if s := mustChosen(t, d); s.Family != FamilyClaude {
				t.Errorf("work=%v kind %q: got %+v, want Claude", isWork, kind, s)
			}
			if !strings.Contains(d.Body(), "Gemini refused: "+GeminiCodeRule) {
				t.Errorf("body %q should say the choice was refused", d.Body())
			}
		}
	}
}

func TestResolveRouteChoice_ClaudeChosenOnNonCodeKind(t *testing.T) {
	for _, kind := range nonCodeKinds {
		d := ResolveRouteChoice(kind, false, openPacer(), RouteChoice{Provider: ProviderClaude}, time.Now())
		if d.HasGemini() {
			t.Fatalf("%s: provider=claude still has Gemini: %+v", kind, d.Candidates)
		}
		if s := mustChosen(t, d); s.Family != FamilyClaude || s.Seat != SeatPersonal {
			t.Errorf("%s: got %+v, want personal Claude", kind, s)
		}
		d = ResolveRouteChoice(kind, true, openPacer(), RouteChoice{Provider: ProviderClaude, Model: "opus"}, time.Now())
		if d.HasGemini() || len(d.Candidates) != 2 || d.Candidates[0].Seat != SeatWork || d.Candidates[1].Seat != SeatPersonal {
			t.Errorf("%s work opus: got %+v, want work then personal Claude", kind, d.Candidates)
		}
	}
}

func TestValidateTaskChoice(t *testing.T) {
	ok := []struct{ kind, provider, model, wantP, wantM string }{
		{"coding", "gemini", "", "gemini", ""}, // personal repo: allowed, Touch ID per run
		{"coding", "", "", "", ""},
		{"coding", "claude", "", "claude", ""},
		{"coding", "claude", "Sonnet", "claude", "sonnet"},
		{"planning", "gemini", "", "gemini", ""},
		{"docs", "agy", "gemini-3.8-flash", "gemini", "gemini-3.8-flash-high"},
		{"review", "gemini", "opus", "gemini", "gemini-3.1-pro-high"},
		{"architecture", "claude", "opus", "claude", "opus"},
	}
	for _, c := range ok {
		got, err := ValidateTaskChoice(c.kind, false, c.provider, c.model)
		if err != nil || got.Provider != c.wantP || got.Model != c.wantM {
			t.Errorf("ValidateTaskChoice(%q,%q,%q) = %+v, %v; want %s/%s", c.kind, c.provider, c.model, got, err, c.wantP, c.wantM)
		}
	}
	for _, kind := range codeKinds {
		_, err := ValidateTaskChoice(kind, true, "gemini", "")
		if !errors.Is(err, ErrGeminiCodeKind) || !strings.Contains(err.Error(), "Gemini never writes code") || !strings.Contains(err.Error(), "work repo") {
			t.Errorf("gemini on %q in a work repo: err = %v, want a clear refusal", kind, err)
		}
		if c, err := ValidateTaskChoice(kind, false, "gemini", ""); err != nil || !NeedsGeminiCodeApproval(kind, false, c) {
			t.Errorf("gemini on %q in a personal repo: %+v %v, want accepted and approval-gated", kind, c, err)
		}
	}
	for _, kind := range nonCodeKinds {
		c, err := ValidateTaskChoice(kind, true, "gemini", "")
		if err != nil || NeedsGeminiCodeApproval(kind, true, c) || NeedsGeminiCodeApproval(kind, false, c) {
			t.Errorf("gemini on non-code %q: %v; must need no approval", kind, err)
		}
	}
	for _, bad := range [][3]string{{"coding", "openai", ""}, {"planning", "gemini", "gpt-9"}, {"coding", "", "gemini-3.1-pro"}, {"coding", "claude", "haiku"}} {
		if _, err := ValidateTaskChoice(bad[0], false, bad[1], bad[2]); err == nil {
			t.Errorf("ValidateTaskChoice(%q) accepted", bad)
		}
	}
}

func TestChoiceFromStoredDropsInvalid(t *testing.T) {
	if c := ChoiceFromStored("openai", "x"); c != (RouteChoice{}) {
		t.Errorf("invalid provider: got %+v, want default", c)
	}
	if c := ChoiceFromStored("gemini", "gpt-9"); c != (RouteChoice{Provider: ProviderGemini}) {
		t.Errorf("invalid model: got %+v, want gemini default model", c)
	}
}

// Board addition: a consumed Touch ID approval opens Gemini for one code run
// in a personal repo; in a work repo it changes nothing.
func TestResolveRouteChoice_CodeApprovalPersonalOnly(t *testing.T) {
	c := RouteChoice{Provider: ProviderGemini, CodeApprovalID: "gate-1"}
	d := ResolveRouteChoice("coding", false, openPacer(), c, time.Now())
	s := mustChosen(t, d)
	if s.Family != FamilyGemini || !s.ChosenByBoard || d.GeminiBarred || d.GeminiCodeApprovalID != "gate-1" {
		t.Fatalf("approved personal coding: got %+v route %+v", s, d)
	}
	if !strings.Contains(d.Body(), "approved by Board Touch ID") {
		t.Errorf("body %q should name the approval", d.Body())
	}
	for _, isWork := range []bool{true} {
		d := ResolveRouteChoice("coding", isWork, openPacer(), c, time.Now())
		if d.HasGemini() || d.GeminiCodeApprovalID != "" || !d.GeminiBarred {
			t.Fatalf("work repo approval must change nothing: %+v", d)
		}
	}
	// No approval: personal coding with provider=gemini stays Claude.
	d = ResolveRouteChoice("coding", false, openPacer(), RouteChoice{Provider: ProviderGemini}, time.Now())
	if d.HasGemini() || d.GeminiCodeApprovalID != "" {
		t.Fatalf("unapproved: got %+v", d)
	}
	// An approval id without provider=gemini opens nothing.
	d = ResolveRouteChoice("coding", false, openPacer(), RouteChoice{CodeApprovalID: "gate-1"}, time.Now())
	if d.HasGemini() {
		t.Fatalf("approval without gemini choice: got %+v", d)
	}
}
