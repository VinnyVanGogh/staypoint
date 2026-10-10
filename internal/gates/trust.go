package gates

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
	"github.com/VinnyVanGogh/staypoint/internal/security"
	"github.com/VinnyVanGogh/staypoint/internal/workspace"
)

// "Trust this task until…" (task-6c1ed91f). One Touch ID creates a
// task-scoped allow rule with match kind "any" and a required expiry. While it
// is active, the task's Red requests are approved as rule:<id>, except:
//   - merges and pushes to protected branches, which wait for the Board;
//   - deletes outside the task's worktree, which wait up to a timeout and are
//     then skipped (the Deferred queue);
//   - Board policy requests (SpecialRunIDs), which rules never approve.
// In tev1 mode the local tev1 model decides instead of a blanket approval;
// a deny, low confidence or error parks the whole task.
//
// Everything that limits a trust (expiry, exclusions, threshold) is decided
// here on the daemon, never taken from the client.

// MatchAny is the trust match kind: any command, task scope only.
const MatchAny = "any"

// Trust presets in minutes.
var TrustPresets = map[string]int{"1h": 60, "4h": 240, "overnight": 720}

// Bounds for a custom trust window.
const (
	MinTrustMinutes = 15
	MaxTrustMinutes = 24 * 60
)

// Settings for trust (settings_kv).
const (
	// SettingTrustDeferMinutes is how long an under-trust delete outside the
	// worktree waits for the Board before it is skipped. Default 10.
	SettingTrustDeferMinutes = "gates.trust_defer_minutes"
	// SettingTev1Threshold is the approve probability tev1 must reach.
	SettingTev1Threshold = "gates.tev1_threshold"
)

// Defaults and bounds for the trust settings.
const (
	// 2 (was 10): under overnight trust a held request stalls its agent for
	// this long before it is skipped to the Board's list (2026-10-07).
	DefaultTrustDeferMinutes = 2
	MaxTrustDeferMinutes     = 120
	DefaultTev1Threshold     = 0.7
	MinTev1Threshold         = 0.5
	MaxTev1Threshold         = 0.99
)

// Tev1Warning is shown in the confirm dialog that enables tev1 mode.
const Tev1Warning = "tev1 is an unproven 4B model; on 2026-10-07 it denied a harmless go test and approved a sqlite3 read at 50%. Expect false denials that park tasks, and possible false approvals."

// TrustDeferMinutes returns the deferral timeout, clamped to 1..120.
func TrustDeferMinutes(db Execer) int {
	v, ok := getSetting(db, SettingTrustDeferMinutes)
	if !ok {
		return DefaultTrustDeferMinutes
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return DefaultTrustDeferMinutes
	}
	if n > MaxTrustDeferMinutes {
		return MaxTrustDeferMinutes
	}
	return n
}

// Tev1Threshold returns the approve threshold, clamped to 0.5..0.99.
func Tev1Threshold(db Execer) float64 {
	v, ok := getSetting(db, SettingTev1Threshold)
	if !ok {
		return DefaultTev1Threshold
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return DefaultTev1Threshold
	}
	return clampThreshold(f)
}

func clampThreshold(f float64) float64 {
	if f < MinTev1Threshold {
		return MinTev1Threshold
	}
	if f > MaxTev1Threshold {
		return MaxTev1Threshold
	}
	return f
}

// TrustSpec is what the Board picks. It has no expiry time and no exclusions
// on purpose: the server derives both.
type TrustSpec struct {
	Preset  string `json:"preset"`  // 1h | 4h | overnight | custom
	Minutes int    `json:"minutes"` // custom only
	Tev1    bool   `json:"tev1"`
	// Tev1Ack confirms the Board saw Tev1Warning; tev1 mode requires it.
	Tev1Ack bool   `json:"tev1_ack"`
	Note    string `json:"note"`
	// MoveToTodo moves a backlog task to todo first, under the same Touch
	// ID (task-40f0a2f0). It never moves a task from any other stage.
	MoveToTodo bool `json:"move_to_todo"`
}

