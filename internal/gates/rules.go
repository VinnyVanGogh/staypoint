// Package gates holds the Board-side logic around security gate requests
// (STA-868): allow rules ("Approve & remember"), the request context they are
// scoped by, advisory recommendations (Together per request, Gemini batch
// review) and the decision log that compares them with the Board.
//
// Nothing here decides a request on its own except a Board-created allow
// rule; advisors only write decision_log rows.
package gates

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/security"
)

// Rule scopes.
const (
	ScopeTask = "task"
	ScopeRepo = "repo"
	ScopeOrg  = "org"
)

// Match kinds. MatchAny (trust.go) is created only by "Trust this task".
const (
	MatchExact  = "exact"
	MatchPrefix = "prefix"
)

// Rule is a Board allow rule: a Red-tier request whose normalized command
// matches Pattern, whose reasons are all among Reasons, whose pinned scripts
// still hash the same, and whose task/repo/org equals ScopeValue is approved
// without waiting for the Board.
type Rule struct {
	ID           int64                 `json:"id"`
	Pattern      string                `json:"pattern"`
	MatchKind    string                `json:"match_kind"`
	Reasons      []string              `json:"reasons"`
	Scripts      []security.ScriptHash `json:"scripts"`
	Scope        string                `json:"scope"`
	ScopeValue   string                `json:"scope_value"`
	SourceGateID string                `json:"source_gate_id,omitempty"`
	Note         string                `json:"note,omitempty"`
	CreatedBy    string                `json:"created_by"`
	CreatedAt    time.Time             `json:"created_at"`
	ExpiresAt    *time.Time            `json:"expires_at,omitempty"`
	DeletedAt    *time.Time            `json:"deleted_at,omitempty"`
	HitCount     int                   `json:"hit_count"`
	LastHitAt    *time.Time            `json:"last_hit_at,omitempty"`
	// Trust rules only (MatchAny): tev1 decides instead of a blanket
	// approval, at this threshold. EndedReason says why a trust ended early.
	Tev1          bool    `json:"tev1,omitempty"`
	Tev1Threshold float64 `json:"tev1_threshold,omitempty"`
	EndedReason   string  `json:"ended_reason,omitempty"`
}

// Execer is satisfied by *sql.DB and *sql.Tx.
type Execer interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// SpecialRunIDs are gate requests that carry Board policy decisions (Gemini
// code grants, tracking-gate overrides). Rules never approve them.
var SpecialRunIDs = map[string]bool{"gemini-code": true, "tracking-gate-override": true}

