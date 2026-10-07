package router

import "github.com/VinnyVanGogh/staypoint/internal/geminiguard"

// GeminiCodeRule names the Board rule for the gates page (STA-816).
const GeminiCodeRule = "Gemini never writes code in a work repo"

// GeminiCodeForbidden is the Board rule (STA-856): in a work repo Gemini must
// never write code. It may still write documentation there (enforced after
// each turn by the harness diff guard), and is unrestricted in personal repos.
// It is deliberately not configurable: there is no switch to turn it off.
func GeminiCodeForbidden(isWork bool) bool { return isWork }

// KindMayWriteCode reports whether a kind of work can produce code. Only
// planning and architecture are docs-shaped; empty and unknown kinds route as
// coding and so may write code.
func KindMayWriteCode(kind WorkKind) bool {
	switch NormalizeWorkKind(string(kind)) {
	case WorkKindPlanning, WorkKindArchitecture:
		return false
	}
	return true
}

// GeminiAllowed reports whether a Gemini slot may be routed for kind in this
// repo. In a work repo Gemini is removed from every chain that may write code:
// the chain becomes work Claude -> personal Claude (same model), and when both
// seats are locked the run waits in the queue instead of falling back to Gemini.
func GeminiAllowed(kind WorkKind, isWork bool) bool {
	return !(GeminiCodeForbidden(isWork) && KindMayWriteCode(kind))
}

// GeminiDocAllowlist is what Gemini may edit in a work repo.
func GeminiDocAllowlist() []string { return geminiguard.Allowlist() }

// claudeOnlyWorkChain replaces a work-repo chain that may write code: Gemini
// slots are dropped, and every local Claude slot on the work seat is followed
// (after all of them) by the same model on the personal seat.
func claudeOnlyWorkChain(chain []KindSlot) []KindSlot {
	out := make([]KindSlot, 0, 2*len(chain))
	var personal []KindSlot
	for _, s := range chain {
		if slotFamily(s) == FamilyGemini {
			continue
		}
		out = append(out, s)
		if isLocalClaudeSlot(s) && s.PoolID == PoolWorkClaude {
			p := s
			p.PoolID = PoolPersonalClaude
			personal = append(personal, p)
		}
	}
	return append(out, personal...)
}

// seatForPool maps a Claude quota pool to its seat.
func seatForPool(id PoolID) Seat {
	if id == PoolWorkClaude {
		return SeatWork
	}
	return SeatPersonal
}
