package router

import (
	"strings"
	"testing"
	"time"
)

func TestIsSeatLimitMessage(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"You've hit your session limit · resets 4:20am", true},
		{"You’ve hit your weekly limit · resets Oct 12, 4am", true},
		{"You've hit your limit · resets 9pm (America/Los_Angeles)", true},
		{"Claude AI usage limit reached|1760000000", true},
		{"  Session limit reached ∙ resets 4am  ", true},
		{"The log said: You've hit your session limit, so I added detection.", false},
		{"You've hit your session limit " + strings.Repeat("and here is a long agent message ", 20), false},
		{"", false},
		{"working on it", false},
	}
	for _, c := range cases {
		if got := IsSeatLimitMessage(c.text); got != c.want {
			t.Errorf("IsSeatLimitMessage(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestSeatLimitResetAt(t *testing.T) {
	loc := time.FixedZone("PT", -7*3600)
	now := time.Date(2026, 10, 9, 3, 14, 0, 0, loc)
	cases := []struct {
		name string
		text string
		want time.Time
	}{
		{"clock later today", "You've hit your session limit · resets 4:20am", time.Date(2026, 10, 9, 4, 20, 0, 0, loc)},
		{"clock already passed rolls to tomorrow", "You've hit your session limit · resets 1am", time.Date(2026, 10, 10, 1, 0, 0, 0, loc)},
		{"pm", "You've hit your limit · resets 9pm (America/Los_Angeles)", time.Date(2026, 10, 9, 21, 0, 0, 0, loc)},
		{"weekly with date", "You've hit your weekly limit · resets Oct 12, 4am", time.Date(2026, 10, 12, 4, 0, 0, 0, loc)},
		{"epoch", "Claude AI usage limit reached|1791550000", time.Unix(1791550000, 0).In(loc)},
		{"no reset time uses default wait", "You've hit your session limit", now.Add(seatLimitDefaultWait)},
		{"capped at a week", "You've hit your weekly limit · resets Sep 1, 4am", now.Add(seatLimitMaxWait)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SeatLimitResetAt(c.text, now); !got.Equal(c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestApplySeatLimits_LocksUntilResetThenClears(t *testing.T) {
	t.Cleanup(ResetSeatLimits)
	now := time.Now()
	NoteSeatLimit(PoolWorkClaude, now.Add(time.Hour), "You've hit your session limit · resets 4:20am")

	state := &PacerState{Pools: map[PoolID]*QuotaPool{}}
	ApplySeatLimits(state, now)
	locked, why := PoolLockReason(state.Pools[PoolWorkClaude], now)
	if !locked || why != "You've hit your session limit · resets 4:20am" {
		t.Fatalf("work seat: locked=%v why=%q", locked, why)
	}
	if locked, _ := PoolLockReason(state.Pools[PoolPersonalClaude], now); locked {
		t.Fatalf("personal seat must stay open")
	}

	later := &PacerState{Pools: map[PoolID]*QuotaPool{}}
	ApplySeatLimits(later, now.Add(2*time.Hour))
	if locked, _ := PoolLockReason(later.Pools[PoolWorkClaude], now.Add(2*time.Hour)); locked {
		t.Fatalf("work seat must reopen after its reset")
	}
}

// Both seats out: a work-repo coding route is all-locked (the run queue
// waits), and Gemini is never offered in its place.
func TestSeatLimits_BothSeatsOutRouteWaitsNoGemini(t *testing.T) {
	t.Cleanup(ResetSeatLimits)
	now := time.Now()
	NoteSeatLimit(PoolWorkClaude, now.Add(time.Hour), "session limit")
	NoteSeatLimit(PoolPersonalClaude, now.Add(time.Hour), "session limit")
	state := &PacerState{Pools: map[PoolID]*QuotaPool{}}
	ApplySeatLimits(state, now)
	r := ResolveRoute("coding", true, state, "", now)
	if !r.AllLocked() || r.HasGemini() {
		t.Fatalf("want all-locked Claude-only route, got %+v", r)
	}
	if !strings.HasPrefix(r.Title(), "Waiting for a Claude seat") {
		t.Errorf("title %q", r.Title())
	}
}

// Work seat out: the route runs on the personal seat.
func TestSeatLimits_WorkOutRouteRunsPersonal(t *testing.T) {
	t.Cleanup(ResetSeatLimits)
	now := time.Now()
	NoteSeatLimit(PoolWorkClaude, now.Add(time.Hour), "You've hit your session limit")
	state := &PacerState{Pools: map[PoolID]*QuotaPool{}}
	ApplySeatLimits(state, now)
	r := ResolveRoute("coding", true, state, "", now)
	s, ok := r.Chosen()
	if !ok || s.Seat != SeatPersonal || s.Family != FamilyClaude {
		t.Fatalf("want personal Claude seat, got %+v", r)
	}
}
