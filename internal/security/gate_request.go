package security

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// GateRequestStatus is the lifecycle state of a pending command-gate approval.
type GateRequestStatus string

const (
	GateRequestPending  GateRequestStatus = "pending"
	GateRequestApproved GateRequestStatus = "approved"
	GateRequestDenied   GateRequestStatus = "denied"
)

// ScriptHash pins a script file a gate request runs (STA-868).
type ScriptHash struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	// Trusted is true only when the hook pinned the command to run exactly
	// these bytes (PinCommand), so the hash describes what runs.
	Trusted bool `json:"trusted"`
	// Content is the script text (truncated) for reviewers.
	Content string `json:"content,omitempty"`
}

// GateRequest is a Board-approval record for a Red-tier command intercepted by
// the pre-tool hook. It persists across daemon restarts so the hook can always
// poll for a decision rather than auto-denying on timeout.
type GateRequest struct {
	ID        string            `json:"id"`
	Cmdline   string            `json:"cmdline"`
	Reasons   []string          `json:"reasons"`
	Status    GateRequestStatus `json:"status"`
	RunID     string            `json:"run_id,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	DecidedAt *time.Time        `json:"decided_at,omitempty"`

	// Context for allow-rule scopes and reviewers (STA-868).
	TaskID  string       `json:"task_id,omitempty"`
	Repo    string       `json:"repo,omitempty"`
	Org     string       `json:"org,omitempty"`
	CWD     string       `json:"cwd,omitempty"`
	Scripts []ScriptHash `json:"scripts,omitempty"`
	// DecidedBy is "board", "rule:<id>", or "" while pending.
	DecidedBy string `json:"decided_by,omitempty"`
}

// GateRequestInput is everything a new gate request records.
type GateRequestInput struct {
	Cmdline string
	Reasons []string
	RunID   string
	TaskID  string
	Repo    string
	Org     string
	CWD     string
	Scripts []ScriptHash
}

const gateRequestCols = `id, cmdline, reasons_json, run_id, status, created_at, decided_at,
	task_id, repo, org, cwd, scripts_json, decided_by`

// Execer is satisfied by *sql.DB and *sql.Tx.
type Execer interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// CreateGateRequest inserts a new pending gate request and returns its id.
func CreateGateRequest(db *sql.DB, cmdline string, reasons []string, runID string) (*GateRequest, error) {
	return InsertGateRequest(db, GateRequestInput{Cmdline: cmdline, Reasons: reasons, RunID: runID}, GateRequestPending, "")
}

// InsertGateRequest inserts a gate request with the given status. A status
// other than pending is decided at creation (an allow-rule match), so it gets
// decided_at and decidedBy.
func InsertGateRequest(db Execer, in GateRequestInput, status GateRequestStatus, decidedBy string) (*GateRequest, error) {
	id, err := genID()
	if err != nil {
		return nil, err
	}
	if in.Reasons == nil {
		in.Reasons = []string{}
	}
	if in.Scripts == nil {
		in.Scripts = []ScriptHash{}
	}
	rj, _ := json.Marshal(in.Reasons)
	sj, _ := json.Marshal(in.Scripts)
	now := time.Now().UTC()
	var decidedAt *string
	var decided *time.Time
	if status != GateRequestPending {
		s := now.Format(time.RFC3339Nano)
		decidedAt, decided = &s, &now
	}
	_, err = db.Exec(
		`INSERT INTO security_gate_requests (id, cmdline, reasons_json, run_id, status, created_at, decided_at,
			task_id, repo, org, cwd, scripts_json, decided_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, in.Cmdline, string(rj), in.RunID, string(status), now.Format(time.RFC3339Nano), decidedAt,
		in.TaskID, in.Repo, in.Org, in.CWD, string(sj), decidedBy,
	)
	if err != nil {
		return nil, fmt.Errorf("create gate request: %w", err)
	}
	return &GateRequest{
		ID: id, Cmdline: in.Cmdline, Reasons: in.Reasons, Status: status, RunID: in.RunID,
		CreatedAt: now, DecidedAt: decided, TaskID: in.TaskID, Repo: in.Repo, Org: in.Org,
		CWD: in.CWD, Scripts: in.Scripts, DecidedBy: decidedBy,
	}, nil
}

// GetGateRequest fetches a single gate request by id.
func GetGateRequest(db Execer, id string) (*GateRequest, error) {
	return scanGateRequest(db.QueryRow(`SELECT `+gateRequestCols+` FROM security_gate_requests WHERE id = ?`, id))
}

// ListPendingGateRequests returns all requests still awaiting a decision.
func ListPendingGateRequests(db *sql.DB) ([]*GateRequest, error) {
	return queryGateRequests(db, `SELECT `+gateRequestCols+`
		 FROM security_gate_requests WHERE status = 'pending' ORDER BY created_at ASC`)
}

// ListGateRequests returns the newest 100 requests with status ("all" for any).
func ListGateRequests(db *sql.DB, status string) ([]*GateRequest, error) {
	if status == "all" {
		return queryGateRequests(db, `SELECT `+gateRequestCols+`
			 FROM security_gate_requests ORDER BY created_at DESC LIMIT 100`)
	}
	return queryGateRequests(db, `SELECT `+gateRequestCols+`
		 FROM security_gate_requests WHERE status = ? ORDER BY created_at DESC LIMIT 100`, status)
}

func queryGateRequests(db *sql.DB, q string, args ...any) ([]*GateRequest, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*GateRequest
	for rows.Next() {
		r, err := scanGateRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DecideGateRequest sets the status to approved or denied and records the timestamp.
func DecideGateRequest(db *sql.DB, id string, approved bool) (*GateRequest, error) {
	return DecideGateRequestTx(db, id, approved, "board")
}

// DecideGateRequestTx decides a pending request through db (a *sql.Tx when the
// caller must commit the decision together with its audit rows).
func DecideGateRequestTx(db Execer, id string, approved bool, decidedBy string) (*GateRequest, error) {
	status := GateRequestDenied
	if approved {
		status = GateRequestApproved
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := db.Exec(
		`UPDATE security_gate_requests SET status = ?, decided_at = ?, decided_by = ?
		 WHERE id = ? AND status = 'pending'`,
		string(status), now, decidedBy, id)
	if err != nil {
		return nil, fmt.Errorf("decide gate request: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, fmt.Errorf("gate request %s not found or already decided", id)
	}
	return GetGateRequest(db, id)
}

type scanner interface {
	Scan(dest ...any) error
}

func scanGateRequest(row scanner) (*GateRequest, error) {
	var (
		r         GateRequest
		rj, sj    string
		runID     sql.NullString
		createdAt string
		decidedAt sql.NullString
	)
	if err := row.Scan(&r.ID, &r.Cmdline, &rj, &runID, &r.Status, &createdAt, &decidedAt,
		&r.TaskID, &r.Repo, &r.Org, &r.CWD, &sj, &r.DecidedBy); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	_ = json.Unmarshal([]byte(rj), &r.Reasons)
	_ = json.Unmarshal([]byte(sj), &r.Scripts)
	r.RunID = runID.String
	if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
		r.CreatedAt = t
	}
	if decidedAt.Valid {
		if t, err := time.Parse(time.RFC3339Nano, decidedAt.String); err == nil {
			r.DecidedAt = &t
		}
	}
	return &r, nil
}

func genID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
