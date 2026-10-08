package db

import (
	"path/filepath"
	"testing"
)

// gateRulesV41 is security_gate_rules as migration 41 left it.
const gateRulesV41 = `CREATE TABLE security_gate_rules (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	pattern        TEXT NOT NULL,
	match_kind     TEXT NOT NULL DEFAULT 'exact' CHECK (match_kind IN ('exact','prefix','any')),
	reasons_json   TEXT NOT NULL DEFAULT '[]',
	scripts_json   TEXT NOT NULL DEFAULT '[]',
	scope          TEXT NOT NULL CHECK (scope IN ('task','repo','org')),
	scope_value    TEXT NOT NULL,
	source_gate_id TEXT NOT NULL DEFAULT '',
	note           TEXT NOT NULL DEFAULT '',
	created_by     TEXT NOT NULL DEFAULT 'board',
	created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
	expires_at     TEXT,
	deleted_at     TEXT,
	hit_count      INTEGER NOT NULL DEFAULT 0,
	last_hit_at    TEXT,
	tev1           INTEGER NOT NULL DEFAULT 0,
	tev1_threshold REAL NOT NULL DEFAULT 0,
	ended_reason   TEXT NOT NULL DEFAULT '',
	CHECK (match_kind <> 'any' OR (scope = 'task' AND expires_at IS NOT NULL))
)`

// task-33692ffb migration 44: the rules table is rebuilt so an 'any' rule may
// be org-scoped (still with a required expiry); every existing row and column
// survives.
func TestMigration44_GateOrgTrust(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "rules.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	conn := store.DB()
	for _, stmt := range []string{
		`DROP TABLE security_gate_rules`,
		gateRulesV41,
		`INSERT INTO security_gate_rules (id, pattern, match_kind, scope, scope_value, note, expires_at, deleted_at,
			hit_count, last_hit_at, tev1, tev1_threshold, ended_reason)
			VALUES (7, '*', 'any', 'task', 'T1', 'overnight', '2099-01-01T00:00:00Z', '2026-10-07T01:00:00Z',
			3, '2026-10-07T00:30:00Z', 1, 0.8, 'revoked by the Board')`,
		`INSERT INTO security_gate_rules (id, pattern, match_kind, scope, scope_value) VALUES (9, 'go test ./...', 'exact', 'repo', '/r')`,
		`DELETE FROM schema_versions WHERE version = 44`,
		`DELETE FROM schema_migrations WHERE version = 44`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// Before 44 an org 'any' rule is refused.
	if _, err := conn.Exec(`INSERT INTO security_gate_rules (pattern, match_kind, scope, scope_value, expires_at)
		VALUES ('*', 'any', 'org', 'acme', '2099-01-01T00:00:00Z')`); err == nil {
		t.Fatal("v41 table accepted an org trust")
	}
	store.Close()

	store, err = Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store.Close()
	conn = store.DB()

	var (
		note, deleted, lastHit, ended string
		hits, tev1                    int
		thr                           float64
	)
	if err := conn.QueryRow(`SELECT note, deleted_at, hit_count, last_hit_at, tev1, tev1_threshold, ended_reason
		FROM security_gate_rules WHERE id = 7`).Scan(&note, &deleted, &hits, &lastHit, &tev1, &thr, &ended); err != nil {
		t.Fatal(err)
	}
	if note != "overnight" || deleted == "" || hits != 3 || lastHit == "" || tev1 != 1 || thr != 0.8 || ended != "revoked by the Board" {
		t.Fatalf("task trust row not kept: %q %q %d %q %d %v %q", note, deleted, hits, lastHit, tev1, thr, ended)
	}
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM security_gate_rules WHERE id = 9 AND match_kind = 'exact' AND scope = 'repo'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("exact rule not kept: %d %v", n, err)
	}

	if _, err := conn.Exec(`INSERT INTO security_gate_rules (pattern, match_kind, scope, scope_value, expires_at)
		VALUES ('*', 'any', 'org', 'acme', '2099-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("org trust with expiry refused: %v", err)
	}
	for _, bad := range []string{
		`INSERT INTO security_gate_rules (pattern, match_kind, scope, scope_value) VALUES ('*', 'any', 'org', 'acme')`,
		`INSERT INTO security_gate_rules (pattern, match_kind, scope, scope_value) VALUES ('*', 'any', 'task', 'T1')`,
		`INSERT INTO security_gate_rules (pattern, match_kind, scope, scope_value, expires_at)
			VALUES ('*', 'any', 'repo', '/r', '2099-01-01T00:00:00Z')`,
	} {
		if _, err := conn.Exec(bad); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
	var idx int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_sgr_scope'`).Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("scope index missing: %d %v", idx, err)
	}
}