// NormalizeCommand collapses runs of spaces and tabs and trims each line.
// Newlines are kept: they separate commands, so "a\nb" and "a b" differ.
func NormalizeCommand(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.Join(strings.Fields(l), " ")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// prefixTailSafe reports whether text after a prefix pattern only adds plain
// arguments: no new command, pipe, redirect, or substitution.
func prefixTailSafe(tail string) bool {
	return !strings.ContainsAny(tail, ";&|<>`\n\\") && !strings.Contains(tail, "$(")
}

// ErrScopeUnavailable is returned when a request has no value for a scope
// (no task attached, no repo resolved).
var ErrScopeUnavailable = errors.New("request has no value for that scope")

// ScopeValueFor returns the request's task, repo or org.
func ScopeValueFor(gr *security.GateRequest, scope string) (string, error) {
	var v string
	switch scope {
	case ScopeTask:
		v = gr.TaskID
	case ScopeRepo:
		v = gr.Repo
	case ScopeOrg:
		v = gr.Org
	default:
		return "", fmt.Errorf("unknown scope %q", scope)
	}
	if v == "" {
		return "", ErrScopeUnavailable
	}
	return v, nil
}

// RuleSpec is what the Board picks when remembering a request.
type RuleSpec struct {
	Scope     string `json:"scope"`
	MatchKind string `json:"match_kind"`
	// Pattern overrides the request's command for a prefix rule; it must be a
	// prefix of the normalized command.
	Pattern          string `json:"pattern"`
	ExpiresInMinutes int    `json:"expires_in_minutes"`
	Note             string `json:"note"`
}

// RuleFromRequest builds a rule for gr. Its scripts are pinned by the hashes
// of the bytes the hook snapshotted and pinned the command to; a request whose
// scripts were not pinned (old hook, unreadable, ambiguous command) cannot be
// remembered.
func RuleFromRequest(gr *security.GateRequest, spec RuleSpec, now time.Time) (Rule, error) {
	if SpecialRunIDs[gr.RunID] {
		return Rule{}, errors.New("policy requests (Gemini code, tracking override) cannot be remembered")
	}
	val, err := ScopeValueFor(gr, spec.Scope)
	if err != nil {
		return Rule{}, err
	}
	kind := spec.MatchKind
	if kind == "" {
		kind = MatchExact
	}
	norm := NormalizeCommand(gr.Cmdline)
	pattern := norm
	switch kind {
	case MatchExact:
	case MatchPrefix:
		pattern = NormalizeCommand(strings.TrimSuffix(strings.TrimSpace(spec.Pattern), "*"))
		if pattern == "" || !strings.HasPrefix(norm, pattern) || !prefixTailSafe(norm[len(pattern):]) {
			return Rule{}, errors.New("prefix pattern must be a prefix of the command followed only by plain arguments")
		}
		if strings.ContainsAny(pattern, "\n") {
			return Rule{}, errors.New("prefix rules must be a single line")
		}
	default:
		return Rule{}, fmt.Errorf("unknown match kind %q", kind)
	}
	pinned := make([]security.ScriptHash, 0, len(gr.Scripts))
	for _, s := range gr.Scripts {
		if !s.Trusted || s.SHA256 == "" {
			return Rule{}, fmt.Errorf("script %s was not pinned to the bytes reviewed, so it cannot be remembered", s.Path)
		}
		pinned = append(pinned, security.ScriptHash{Path: s.Path, SHA256: s.SHA256, Trusted: true})
	}
	r := Rule{
		Pattern: pattern, MatchKind: kind, Reasons: append([]string{}, gr.Reasons...), Scripts: pinned,
		Scope: spec.Scope, ScopeValue: val, SourceGateID: gr.ID, Note: spec.Note, CreatedBy: "board",
		CreatedAt: now.UTC(),
	}
	if spec.ExpiresInMinutes < 0 {
		return Rule{}, errors.New("expiry must not be negative")
	}
	if spec.ExpiresInMinutes > 0 {
		t := now.UTC().Add(time.Duration(spec.ExpiresInMinutes) * time.Minute)
		r.ExpiresAt = &t
	}
	return r, nil
}

func fmtTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// InsertRule stores r and returns it with its id.
func InsertRule(db Execer, r Rule) (*Rule, error) {
	if r.Reasons == nil {
		r.Reasons = []string{}
	}
	if r.Scripts == nil {
		r.Scripts = []security.ScriptHash{}
	}
	rj, _ := json.Marshal(r.Reasons)
	sj, _ := json.Marshal(r.Scripts)
	tev1 := 0
	if r.Tev1 {
		tev1 = 1
	}
	res, err := db.Exec(`INSERT INTO security_gate_rules
		(pattern, match_kind, reasons_json, scripts_json, scope, scope_value, source_gate_id, note, created_by, created_at, expires_at,
		 tev1, tev1_threshold)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Pattern, r.MatchKind, string(rj), string(sj), r.Scope, r.ScopeValue, r.SourceGateID, r.Note,
		r.CreatedBy, r.CreatedAt.UTC().Format(time.RFC3339Nano), fmtTime(r.ExpiresAt), tev1, r.Tev1Threshold)
	if err != nil {
		return nil, fmt.Errorf("insert gate rule: %w", err)
	}
	r.ID, _ = res.LastInsertId()
	return &r, nil
}

const ruleCols = `id, pattern, match_kind, reasons_json, scripts_json, scope, scope_value, source_gate_id, note,
	created_by, created_at, expires_at, deleted_at, hit_count, last_hit_at, tev1, tev1_threshold, ended_reason`

func parseTime(ns sql.NullString) *time.Time {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, ns.String)
	if err != nil {
		return nil
	}
	return &t
}

func scanRule(row interface{ Scan(...any) error }) (*Rule, error) {
	var (
		r                         Rule
		rj, sj, created           string
		expires, deleted, lastHit sql.NullString
	)
	if err := row.Scan(&r.ID, &r.Pattern, &r.MatchKind, &rj, &sj, &r.Scope, &r.ScopeValue, &r.SourceGateID,
		&r.Note, &r.CreatedBy, &created, &expires, &deleted, &r.HitCount, &lastHit,
		&r.Tev1, &r.Tev1Threshold, &r.EndedReason); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(rj), &r.Reasons)
	_ = json.Unmarshal([]byte(sj), &r.Scripts)
	if t, err := time.Parse(time.RFC3339Nano, created); err == nil {
		r.CreatedAt = t
	}
	r.ExpiresAt, r.DeletedAt, r.LastHitAt = parseTime(expires), parseTime(deleted), parseTime(lastHit)
	return &r, nil
}

// ListRules returns rules, newest first. Deleted rules are included only when asked.
func ListRules(db Execer, includeDeleted bool) ([]*Rule, error) {
	q := `SELECT ` + ruleCols + ` FROM security_gate_rules`
	if !includeDeleted {
		q += ` WHERE deleted_at IS NULL`
	}
	rows, err := db.Query(q + ` ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Rule{}
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRule fetches one rule (nil when absent).
func GetRule(db Execer, id int64) (*Rule, error) {
	r, err := scanRule(db.QueryRow(`SELECT `+ruleCols+` FROM security_gate_rules WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// DeleteRule soft-deletes a rule; its hit history stays for the audit trail.
func DeleteRule(db Execer, id int64, now time.Time) error {
	res, err := db.Exec(`UPDATE security_gate_rules SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
		now.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// RecordHit counts an auto-approval by rule id.
func RecordHit(db Execer, id int64, now time.Time) error {
	_, err := db.Exec(`UPDATE security_gate_rules SET hit_count = hit_count + 1, last_hit_at = ? WHERE id = ?`,
		now.UTC().Format(time.RFC3339Nano), id)
	return err
}

// Matches reports whether rule r approves request in at time now. scripts
// are the request's scripts as read now.
func (r *Rule) Matches(in security.GateRequestInput, now time.Time) bool {
	if r.DeletedAt != nil || (r.ExpiresAt != nil && !now.Before(*r.ExpiresAt)) {
		return false
	}
	if SpecialRunIDs[in.RunID] {
		return false
	}
	switch r.Scope {
	case ScopeTask:
		if in.TaskID == "" || in.TaskID != r.ScopeValue {
			return false
		}
	case ScopeRepo:
		if in.Repo == "" || in.Repo != r.ScopeValue {
			return false
		}
	case ScopeOrg:
		if in.Org == "" || in.Org != r.ScopeValue {
			return false
		}
	default:
		return false
	}
	norm := NormalizeCommand(in.Cmdline)
	switch r.MatchKind {
	case MatchExact:
		if norm != r.Pattern {
			return false
		}
	case MatchPrefix:
		if !strings.HasPrefix(norm, r.Pattern) || !prefixTailSafe(norm[len(r.Pattern):]) {
			return false
		}
	default:
		return false
	}
	// Every reason the request is held for must have been approved.
	allowed := map[string]bool{}
	for _, a := range r.Reasons {
		allowed[a] = true
	}
	for _, a := range in.Reasons {
		if !allowed[a] {
			return false
		}
	}
	// Every script it runs must be pinned, trusted, and unchanged.
	pinned := map[string]string{}
	for _, s := range r.Scripts {
		pinned[s.Path] = s.SHA256
	}
	for _, s := range in.Scripts {
		want, ok := pinned[s.Path]
		if !ok || !s.Trusted || s.SHA256 == "" || s.SHA256 != want {
			return false
		}
	}
	return len(in.Scripts) == len(r.Scripts)
}

// MatchRule returns the oldest active rule approving in, or nil.
func MatchRule(db Execer, in security.GateRequestInput, now time.Time) (*Rule, error) {
	if SpecialRunIDs[in.RunID] {
		return nil, nil
	}
	rules, err := ListRules(db, false)
	if err != nil {
		return nil, err
	}
	for i := len(rules) - 1; i >= 0; i-- {
		if rules[i].Matches(in, now) {
			return rules[i], nil
		}
	}
	return nil, nil
}
