package db

import (
	"path/filepath"
	"strings"
	"testing"
)

func tableExists(t *testing.T, s *Store, name string) bool {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// The live DB recorded migration 20 (from a branch build with different DDL)
// but had no security_gate_audit_log, so gate decisions failed their audit
// write (STA-868). Reopening must recreate the table and keep all data.
func TestRepairSchema_RecreatesTableRecordedButMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "staypoint.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO settings_kv (key, value) VALUES ('keep', 'me')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`DROP TABLE security_gate_audit_log`); err != nil {
		t.Fatal(err)
	}
	var v int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM schema_versions WHERE version = 20`).Scan(&v); err != nil || v != 1 {
		t.Fatalf("migration 20 should stay recorded: %d %v", v, err)
	}
	if tableExists(t, s, "security_gate_audit_log") {
		t.Fatal("setup: table should be gone")
	}

	var logs []string
	actions, err := RepairSchema(s.DB(), func(l string) { logs = append(logs, l) })
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Table != "security_gate_audit_log" || actions[0].Migration != 20 {
		t.Fatalf("actions: %+v", actions)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "schema repair: created security_gate_audit_log (migration 20 recorded but table missing)") {
		t.Fatalf("log: %v", logs)
	}
	if !tableExists(t, s, "security_gate_audit_log") {
		t.Fatal("table not recreated")
	}
	var idx int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_sga_gate'`).Scan(&idx)
	if idx != 1 {
		t.Fatal("index not recreated")
	}
	if _, err := s.DB().Exec(`INSERT INTO security_gate_audit_log (gate_id, actor_id, event_type) VALUES ('g','board','x')`); err != nil {
		t.Fatalf("recreated table unusable: %v", err)
	}
	var val string
	if err := s.DB().QueryRow(`SELECT value FROM settings_kv WHERE key='keep'`).Scan(&val); err != nil || val != "me" {
		t.Fatalf("data touched: %q %v", val, err)
	}
	// Idempotent.
	if again, err := RepairSchema(s.DB(), func(string) {}); err != nil || len(again) != 0 {
		t.Fatalf("second repair: %+v %v", again, err)
	}
	s.Close()
}

func TestRepairSchema_RunsOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "staypoint.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`DROP TABLE security_gate_audit_log`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !tableExists(t, s, "security_gate_audit_log") {
		t.Fatal("Open did not repair the missing table")
	}
}

func TestRepairSchema_SkipsUnrecordedMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "staypoint.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Not recorded: the normal migration runner owns it, repair must not.
	if _, err := s.DB().Exec(`DROP TABLE security_gate_audit_log; DELETE FROM schema_versions WHERE version = 20`); err != nil {
		t.Fatal(err)
	}
	actions, err := RepairSchema(s.DB(), func(string) {})
	if err != nil || len(actions) != 0 {
		t.Fatalf("unrecorded migration repaired: %+v %v", actions, err)
	}
}
