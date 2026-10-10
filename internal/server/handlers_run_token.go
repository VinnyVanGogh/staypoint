package server

import (
	"encoding/json"
	"net/http"
)

// runTokenCheckRequest asks whether token is a live run's token for TaskID.
type runTokenCheckRequest struct {
	TaskID string `json:"task_id"`
	Token  string `json:"token"`
}

// checkRunToken answers the ops-tool MCP server: 200 {task_id, run_id} when
// the token belongs to a live run of task_id, 403 otherwise (task-7d279c9d,
// Board review #4). It only confirms a token the caller already holds; there
// is no route that issues one, so the daemon's auth token buys nothing here.
func (s *Server) checkRunToken(w http.ResponseWriter, r *http.Request) {
	var req runTokenCheckRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if s.opts.RunTokens == nil {
		writeError(w, http.StatusForbidden, "this daemon issues no run tokens")
		return
	}
	ref, err := s.opts.RunTokens.Check(req.TaskID, req.Token)
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, struct {
		TaskID string `json:"task_id"`
		RunID  string `json:"run_id"`
	}{ref.TaskID, ref.RunID})
}
