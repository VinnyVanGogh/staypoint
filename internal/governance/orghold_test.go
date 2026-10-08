package governance_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/names"
	"github.com/VinnyVanGogh/staypoint/internal/names/namestest"
)

// sqlHeld reports whether the Claim SQL fragment sees org as held, for a
// task row with that organization.
func sqlHeld(t *testing.T, conn *sql.DB, id, org string) bool {
	t.Helper()
	if _, err := conn.Exec(`INSERT OR REPLACE INTO tasks (id, name, repo_path, status, account_role, execution_stage, organization)
		VALUES (?, 'test task', '/tmp', 'active', 'personal', 'todo', ?)`, id, org); err != nil {
		t.Fatalf("insert task: %v", err)
	}
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM tasks WHERE id = ? AND `+governance.OrgNotHeldSQL("tasks"), id).Scan(&n); err != nil {
		t.Fatalf("claim SQL: %v", err)
	}
	return n == 0
}

func TestOrgHold_Lookalikes(t *testing.T) {
	conn := openTestDB(t)
	if err := governance.SetOrgHold(conn, "Managed Solution", true); err != nil {
		t.Fatal(err)
	}
	nbsp, zwsp := string(rune(0x00A0)), string(rune(0x200B))
	for i, org := range []string{
		"managed solution", "MANAGED SOLUTION", " Managed  Solution\t",
		"Managed" + nbsp + "Solution", "M" + string(rune(0x0430)) + "naged Solution",
		"Man" + zwsp + "aged Solution", string(rune(0x039C)) + "anaged Solution",
	} {
		if !governance.OrgHeld(conn, org) {
			t.Errorf("OrgHeld(%q) = false", org)
		}
		if !sqlHeld(t, conn, "t"+string(rune('a'+i)), org) {
			t.Errorf("claim SQL: %q not held", org)
		}
	}
	for i, org := range []string{"Managed Solutions", "Other", "", zwsp} {
		if governance.OrgHeld(conn, org) || sqlHeld(t, conn, "u"+string(rune('a'+i)), org) {
			t.Errorf("%q held, want not held", org)
		}
	}
	if err := governance.SetOrgHold(conn, zwsp+" ", true); err == nil {
		t.Error("SetOrgHold on an invisible-only name must fail")
	}
}

// A hold written by an older build (key folded by SQLite lower(trim()) only)
// still holds every spelling, and lifting clears it.
func TestOrgHold_LegacyKey(t *testing.T) {
	conn := openTestDB(t)
	if _, err := conn.Exec(`INSERT INTO settings_kv (key, value, updated_at) VALUES (?, '1', 'x')`,
		governance.OrgHoldPrefix+"Équipe Rouge"); err != nil {
		t.Fatal(err)
	}
	for i, org := range []string{"ÉQUIPE ROUGE", "equipe rouge", "Equipe  Rouge"} {
		if !governance.OrgHeld(conn, org) || !sqlHeld(t, conn, "l"+string(rune('a'+i)), org) {
			t.Errorf("legacy hold does not hold %q", org)
		}
	}
	if err := governance.SetOrgHold(conn, "equipe rouge", false); err != nil {
		t.Fatal(err)
	}
	if governance.OrgHeld(conn, "Équipe Rouge") || sqlHeld(t, conn, "la", "Équipe Rouge") {
		t.Error("lifting did not clear the legacy key")
	}
}

// FuzzOrgHold: a hold placed under any spelling of a name holds every other
// spelling of it, in Go (OrgHeld) and in the Claim SQL alike; for an
// arbitrary organization the two always agree; and lifting under any
// spelling lifts it everywhere.
func FuzzOrgHold(f *testing.F) {
	f.Add("Managed Solution", []byte{1, 2, 3, 0x84}, "managed solution")
	f.Add("acme", []byte{5, 4, 2}, "Acme Corp")
	f.Add("x", []byte{}, "\xff")
	store, err := db.Open(filepath.Join(f.TempDir(), "fuzz.db"))
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { store.Close() })
	conn := store.DB()
	f.Fuzz(func(t *testing.T, org string, seed []byte, other string) {
		if _, err := conn.Exec(`DELETE FROM settings_kv WHERE substr(key, 1, ?) = ?`, len(governance.OrgHoldPrefix), governance.OrgHoldPrefix); err != nil {
			t.Fatal(err)
		}
		a := namestest.ASCIIName(org)
		if a == "" {
			return
		}
		if err := governance.SetOrgHold(conn, namestest.Variant(a, seed), true); err != nil {
			t.Fatalf("SetOrgHold(%q): %v", namestest.Variant(a, seed), err)
		}
		rev := append([]byte(nil), seed...)
		for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
			rev[i], rev[j] = rev[j], rev[i]
		}
		v := namestest.Variant(a, append(rev, 2))
		if !governance.OrgHeld(conn, v) || !sqlHeld(t, conn, "v", v) {
			t.Fatalf("hold on %q does not hold %q", namestest.Variant(a, seed), v)
		}
		want := names.Normalize(other) == a
		if got, gotSQL := governance.OrgHeld(conn, other), sqlHeld(t, conn, "o", other); got != want || gotSQL != want {
			t.Fatalf("org %q under hold %q: OrgHeld %v, SQL %v, want %v", other, a, got, gotSQL, want)
		}
		if err := governance.SetOrgHold(conn, v, false); err != nil {
			t.Fatal(err)
		}
		if governance.OrgHeld(conn, a) || sqlHeld(t, conn, "v", a) {
			t.Fatalf("lifting under %q left %q held", v, a)
		}
	})
}
