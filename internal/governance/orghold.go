package governance

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Org hold (2026-10-07): a Board-only switch per organization. While it is
// on, no task in that organization is claimed by a run, whatever woke it
// (assignment, comment, Run Now, blocker cleared, a queued slot). The Claim
// SQL enforces it (OrgNotHeldSQL), so every run path is covered; the wake
// paths check it first only to log the refusal as "held".
//
// It lives in settings_kv under "org_hold.<lower(org)>" = "1". Writes go
// through the Board-gated API (passkey) or the Board-only CLI; agents have no
// MCP tool and the agent token is refused by the gate.

// OrgHoldPrefix prefixes the settings_kv key of an organization's hold.
const OrgHoldPrefix = "org_hold."

// ErrOrgHeld is returned when a task's organization is on hold.
var ErrOrgHeld = errors.New("organization is on hold")

// OrgHoldKey is the settings_kv key for org's hold.
func OrgHoldKey(org string) string {
	return OrgHoldPrefix + strings.ToLower(strings.TrimSpace(org))
}

// OrgHeld reports whether org is on hold. An empty org is never held. A read
// error reports held (fail closed): a broken settings table must not let
// runs through a hold.
func OrgHeld(db *sql.DB, org string) bool {
	if strings.TrimSpace(org) == "" {
		return false
	}
	var v string
	err := db.QueryRow(`SELECT value FROM settings_kv WHERE key = ?`, OrgHoldKey(org)).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		return true
	}
	return strings.TrimSpace(v) == "1"
}

// TaskOrgHeld reports whether taskID's organization is on hold, and the
// organization. A missing task is not held.
func TaskOrgHeld(db *sql.DB, taskID string) (bool, string) {
	var org string
	if err := db.QueryRow(`SELECT COALESCE(organization, '') FROM tasks WHERE id = ?`, taskID).Scan(&org); err != nil {
		return false, ""
	}
	return OrgHeld(db, org), org
}

// SetOrgHold places (held) or lifts the hold on org. Callers must have passed
// the Board gate.
func SetOrgHold(db *sql.DB, org string, held bool) error {
	org = strings.TrimSpace(org)
	if org == "" {
		return fmt.Errorf("organization is required")
	}
	v := "0"
	if held {
		v = "1"
	}
	if _, err := db.Exec(
		`INSERT INTO settings_kv (key, value, updated_at) VALUES (?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		OrgHoldKey(org), v); err != nil {
		return fmt.Errorf("set org hold: %w", err)
	}
	return nil
}

// HeldOrgs returns the lower-cased names of the organizations on hold.
func HeldOrgs(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT key FROM settings_kv WHERE key LIKE ? AND value = '1' ORDER BY key`, OrgHoldPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, strings.TrimPrefix(k, OrgHoldPrefix))
	}
	return out, rows.Err()
}

// OrgNotHeldSQL is a WHERE fragment true when the tasks row aliased as alias
// ("" for an unaliased tasks table) is not in a held organization.
func OrgNotHeldSQL(alias string) string {
	col := "organization"
	if alias != "" {
		col = alias + ".organization"
	}
	return `NOT EXISTS (SELECT 1 FROM settings_kv hk WHERE hk.key = '` + OrgHoldPrefix + `' || lower(trim(COALESCE(` + col + `, ''))) AND hk.value = '1' AND trim(COALESCE(` + col + `, '')) != '')`
}