// TrustMinutes validates the spec's window.
func (s TrustSpec) TrustMinutes() (int, error) {
	if s.Preset == "custom" {
		if s.Minutes < MinTrustMinutes || s.Minutes > MaxTrustMinutes {
			return 0, fmt.Errorf("custom trust must be %d to %d minutes", MinTrustMinutes, MaxTrustMinutes)
		}
		return s.Minutes, nil
	}
	m, ok := TrustPresets[s.Preset]
	if !ok {
		return 0, fmt.Errorf("unknown trust preset %q (1h, 4h, overnight or custom)", s.Preset)
	}
	return m, nil
}

// NewTrust builds the trust rule for taskID.
func NewTrust(taskID string, spec TrustSpec, threshold float64, now time.Time) (Rule, error) {
	if taskID == "" {
		return Rule{}, ErrScopeUnavailable
	}
	mins, err := spec.TrustMinutes()
	if err != nil {
		return Rule{}, err
	}
	if spec.Tev1 && !spec.Tev1Ack {
		return Rule{}, errors.New("tev1 mode needs the warning acknowledged (tev1_ack)")
	}
	exp := now.UTC().Add(time.Duration(mins) * time.Minute)
	r := Rule{
		Pattern: "*", MatchKind: MatchAny, Reasons: []string{}, Scripts: []security.ScriptHash{},
		Scope: ScopeTask, ScopeValue: taskID, Note: spec.Note, CreatedBy: "board",
		CreatedAt: now.UTC(), ExpiresAt: &exp, Tev1: spec.Tev1,
	}
	if spec.Tev1 {
		r.Tev1Threshold = clampThreshold(threshold)
	}
	return r, nil
}

// IsTrust reports whether r is a task trust rule.
func (r *Rule) IsTrust() bool { return r != nil && r.MatchKind == MatchAny }

// Active reports whether r is neither ended nor expired at now.
func (r *Rule) Active(now time.Time) bool {
	return r.DeletedAt == nil && r.ExpiresAt != nil && now.Before(*r.ExpiresAt)
}

// Task execution stages in which a trust may exist, and in which it approves.
var (
	trustLiveStages    = map[string]bool{"todo": true, "in_progress": true, "paused": true}
	trustRunningStages = map[string]bool{"in_progress": true, "paused": true}
)

