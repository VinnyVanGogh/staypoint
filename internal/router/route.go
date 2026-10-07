package router

import (
	"errors"
	"fmt"
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
	// ChosenByBoard marks a Gemini slot the Board chose explicitly
	// (provider=gemini) rather than the kind's default chain.
	ChosenByBoard bool
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
	if s.ChosenByBoard {
		return s.DisplayName() + " · chosen by Board"
	}
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
	// Provider is the task's explicit provider choice ("" = default Claude,
	// "claude", "gemini").
	Provider string
	// GeminiChosen is set when the Board chose provider=gemini for the task.
	GeminiChosen bool
	// GeminiBarred is set when GeminiCodeForbidden applies (a kind that may
	// write code, any repo): Gemini is kept out of the chain even when the
	// Board chose it, and locked Claude seats mean waiting.
	GeminiBarred bool
	// GeminiCodeApprovalID is set when this run may let Gemini write code: a
	// personal repo, provider=gemini, and a Board Touch ID approval consumed
	// for this run. The harness guard then allows code changes.
	GeminiCodeApprovalID string
}

// HasGemini reports whether any planned slot (viable or locked) is Gemini.
func (r KindRoute) HasGemini() bool {
	for _, c := range r.Candidates {
		if c.Family == FamilyGemini {
			return true
		}
	}
	for _, l := range r.Locked {
		if l.Slot.Family == FamilyGemini {
			return true
		}
	}
	return false
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
		if !r.HasGemini() {
			if len(r.Locked) > 0 {
				return "Waiting for a Claude seat: " + joinReasons(r.Locked)
			}
			return "Waiting for a Claude seat"
		}
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
	if r.GeminiChosen {
		parts = append(parts, "provider: gemini (chosen by Board)")
	}
	if r.GeminiCodeApprovalID != "" {
		parts = append(parts, "Gemini code approved by Board Touch ID for this run (gate "+r.GeminiCodeApprovalID+")")
	}
	if r.ModelOverride != "" {
		parts = append(parts, "model override: "+r.ModelOverride)
	}
	if r.AllLocked() && len(r.Locked) > 0 {
		parts = append(parts, joinReasons(r.Locked))
	}
	// The rule is named only where it changed the outcome: a Board choice it
	// refused, or a wait with no Gemini fallback.
	if r.GeminiBarred && r.GeminiChosen {
		parts = append(parts, "Gemini refused: "+GeminiCodeRule)
	} else if r.GeminiBarred && r.AllLocked() {
		parts = append(parts, "Gemini barred: "+GeminiCodeRule)
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
// to the repo's seat: work repos bill the work seat (~/.claude-work) and fall
// back to the personal seat, personal repos use the personal seat. Gemini is
// removed from every kind that may write code (GeminiAllowed).
func ChainsForRepo(isWork bool) map[WorkKind][]KindSlot {
	chains := DefaultKindChains()
	for kind, chain := range chains {
		chains[kind] = bindChain(chain, kind, isWork, GeminiAllowed(kind, isWork))
	}
	return chains
}

// bindChain binds Claude slots to the repo's seat, drops Gemini unless
// allowGemini, and adds the personal-seat fallback in work repos.
func bindChain(chain []KindSlot, kind WorkKind, isWork, allowGemini bool) []KindSlot {
	pool, _ := claudeSeat(isWork)
	out := append([]KindSlot(nil), chain...)
	for i := range out {
		if isLocalClaudeSlot(out[i]) {
			out[i].PoolID = pool
		}
	}
	if !allowGemini {
		out = dropGemini(out)
	}
	return withPersonalFallback(out)
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

// Task provider choices (tasks.provider).
const (
	ProviderDefault = ""
	ProviderClaude  = "claude"
	ProviderGemini  = "gemini"
)

// ValidProviders are the accepted explicit task providers.
var ValidProviders = []string{ProviderClaude, ProviderGemini}

// RouteChoice is a task's explicit provider/model choice. The zero value is
// the default: Claude Opus on the repo's seat.
type RouteChoice struct {
	Provider string // ProviderDefault, ProviderClaude or ProviderGemini
	Model    string // canonical: "opus", "sonnet", "gemini-3.1-pro-high", "gemini-3.8-flash-high"
	// CodeApprovalID is the Board Touch ID approval (gate request id) that
	// lets provider=gemini run a code kind for this one run, personal repos
	// only (GeminiCodeApprovalAllowed). Set by the daemon after it consumed
	// the approval; never stored on the task.
	CodeApprovalID string
}

// NormalizeProvider maps a stored or user-typed provider to its canonical
// value. ok is false for an unrecognised value.
func NormalizeProvider(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto", "default":
		return ProviderDefault, true
	case "claude", "anthropic":
		return ProviderClaude, true
	case "gemini", "agy", "antigravity":
		return ProviderGemini, true
	}
	return "", false
}

// normalizeGeminiModel maps Gemini model spellings to the routed CLI model.
func normalizeGeminiModel(m string) string {
	switch m {
	case "gemini-3.1-pro", "gemini-3.1-pro-high", "pro", "gemini-pro":
		return "gemini-3.1-pro-high"
	case "gemini-3.8-flash", "gemini-3.8-flash-high", "flash", "gemini-flash":
		return "gemini-3.8-flash-high"
	}
	return ""
}

// NormalizeRouteChoice validates a provider/model pair from the API or CLI.
// A Gemini model requires provider gemini: Gemini is never inferred.
func NormalizeRouteChoice(provider, model string) (RouteChoice, error) {
	p, ok := NormalizeProvider(provider)
	if !ok {
		return RouteChoice{}, fmt.Errorf("invalid provider %q: must be claude or gemini", provider)
	}
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return RouteChoice{Provider: p}, nil
	}
	if p == ProviderGemini {
		if g := normalizeGeminiModel(m); g != "" {
			return RouteChoice{Provider: p, Model: g}, nil
		}
		if c := NormalizeModelOverride(m); c != "" {
			return RouteChoice{Provider: p, Model: PairModelBidirectional(c)}, nil
		}
		return RouteChoice{}, fmt.Errorf("invalid gemini model %q: must be gemini-3.1-pro-high or gemini-3.8-flash-high", model)
	}
	if c := NormalizeModelOverride(m); c != "" {
		return RouteChoice{Provider: p, Model: c}, nil
	}
	if normalizeGeminiModel(m) != "" {
		return RouteChoice{}, fmt.Errorf("model %q is a Gemini model: set provider gemini to choose Gemini", model)
	}
	return RouteChoice{}, fmt.Errorf("invalid model %q: must be opus or sonnet", model)
}

// ErrGeminiCodeKind is returned when the Board picks provider=gemini for a
// kind that may write code.
var ErrGeminiCodeKind = errors.New("provider gemini refused")

// ValidateTaskChoice normalises a task's provider/model choice and applies
// GeminiCodeForbidden at create/update time: provider=gemini on a code kind
// (coding, qa, empty/unknown) is refused in a work repo with a clear error.
// In a personal repo it is accepted, and every run then waits for a Board
// Touch ID approval (NeedsGeminiCodeApproval).
func ValidateTaskChoice(kind string, isWork bool, provider, model string) (RouteChoice, error) {
	c, err := NormalizeRouteChoice(provider, model)
	if err != nil {
		return RouteChoice{}, err
	}
	k := NormalizeWorkKind(kind)
	if c.Provider == ProviderGemini && !GeminiChoiceAllowed(k, isWork) {
		return RouteChoice{}, fmt.Errorf("%w for work_kind %s in a work repo: %s, even with Board approval; Gemini may only take planning, architecture, review or docs tasks there", ErrGeminiCodeKind, k, GeminiCodeRule)
	}
	return c, nil
}

// NeedsGeminiCodeApproval reports whether a run of this task must wait for a
// Board Touch ID approval: provider=gemini on a code kind in a personal repo.
func NeedsGeminiCodeApproval(kind string, isWork bool, c RouteChoice) bool {
	p, _ := NormalizeProvider(c.Provider)
	return p == ProviderGemini && !isWork && KindMayWriteCode(NormalizeWorkKind(kind))
}

// ChoiceFromStored rebuilds a task's choice from its stored columns, dropping
// anything invalid (an invalid stored provider routes as the default, never
// as Gemini).
func ChoiceFromStored(provider, model string) RouteChoice {
	c, err := NormalizeRouteChoice(provider, model)
	if err == nil {
		return c
	}
	if p, ok := NormalizeProvider(provider); ok {
		if c2, err2 := NormalizeRouteChoice(p, ""); err2 == nil {
			return c2
		}
	}
	return RouteChoice{}
}

// ResolveRoute resolves the routing chain for one task run with no explicit
// provider choice. modelOverride ("opus"/"sonnet", invalid values ignored)
// pins a Claude-first chain with that model; non-code kinds keep the Gemini
// pair as fallback. See ResolveRouteChoice.
func ResolveRoute(kind string, isWork bool, pacer *PacerState, modelOverride string, now time.Time) KindRoute {
	return ResolveRouteChoice(kind, isWork, pacer, RouteChoice{Model: NormalizeModelOverride(modelOverride)}, now)
}

// ResolveRouteChoice resolves the routing chain for one task run.
//
// kind is the stored work_kind (normalised; unknown → coding). isWork selects
// the Claude seat. GeminiCodeForbidden (all repos): a code kind (coding, qa,
// unknown) is Claude only whatever the choice — repo seat, then (work repos)
// the personal seat — and with every seat locked the route is all-locked so
// the run queue waits. Non-code kinds run Gemini first by default.
//
// choice.Provider: "" uses the kind's default chain; "claude" removes Gemini;
// "gemini" puts the chosen Gemini model first (label "chosen by Board") with
// its Claude pair as fallback, and is refused (GeminiBarred, Claude only) on a
// code kind.
func ResolveRouteChoice(kind string, isWork bool, pacer *PacerState, choice RouteChoice, now time.Time) KindRoute {
	approval := choice.CodeApprovalID
	if c, err := NormalizeRouteChoice(choice.Provider, choice.Model); err == nil {
		choice = c
	} else {
		choice = ChoiceFromStored(choice.Provider, "")
	}
	r := KindRoute{
		Kind:         NormalizeWorkKind(kind),
		IsWork:       isWork,
		Provider:     choice.Provider,
		GeminiChosen: choice.Provider == ProviderGemini,
	}
	allowGemini := GeminiAllowed(r.Kind, isWork)
	if !allowGemini && r.GeminiChosen && approval != "" && GeminiCodeApprovalAllowed(isWork) {
		// Board Touch ID approval for this one run (personal repo only).
		allowGemini = true
		r.GeminiCodeApprovalID = approval
	}
	r.GeminiBarred = !allowGemini

	var chain []KindSlot
	switch {
	case r.GeminiChosen:
		gm := choice.Model
		if gm == "" {
			gm = "gemini-3.1-pro-high"
		}
		r.ModelOverride = gm
		cm := PairModelBidirectional(gm)
		chain = bindChain([]KindSlot{
			{Provider: "gemini", Model: gm, PoolID: PoolGeminiNative, Enabled: true},
			{Provider: "claude-" + cm, Model: cm, PoolID: PoolPersonalClaude, Enabled: true},
		}, r.Kind, isWork, allowGemini)
	case choice.Model != "":
		r.ModelOverride = choice.Model
		pinned := []KindSlot{{Provider: "claude-" + choice.Model, Model: choice.Model, PoolID: PoolPersonalClaude, Enabled: true}}
		if choice.Provider != ProviderClaude {
			pinned = append(pinned, KindSlot{Provider: "gemini", Model: PairModelBidirectional(choice.Model), PoolID: PoolGeminiNative, Enabled: true})
		}
		chain = bindChain(pinned, r.Kind, isWork, allowGemini)
	case choice.Provider == ProviderClaude:
		chain = dropGemini(ChainsForRepo(isWork)[r.Kind])
	default:
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
		switch slot.Family {
		case FamilyClaude:
			slot.Seat = seatForPool(ks.PoolID)
		case FamilyGemini:
			if r.GeminiBarred {
				// Defence in depth: bindChain already dropped Gemini for
				// code kinds; never route one that slipped in.
				continue
			}
			slot.ChosenByBoard = r.GeminiChosen
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
