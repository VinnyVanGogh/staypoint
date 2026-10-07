package router

import (
	"strings"
	"time"
)

// Slot families: which CLI a routed slot runs on.
const (
	FamilyClaude = "claude"
	FamilyGemini = "gemini"
	FamilyCloud  = "cloud"
)

// Seat is the Claude account a slot bills against.
type Seat string

const (
	SeatNone     Seat = ""
	SeatPersonal Seat = "personal"
	SeatWork     Seat = "work"
)

// RouteSlot is one concrete, spawnable entry of a resolved kind route.
type RouteSlot struct {
	// Family is FamilyClaude, FamilyGemini or FamilyCloud.
	Family string
	// Model is the exact CLI --model value ("opus", "gemini-3.1-pro-high").
	Model string
	// PoolID is the quota pool the slot burns.
	PoolID PoolID
	// Seat is the Claude account for Claude slots; SeatNone otherwise.
	Seat Seat
}

// DisplayName is the human label for the slot's model: "Claude Opus".
func (s RouteSlot) DisplayName() string {
	switch strings.ToLower(s.Model) {
	case "opus":
		if s.Family == FamilyCloud {
			return "Claude Cloud Opus"
		}
		return "Claude Opus"
	case "sonnet":
		return "Claude Sonnet"
	case "gemini-3.1-pro", "gemini-3.1-pro-high":
		return "Gemini 3.1 Pro"
	case "gemini-3.8-flash", "gemini-3.8-flash-high":
		return "Gemini 3.8 Flash"
	}
	if s.Model != "" {
		return s.Model
	}
	return s.Family
}

// Label is DisplayName plus the seat for Claude slots:
// "Claude Opus · work seat".
func (s RouteSlot) Label() string {
	if s.Seat == SeatNone {
		return s.DisplayName()
	}
	return s.DisplayName() + " · " + string(s.Seat) + " seat"
}

// lockSubject names what was locked in a skip reason: "work seat" / "Gemini".
func (s RouteSlot) lockSubject() string {
	switch {
	case s.Seat != SeatNone:
		return string(s.Seat) + " seat"
	case s.Family == FamilyGemini:
		return "Gemini"
	default:
		return s.DisplayName()
	}
}

// LockedReason renders a skip reason for this slot: "work seat locked (weekly limit)".
func (s RouteSlot) LockedReason(reason string) string {
	return s.lockSubject() + " locked (" + reason + ")"
}

// SkippedSlot is a slot ahead of the chosen one that could not run.
type SkippedSlot struct {
	Slot   RouteSlot
	Reason string // already rendered: "work seat locked (weekly limit)"
}

// KindRoute is the per-run routing decision for a task: which slot runs first,
// what it falls back to, and why anything ahead of it was skipped. The daemon
// computes it once per run and uses it for both the spawned CLI and the route
// row, so the two cannot disagree.
type KindRoute struct {
	Kind          WorkKind
	IsWork        bool
	ModelOverride string
	// Candidates are the viable slots in order; Candidates[0] is the chosen one.
	// Empty means every slot was locked or disabled.
	Candidates []RouteSlot
	// Skipped lists slots ahead of Candidates[0] that were locked.
	Skipped []SkippedSlot
	// Locked lists every locked slot (ahead of or behind the chosen one).
	Locked []SkippedSlot
}

// Chosen returns the slot that runs first.
func (r KindRoute) Chosen() (RouteSlot, bool) {
	if len(r.Candidates) == 0 {
		return RouteSlot{}, false
	}
	return r.Candidates[0], true
}

// AllLocked reports whether no slot can run.
func (r KindRoute) AllLocked() bool { return len(r.Candidates) == 0 }

// Title is the route-row headline.
func (r KindRoute) Title() string {
	s, ok := r.Chosen()
	if !ok {
		return "All providers locked"
	}
	if len(r.Skipped) == 0 {
		return "Ran on " + s.Label()
	}
	return "Fell back to " + s.Label() + ": " + joinReasons(r.Skipped)
}

// Body is the route-row detail line.
func (r KindRoute) Body() string {
	parts := []string{"Kind of work: " + string(r.Kind)}
	if r.ModelOverride != "" {
		parts = append(parts, "model override: "+r.ModelOverride)
	}
	if r.AllLocked() && len(r.Locked) > 0 {
		parts = append(parts, joinReasons(r.Locked))
	}
	return strings.Join(parts, " · ")
}

func joinReasons(s []SkippedSlot) string {
	out := make([]string, len(s))
	for i, k := range s {
		out[i] = k.Reason
	}
	return strings.Join(out, "; ")
}

// NormalizeWorkKind maps a stored work_kind to a known kind. Empty and unknown
// values route as coding, which is Claude-first: an unrecognised kind must
// never silently land on Gemini.
func NormalizeWorkKind(s string) WorkKind {
	k := WorkKind(strings.ToLower(strings.TrimSpace(s)))
	for _, v := range ValidWorkKinds {
		if k == v {
			return k
		}
	}
	return WorkKindCoding
}

