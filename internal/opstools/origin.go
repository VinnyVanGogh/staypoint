package opstools

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Board review #2 H1: an agent could start its own `staypoint mcp` with a
// fake HOME (so a fake config.toml) or with STAYPOINT_TASK_ID unset, and the
// ops tools ran under whatever that process was told. The harness now hands
// each run a token, and the MCP server runs ops tools only for a task whose
// token the daemon confirms.
//
// Board review #4: the first version derived tokens from a key file in the
// data dir. Same-uid Bash can read (and overwrite) any such file however the
// classifier spells its path, so a key file let an agent mint a token for
// any task. Tokens are now random per run, held only in the daemon's memory,
// bound to (task, run), and revoked when the run ends. Nothing on disk can
// mint one, and a token from a run that has ended is worthless.

// RunTokenEnv carries the run token from the harness to the MCP server. Its
// name avoids TOKEN/SECRET/KEY so env sanitizers that drop secret-looking
// names (security.SanitizeEnv) pass it through to the MCP server.
const RunTokenEnv = "STAYPOINT_RUN_TICKET"

// legacyOpsKeyFile is the key file the first version kept in the data dir.
const legacyOpsKeyFile = "ops_key"

// RunTokens is the daemon's in-memory registry of live run tokens.
type RunTokens struct {
	mu sync.Mutex
	// live maps sha256(token) to the run it was issued to, so a lookup never
	// compares the secret itself.
	live map[[32]byte]liveRun
}

type liveRun struct {
	ref RunRef
	// token keys the proof the daemon answers a check with (CheckHash).
	token string
}

// RunRef is the run a token was issued to.
type RunRef struct {
	TaskID string
	RunID  string
}

// NewRunTokens returns an empty registry.
func NewRunTokens() *RunTokens {
	return &RunTokens{live: map[[32]byte]liveRun{}}
}

// Issue mints a random token for taskID's run runID. revoke ends it; call it
// when the run ends.
func (r *RunTokens) Issue(taskID, runID string) (token string, revoke func(), err error) {
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(runID) == "" {
		return "", nil, errors.New("run token: task and run IDs are required")
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("run token: %w", err)
	}
	token = hex.EncodeToString(buf)
	h := sha256.Sum256([]byte(token))
	r.mu.Lock()
	r.live[h] = liveRun{ref: RunRef{TaskID: taskID, RunID: runID}, token: token}
	r.mu.Unlock()
	var once sync.Once
	return token, func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.live, h)
			r.mu.Unlock()
		})
	}, nil
}

// CheckHash reports the run a live token belongs to, if it was issued to
// taskID. The client sends only TokenHash(token), never the token: the
// daemon answers with RunCheckProof over nonce, which only a holder of the
// token can compute. A listener posing as the daemon (say on
// its port while it restarts) sees the hash and cannot forge the proof, so
// it cannot vouch for a token it was never issued.
func (r *RunTokens) CheckHash(taskID, tokenHash, nonce string) (ref RunRef, proof string, err error) {
	if strings.TrimSpace(taskID) == "" {
		return RunRef{}, "", errors.New("no STAYPOINT_TASK_ID: ops tools run only inside a StayPoint run")
	}
	raw, derr := hex.DecodeString(tokenHash)
	if derr != nil || len(raw) != sha256.Size {
		return RunRef{}, "", fmt.Errorf("%s is not a live run's token (malformed hash)", RunTokenEnv)
	}
	var h [32]byte
	copy(h[:], raw)
	r.mu.Lock()
	e, ok := r.live[h]
	r.mu.Unlock()
	switch {
	case !ok:
		return RunRef{}, "", fmt.Errorf("%s is not a live run's token (the run ended, or it was never issued)", RunTokenEnv)
	case e.ref.TaskID != taskID:
		return RunRef{}, "", fmt.Errorf("%s does not belong to task %s", RunTokenEnv, taskID)
	}
	return e.ref, RunCheckProof(e.token, nonce, e.ref), nil
}

// TokenHash is what a client sends the daemon instead of its run token.
func TokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// RunCheckProof is the daemon's answer to a check: an HMAC keyed by the run
// token over the client's nonce and the run it names.
func RunCheckProof(token, nonce string, ref RunRef) string {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte("staypoint-run-check\x00" + nonce + "\x00" + ref.TaskID + "\x00" + ref.RunID))
	return hex.EncodeToString(m.Sum(nil))
}

// VerifyRunCheckProof reports whether proof is the daemon's answer for
// token, nonce and ref.
func VerifyRunCheckProof(token, nonce string, ref RunRef, proof string) bool {
	return hmac.Equal([]byte(RunCheckProof(token, nonce, ref)), []byte(proof))
}

// RemoveLegacyOpsKey deletes the ops_key file the first version left in
// dataDir; nothing reads it any more, and a key on disk only invites the
// confusion that it still matters.
func RemoveLegacyOpsKey(dataDir string) error {
	if dataDir == "" {
		return nil
	}
	err := os.Remove(filepath.Join(dataDir, legacyOpsKeyFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
