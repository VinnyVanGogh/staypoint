package server

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
)

// Drain mode for zero-kill deploys (task-db71fba9). scripts/reinstall-daemon.sh
// puts the daemon in drain, waits until no run is live, then swaps the binary.
//
// Starting and cancelling a drain are Board-only (Board session cookie): a
// drain stops every new run, so an agent that could start one could stall
// all work. Reading the status is open to the normal API token.

type drainResponse struct {
	orchestrator.DrainStatus
	Label string `json:"label"`
}

func writeDrain(w http.ResponseWriter, st orchestrator.DrainStatus) {
	writeJSONUnescaped(w, drainResponse{DrainStatus: st, Label: st.Label()})
}

// GetDrain is GET /api/daemon/drain.
func GetDrain(w http.ResponseWriter, r *http.Request) {
	writeDrain(w, orchestrator.GlobalRunSlots.DrainStatus())
}

// StartDrain is POST /api/daemon/drain {"mode":"finish"|"boundary"|"now"}.
// A drain only escalates; a lower mode than the current one changes nothing.
func StartDrain(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	mode, err := orchestrator.ParseDrainMode(req.Mode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeDrain(w, orchestrator.GlobalRunSlots.StartDrain(mode))
}

// CancelDrain is DELETE /api/daemon/drain: back to normal, and the queued
// runs that fit start now.
func CancelDrain(w http.ResponseWriter, r *http.Request) {
	writeDrain(w, orchestrator.GlobalRunSlots.CancelDrain())
}
