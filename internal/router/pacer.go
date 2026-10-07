package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type PoolID string

const (
	PoolWorkClaude     PoolID = "work-claude"
	PoolPersonalClaude PoolID = "personal-claude"
	PoolGeminiNative   PoolID = "gemini-native"
	Pool3PClaude       PoolID = "3p-claude"
)

type QuotaWindow struct {
	UsedPct      float64   `json:"used_pct"`
	RemainingPct float64   `json:"remaining_pct"`
	ResetsAt     time.Time `json:"resets_at"`
	IsLocked     bool      `json:"is_locked"`
	// Known is false when no source reported this window; UsedPct/RemainingPct
	// are then optimistic defaults, not measurements, and must not be displayed
	// as data or used for routing pressure.
	Known bool `json:"known"`
}

type QuotaPool struct {
	ID            PoolID      `json:"id"`
	Name          string      `json:"name"`
	AccountEmail  string      `json:"account_email,omitempty"`
	FiveHour      QuotaWindow `json:"five_hour"`
	Weekly        QuotaWindow `json:"weekly"`
	IsLocked      bool        `json:"is_locked"`
	LockoutUntil  time.Time   `json:"lockout_until,omitempty"`
	LockoutReason string      `json:"lockout_reason,omitempty"`
	TurnsRunway   int         `json:"turns_runway"`
	Turns5h       int         `json:"turns_5h"`
	TurnsWeekly   int         `json:"turns_weekly"`
	BurnRate5h    float64     `json:"burn_rate_5h"` // % per turn
	BurnRateW     float64     `json:"burn_rate_w"`  // % per turn
	LastUpdated   time.Time   `json:"last_updated"`
}

type PacerState struct {
	Pools       map[PoolID]*QuotaPool `json:"pools"`
	LastUpdated time.Time             `json:"last_updated"`
}

type rawSampleJSON struct {
	Timestamp      string  `json:"ts"`
	SessionID      string  `json:"session_id"`
	ModelID        string  `json:"model_id"`
	FiveHourPct    float64 `json:"five_hour_pct"`
	SevenDayPct    float64 `json:"seven_day_pct"`
	FiveHourResets float64 `json:"five_hour_resets_at"`
	SevenDayResets float64 `json:"seven_day_resets_at"`
	AccountEmail   string  `json:"account_email"`
}

type rawEstimatesJSON struct {
	PctPerTurn struct {
		FiveHour struct {
			MeanPct float64 `json:"mean_pct"`
		} `json:"five_hour"`
		SevenDay struct {
			MeanPct float64 `json:"mean_pct"`
		} `json:"seven_day"`
	} `json:"pct_per_turn"`
}

