package server

import (
	"encoding/json"
	"net/http"
)

// runTokenCheckRequest asks whether the token hashing to TokenHash is a live
// run's token for TaskID. The client never sends the token itself
// (opstools.RunTokens.CheckHash).
type runTokenCheckRequest struct {
	TaskID    string `json:"task_id"`
	TokenHash string `json:"token_hash"`
	Nonce     string `json:"nonce"`
}

// checkRunToken answers the ops-tool MCP server: 200 {task_id, run_id,
// proof} when the token belongs to a live run of task_id, 403 otherwise
// (task-7d279c9d, Board review #4). The proof, keyed by the token, shows the
// answer came from the daemon that issued it. It only confirms a token the
// caller already holds; no route issues one, so the daemon's auth token buys
// nothing here.
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
	if len(req.Nonce) < 32 {
		writeError(w, http.StatusBadRequest, "nonce must be at least 32 characters")
		return
	}
	ref, proof, err := s.opts.RunTokens.CheckHash(req.TaskID, req.TokenHash, req.Nonce)
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, struct {
		TaskID string `json:"task_id"`
		RunID  string `json:"run_id"`
		Proof  string `json:"proof"`
	}{ref.TaskID, ref.RunID, proof})
}
