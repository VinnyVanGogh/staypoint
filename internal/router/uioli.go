package router

import (
	"fmt"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/config"
)

// Use-it-or-lose-it (UIOLI) routing: a weekly quota window that is about to
// reset with unspent quota is wasted capacity, so near the end of the window the
// router prefers the pool that would otherwise expire unused.
const (
	DefaultUIOLIWindowHours     = 24.0 // only the last day of the weekly window
	DefaultUIOLIMinRemainingPct = 15.0 // floor at the start of the window
	DefaultUIOLIFinalFloorPct   = 3.0  // floor right before the reset: crumbs still expire
	DefaultUIOLIMinPctPerHour   = 1.0  // remaining% / hours-to-reset burn-down needed
	DefaultUIOLIFiveHourFloor   = 10.0 // 5h window must have more than this left

	// UIOLIHighPriorityModel is the model UIOLI steers to on the pressured
	// Claude seat when no model was explicitly requested.
	UIOLIHighPriorityModel = "claude-fable-5-1"
)

// UIOLIConfig tunes end-of-window burn-down routing. Zero values select the
// defaults above. It is inactive outside the end-of-window case by design:
// hours-to-reset must be <= WindowHours before it can trigger.
type UIOLIConfig struct {
	Disabled        bool
	WindowHours     float64
	MinRemainingPct float64
	FinalFloorPct   float64
	MinPctPerHour   float64
	// FiveHourFloorPct is the 5h-window remaining% at or below which UIOLI
	// stops steering, so burning down the weekly window can't drive the 5h
	// window into a lock that pushes work-repo runs to the personal fallback.
	FiveHourFloorPct float64
	// HighPriorityOnly limits the model steer to high-priority work. Off by
	// default: in the window every run burns quota that would expire unused.
	HighPriorityOnly bool
}

func (c UIOLIConfig) withDefaults() UIOLIConfig {
	if c.WindowHours <= 0 {
		c.WindowHours = DefaultUIOLIWindowHours
	}
	if c.MinRemainingPct <= 0 {
		c.MinRemainingPct = DefaultUIOLIMinRemainingPct
	}
	if c.FinalFloorPct <= 0 {
		c.FinalFloorPct = DefaultUIOLIFinalFloorPct
	}
	if c.FinalFloorPct > c.MinRemainingPct {
		c.FinalFloorPct = c.MinRemainingPct
	}
	if c.MinPctPerHour <= 0 {
		c.MinPctPerHour = DefaultUIOLIMinPctPerHour
	}
	if c.FiveHourFloorPct <= 0 {
		c.FiveHourFloorPct = DefaultUIOLIFiveHourFloor
	}
	return c
}

// UIOLIPressure is the reset-time-aware burn-down state of a pool.
type UIOLIPressure struct {
	Active         bool
	RemainingPct   float64
	HoursToReset   float64
	FloorPct       float64 // minimum remaining% at this point in the window
	PctPerHourNeed float64 // remaining% / hours-to-reset
}

// UIOLIPressure evaluates whether the pool's weekly window is in the
// end-of-window burn-down zone: known data, a future reset within WindowHours,
// at least FloorPct unspent (inclusive), and a required burn rate of at least
// MinPctPerHour, with the 5h window above FiveHourFloorPct (or unreported).
// The floor falls linearly from MinRemainingPct at the start of
// the window to FinalFloorPct at the reset, so the last few percent still get
// used instead of expiring.
func (p *QuotaPool) UIOLIPressure(now time.Time, cfg UIOLIConfig) UIOLIPressure {
	cfg = cfg.withDefaults()
	var out UIOLIPressure
	if cfg.Disabled || p == nil || p.IsLocked || !p.Weekly.Known || !p.Weekly.ResetsAt.After(now) {
		return out
	}
	if p.FiveHour.Known && p.FiveHour.RemainingPct <= cfg.FiveHourFloorPct {
		return out
	}
	out.RemainingPct = p.Weekly.RemainingPct
	out.HoursToReset = p.Weekly.ResetsAt.Sub(now).Hours()
	if out.HoursToReset > cfg.WindowHours {
		return out
	}
	out.FloorPct = max(cfg.MinRemainingPct*out.HoursToReset/cfg.WindowHours, cfg.FinalFloorPct)
	if out.RemainingPct < out.FloorPct {
		return out
	}
	// Floor the divisor so a reset seconds away does not produce Inf.
	out.PctPerHourNeed = out.RemainingPct / max(out.HoursToReset, 0.25)
	out.Active = out.PctPerHourNeed >= cfg.MinPctPerHour
	return out
}

// Steers reports whether active pressure changes the model of this run: always,
// unless HighPriorityOnly is set and the run is not high priority.
func (c UIOLIConfig) Steers(highPriority bool) bool {
	return !c.HighPriorityOnly || highPriority
}

// UIOLIFromConfig builds the UIOLI tuning from user config; nil yields defaults.
func UIOLIFromConfig(c *config.Config) UIOLIConfig {
	if c == nil {
		return UIOLIConfig{}
	}
	return UIOLIConfig{
		Disabled:         c.UIOLIDisabled,
		WindowHours:      c.UIOLIWindowHours,
		MinRemainingPct:  c.UIOLIMinRemainingPct,
		FinalFloorPct:    c.UIOLIFinalFloorPct,
		MinPctPerHour:    c.UIOLIMinPctPerHour,
		FiveHourFloorPct: c.UIOLIFiveHourFloorPct,
		HighPriorityOnly: c.UIOLIHighPriorityOnly,
	}
}

// IsHighPriority reports whether a stored task priority counts as high.
func IsHighPriority(priority string) bool {
	switch strings.ToLower(strings.TrimSpace(priority)) {
	case "high", "urgent", "critical":
		return true
	}
	return false
}

func (u UIOLIPressure) describe() string {
	return fmt.Sprintf("use-it-or-lose-it: %.0f%% weekly left, resets in %s (%.1f%%/h to burn down)",
		u.RemainingPct, FormatDuration(time.Duration(u.HoursToReset*float64(time.Hour))), u.PctPerHourNeed)
}

// Note is the log and run-timeline line for an active steer:
// "UIOLI active: pool work-claude, 15% weekly left, 6.9h to reset, model claude-fable-5-1".
func (u UIOLIPressure) Note(pool PoolID, model string) string {
	return fmt.Sprintf("UIOLI active: pool %s, %.1f%% weekly left, %.1fh to reset, model %s",
		pool, u.RemainingPct, u.HoursToReset, model)
}
