// Package geminiapproval is the Board Touch ID (passkey) approval that lets
// Gemini write code in a PERSONAL repo (Board rule, 2026-10-06). In a work repo
// Gemini never writes code, approval or not: every function here refuses work
// repos, and callers re-check the repo at use time, so a forged approval row
// in the database grants nothing there.
//
// Approvals reuse the security gate-request flow (security_gate_requests with
// run_id RunID): an agent or the CLI may file a request, only the Board may
// approve it (POST /api/security/gate-requests/{id}/decide is wrapped by
// WrapBoardAction: Board cookie + passkey), and decisions are audit-logged.
//
// Scope of one approval:
//   - a daemon task run: one run. The daemon consumes the approval when the
//     run starts (gemini_code_grant_uses); the next run needs a new one.
//   - an interactive agy session: one conversationId, for at most
//     MaxSessionHours after approval.
package geminiapproval

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/security"
)

const (
	// RunID marks a security gate request as a Gemini-code approval.
	RunID = "gemini-code"
	// MaxSessionHours caps an interactive session approval.
	MaxSessionHours = 8
	// WaitingReason prefixes the block reason of a task held for approval.
	WaitingReason = "Waiting for Board Touch ID: Gemini code"
	// DeniedReason prefixes the block reason of a task whose request was denied.
	DeniedReason = "Board denied Gemini code"
)

// ErrWorkRepo is returned for any approval in a work repo.
var ErrWorkRepo = errors.New("Gemini never writes code in a work repo, even with Board approval")

// Scope is what one approval covers: a task run or an interactive session.
type Scope struct {
	TaskID    string
	SessionID string
	Repo      string
}

// Cmdline is the gate request text for scope (parsed back by Parse).
func (s Scope) Cmdline() string {
	if s.TaskID != "" {
		return fmt.Sprintf("staypoint gate gemini-code --task %s --repo %q", s.TaskID, s.Repo)
	}
	return fmt.Sprintf("staypoint gate gemini-code --session %s --repo %q", s.SessionID, s.Repo)
}

// Title is the human summary shown to the Board.
func (s Scope) Title() string {
	if s.TaskID != "" {
		return fmt.Sprintf("Gemini code: %s in %s", s.TaskID, s.Repo)
	}
	return fmt.Sprintf("Gemini code: agy session %s in %s (up to %dh)", s.SessionID, s.Repo, MaxSessionHours)
}

var (
	idRe      = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	cmdlineRe = regexp.MustCompile(`^staypoint gate gemini-code --(task|session) ([A-Za-z0-9._:-]{1,128}) --repo ("(?:[^"\\]|\\.)*")$`)
)

// Parse extracts the scope from a gate request cmdline.
func Parse(cmdline string) (Scope, bool) {
	m := cmdlineRe.FindStringSubmatch(strings.TrimSpace(cmdline))
	if m == nil {
		return Scope{}, false
	}
	repo, err := strconv.Unquote(m[3])
	if err != nil || repo == "" {
		return Scope{}, false
	}
	s := Scope{Repo: repo}
	if m[1] == "task" {
		s.TaskID = m[2]
	} else {
		s.SessionID = m[2]
	}
	return s, true
}

// Validate checks a scope before a request is filed or approved.
func Validate(s Scope, isWork func(string) bool) error {
	if (s.TaskID == "") == (s.SessionID == "") {
		return errors.New("gemini-code approval needs exactly one of a task or a session")
	}
	id := s.TaskID + s.SessionID
	if !idRe.MatchString(id) {
		return fmt.Errorf("invalid id %q", id)
	}
	if strings.TrimSpace(s.Repo) == "" {
		return errors.New("gemini-code approval needs a repo path")
	}
	if isWork == nil || isWork(s.Repo) {
		return ErrWorkRepo
	}
	return nil
}

