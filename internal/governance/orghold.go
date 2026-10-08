package governance

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"

	"modernc.org/sqlite"

	"github.com/VinnyVanGogh/staypoint/internal/names"
)

// Org hold (2026-10-07): a Board-only switch per organization. While it is
// on, no task in that organization is claimed by a run, whatever woke it
// (assignment, comment, Run Now, blocker cleared, a queued slot). The Claim
// SQL enforces it (OrgNotHeldSQL), so every run path is covered; the wake
// paths check it first only to log the refusal as "held".
//
// It lives in settings_kv under "org_hold.<names.Normalize(org)>" = "1".
// Writes go through the Board-gated API (passkey) or the Board-only CLI;
// agents have no MCP tool and the agent token is refused by the gate.
//
// A hold matches every organization name that names.Normalize folds to the
// held one (case, whitespace, invisible characters, accents, lookalike
// letters), so a task cannot slip past a hold on "Managed Solution" by
// spelling it with a no-break space or a Cyrillic "а". Both sides of the
// comparison are normalised, in Go and in SQL (NormNameSQLFunc), so keys
// written by older builds (SQLite lower(trim())) still match.

// OrgHoldPrefix prefixes the settings_kv key of an organization's hold.
const OrgHoldPrefix = "org_hold."

// NormNameSQLFunc is the SQLite function names.Normalize is registered as,
// for every connection the "sqlite" driver opens in a binary that links this
// package (which every binary building OrgNotHeldSQL does).
const NormNameSQLFunc = "staypoint_norm_name"

func init() {
	if err := sqlite.RegisterDeterministicScalarFunction(NormNameSQLFunc, 1,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			switch v := args[0].(type) {
			case nil:
				return "", nil
			case string:
				return names.Normalize(v), nil
			case []byte:
				return names.Normalize(string(v)), nil
			default:
				return names.Normalize(fmt.Sprint(v)), nil
			}
		}); err != nil {
		panic(fmt.Sprintf("register %s: %v", NormNameSQLFunc, err))
	}
}

// ErrOrgHeld is returned when a task's organization is on hold.
var ErrOrgHeld = errors.New("organization is on hold")

// OrgHoldKey is the settings_kv key for org's hold.
func OrgHoldKey(org string) string {
	return OrgHoldPrefix + names.Normalize(org)
}

// NormalizeOrg is an organization name as org trusts key it: SQLite's
// lower(trim(org)) (spaces only, ASCII only). It is deliberately narrower
// than names.Normalize, which holds use: a trust is an allow list, and
// folding lookalikes there would widen what it approves.
func NormalizeOrg(org string) string {
	return sqliteLower(strings.Trim(org, " "))
}

// sqliteLower folds ASCII A-Z only, like SQLite's built-in lower().
func sqliteLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// OrgHeld reports whether org is on hold. An empty org is never held. A read
// error reports held (fail closed): a broken settings table must not let
// runs through a hold.
func OrgHeld(db *sql.DB, org string) bool {
	n := names.Normalize(org)
	if n == "" {
		return false
	}
	keys, err := orgHoldKeys(db, n)
	if err != nil {
		return true
	}
	for _, v := range keys {
		if strings.TrimSpace(v) == "1" {
			return true
		}
	}
	return false
}

// orgHoldKeys returns the value of every hold key whose organization
// normalises to n, keyed by the settings_kv key. More than one key can match
// when an older build wrote the hold under a differently folded name.
func orgHoldKeys(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, n string) (map[string]string, error) {
	rows, err := q.Query(`SELECT key, value FROM settings_kv WHERE substr(key, 1, ?) = ?`, len(OrgHoldPrefix), OrgHoldPrefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		if names.Normalize(strings.TrimPrefix(k, OrgHoldPrefix)) == n {
			out[k] = v
		}
	}
	return out, rows.Err()
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
// the Board gate. Lifting clears every key that matches org (see
// orgHoldKeys), so a hold written under an older key cannot outlive it.
func SetOrgHold(db *sql.DB, org string, held bool) error {
	n := names.Normalize(org)
	if n == "" {
		return fmt.Errorf("organization is required")
	}
	v := "0"
	if held {
		v = "1"
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("set org hold: %w", err)
	}
	defer tx.Rollback()
	keys, err := orgHoldKeys(tx, n)
	if err != nil {
		return fmt.Errorf("set org hold: %w", err)
	}
	keys[OrgHoldPrefix+n] = v
	for k := range keys {
		if _, err := tx.Exec(
			`INSERT INTO settings_kv (key, value, updated_at) VALUES (?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			k, v); err != nil {
			return fmt.Errorf("set org hold: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
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
	norm := NormNameSQLFunc + `(COALESCE(` + col + `, ''))`
	return fmt.Sprintf(`NOT EXISTS (SELECT 1 FROM settings_kv hk WHERE substr(hk.key, 1, %d) = '%s' AND hk.value = '1'`+
		` AND %s(substr(hk.key, %d)) = %s AND %s != '')`,
		len(OrgHoldPrefix), OrgHoldPrefix, NormNameSQLFunc, len(OrgHoldPrefix)+1, norm, norm)
}
