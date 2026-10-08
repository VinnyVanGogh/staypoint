package gates

import (
	"strings"
	"testing"
	"time"
)

// task-33692ffb: org trust rules, matching and limits.

func seedOrgTask(t *testing.T, tdb Execer, id, org, stage string) {
	t.Helper()
	if _, err := tdb.Exec(`INSERT INTO tasks (id, name, repo_path, organization, execution_stage, status) VALUES (?, ?, '', ?, ?, 'active')`,
		id, id, org, stage); err != nil {
		t.Fatal(err)
	}
}

func TestOrgTrust_LimitsAndNormalisation(t *testing.T) {
	now := time.Date(2026, 10, 7, 21, 0, 0, 0, time.UTC)
	r, err := NewOrgTrust(OrgTrustSpec{Org: "  Acme Corp ", Preset: "overnight"}, 0.7, now)
	if err != nil {
		t.Fatal(err)
	}
	if r.Scope != ScopeOrg || r.ScopeValue != "acme corp" || r.MatchKind != MatchAny || !r.ExpiresAt.Equal(now.Add(12*time.Hour)) {
		t.Fatalf("rule: %+v", r)
	}
	for _, spec := range []OrgTrustSpec{
		{Org: "acme", Preset: "custom", Minutes: MaxOrgTrustMinutes + 1}, // over 16h
		{Org: "acme", Preset: "custom", Minutes: 5},                      // under 15 min
		{Org: "acme", Preset: ""},                                        // no expiry
		{Org: "   ", Preset: "1h"},                                       // no org
		{Org: "acme", Preset: "1h", Tev1: true},                          // tev1 not acknowledged
	} {
		if _, err := NewOrgTrust(spec, 0.7, now); err == nil {
			t.Errorf("accepted %+v", spec)
		}
	}
	if _, err := NewOrgTrust(OrgTrustSpec{Org: "acme", Preset: "custom", Minutes: MaxOrgTrustMinutes}, 0.7, now); err != nil {
		t.Fatalf("16h refused: %v", err)
	}
}

func TestOrgTrust_Matching(t *testing.T) {
	d := openDB(t)
	now := time.Now().UTC()
	seedOrgTask(t, d, "A1", "Acme", "in_progress")
	seedOrgTask(t, d, "A2", " ACME ", "in_progress")
	seedOrgTask(t, d, "A3", "acme", "todo") // not running
	seedOrgTask(t, d, "B1", "Beta", "in_progress")
	seedOrgTask(t, d, "N1", "", "in_progress")

	draft, err := NewOrgTrust(OrgTrustSpec{Org: "aCmE", Preset: "1h"}, 0.7, now)
	if err != nil {
		t.Fatal(err)
	}
	rule, err := InsertOrgTrust(d, draft, now)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"A1": true, "A2": true, "A3": false, "B1": false, "N1": false, "missing": false, "": false} {
		got, err := ActiveOrgTrust(d, id, now)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if (got != nil) != want || (got != nil && got.ID != rule.ID) {
			t.Errorf("%s: got %v, want match=%v", id, got, want)
		}
	}
	// Org trust rules never match as ordinary allow rules or task trusts.
	if got, _ := ActiveTrust(d, "A1", now); got != nil {
		t.Fatalf("org trust surfaced as a task trust: %+v", got)
	}

	// Expired: no match.
	if got, _ := ActiveOrgTrust(d, "A1", now.Add(time.Hour+time.Second)); got != nil {
		t.Fatal("expired org trust matched")
	}
	// Revoked: no match, at once.
	if ok, err := EndTrust(d, rule.ID, "revoked by the Board", now); err != nil || !ok {
		t.Fatalf("end: %v %v", ok, err)
	}
	if got, _ := ActiveOrgTrust(d, "A1", now); got != nil {
		t.Fatal("revoked org trust matched")
	}
	if act, _ := ActiveOrgTrusts(d, now); len(act) != 0 {
		t.Fatalf("revoked trust listed active: %v", act)
	}
}

func TestOrgTrust_ReplacesAndFailsClosed(t *testing.T) {
	d := openDB(t)
	now := time.Now().UTC()
	seedOrgTask(t, d, "A1", "acme", "in_progress")
	first, _ := NewOrgTrust(OrgTrustSpec{Org: "acme", Preset: "1h"}, 0.7, now)
	r1, err := InsertOrgTrust(d, first, now)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := NewOrgTrust(OrgTrustSpec{Org: "ACME", Preset: "4h"}, 0.7, now)
	r2, err := InsertOrgTrust(d, second, now)
	if err != nil {
		t.Fatal(err)
	}
	act, _ := ActiveOrgTrusts(d, now)
	if len(act) != 1 || act[0].ID != r2.ID || r1.ID == r2.ID {
		t.Fatalf("want only the replacement active: %v", act)
	}
	// A broken tasks table is an error, never a silent approval.
	if _, err := d.Exec(`ALTER TABLE tasks RENAME TO tasks_gone`); err != nil {
		t.Fatal(err)
	}
	if got, err := ActiveOrgTrust(d, "A1", now); err == nil || got != nil {
		t.Fatalf("lookup error not surfaced: %v %v", got, err)
	} else if !strings.Contains(err.Error(), "tasks") {
		t.Logf("lookup error: %v", err)
	}
}
