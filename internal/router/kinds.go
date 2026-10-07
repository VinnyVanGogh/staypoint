package router

import (
	"strings"
	"time"
)

// WorkKind is the category of work driving model-routing decisions.
type WorkKind string

const (
	WorkKindCoding       WorkKind = "coding"
	WorkKindReview       WorkKind = "review"
	WorkKindArchitecture WorkKind = "architecture"
	WorkKindPlanning     WorkKind = "planning"
	WorkKindQA           WorkKind = "qa"
	// WorkKindDocs is documentation work: non-code, Gemini-first.
	WorkKindDocs WorkKind = "docs"
)

// ValidWorkKinds is the complete set of accepted WorkKind values.
var ValidWorkKinds = []WorkKind{
	WorkKindCoding,
	WorkKindReview,
	WorkKindArchitecture,
	WorkKindPlanning,
	WorkKindQA,
	WorkKindDocs,
}

// KindSlot is one ordered entry in a work-kind routing chain.
type KindSlot struct {
	// Provider identifies the pool: "claude-opus", "claude-sonnet",
	// "gemini-3.1-pro", "gemini-3.8-flash", "claude-cloud".
	Provider string
	// Model is the exact model flag to pass to the CLI
	// (e.g. "opus", "sonnet", "gemini-3.1-pro-high", "gemini-3.8-flash-high").
	Model string
	// PoolID is used to check quota lockout via PacerState.
	PoolID PoolID
	// Enabled controls whether the slot participates at all.
	// Cloud slot is off until STA-410 lands.
	Enabled bool
	// CloudCreditExpires is RFC3339; non-empty slots skip when expired.
	// Empty means no credit expiry.
	CloudCreditExpires string
}

// DefaultKindChains returns the built-in routing table when no [routing] section
// is present in config.toml. Claude slots carry PoolPersonalClaude as a
// placeholder; ChainsForRepo / ResolveRoute bind them to the repo's seat
// (work repos then fall back to the personal seat).
//
// Board rule (GeminiCodeForbidden, all repos): code kinds are Claude only and
// wait in the queue when every Claude seat is locked; non-code kinds run
// Gemini first with Claude as the fallback.
//
//	coding       : Claude Opus                              (code)
//	qa           : Claude Opus                              (code: writes tests)
//	review       : Gemini 3.1 Pro → Claude Opus             (non-code, advisory)
//	architecture : Gemini 3.1 Pro → Claude Opus → Claude Cloud [off]
//	planning     : Gemini 3.8 Flash → Claude Sonnet → Claude Cloud [off]
//	docs         : Gemini 3.8 Flash → Claude Sonnet
//
// Empty or unknown work_kind routes as coding (see NormalizeWorkKind).
func DefaultKindChains() map[WorkKind][]KindSlot {
	opus := func() KindSlot {
		return KindSlot{Provider: "claude-opus", Model: "opus", PoolID: PoolPersonalClaude, Enabled: true}
	}
	sonnet := func() KindSlot {
		return KindSlot{Provider: "claude-sonnet", Model: "sonnet", PoolID: PoolPersonalClaude, Enabled: true}
	}
	pro := func() KindSlot {
		return KindSlot{Provider: "gemini-3.1-pro", Model: "gemini-3.1-pro-high", PoolID: PoolGeminiNative, Enabled: true}
	}
	flash := func() KindSlot {
		return KindSlot{Provider: "gemini-3.8-flash", Model: "gemini-3.8-flash-high", PoolID: PoolGeminiNative, Enabled: true}
	}
	// Cloud slot off until STA-410 lands; gated by cloud_credit_expires = 2026-11-04.
	cloud := func() KindSlot {
		return KindSlot{Provider: "claude-cloud", Model: "opus", PoolID: "", Enabled: false, CloudCreditExpires: "2026-11-04T00:00:00Z"}
	}
	return map[WorkKind][]KindSlot{
		WorkKindCoding:       {opus()},
		WorkKindQA:           {opus()},
		WorkKindReview:       {pro(), opus()},
		WorkKindArchitecture: {pro(), opus(), cloud()},
		WorkKindPlanning:     {flash(), sonnet(), cloud()},
		WorkKindDocs:         {flash(), sonnet()},
	}
}

// ResolveKindChain walks the chain for kind, skipping locked, expired, or
// disabled slots, and returns the first viable slot. Returns nil when every
// slot is unavailable.
func ResolveKindChain(kind WorkKind, chains map[WorkKind][]KindSlot, pacer *PacerState) *KindSlot {
	chain, ok := chains[kind]
	if !ok {
		return nil
	}
	now := time.Now()
	for i := range chain {
		s := &chain[i]
		if !s.Enabled {
			continue
		}
		// Skip expired cloud credit.
		if s.CloudCreditExpires != "" {
			if exp, err := time.Parse(time.RFC3339, s.CloudCreditExpires); err == nil && now.After(exp) {
				continue
			}
		}
		// Cloud slots with no pool are skipped via the enabled flag above; if
		// somehow enabled with no pool, allow through (no quota check possible).
		if s.PoolID == "" {
			return s
		}
		if pacer != nil {
			if locked, _ := PoolLockReason(pacer.Pools[s.PoolID], now); locked {
				continue
			}
		}
		return s
	}
	return nil
}

// PairModelBidirectional maps a model to its cross-provider equivalent:
//
//	Claude Opus  <->  Gemini 3.1 Pro
//	Claude Sonnet <-> Gemini 3.8 Flash
//
// Returns "" for unknown models.
func PairModelBidirectional(model string) string {
	switch strings.ToLower(model) {
	case "opus":
		return "gemini-3.1-pro-high"
	case "gemini-3.1-pro", "gemini-3.1-pro-high":
		return "opus"
	case "sonnet":
		return "gemini-3.8-flash-high"
	case "gemini-3.8-flash", "gemini-3.8-flash-high":
		return "sonnet"
	}
	return ""
}
