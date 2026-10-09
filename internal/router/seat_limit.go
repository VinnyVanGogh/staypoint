package router

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A Claude seat can run out before the pacer's quota data shows it: the CLI
// itself answers "You've hit your session limit · resets 4:20am" and exits.
// That answer is the most current quota reading there is, so the daemon
// records it here and every pacer load overlays it (LoadPacerState), which
// makes routing skip the seat and the run queue wait for its reset.

// seatLimitDefaultWait is how long a seat stays marked out when its limit
// message carries no parseable reset time. Short on purpose: a wrong guess
// costs one quick refused CLI call, which re-marks the seat.
const seatLimitDefaultWait = 30 * time.Minute

// seatLimitMaxWait caps a parsed reset time (a weekly window is the longest).
const seatLimitMaxWait = 7 * 24 * time.Hour

type seatLimit struct {
	until  time.Time
	reason string
}

var (
	seatLimitsMu sync.Mutex
	seatLimits   = map[PoolID]seatLimit{}
)

// NoteSeatLimit records that pool's CLI refused work with a limit message
// until the given time. A later until for the same pool wins.
func NoteSeatLimit(pool PoolID, until time.Time, reason string) {
	seatLimitsMu.Lock()
	defer seatLimitsMu.Unlock()
	if cur, ok := seatLimits[pool]; ok && cur.until.After(until) {
		return
	}
	seatLimits[pool] = seatLimit{until: until, reason: reason}
}

// ResetSeatLimits forgets every recorded seat limit (tests).
func ResetSeatLimits() {
	seatLimitsMu.Lock()
	defer seatLimitsMu.Unlock()
	seatLimits = map[PoolID]seatLimit{}
}

// ApplySeatLimits locks every pool in state whose CLI reported a limit that
// has not reset yet at now. Expired entries are dropped.
func ApplySeatLimits(state *PacerState, now time.Time) {
	if state == nil {
		return
	}
	seatLimitsMu.Lock()
	defer seatLimitsMu.Unlock()
	for id, l := range seatLimits {
		if !l.until.After(now) {
			delete(seatLimits, id)
			continue
		}
		if state.Pools == nil {
			state.Pools = make(map[PoolID]*QuotaPool)
		}
		p := state.Pools[id]
		if p == nil {
			p = &QuotaPool{ID: id}
			state.Pools[id] = p
		}
		p.IsLocked = true
		p.LockoutReason = l.reason
		if l.until.After(p.LockoutUntil) {
			p.LockoutUntil = l.until
		}
		p.TurnsRunway, p.Turns5h, p.TurnsWeekly = 0, 0, 0
	}
}

// seatLimitRe matches the CLI's own limit answers, anchored at the start of
// the message so an agent merely quoting one does not count:
//
//	You've hit your session limit · resets 4:20am
//	You've hit your weekly limit · resets Oct 12, 4am
//	Claude AI usage limit reached|1760000000
var seatLimitRe = regexp.MustCompile(`(?i)^(you['’]ve (hit|reached) your( \w+)? limit|claude( ai)? usage limit reached|(session|weekly|usage) limit reached)`)

// seatLimitMaxLen bounds a limit answer: the CLI's are one short line, an
// agent's message that happens to start with the same words is longer.
const seatLimitMaxLen = 300

// IsSeatLimitMessage reports whether text is a Claude CLI limit answer.
func IsSeatLimitMessage(text string) bool {
	t := strings.TrimSpace(text)
	return t != "" && len(t) <= seatLimitMaxLen && seatLimitRe.MatchString(t)
}

var (
	resetEpochRe = regexp.MustCompile(`\|(\d{10})\b`)
	resetDateRe  = regexp.MustCompile(`(?i)resets\s+([a-z]{3})[a-z]*\s+(\d{1,2}),?\s+(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)`)
	resetClockRe = regexp.MustCompile(`(?i)resets\s+(?:at\s+)?(\d{1,2})(?::(\d{2}))?\s*(am|pm)`)
)

// SeatLimitResetAt parses when a limit answer says the seat resets, in now's
// location. It falls back to now+seatLimitDefaultWait when the message has no
// reset time it understands, and caps the result at seatLimitMaxWait.
func SeatLimitResetAt(text string, now time.Time) time.Time {
	t, ok := parseSeatLimitReset(text, now)
	if !ok || !t.After(now) {
		return now.Add(seatLimitDefaultWait)
	}
	if t.Sub(now) > seatLimitMaxWait {
		return now.Add(seatLimitMaxWait)
	}
	return t
}

func parseSeatLimitReset(text string, now time.Time) (time.Time, bool) {
	if m := resetEpochRe.FindStringSubmatch(text); m != nil {
		sec, err := strconv.ParseInt(m[1], 10, 64)
		if err == nil {
			return time.Unix(sec, 0).In(now.Location()), true
		}
	}
	if m := resetDateRe.FindStringSubmatch(text); m != nil {
		mon, err := time.Parse("Jan", strings.ToUpper(m[1][:1])+strings.ToLower(m[1][1:]))
		if err == nil {
			day, _ := strconv.Atoi(m[2])
			h, min := clock12(m[3], m[4], m[5])
			t := time.Date(now.Year(), mon.Month(), day, h, min, 0, 0, now.Location())
			if !t.After(now) {
				t = t.AddDate(1, 0, 0)
			}
			return t, true
		}
	}
	if m := resetClockRe.FindStringSubmatch(text); m != nil {
		h, min := clock12(m[1], m[2], m[3])
		t := time.Date(now.Year(), now.Month(), now.Day(), h, min, 0, 0, now.Location())
		if !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
		return t, true
	}
	return time.Time{}, false
}

// clock12 converts "4", "20", "am" to 24-hour hour and minute.
func clock12(hs, ms, ampm string) (int, int) {
	h, _ := strconv.Atoi(hs)
	m, _ := strconv.Atoi(ms)
	h %= 12
	if strings.EqualFold(ampm, "pm") {
		h += 12
	}
	return h, m
}
