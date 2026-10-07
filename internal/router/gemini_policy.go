package router

import "github.com/VinnyVanGogh/staypoint/internal/geminiguard"

// GeminiCodeRule names the Board rule for the gates page (STA-816).
const GeminiCodeRule = "Gemini never writes code, in any repo"

// GeminiNonCodeRule names the routing half of the rule for the gates page.
const GeminiNonCodeRule = "Gemini may do non-code work (planning, architecture, review, docs)"

// GeminiCodeForbidden is the Board rule (STA-856, revised 2026-10-06): Gemini
// never writes code, in any repo. It may do non-code work (KindMayWriteCode
// false), with every Gemini turn checked by the harness diff guard
// (geminiguard) and code changes reverted. It is deliberately not
// configurable: there is no switch to turn it off. isWork is kept so call
// sites read as the rule they apply; the answer no longer depends on it.
func GeminiCodeForbidden(isWork bool) bool { return true }

// KindMayWriteCode reports whether a kind of work can produce code. Planning,
// architecture, review and docs are non-code; coding, qa (writes tests) and
// empty or unknown kinds may write code.
func KindMayWriteCode(kind WorkKind) bool {
	switch NormalizeWorkKind(string(kind)) {
	case WorkKindPlanning, WorkKindArchitecture, WorkKindReview, WorkKindDocs:
		return false
	}
	return true
}

// GeminiAllowed reports whether a Gemini slot may be routed for kind in this
// repo: only for non-code kinds. Code kinds are Claude only (work repos: work
// seat -> personal seat; personal repos: personal seat) and wait in the queue
// when every seat is locked, never falling back to Gemini.
func GeminiAllowed(kind WorkKind, isWork bool) bool {
	return !(GeminiCodeForbidden(isWork) && KindMayWriteCode(kind))
}

// GeminiCodeApprovalAllowed is the Board addition (2026-10-06): in a personal
// repo the Board may let Gemini write code with a Touch ID (passkey) approval
// scoped to one task run or one agy session. In a work repo it is a hard no,
// approval or not.
func GeminiCodeApprovalAllowed(isWork bool) bool { return !isWork }

// GeminiChoiceAllowed reports whether the Board may set provider=gemini on a
// task of this kind in this repo: always for non-code kinds; for a code kind
// only in a personal repo, where each run then needs a Touch ID approval.
func GeminiChoiceAllowed(kind WorkKind, isWork bool) bool {
	return !KindMayWriteCode(kind) || GeminiCodeApprovalAllowed(isWork)
}

// GeminiReviewAdvisory is the Board rule that a Gemini review is advisory: it
// never approves, merges or satisfies a review gate on its own.
func GeminiReviewAdvisory() bool { return true }

// GeminiDocAllowlist is what Gemini may edit (any repo).
func GeminiDocAllowlist() []string { return geminiguard.Allowlist() }

// claudeOnlyWorkChain removes Gemini from a chain: Gemini slots are dropped,
// and every local Claude slot on the work seat is followed (after all of
// them) by the same model on the personal seat.
func claudeOnlyWorkChain(chain []KindSlot) []KindSlot {
	return withPersonalFallback(dropGemini(chain))
}

func dropGemini(chain []KindSlot) []KindSlot {
	out := make([]KindSlot, 0, len(chain))
	for _, s := range chain {
		if slotFamily(s) != FamilyGemini {
			out = append(out, s)
		}
	}
	return out
}

// withPersonalFallback appends, after the whole chain, the personal-seat copy
// of every local Claude slot on the work seat (work seat -> personal seat).
func withPersonalFallback(chain []KindSlot) []KindSlot {
	out := append([]KindSlot(nil), chain...)
	for _, s := range chain {
		if isLocalClaudeSlot(s) && s.PoolID == PoolWorkClaude {
			p := s
			p.PoolID = PoolPersonalClaude
			out = append(out, p)
		}
	}
	return out
}

// seatForPool maps a Claude quota pool to its seat.
func seatForPool(id PoolID) Seat {
	if id == PoolWorkClaude {
		return SeatWork
	}
	return SeatPersonal
}
