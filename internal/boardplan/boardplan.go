// Package boardplan stores Board action plans (task-e1b24d66).
//
// A Board session proposes a batch of Board actions (Approve & Merge, Send
// Back, Mark done, Run Now, Cancel, gate approve/deny, Unblock). Proposing
// changes nothing. The Board reviews the plan on one page, unticks any rows
// it does not want, and signs the selected rows with one passkey assertion.
// The signature covers SelectionHash: the plan id, its content hash and the
// exact selected rows, so a plan edited after signing, a different selection
// or another plan never matches. A plan executes at most once.
package boardplan

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Action kinds a plan may hold. Each maps to an existing per-task button.
const (
	ApproveMerge = "approve_merge"
	SendBack     = "send_back"
	MarkDone     = "mark_done"
	RunNow       = "run_now"
	Cancel       = "cancel"
	GateApprove  = "gate_approve"
	GateDeny     = "gate_deny"
	Unblock      = "unblock"
)

// Plan statuses.
const (
	StatusPending   = "pending"
	StatusExecuting = "executing"
	StatusExecuted  = "executed"
	StatusDiscarded = "discarded"
)

// MaxActions caps one plan; a plan is meant to replace a handful of clicks.
const MaxActions = 50

var (
	ErrNotFound   = errors.New("board plan not found")
	ErrNotPending = errors.New("board plan is not pending")
	ErrChanged    = errors.New("board plan changed since it was signed")
)