// LoadPacerState reads live quotas cached in the StayPoint database by the
// in-process quota poller (internal/telemetry/quota),
// statusline samples from ~/.config/token-telemetry/statusline-samples.ndjson,
// and burn rates from ~/.config/token-telemetry/statusline-estimates.json.
func LoadPacerState() (*PacerState, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}

	state := &PacerState{
		Pools:       make(map[PoolID]*QuotaPool),
		LastUpdated: time.Now(),
	}

	// Default burn rates
	claudeBurn5h := 5.62
	claudeBurnW := 1.26
	geminiBurn5h := 1.50
	geminiBurnW := 0.40

	// Try reading statusline-estimates.json for dynamically learned burn rates
	estimatesPath := filepath.Join(home, ".config", "token-telemetry", "statusline-estimates.json")
	if estData, err := os.ReadFile(estimatesPath); err == nil {
		var est rawEstimatesJSON
		if json.Unmarshal(estData, &est) == nil {
			if est.PctPerTurn.FiveHour.MeanPct > 0.01 {
				claudeBurn5h = est.PctPerTurn.FiveHour.MeanPct
			}
			if est.PctPerTurn.SevenDay.MeanPct > 0.01 {
				claudeBurnW = est.PctPerTurn.SevenDay.MeanPct
			}
		}
	}

	// Initialize the 4 pools
	state.Pools[PoolWorkClaude] = &QuotaPool{
		ID:         PoolWorkClaude,
		Name:       "Claude (Work)",
		BurnRate5h: claudeBurn5h,
		BurnRateW:  claudeBurnW,
		FiveHour:   QuotaWindow{RemainingPct: 100},
		Weekly:     QuotaWindow{RemainingPct: 100},
	}
	state.Pools[PoolPersonalClaude] = &QuotaPool{
		ID:         PoolPersonalClaude,
		Name:       "Claude (Personal)",
		BurnRate5h: claudeBurn5h,
		BurnRateW:  claudeBurnW,
		FiveHour:   QuotaWindow{RemainingPct: 100},
		Weekly:     QuotaWindow{RemainingPct: 100},
	}
	state.Pools[PoolGeminiNative] = &QuotaPool{
		ID:         PoolGeminiNative,
		Name:       "Gemini Native",
		BurnRate5h: geminiBurn5h,
		BurnRateW:  geminiBurnW,
		FiveHour:   QuotaWindow{RemainingPct: 100},
		Weekly:     QuotaWindow{RemainingPct: 100},
	}
	state.Pools[Pool3PClaude] = &QuotaPool{
		ID:         Pool3PClaude,
		Name:       "Antigravity 3P",
		BurnRate5h: claudeBurn5h,
		BurnRateW:  claudeBurnW,
		FiveHour:   QuotaWindow{RemainingPct: 100},
		Weekly:     QuotaWindow{RemainingPct: 100},
	}

	// 1. Apply cached live quotas. Stale or missing rows are ignored, which
	// fails open to the statusline samples and optimistic defaults below.
	applyLiveQuotas(state, time.Now())

	// 2. Read latest statusline-samples.ndjson (fast tail read)
	samplesPath := filepath.Join(home, ".config", "token-telemetry", "statusline-samples.ndjson")
	readSamplesTail(samplesPath, state)

	// 2b. Overlay from ~/.config/rate-limits/state.json if available
	stateJSONPath := filepath.Join(home, ".config", "rate-limits", "state.json")
	applyStateJSON(stateJSONPath, state)

	// 3. Evaluate lockouts from window thresholds
	now := time.Now()
	for _, pool := range state.Pools {
		// Auto-reset windows whose reset timestamp has passed. Clear the stale
		// ResetsAt so the UI does not show "resets soon" against a past timestamp.
		if !pool.FiveHour.ResetsAt.IsZero() && now.After(pool.FiveHour.ResetsAt) && pool.FiveHour.ResetsAt.After(pool.LastUpdated) {
			pool.FiveHour.UsedPct = 0.0
			pool.FiveHour.RemainingPct = 100.0
			pool.FiveHour.Known = true
			pool.FiveHour.IsLocked = false
			pool.FiveHour.ResetsAt = time.Time{}
		}
		if !pool.Weekly.ResetsAt.IsZero() && now.After(pool.Weekly.ResetsAt) && pool.Weekly.ResetsAt.After(pool.LastUpdated) {
			pool.Weekly.UsedPct = 0.0
			pool.Weekly.RemainingPct = 100.0
			pool.Weekly.Known = true
			pool.Weekly.IsLocked = false
			pool.Weekly.ResetsAt = time.Time{}
		}

		if pool.FiveHour.RemainingPct <= 0.0 || pool.FiveHour.UsedPct >= 100.0 {
			if pool.FiveHour.ResetsAt.After(now) {
				pool.IsLocked = true
				pool.FiveHour.IsLocked = true
				pool.LockoutUntil = pool.FiveHour.ResetsAt
				pool.LockoutReason = "5-hour quota exhausted (100% used)"
			}
		} else if pool.Weekly.RemainingPct <= 0.0 || pool.Weekly.UsedPct >= 100.0 {
			if pool.Weekly.ResetsAt.After(now) {
				pool.IsLocked = true
				pool.Weekly.IsLocked = true
				pool.LockoutUntil = pool.Weekly.ResetsAt
				pool.LockoutReason = "Weekly quota exhausted (100% used)"
			}
		}

		// Calculate turns runway
		if pool.IsLocked {
			pool.TurnsRunway = 0
			pool.Turns5h = 0
			pool.TurnsWeekly = 0
		} else {
			if pool.BurnRate5h > 0 {
				pool.Turns5h = int(pool.FiveHour.RemainingPct / pool.BurnRate5h)
			}
			if pool.BurnRateW > 0 {
				pool.TurnsWeekly = int(pool.Weekly.RemainingPct / pool.BurnRateW)
			}
			pool.TurnsRunway = pool.Turns5h
			if pool.TurnsWeekly < pool.TurnsRunway {
				pool.TurnsRunway = pool.TurnsWeekly
			}
			if pool.TurnsRunway < 0 {
				pool.TurnsRunway = 0
			}
		}
	}

	return state, nil
}

