package telemetry

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/alerts"
)

var whitespaceRegex = regexp.MustCompile(`\s+`)

type CircuitBreaker struct {
	SessionID        string     `json:"session_id"`
	RepoPath         string     `json:"repo_path"`
	AgentType        string     `json:"agent_type"`
	IsTripped        bool       `json:"is_tripped"`
	TripCount        int        `json:"trip_count"`
	FailureSignature string     `json:"failure_signature"`
	FailingTool      string     `json:"failing_tool"`
	FailingCommand   string     `json:"failing_command"`
	LastError        string     `json:"last_error"`
	TrippedAt        *time.Time `json:"tripped_at,omitempty"`
	ClearedAt        *time.Time `json:"cleared_at,omitempty"`
}

type FailureRecord struct {
	Tool      string
	Command   string
	Signature string
	ErrorText string
	Timestamp time.Time
}

type BreakerTracker struct {
	mu       sync.Mutex
	failures map[string][]FailureRecord
}

func NewBreakerTracker() *BreakerTracker {
	return &BreakerTracker{
		failures: make(map[string][]FailureRecord),
	}
}

// ComputeSignature calculates a stable signature for a failing tool execution
func ComputeSignature(tool, command, errText string) string {
	toolNorm := strings.ToLower(strings.TrimSpace(tool))
	cmdNorm := strings.ToLower(strings.TrimSpace(whitespaceRegex.ReplaceAllString(command, " ")))
	if len(cmdNorm) > 120 {
		cmdNorm = cmdNorm[:120]
	}
	errNorm := strings.ToLower(strings.TrimSpace(whitespaceRegex.ReplaceAllString(errText, " ")))
	if len(errNorm) > 120 {
		errNorm = errNorm[:120]
	}

	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%s:%s:%s", toolNorm, cmdNorm, errNorm)))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// RecordFailure records a tool or command failure.
// Returns (isTripped, tripReason, error).
func (b *BreakerTracker) RecordFailure(meshDB *sql.DB, sessionID, repoPath, agentType, tool, command, errText string) (bool, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now().UTC()
	sig := ComputeSignature(tool, command, errText)

	rec := FailureRecord{
		Tool:      tool,
		Command:   command,
		Signature: sig,
		ErrorText: errText,
		Timestamp: now,
	}

	// Filter out events older than 5 minutes
	cutoff := now.Add(-5 * time.Minute)
	var active []FailureRecord
	for _, f := range b.failures[sessionID] {
		if f.Timestamp.After(cutoff) {
			active = append(active, f)
		}
	}
	active = append(active, rec)
	b.failures[sessionID] = active

	// Check 1: Consecutive identical failures (>= 3)
	consecutiveSame := 0
	for i := len(active) - 1; i >= 0; i-- {
		if active[i].Signature == sig {
			consecutiveSame++
		} else {
			break
		}
	}

	isTripped := false
	var reason string
	// alertReason is reason without the command or error text, which can
	// hold tokens or env values. It is what reaches the Board alert feed.
	var alertReason string

	if consecutiveSame >= 3 {
		isTripped = true
		reason = fmt.Sprintf("Repeating failure loop: %d consecutive identical failures of tool %q (%s)", consecutiveSame, tool, command)
		alertReason = fmt.Sprintf("repeating failure loop, %d consecutive identical failures of tool %q", consecutiveSame, tool)
	} else if len(active) >= 5 {
		// Check 2: High frequency burst failure (>= 5 failures within 5 minutes)
		isTripped = true
		reason = fmt.Sprintf("Failure spiral: %d errors encountered in under 5 minutes (last: %s)", len(active), tool)
		alertReason = fmt.Sprintf("failure spiral, %d errors in under 5 minutes (last tool %q)", len(active), tool)
	}

	if isTripped && meshDB != nil {
		nowStr := now.Format(time.RFC3339Nano)
		query := `
		INSERT INTO agent_circuit_breakers (
			session_id, repo_path, agent_type, is_tripped, trip_count,
			failure_signature, failing_tool, failing_command, last_error, tripped_at
		) VALUES (?, ?, ?, 1, 1, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			is_tripped = 1,
			trip_count = trip_count + 1,
			failure_signature = excluded.failure_signature,
			failing_tool = excluded.failing_tool,
			failing_command = excluded.failing_command,
			last_error = excluded.last_error,
			tripped_at = excluded.tripped_at;
		`
		if _, err := meshDB.Exec(query, sessionID, repoPath, agentType, sig, tool, command, errText, nowStr); err != nil {
			return true, reason, fmt.Errorf("failed to persist circuit breaker trip: %w", err)
		}

		repoName := filepath.Base(repoPath)
		if repoName == "" || repoName == "." {
			repoName = "workspace"
		}
		SendAlert(alerts.Alert{
			Kind:      "circuit_breaker_tripped",
			Severity:  alerts.SeverityCritical,
			Title:     "[Staypoint] Circuit Breaker Tripped!",
			Message:   fmt.Sprintf("Agent %s in %s paused: %s", agentType, repoName, alertReason),
			DedupeKey: "circuit_breaker:" + sessionID,
		})
	}

	return isTripped, reason, nil
}

// RecordSuccess clears in-memory failure streak when agent tool succeeds.
func (b *BreakerTracker) RecordSuccess(meshDB *sql.DB, sessionID string) error {
	b.mu.Lock()
	delete(b.failures, sessionID)
	b.mu.Unlock()
	return nil
}

// ResetCircuitBreaker manually resets a tripped circuit breaker.
func (b *BreakerTracker) ResetCircuitBreaker(meshDB *sql.DB, sessionID string) error {
	b.mu.Lock()
	delete(b.failures, sessionID)
	b.mu.Unlock()

	if meshDB == nil {
		return nil
	}

	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	query := `
	UPDATE agent_circuit_breakers
	SET is_tripped = 0, cleared_at = ?
	WHERE session_id = ?;
	`
	_, err := meshDB.Exec(query, nowStr, sessionID)
	return err
}

// GetCircuitBreaker returns breaker status for a specific session.
func GetCircuitBreaker(meshDB *sql.DB, sessionID string) (*CircuitBreaker, error) {
	if meshDB == nil {
		return nil, nil
	}

	query := `
	SELECT session_id, repo_path, agent_type, is_tripped, trip_count,
	       COALESCE(failure_signature, ''), COALESCE(failing_tool, ''),
	       COALESCE(failing_command, ''), COALESCE(last_error, ''),
	       tripped_at, cleared_at
	FROM agent_circuit_breakers
	WHERE session_id = ?;
	`
	row := meshDB.QueryRow(query, sessionID)
	var cb CircuitBreaker
	var isTrippedInt int
	var trippedAtStr, clearedAtStr sql.NullString

	err := row.Scan(
		&cb.SessionID, &cb.RepoPath, &cb.AgentType, &isTrippedInt, &cb.TripCount,
		&cb.FailureSignature, &cb.FailingTool, &cb.FailingCommand, &cb.LastError,
		&trippedAtStr, &clearedAtStr,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	cb.IsTripped = isTrippedInt == 1
	if trippedAtStr.Valid && trippedAtStr.String != "" {
		if t, err := time.Parse(time.RFC3339Nano, trippedAtStr.String); err == nil {
			cb.TrippedAt = &t
		}
	}
	if clearedAtStr.Valid && clearedAtStr.String != "" {
		if t, err := time.Parse(time.RFC3339Nano, clearedAtStr.String); err == nil {
			cb.ClearedAt = &t
		}
	}

	return &cb, nil
}

// ListCircuitBreakers lists circuit breakers for a repo or all repos.
func ListCircuitBreakers(meshDB *sql.DB, repoPath string, activeOnly bool) ([]CircuitBreaker, error) {
	if meshDB == nil {
		return nil, nil
	}

	query := `
	SELECT session_id, repo_path, agent_type, is_tripped, trip_count,
	       COALESCE(failure_signature, ''), COALESCE(failing_tool, ''),
	       COALESCE(failing_command, ''), COALESCE(last_error, ''),
	       tripped_at, cleared_at
	FROM agent_circuit_breakers
	WHERE 1=1
	`
	var args []interface{}
	if repoPath != "" {
		query += " AND (repo_path = ? OR ? LIKE repo_path || '%')"
		args = append(args, repoPath, repoPath)
	}
	if activeOnly {
		query += " AND is_tripped = 1"
	}
	query += " ORDER BY tripped_at DESC;"

	rows, err := meshDB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []CircuitBreaker
	for rows.Next() {
		var cb CircuitBreaker
		var isTrippedInt int
		var trippedAtStr, clearedAtStr sql.NullString

		if err := rows.Scan(
			&cb.SessionID, &cb.RepoPath, &cb.AgentType, &isTrippedInt, &cb.TripCount,
			&cb.FailureSignature, &cb.FailingTool, &cb.FailingCommand, &cb.LastError,
			&trippedAtStr, &clearedAtStr,
		); err != nil {
			continue
		}

		cb.IsTripped = isTrippedInt == 1
		if trippedAtStr.Valid && trippedAtStr.String != "" {
			if t, err := time.Parse(time.RFC3339Nano, trippedAtStr.String); err == nil {
				cb.TrippedAt = &t
			}
		}
		if clearedAtStr.Valid && clearedAtStr.String != "" {
			if t, err := time.Parse(time.RFC3339Nano, clearedAtStr.String); err == nil {
				cb.ClearedAt = &t
			}
		}
		result = append(result, cb)
	}

	return result, nil
}