// Action is one row of a plan.
type Action struct {
	TaskID          string `json:"task_id"`
	Action          string `json:"action"`
	Text            string `json:"text,omitempty"`
	GateID          string `json:"gate_id,omitempty"`
	ExpectedHeadSHA string `json:"expected_head_sha,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

// Result is one executed row's outcome.
type Result struct {
	Index  int    `json:"index"`
	Status string `json:"status"` // ok, failed
	HTTP   int    `json:"http_status,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Plan is a stored plan.
type Plan struct {
	ID               string     `json:"id"`
	Proposer         string     `json:"proposer"`
	ProposerKind     string     `json:"proposer_kind"`
	Actions          []Action   `json:"actions"`
	ContentHash      string     `json:"content_hash"`
	Status           string     `json:"status"`
	Selected         []int      `json:"selected,omitempty"`
	SelectionHash    string     `json:"selection_hash,omitempty"`
	SignerCredential string     `json:"signer_credential,omitempty"`
	Results          []Result   `json:"results,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	ExecutedAt       *time.Time `json:"executed_at,omitempty"`
}

// Validate checks every action is complete for its kind.
func Validate(actions []Action) error {
	if len(actions) == 0 {
		return errors.New("plan has no actions")
	}
	if len(actions) > MaxActions {
		return fmt.Errorf("plan has %d actions; the limit is %d", len(actions), MaxActions)
	}
	for i, a := range actions {
		row := fmt.Sprintf("action %d (%s)", i+1, a.Action)
		switch a.Action {
		case GateApprove, GateDeny:
			if strings.TrimSpace(a.GateID) == "" {
				return fmt.Errorf("%s: gate_id is required", row)
			}
		case ApproveMerge, SendBack, MarkDone, RunNow, Cancel, Unblock:
			if strings.TrimSpace(a.TaskID) == "" {
				return fmt.Errorf("%s: task_id is required", row)
			}
		default:
			return fmt.Errorf("action %d: unknown action %q", i+1, a.Action)
		}
		if a.Action == ApproveMerge && strings.TrimSpace(a.ExpectedHeadSHA) == "" {
			return fmt.Errorf("%s: expected_head_sha is required", row)
		}
		if a.Action == SendBack && strings.TrimSpace(a.Text) == "" {
			return fmt.Errorf("%s: text (the send-back comment) is required", row)
		}
	}
	return nil
}

// ContentHash is the sha256 of the plan's canonical JSON actions.
func ContentHash(actions []Action) string {
	b, _ := json.Marshal(actions)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// NormalizeSelection sorts and de-duplicates selected row indexes and checks
// they are in range. An empty selection is refused.
func NormalizeSelection(selected []int, n int) ([]int, error) {
	if len(selected) == 0 {
		return nil, errors.New("no actions selected")
	}
	seen := map[int]bool{}
	out := make([]int, 0, len(selected))
	for _, i := range selected {
		if i < 0 || i >= n {
			return nil, fmt.Errorf("selected index %d out of range", i)
		}
		if !seen[i] {
			seen[i] = true
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out, nil
}

// SelectionHash is what the Board's passkey signs: the plan id, its content
// hash and the selected rows themselves (not just their indexes), so it is
// different for any other plan, any edit and any other selection.
func SelectionHash(p *Plan, selected []int) []byte {
	rows := make([]struct {
		Index  int    `json:"i"`
		Action Action `json:"a"`
	}, len(selected))
	for k, i := range selected {
		rows[k].Index, rows[k].Action = i, p.Actions[i]
	}
	b, _ := json.Marshal(rows)
	h := sha256.New()
	fmt.Fprintf(h, "staypoint-board-plan-v1\n%s\n%s\n", p.ID, p.ContentHash)
	h.Write(b)
	return h.Sum(nil)
}

// Create stores a new pending plan.
func Create(db *sql.DB, proposer, proposerKind string, actions []Action) (*Plan, error) {
	if err := Validate(actions); err != nil {
		return nil, err
	}
	for i := range actions {
		a := &actions[i]
		a.TaskID, a.GateID, a.ExpectedHeadSHA = strings.TrimSpace(a.TaskID), strings.TrimSpace(a.GateID), strings.TrimSpace(a.ExpectedHeadSHA)
	}
	b, err := json.Marshal(actions)
	if err != nil {
		return nil, err
	}
	id := "plan-" + uuid.New().String()[:8]
	hash := ContentHash(actions)
	if _, err := db.Exec(
		`INSERT INTO board_action_plans (id, proposer, proposer_kind, actions_json, content_hash) VALUES (?, ?, ?, ?, ?)`,
		id, proposer, proposerKind, string(b), hash,
	); err != nil {
		return nil, err
	}
	return Get(db, id)
}

const cols = `id, proposer, proposer_kind, actions_json, content_hash, status,
	COALESCE(selected_json,''), COALESCE(selection_hash,''), COALESCE(signer_credential,''),
	COALESCE(results_json,''), created_at, COALESCE(executed_at,'')`

type scanner interface{ Scan(...any) error }

func scan(s scanner) (*Plan, error) {
	var p Plan
	var actions, selected, results, created, executed string
	if err := s.Scan(&p.ID, &p.Proposer, &p.ProposerKind, &actions, &p.ContentHash, &p.Status,
		&selected, &p.SelectionHash, &p.SignerCredential, &results, &created, &executed); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(actions), &p.Actions); err != nil {
		return nil, fmt.Errorf("plan %s: bad actions_json: %w", p.ID, err)
	}
	if selected != "" {
		_ = json.Unmarshal([]byte(selected), &p.Selected)
	}
	if results != "" {
		_ = json.Unmarshal([]byte(results), &p.Results)
	}
	p.CreatedAt = parseTime(created)
	if executed != "" {
		t := parseTime(executed)
		p.ExecutedAt = &t
	}
	return &p, nil
}

func parseTime(s string) time.Time {
	for _, layout := range []string{"2006-01-02T15:04:05.000Z", time.RFC3339Nano, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// Get loads a plan.
func Get(db *sql.DB, id string) (*Plan, error) {
	p, err := scan(db.QueryRow(`SELECT `+cols+` FROM board_action_plans WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// List returns plans, newest first; status "" lists all.
func List(db *sql.DB, status string, limit int) ([]*Plan, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT ` + cols + ` FROM board_action_plans`
	args := []any{}
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Plan{}
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Claim moves a pending plan to executing, recording the signed selection.
// It succeeds only while the stored content hash still equals contentHash, so
// a plan edited after signing, or one already executed (a replay), is refused.
func Claim(db *sql.DB, id, contentHash string, selected []int, selectionHash, signer string) error {
	sel, _ := json.Marshal(selected)
	res, err := db.Exec(`UPDATE board_action_plans
		SET status = 'executing', selected_json = ?, selection_hash = ?, signer_credential = ?
		WHERE id = ? AND status = 'pending' AND content_hash = ?`,
		string(sel), selectionHash, signer, id, contentHash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	p, err := Get(db, id)
	if err != nil {
		return err
	}
	if p.Status != StatusPending {
		return ErrNotPending
	}
	return ErrChanged
}

// Finish records the per-row results and marks the plan executed.
func Finish(db *sql.DB, id string, results []Result) error {
	b, _ := json.Marshal(results)
	_, err := db.Exec(`UPDATE board_action_plans
		SET status = 'executed', results_json = ?, executed_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ? AND status = 'executing'`, string(b), id)
	return err
}

// RecordResults saves the results so far of a plan that is executing.
func RecordResults(db *sql.DB, id string, results []Result) error {
	b, _ := json.Marshal(results)
	_, err := db.Exec(`UPDATE board_action_plans SET results_json = ? WHERE id = ? AND status = 'executing'`, string(b), id)
	return err
}

// SweepInterrupted closes plans a previous daemon process left executing.
// Selected rows with no recorded result get status "unknown": the row may
// or may not have run, so the Board checks the task before retrying. Returns
// how many plans it closed. Call once at startup, before any plan executes.
func SweepInterrupted(db *sql.DB) (int64, error) {
	plans, err := List(db, StatusExecuting, 1000)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, p := range plans {
		done := map[int]bool{}
		for _, r := range p.Results {
			done[r.Index] = true
		}
		results := p.Results
		for _, i := range p.Selected {
			if !done[i] {
				results = append(results, Result{Index: i, Status: "unknown",
					Error: "interrupted: the daemon stopped before this row reported; check the task before retrying"})
			}
		}
		if err := Finish(db, p.ID, results); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Discard drops a pending plan without running anything.
func Discard(db *sql.DB, id string) error {
	res, err := db.Exec(`UPDATE board_action_plans SET status = 'discarded' WHERE id = ? AND status = 'pending'`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := Get(db, id); err != nil {
			return err
		}
		return ErrNotPending
	}
	return nil
}