// ValidModelOverrides are the accepted per-task Claude model overrides.
var ValidModelOverrides = []string{"opus", "sonnet"}

// NormalizeModelOverride returns the canonical override, or "" when s is empty
// or not an accepted value.
func NormalizeModelOverride(s string) string {
	m := strings.ToLower(strings.TrimSpace(s))
	for _, v := range ValidModelOverrides {
		if m == v {
			return m
		}
	}
	return ""
}

// claudeSeat returns the Claude pool and seat for a repo.
func claudeSeat(isWork bool) (PoolID, Seat) {
	if isWork {
		return PoolWorkClaude, SeatWork
	}
	return PoolPersonalClaude, SeatPersonal
}

// ChainsForRepo returns DefaultKindChains with every local Claude slot bound
// to the repo's seat: work repos bill the work seat (~/.claude-work), personal
// repos the personal seat.
func ChainsForRepo(isWork bool) map[WorkKind][]KindSlot {
	pool, _ := claudeSeat(isWork)
	chains := DefaultKindChains()
	for kind, chain := range chains {
		for i := range chain {
			if isLocalClaudeSlot(chain[i]) {
				chain[i].PoolID = pool
			}
		}
		chains[kind] = chain
	}
	return chains
}

func isLocalClaudeSlot(s KindSlot) bool {
	return strings.HasPrefix(s.Provider, "claude-") && s.Provider != "claude-cloud"
}

func slotFamily(s KindSlot) string {
	switch {
	case s.Provider == "claude-cloud":
		return FamilyCloud
	case strings.HasPrefix(s.Provider, "claude-"):
		return FamilyClaude
	default:
		return FamilyGemini
	}
}

// PoolLockReason reports whether a quota pool cannot take work right now, and
// why. A pool is locked when hard-locked, or when its 5h or weekly window is
// spent and has not reset yet. A nil pool (no data) is not locked.
func PoolLockReason(pool *QuotaPool, now time.Time) (bool, string) {
	if pool == nil {
		return false, ""
	}
	if pool.IsLocked {
		if pool.LockoutReason != "" {
			return true, pool.LockoutReason
		}
		return true, "locked"
	}
	if pool.FiveHour.IsLocked || (pool.FiveHour.ResetsAt.After(now) && pool.FiveHour.RemainingPct <= 0) {
		return true, "5h limit reached"
	}
	// Weekly only counts when measured: an unreported window's zero-value
	// RemainingPct is not evidence of exhaustion.
	if pool.Weekly.IsLocked || (pool.Weekly.Known && pool.Weekly.ResetsAt.After(now) && pool.Weekly.RemainingPct <= 0) {
		return true, "weekly limit reached"
	}
	return false, ""
}

// ResolveRoute resolves the routing chain for one task run.
//
// kind is the stored work_kind (normalised; unknown → coding). isWork selects
// the Claude seat. modelOverride ("opus"/"sonnet", invalid values ignored)
// pins a Claude-first chain with that model, falling back to its Gemini pair.
func ResolveRoute(kind string, isWork bool, pacer *PacerState, modelOverride string, now time.Time) KindRoute {
	r := KindRoute{
		Kind:          NormalizeWorkKind(kind),
		IsWork:        isWork,
		ModelOverride: NormalizeModelOverride(modelOverride),
	}
	pool, seat := claudeSeat(isWork)

	var chain []KindSlot
	if r.ModelOverride != "" {
		chain = []KindSlot{
			{Provider: "claude-" + r.ModelOverride, Model: r.ModelOverride, PoolID: pool, Enabled: true},
			{Provider: "gemini", Model: PairModelBidirectional(r.ModelOverride), PoolID: PoolGeminiNative, Enabled: true},
		}
	} else {
		chain = ChainsForRepo(isWork)[r.Kind]
	}

	for _, ks := range chain {
		if !ks.Enabled {
			continue
		}
		if ks.CloudCreditExpires != "" {
			if exp, err := time.Parse(time.RFC3339, ks.CloudCreditExpires); err == nil && now.After(exp) {
				continue
			}
		}
		slot := RouteSlot{Family: slotFamily(ks), Model: ks.Model, PoolID: ks.PoolID}
		if slot.Family == FamilyClaude {
			slot.Seat = seat
		}
		var p *QuotaPool
		if pacer != nil && ks.PoolID != "" {
			p = pacer.Pools[ks.PoolID]
		}
		if locked, why := PoolLockReason(p, now); locked {
			sk := SkippedSlot{Slot: slot, Reason: slot.LockedReason(why)}
			r.Locked = append(r.Locked, sk)
			if len(r.Candidates) == 0 {
				r.Skipped = append(r.Skipped, sk)
			}
			continue
		}
		r.Candidates = append(r.Candidates, slot)
	}
	return r
}
