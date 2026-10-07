package server

import (
	"net/http"
)

// STA-859: the task API's Board overrides (`override` on done/stage, which
// closes a parent with open children, and `allow_deep` on child create, which
// nests past the depth cap) used to be trusted straight from the request
// body, so any daemon token holder, agents included, could set them. They are
// now honored only when the request also passes the Board gate
// (WrapBoardAction: Board session cookie + passkey assertion). The routes stay
// agent-callable without the flags; with a flag and no Board gate the request
// is refused with the gate's 403 and nothing is written.

// SetBoardGate wires the Board gate the override flags must pass. Without one,
// every override request is refused (fail closed).
func (h *TasksHandler) SetBoardGate(gate func(http.Handler) http.Handler) {
	h.boardGate = gate
}

// requireBoardForOverride runs the Board gate for a request that asked for a
// Board override. It returns true when the gate passed; otherwise it has
// written the gate's refusal (403 board_session_required for a token-only
// caller, or the passkey error code) and the caller must stop.
//
// The gate only reads headers and cookies, so running it after the handler
// has decoded the body is safe.
func (h *TasksHandler) requireBoardForOverride(w http.ResponseWriter, r *http.Request, flag string) bool {
	if h.boardGate == nil {
		writeBoardError(w, "board_session_required", "forbidden: "+flag+" is a Board override and requires a Board session and passkey")
		return false
	}
	passed := false
	refusal := &boardGateRefusal{header: http.Header{}, status: http.StatusOK}
	h.boardGate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		passed = true
	})).ServeHTTP(refusal, r)
	if passed {
		return true
	}
	if refusal.status == http.StatusOK {
		// A gate that neither passed nor wrote a refusal still fails closed.
		writeBoardError(w, "board_session_required", "forbidden: "+flag+" is a Board override and requires a Board session and passkey")
		return false
	}
	for k, v := range refusal.header {
		w.Header()[k] = v
	}
	w.WriteHeader(refusal.status)
	_, _ = w.Write(refusal.body.Bytes())
	return false
}
