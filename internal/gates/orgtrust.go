package gates

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/security"
)

// "Trust this organization until…" (task-33692ffb). One Touch ID creates an
// org-scoped allow rule with match kind "any" and a required expiry of at
// most 16h. While it is active, Red requests of every running task in the
// organization are approved as rule:<id> exactly as a task trust approves
// them: the same exclusions wait for the Board (protected merges/pushes,
// deferred deletes outside the worktree, Board policy requests), the same
// tev1 mode applies, and on top of them the Board's unattended-run rules
// (security.AnalyzeBoardRulesForTaskIn: prod writes, external API writes, data
// deletes, sending PII) always wait.
//
// A task's own trust, when it has one, wins over its organization's.

// MaxOrgTrustMinutes caps an org trust window (16h).
const MaxOrgTrustMinutes = 16 * 60

// OrgTrustSpec is what the Board picks for an org trust. Like TrustSpec it
// has no expiry time and no exclusions: the server derives both.
type OrgTrustSpec struct {
	Org     string `json:"org"`
	Preset  string `json:"preset"`  // 1h | 4h | overnight | custom
	Minutes int    `json:"minutes"` // custom only
	Tev1    bool   `json:"tev1"`
	Tev1Ack bool   `json:"tev1_ack"`
	Note    string `json:"note"`
}

// NormalizeOrg keys an org trust: SQLite's lower(trim()), matching the SQL
// below. Unlike an org hold it does not fold lookalikes (names.Normalize):
// a trust approves, and wider folding would widen what it approves.
func NormalizeOrg(org string) string { return governance.NormalizeOrg(org) }

// NewOrgTrust builds the trust rule for spec.Org.
func NewOrgTrust(spec OrgTrustSpec, threshold float64, now time.Time) (Rule, error) {
	org := NormalizeOrg(spec.Org)
	if org == "" {
		return Rule{}, errors.New("organization required")
	}
	ts := TrustSpec{Preset: spec.Preset, Minutes: spec.Minutes, Tev1: spec.Tev1, Tev1Ack: spec.Tev1Ack, Note: spec.Note}
	mins, err := ts.TrustMinutes()
	if err != nil {
		return Rule{}, err
	}
	if mins > MaxOrgTrustMinutes {
		return Rule{}, fmt.Errorf("an organization trust is at most %d minutes", MaxOrgTrustMinutes)
	}
	r, err := NewTrust("org:"+org, ts, threshold, now)
	if err != nil {
		return Rule{}, err
	}
	r.Scope, r.ScopeValue = ScopeOrg, org
	return r, nil
}

// IsOrgTrust reports whether r is an organization trust rule.
func (r *Rule) IsOrgTrust() bool { return r.IsTrust() && r.Scope == ScopeOrg }

// OrgExists reports whether any task belongs to org (normalised).
func OrgExists(db Execer, org string) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE lower(trim(COALESCE(organization,''))) = ?`, NormalizeOrg(org)).Scan(&n)
	return n > 0, err
}

// InsertOrgTrust ends the organization's current trust (one per org) and
// stores r.
func InsertOrgTrust(db Execer, r Rule, now time.Time) (*Rule, error) {
	if !r.IsOrgTrust() || r.ExpiresAt == nil || r.ScopeValue != NormalizeOrg(r.ScopeValue) || r.ScopeValue == "" {
		return nil, errors.New("not an organization trust rule")
	}
	if _, err := db.Exec(`UPDATE security_gate_rules SET deleted_at = ?, ended_reason = 'replaced'
		WHERE match_kind = 'any' AND scope = 'org' AND scope_value = ? AND deleted_at IS NULL`,
		now.UTC().Format(time.RFC3339Nano), r.ScopeValue); err != nil {
		return nil, err
	}
	return InsertRule(db, r)
}

// OrgTrustsFor returns org's trust rules, newest first.
func OrgTrustsFor(db Execer, org string) ([]*Rule, error) {
	rows, err := db.Query(`SELECT `+ruleCols+` FROM security_gate_rules
		WHERE match_kind = 'any' AND scope = 'org' AND scope_value = ? ORDER BY id DESC`, NormalizeOrg(org))
	if err != nil {
		return nil, err
	}
	return collectRules(rows)
}

// ActiveOrgTrusts returns every org trust active at now, newest first.
func ActiveOrgTrusts(db Execer, now time.Time) ([]*Rule, error) {
	rows, err := db.Query(`SELECT ` + ruleCols + ` FROM security_gate_rules
		WHERE match_kind = 'any' AND scope = 'org' AND deleted_at IS NULL ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	all, err := collectRules(rows)
	if err != nil {
		return nil, err
	}
	out := []*Rule{}
	for _, r := range all {
		if r.Active(now) {
			out = append(out, r)
		}
	}
	return out, nil
}

// ActiveOrgTrust returns the org trust approving taskID's requests now, or
// nil. The organization is read from the task row, never from the request.
// Like a task trust it approves only while the task is running; unlike one it
// is not ended when a task stops, since it covers the whole organization.
// Any lookup error is returned, and callers treat it as no trust.
func ActiveOrgTrust(db Execer, taskID string, now time.Time) (*Rule, error) {
	if taskID == "" {
		return nil, nil
	}
	var org, stage, status string
	err := db.QueryRow(`SELECT COALESCE(organization,''), COALESCE(execution_stage,''), COALESCE(status,'')
		FROM tasks WHERE id = ?`, taskID).Scan(&org, &stage, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	org = NormalizeOrg(org)
	if org == "" || status != "active" || !trustRunningStages[stage] {
		return nil, nil
	}
	trusts, err := OrgTrustsFor(db, org)
	if err != nil {
		return nil, err
	}
	for _, r := range trusts {
		if r.Active(now) {
			return r, nil
		}
	}
	return nil, nil
}

// OrgHeldRequests returns the org's requests still waiting for the Board,
// newest first.
func OrgHeldRequests(db *sql.DB, org string) ([]*security.GateRequest, error) {
	ids, err := idsWhere(db, `SELECT r.id FROM security_gate_requests r JOIN tasks t ON t.id = r.task_id
		WHERE lower(trim(COALESCE(t.organization,''))) = ? AND r.status = 'pending'
		ORDER BY r.created_at DESC LIMIT 200`, NormalizeOrg(org))
	if err != nil {
		return nil, err
	}
	return getRequests(db, ids)
}

// TrustLabel names a trust's scope for audit messages ("task T1", "org acme").
func TrustLabel(r *Rule) string {
	if r.IsOrgTrust() {
		return "org " + r.ScopeValue
	}
	return "task " + r.ScopeValue
}