// FormatPct renders a window's used or remaining percentage, or "n/a" when the
// window was never reported so a missing reading is not shown as "0%".
func (w QuotaWindow) FormatPct(remaining bool, prec int) string {
	if !w.Known {
		return "n/a"
	}
	v := w.UsedPct
	if remaining {
		v = w.RemainingPct
	}
	return fmt.Sprintf("%.*f%%", prec, v)
}

// FormatReset renders a reset time relative to now, e.g. "today 4:00 PM",
// "tomorrow 9:00 AM" or "Fri 4:00 PM". Empty when the reset time is unknown.
func FormatReset(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	t = t.In(now.Location())
	clock := t.Format("3:04 PM")
	ty, tm, td := t.Date()
	ny, nm, nd := now.Date()
	if ty == ny && tm == nm && td == nd {
		return "today " + clock
	}
	ty, tm, td = now.AddDate(0, 0, 1).Date()
	if t.Year() == ty && t.Month() == tm && t.Day() == td {
		return "tomorrow " + clock
	}
	return t.Format("Mon ") + clock
}

// maxSampleAge is the oldest statusline sample readSamplesTail will use.
const maxSampleAge = 7 * 24 * time.Hour

// readSamplesTail performs a fast reverse-seek read on statusline-samples.ndjson
// to capture the most recent samples for work and personal Claude accounts.
func readSamplesTail(path string, state *PacerState) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil || stat.Size() == 0 {
		return
	}

	tailBytes := int64(128 * 1024)
	if stat.Size() < tailBytes {
		tailBytes = stat.Size()
	}

	offset := stat.Size() - tailBytes
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return
	}

	buf := make([]byte, tailBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return
	}
	buf = buf[:n]

	// Split by newline and process backwards
	lines := bytes.Split(buf, []byte("\n"))
	workFound := false
	personalFound := false

	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}

		var sample rawSampleJSON
		if err := json.Unmarshal(line, &sample); err != nil {
			continue
		}
		// A sample older than the weekly window says nothing about either
		// window now. Using it let a seat that stopped sampling (e.g. work
		// moved to its own config dir) show a stale or auto-reset 0% as if
		// it were measured.
		if ts, err := time.Parse(time.RFC3339, sample.Timestamp); err == nil && time.Since(ts) > maxSampleAge {
			continue
		}

		email := strings.ToLower(sample.AccountEmail)
		isWork := !strings.Contains(email, "gmail.com") && !strings.Contains(email, "personal") && email != ""
		isPersonal := strings.Contains(email, "gmail.com") || (!isWork && email != "")

		if isWork && !workFound {
			p := state.Pools[PoolWorkClaude]
			sampleTime, _ := time.Parse(time.RFC3339, sample.Timestamp)
			if p.LastUpdated.IsZero() || sampleTime.After(p.LastUpdated) {
				p.FiveHour.Known, p.Weekly.Known = true, true
				p.FiveHour.UsedPct = sample.FiveHourPct
				p.FiveHour.RemainingPct = math.Max(0, 100.0-sample.FiveHourPct)
				if sample.FiveHourResets > 0 {
					p.FiveHour.ResetsAt = time.Unix(int64(sample.FiveHourResets), 0)
				}
				p.Weekly.UsedPct = sample.SevenDayPct
				p.Weekly.RemainingPct = math.Max(0, 100.0-sample.SevenDayPct)
				if sample.SevenDayResets > 0 {
					p.Weekly.ResetsAt = time.Unix(int64(sample.SevenDayResets), 0)
				}
				p.LastUpdated = sampleTime
			} else {
				if !p.Weekly.Known && sample.SevenDayResets > 0 && time.Unix(int64(sample.SevenDayResets), 0).After(time.Now()) {
					p.Weekly.Known = true
					p.Weekly.UsedPct = sample.SevenDayPct
					p.Weekly.RemainingPct = math.Max(0, 100.0-sample.SevenDayPct)
					p.Weekly.ResetsAt = time.Unix(int64(sample.SevenDayResets), 0)
				}
				if !p.FiveHour.Known && sample.FiveHourResets > 0 && time.Unix(int64(sample.FiveHourResets), 0).After(time.Now()) {
					p.FiveHour.Known = true
					p.FiveHour.UsedPct = sample.FiveHourPct
					p.FiveHour.RemainingPct = math.Max(0, 100.0-sample.FiveHourPct)
					p.FiveHour.ResetsAt = time.Unix(int64(sample.FiveHourResets), 0)
				}
			}
			workFound = true
		} else if isPersonal && !personalFound {
			p := state.Pools[PoolPersonalClaude]
			sampleTime, _ := time.Parse(time.RFC3339, sample.Timestamp)
			// Only override if newer or if state.json was missing data
			if p.LastUpdated.IsZero() || sampleTime.After(p.LastUpdated) {
				p.FiveHour.Known, p.Weekly.Known = true, true
				p.FiveHour.UsedPct = sample.FiveHourPct
				p.FiveHour.RemainingPct = math.Max(0, 100.0-sample.FiveHourPct)
				if sample.FiveHourResets > 0 {
					p.FiveHour.ResetsAt = time.Unix(int64(sample.FiveHourResets), 0)
				}
				p.Weekly.UsedPct = sample.SevenDayPct
				p.Weekly.RemainingPct = math.Max(0, 100.0-sample.SevenDayPct)
				if sample.SevenDayResets > 0 {
					p.Weekly.ResetsAt = time.Unix(int64(sample.SevenDayResets), 0)
				}
				p.LastUpdated = sampleTime
			} else {
				if !p.Weekly.Known && sample.SevenDayResets > 0 && time.Unix(int64(sample.SevenDayResets), 0).After(time.Now()) {
					p.Weekly.Known = true
					p.Weekly.UsedPct = sample.SevenDayPct
					p.Weekly.RemainingPct = math.Max(0, 100.0-sample.SevenDayPct)
					p.Weekly.ResetsAt = time.Unix(int64(sample.SevenDayResets), 0)
				}
				if !p.FiveHour.Known && sample.FiveHourResets > 0 && time.Unix(int64(sample.FiveHourResets), 0).After(time.Now()) {
					p.FiveHour.Known = true
					p.FiveHour.UsedPct = sample.FiveHourPct
					p.FiveHour.RemainingPct = math.Max(0, 100.0-sample.FiveHourPct)
					p.FiveHour.ResetsAt = time.Unix(int64(sample.FiveHourResets), 0)
				}
			}
			personalFound = true
		}

		if workFound && personalFound {
			break
		}
	}
}