// TaskStage returns a task's execution stage and status ("" when missing).
func TaskStage(db Execer, taskID string) (stage, status string, err error) {
	err = db.QueryRow(`SELECT COALESCE(execution_stage,''), COALESCE(status,'') FROM tasks WHERE id = ?`, taskID).Scan(&stage, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return stage, status, err
}

// CanTrust reports whether a task may be trusted now.
func CanTrust(db Execer, taskID string) error {
	stage, status, err := TaskStage(db, taskID)
	if err != nil {
		return err
	}
	if stage == "" && status == "" {
		return errors.New("task not found")
	}
	if status != "active" || !trustLiveStages[stage] {
		return fmt.Errorf("task is %s; only a todo or in-progress task can be trusted", stage)
	}
	return nil
}

// InsertTrust ends the task's current trust (one per task) and stores r.
func InsertTrust(db Execer, r Rule, now time.Time) (*Rule, error) {
	if !r.IsTrust() || r.Scope != ScopeTask || r.ExpiresAt == nil {
		return nil, errors.New("not a task trust rule")
	}
	if _, err := db.Exec(`UPDATE security_gate_rules SET deleted_at = ?, ended_reason = 'replaced'
		WHERE match_kind = 'any' AND scope = 'task' AND scope_value = ? AND deleted_at IS NULL`,
		now.UTC().Format(time.RFC3339Nano), r.ScopeValue); err != nil {
		return nil, err
	}
	return InsertRule(db, r)
}

// EndTrust ends a trust now (revoked, task left in_progress). It reports
// false when the trust was already ended.
func EndTrust(db Execer, id int64, reason string, now time.Time) (bool, error) {
	res, err := db.Exec(`UPDATE security_gate_rules SET deleted_at = ?, ended_reason = ?
		WHERE id = ? AND match_kind = 'any' AND deleted_at IS NULL`, now.UTC().Format(time.RFC3339Nano), reason, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// TrustsForTask returns the task's trust rules, newest first.
func TrustsForTask(db Execer, taskID string) ([]*Rule, error) {
	rows, err := db.Query(`SELECT `+ruleCols+` FROM security_gate_rules
		WHERE match_kind = 'any' AND scope = 'task' AND scope_value = ? ORDER BY id DESC`, taskID)
	if err != nil {
		return nil, err
	}
	return collectRules(rows)
}

// RecentTrusts returns task trusts that are live or ended after since, newest first.
func RecentTrusts(db Execer, since time.Time) ([]*Rule, error) {
	rows, err := db.Query(`SELECT ` + ruleCols + ` FROM security_gate_rules WHERE match_kind = 'any' AND scope = 'task' ORDER BY id DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	all, err := collectRules(rows)
	if err != nil {
		return nil, err
	}
	out := []*Rule{}
	for _, r := range all {
		end := r.ExpiresAt
		if r.DeletedAt != nil && (end == nil || r.DeletedAt.Before(*end)) {
			end = r.DeletedAt
		}
		if end == nil || end.After(since) {
			out = append(out, r)
		}
	}
	return out, nil
}

// ActiveTrust returns the trust approving taskID's requests now, or nil. A
// trust whose task has left in_progress is ended on the spot, so it cannot
// come back if the task is resumed later.
func ActiveTrust(db Execer, taskID string, now time.Time) (*Rule, error) {
	if taskID == "" {
		return nil, nil
	}
	trusts, err := TrustsForTask(db, taskID)
	if err != nil {
		return nil, err
	}
	var live *Rule
	for _, r := range trusts {
		if r.Active(now) {
			live = r
			break
		}
	}
	if live == nil {
		return nil, nil
	}
	stage, status, err := TaskStage(db, taskID)
	if err != nil {
		return nil, err
	}
	if status != "active" || !trustLiveStages[stage] {
		_, err := EndTrust(db, live.ID, "task left in_progress ("+stage+")", now)
		return nil, err
	}
	if !trustRunningStages[stage] {
		return nil, nil
	}
	return live, nil
}

// SweepTrusts ends every live trust whose task has left in_progress.
func SweepTrusts(db Execer, now time.Time) error {
	rows, err := db.Query(`SELECT DISTINCT scope_value FROM security_gate_rules
		WHERE match_kind = 'any' AND scope = 'task' AND deleted_at IS NULL AND expires_at IS NOT NULL`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := ActiveTrust(db, id, now); err != nil {
			return err
		}
	}
	return nil
}

func collectRules(rows *sql.Rows) ([]*Rule, error) {
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

// DecidedByTrust is the decided_by of a request a trust approved.
func DecidedByTrust(id int64) string { return fmt.Sprintf("rule:%d", id) }

// DecidedByTev1 is the decided_by of a request tev1 decided under a trust.
func DecidedByTev1(id int64) string { return fmt.Sprintf("rule:%d:tev1", id) }

// TrustRequests returns the requests a trust decided (auto-approved, tev1
// approved or denied), newest first.
func TrustRequests(db *sql.DB, ruleIDs []int64) ([]*security.GateRequest, error) {
	if len(ruleIDs) == 0 {
		return []*security.GateRequest{}, nil
	}
	var conds []string
	var args []any
	for _, id := range ruleIDs {
		conds = append(conds, "decided_by = ? OR decided_by = ?")
		args = append(args, DecidedByTrust(id), DecidedByTev1(id))
	}
	ids, err := idsWhere(db, `SELECT id FROM security_gate_requests WHERE `+strings.Join(conds, " OR ")+
		` ORDER BY decided_at DESC, created_at DESC LIMIT 500`, args...)
	if err != nil {
		return nil, err
	}
	return getRequests(db, ids)
}

// TaskHeldRequests returns the task's requests still waiting for the Board
// (pending or deferred), newest first.
func TaskHeldRequests(db *sql.DB, taskID string) ([]*security.GateRequest, error) {
	ids, err := idsWhere(db, `SELECT id FROM security_gate_requests WHERE task_id = ? AND status = 'pending'
		ORDER BY created_at DESC LIMIT 200`, taskID)
	if err != nil {
		return nil, err
	}
	return getRequests(db, ids)
}

func idsWhere(db *sql.DB, q string, args ...any) ([]string, error) {
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func getRequests(db *sql.DB, ids []string) ([]*security.GateRequest, error) {
	out := make([]*security.GateRequest, 0, len(ids))
	for _, id := range ids {
		gr, err := security.GetGateRequest(db, id)
		if err != nil {
			return nil, err
		}
		if gr != nil {
			out = append(out, gr)
		}
	}
	return out, nil
}

// ── Request context for the exclusions ─────────────────────────────────────

// TrustContextFor builds what AnalyzeForTrust needs for a request of taskID:
// the dirs deletes may touch (the task's worktree and scratch dirs) and the
// protected branches (main, master, dev-server and the repo's default).
// db must not be inside a transaction on a single-connection pool.
func TrustContextFor(db Execer, taskID, cwd string, scripts []security.ScriptHash) security.TrustContext {
	tc := security.TrustContext{
		CWD:         cwd,
		Protected:   map[string]bool{"main": true, "master": true, "dev-server": true},
		Scripts:     scripts,
		PushTargets: PushTargets,
	}
	if h, err := os.UserHomeDir(); err == nil {
		tc.Home = h
	}
	tc.Allowed = append(tc.Allowed, security.DefaultScratchDirs()...)
	if d, err := workspace.ScratchDir(taskID); err == nil {
		tc.Allowed = append(tc.Allowed, d)
	}
	var repo string
	_ = db.QueryRow(`SELECT COALESCE(repo_path,'') FROM tasks WHERE id = ?`, taskID).Scan(&repo)
	if td, err := workspace.DescribeTaskDir(repo, taskID); err == nil {
		switch {
		case td.Scratch:
			// already allowed
		case td.Git:
			tc.Allowed = append(tc.Allowed, filepath.Join(td.Dir, ".worktrees", taskID))
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if ref, err := workspace.DefaultBranchRef(ctx, td.Dir); err == nil {
				name := strings.TrimPrefix(strings.TrimPrefix(ref, "refs/remotes/origin/"), "refs/heads/")
				if name != "" && name != "HEAD" {
					tc.Protected[name] = true
				}
			}
			cancel()
		default:
			// A non-git task runs directly in its dir.
			tc.Allowed = append(tc.Allowed, td.Dir)
		}
	}
	return tc
}

// PushTargets returns the branches a bare `git push` in dir would update:
// the current branch and, when set, its push destination.
func PushTargets(dir string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := gitexec.Command(ctx, "-C", dir, "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		return nil, fmt.Errorf("current branch: %w", err)
	}
	targets := []string{strings.TrimSpace(string(out))}
	if p, err := gitexec.Command(ctx, "-C", dir, "rev-parse", "--abbrev-ref", "@{push}").Output(); err == nil {
		s := strings.TrimSpace(string(p))
		if i := strings.Index(s, "/"); i >= 0 {
			s = s[i+1:]
		}
		if s != "" {
			targets = append(targets, s)
		}
	}
	return targets, nil
}