// Request files a pending Board approval for scope. Work repos are refused.
func Request(conn *sql.DB, s Scope, isWork func(string) bool) (*security.GateRequest, error) {
	if err := Validate(s, isWork); err != nil {
		return nil, err
	}
	reasons := []string{
		s.Title(),
		"Board rule: Gemini never writes code; in a personal repo the Board may allow it for this one task run or agy session. Work repos: never.",
	}
	return security.CreateGateRequest(conn, s.Cmdline(), reasons, RunID)
}

// State is the approval state of a task's latest unused request.
type State string

const (
	StateNone     State = ""
	StatePending  State = "pending"
	StateApproved State = "approved"
	StateDenied   State = "denied"
)

// TaskGrant is a task's latest request not yet consumed by a run.
type TaskGrant struct {
	GateID string
	State  State
}

// LatestTaskGrant returns the newest request for taskID in repo that no run
// has consumed. A request for a work repo never counts (forged rows included).
func LatestTaskGrant(conn *sql.DB, taskID, repo string, isWork func(string) bool) (TaskGrant, error) {
	if isWork == nil || isWork(repo) {
		return TaskGrant{}, ErrWorkRepo
	}
	rows, err := conn.Query(
		`SELECT r.id, r.cmdline, r.status FROM security_gate_requests r
		 LEFT JOIN gemini_code_grant_uses u ON u.gate_request_id = r.id
		 WHERE r.run_id = ? AND u.gate_request_id IS NULL
		 ORDER BY r.created_at DESC`, RunID)
	if err != nil {
		return TaskGrant{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, cmdline, status string
		if err := rows.Scan(&id, &cmdline, &status); err != nil {
			return TaskGrant{}, err
		}
		s, ok := Parse(cmdline)
		if !ok || s.TaskID != taskID || s.Repo != repo || isWork(s.Repo) {
			continue
		}
		return TaskGrant{GateID: id, State: State(status)}, nil
	}
	return TaskGrant{}, rows.Err()
}

// Consume marks a decided request as used by runID, so it covers exactly one
// run (an approval) or refuses exactly one run (a denial). It fails if the
// request was already consumed.
func Consume(conn *sql.DB, gateID, taskID, runID string) error {
	res, err := conn.Exec(
		`INSERT OR IGNORE INTO gemini_code_grant_uses (gate_request_id, task_id, run_id) VALUES (?, ?, ?)`,
		gateID, taskID, runID)
	if err != nil {
		return fmt.Errorf("consume gemini-code approval: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("gemini-code approval %s was already used", gateID)
	}
	return nil
}

// SessionApproved reports whether the Board approved Gemini code for an
// interactive agy session in repo within the last MaxSessionHours. Work
// repos are never approved.
func SessionApproved(conn *sql.DB, sessionID, repo string, isWork func(string) bool, now time.Time) (bool, error) {
	if strings.TrimSpace(sessionID) == "" || isWork == nil || isWork(repo) {
		return false, nil
	}
	since := now.Add(-MaxSessionHours * time.Hour).UTC().Format(time.RFC3339Nano)
	rows, err := conn.Query(
		`SELECT cmdline, decided_at FROM security_gate_requests
		 WHERE run_id = ? AND status = 'approved' AND decided_at IS NOT NULL AND decided_at >= ?`,
		RunID, since)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cmdline, decided string
		if err := rows.Scan(&cmdline, &decided); err != nil {
			return false, err
		}
		s, ok := Parse(cmdline)
		if !ok || s.SessionID != sessionID || isWork(s.Repo) {
			continue
		}
		if s.Repo == repo || strings.HasPrefix(repo, strings.TrimSuffix(s.Repo, "/")+"/") {
			at, err := time.Parse(time.RFC3339Nano, decided)
			if err == nil && now.Before(at.Add(MaxSessionHours*time.Hour)) {
				return true, nil
			}
		}
	}
	return false, rows.Err()
}

// RequestCommand is the exact command an agy session runs to ask the Board.
func RequestCommand(sessionID string) string {
	return "staypoint gate gemini-code --session " + sessionID
}