// FormatDuration formats remaining duration nicely (e.g. "42m", "3h 12m", "2d 4h")
func FormatDuration(d time.Duration) string {
	if d <= 0 {
		return "now"
	}
	d = d.Round(time.Minute)
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60

	if days > 0 {
		return fmt.Sprintf("%dd %dh", days, hours)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

// OnWeeklyPace reports whether the pool's weekly burn will last until its reset.
// Mirrors calculate_pacing in rate-limit-notifier.py: on pace when burn rate
// (%/day so far) <= sustainable budget (%/day to reset) or projected exhaustion
// falls after the reset. An exhausted weekly window is never on pace.
func (p *QuotaPool) OnWeeklyPace(now time.Time) bool {
	used := p.Weekly.UsedPct
	remaining := math.Max(0, 100.0-used)
	if remaining <= 0 {
		return false
	}
	if used <= 0 {
		return true
	}
	daysLeft := 5.0 // fallback when the reset time is missing, same as the notifier
	if p.Weekly.ResetsAt.After(now) {
		daysLeft = math.Max(0.01, p.Weekly.ResetsAt.Sub(now).Hours()/24.0)
	}
	daysElapsed := math.Max(0.1, 7.0-daysLeft)
	burn := used / daysElapsed
	budget := remaining / daysLeft
	return burn <= budget || remaining/burn >= daysLeft
}

type rawStateJSON struct {
	Quotas map[string]struct {
		FiveHourRemaining float64 `json:"five_hour_remaining"`
		FiveHourUsed      float64 `json:"five_hour_used"`
		FiveHourResetsAt  float64 `json:"five_hour_resets_at"`
		WeeklyRemaining   float64 `json:"weekly_remaining"`
		WeeklyUsed        float64 `json:"weekly_used"`
		WeeklyResetsAt    float64 `json:"weekly_resets_at"`
		LastUpdated       string  `json:"last_updated"`
	} `json:"quotas"`
	Lockouts map[string]struct {
		Locked       bool   `json:"locked"`
		ResetsAt     int64  `json:"resets_at"`
		ResetTimeStr string `json:"reset_time_str"`
	} `json:"lockouts"`
}

func applyStateJSON(path string, state *PacerState) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var sj rawStateJSON
	if err := json.Unmarshal(data, &sj); err != nil {
		return
	}

	// The writers also keep a generic "Claude" key holding whichever seat's
	// statusline ran last. Map order is random, so letting it compete with the
	// seat keys flipped the personal pool between seats per load (STA-283). It
	// only backfills personal when no personal entry exists.
	hasPersonal := false
	for qName := range sj.Quotas {
		lower := strings.ToLower(qName)
		if strings.Contains(lower, "personal") && strings.Contains(lower, "claude") {
			hasPersonal = true
		}
	}

	for qName, qVal := range sj.Quotas {
		lower := strings.ToLower(qName)
		var targetPoolID PoolID
		switch {
		case strings.Contains(lower, "3p") || strings.Contains(lower, "antigravity"):
			targetPoolID = Pool3PClaude
		case strings.Contains(lower, "work") && strings.Contains(lower, "claude"):
			targetPoolID = PoolWorkClaude
		case strings.Contains(lower, "personal") && strings.Contains(lower, "claude"):
			targetPoolID = PoolPersonalClaude
		case strings.Contains(lower, "claude"):
			if hasPersonal {
				continue
			}
			targetPoolID = PoolPersonalClaude
		case strings.Contains(lower, "gemini"):
			targetPoolID = PoolGeminiNative
		default:
			continue
		}

		p := state.Pools[targetPoolID]
		if p == nil {
			continue
		}

		p.FiveHour.UsedPct = qVal.FiveHourUsed
		p.FiveHour.RemainingPct = qVal.FiveHourRemaining
		p.FiveHour.Known = true
		if qVal.FiveHourResetsAt > 0 {
			p.FiveHour.ResetsAt = time.Unix(int64(qVal.FiveHourResetsAt), 0)
		}

		p.Weekly.UsedPct = qVal.WeeklyUsed
		p.Weekly.RemainingPct = qVal.WeeklyRemaining
		p.Weekly.Known = true
		if qVal.WeeklyResetsAt > 0 {
			p.Weekly.ResetsAt = time.Unix(int64(qVal.WeeklyResetsAt), 0)
		}

		if qVal.LastUpdated != "" {
			if t, err := time.Parse(time.RFC3339, qVal.LastUpdated); err == nil {
				if t.After(p.LastUpdated) {
					p.LastUpdated = t
				}
			}
		}
	}

	for lName, lVal := range sj.Lockouts {
		lower := strings.ToLower(lName)
		var targetPoolID PoolID
		switch {
		case strings.Contains(lower, "3p") || strings.Contains(lower, "antigravity"):
			targetPoolID = Pool3PClaude
		case strings.Contains(lower, "work") && strings.Contains(lower, "claude"):
			targetPoolID = PoolWorkClaude
		case strings.Contains(lower, "personal") || strings.Contains(lower, "claude"):
			targetPoolID = PoolPersonalClaude
		case strings.Contains(lower, "gemini"):
			targetPoolID = PoolGeminiNative
		default:
			continue
		}

		if p := state.Pools[targetPoolID]; p != nil && lVal.Locked {
			p.IsLocked = true
			p.LockoutReason = "Rate limit lockout triggered"
			if lVal.ResetsAt > 0 {
				p.LockoutUntil = time.Unix(lVal.ResetsAt, 0)
			}
		}
	}
}
