package shipreview

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	_ "modernc.org/sqlite"
)

// Filesystem-independent checks of the #245 name logic: these hold on a
// case-sensitive filesystem too, where the alias itself cannot be built.
func TestProtectedName_EqualFold(t *testing.T) {
	for _, b := range []string{"main", "Main", "MAIN", "master", "Master", "dev-server", "Dev-Server", "DEV-SERVER", "HEAD", "head"} {
		if !protectedName(b) {
			t.Errorf("protectedName(%q) = false", b)
		}
	}
	if !protectedName("Release", "", "release") || !protectedName("TRUNK", "trunk") {
		t.Error("extra names must compare ignoring case")
	}
	for _, b := range []string{"mainline", "fix/main", "dev-server-2", "feature/x"} {
		if protectedName(b, "", "release") {
			t.Errorf("protectedName(%q) = true", b)
		}
	}
}

func TestMatchExactRefs_CaseSensitiveAndNoChildren(t *testing.T) {
	out := "refs/heads/main\nrefs/heads/Feature/x\nrefs/remotes/origin/fix/a/b\n"
	got := matchExactRefs(out, "refs/heads/Main", "refs/heads/feature/x", "refs/remotes/origin/fix/a", "refs/heads/main")
	if len(got) != 1 || !got["refs/heads/main"] {
		t.Fatalf("matchExactRefs = %v, want only refs/heads/main", got)
	}
}

func TestLsRemoteTip_ExactRefOnly(t *testing.T) {
	out := "aaa\trefs/heads/x/refs/heads/fix\nbbb\trefs/heads/fix\n"
	if got := lsRemoteTip(out, "fix"); got != "bbb" {
		t.Fatalf("lsRemoteTip = %q, want bbb", got)
	}
	if got := lsRemoteTip("ccc\trefs/heads/main\n", "Main"); got != "" {
		t.Fatalf("lsRemoteTip matched across case: %q", got)
	}
}

func TestBranchOwnedByTask(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE task_work_products (id INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT, product_type TEXT,
		reference TEXT, provenance TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')))`); err != nil {
		t.Fatal(err)
	}
	ins := func(task, ref, prov string) {
		if _, err := db.Exec(`INSERT INTO task_work_products (task_id, product_type, reference, provenance) VALUES (?, 'branch', ?, ?)`, task, ref, prov); err != nil {
			t.Fatal(err)
		}
	}
	ins("t1", "fix/mine", ProvenanceTask)
	ins("t2", "fix/mine", ProvenanceTask) // later claim by another task
	ins("t1", "origin/fix/pushed", ProvenanceTask)
	ins("t1", "fix/theirs", ProvenanceForeign)
	ins("t1", "fix/legacy", "")
	ctx := context.Background()
	for _, c := range []struct {
		task, branch string
		want         bool
	}{
		{"t1", "staypoint/t1", true},
		{"t1", "staypoint/t2", false},
		{"t1", "fix/mine", true},
		{"t2", "fix/mine", false}, // t1 registered it first
		{"t1", "Fix/Mine", false}, // exact case only
		{"t1", "fix/pushed", true},
		{"t1", "fix/theirs", false},
		{"t1", "fix/legacy", false},
		{"t1", "fix/unknown", false},
	} {
		if got := BranchOwnedByTask(ctx, db, c.task, c.branch); got != c.want {
			t.Errorf("BranchOwnedByTask(%s, %s) = %v, want %v", c.task, c.branch, got, c.want)
		}
	}
	card := &Card{TaskID: "t1", Branch: "fix/theirs"}
	err = CleanupTaskBranch(ctx, db, t.TempDir(), card, "abc")
	if !errors.Is(err, ErrBranchNotOwned) || err.Error() != "branch fix/theirs is not owned by this task; not deleted" {
		t.Fatalf("CleanupTaskBranch = %v", err)
	}
}
