package adapter

import (
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/router"
)

// SeatLimitError is returned when a turn ended because a Claude seat (or every
// seat in the chain) is out of quota: the CLI answered with its own limit
// message, or every candidate was already locked. It is a wait, not a broken
// provider: the harness must not count it as an adapter failure.
type SeatLimitError struct {
	// Seats lists the seats that reported or were locked, in chain order:
	// "work seat locked (You've hit your session limit · resets 4:20am)".
	Seats []string
	// AllLocked is set when no candidate in the chain could take the turn, so
	// the run has to wait for a reset.
	AllLocked bool
}

func (e *SeatLimitError) Error() string {
	if e.AllLocked {
		return "every seat is out of quota: " + strings.Join(e.Seats, "; ")
	}
	return "seat ran out of quota mid-turn: " + strings.Join(e.Seats, "; ")
}

// SeatsExhausted reports whether the run must wait for a reset (no seat left).
// The orchestrator checks for this method without importing this package.
func (e *SeatLimitError) SeatsExhausted() bool { return e.AllLocked }

// seatLimitText returns the limit message carried by line, if line is a
// Claude CLI limit answer: an assistant text block or an error result whose
// text is the CLI's limit message (router.IsSeatLimitMessage).
func seatLimitText(line []byte, parse func([]byte) ([]StreamDelta, error)) (string, bool) {
	deltas, err := parse(line)
	if err != nil {
		return "", false
	}
	for _, d := range deltas {
		switch d.Kind {
		case DeltaText:
			if d.FromUser {
				continue
			}
		case DeltaResult:
		default:
			continue
		}
		if router.IsSeatLimitMessage(d.Text) {
			return strings.TrimSpace(d.Text), true
		}
	}
	return "", false
}
