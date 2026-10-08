package shipreview

import (
	"context"
	"database/sql"
	"errors"
	"os/exec"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/VinnyVanGogh/staypoint/internal/names"
	"github.com/VinnyVanGogh/staypoint/internal/names/namestest"
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

func TestProtectedName_Lookalikes(t *testing.T) {
	cyrA := string(rune(0x0430))
	zwsp := string(rune(0x200B))
	fullM := string(rune(0xFF4D))
	for _, b := range []string{"m" + cyrA + "in", "ma" + zwsp + "in", fullM + "ain", " main", "Dev-Serv" + string(rune(0x0435)) + "r", "M" + cyrA + "STER"} {
		if !protectedName(b) {
			t.Errorf("protectedName(%q) = false", b)
		}
	}
	if !protectedName("r"+string(rune(0x0435))+"lease", "release") {
		t.Error("extra names must compare through names.Normalize")
	}
}

// FuzzProtectedName: every case, lookalike, accent, zero-width or whitespace
// spelling of a protected name or an extra name is protected, and a name
// that normalises to something else is not.
func FuzzProtectedName(f *testing.F) {
	for _, s := range []string{"main", "master", "dev-server", "head", "release", "feature/x"} {
		f.Add(s, []byte{1, 2, 3, 4, 0x85}, "release")
	}
	f.Fuzz(func(t *testing.T, branch string, seed []byte, extra string) {
		ex := namestest.ASCIIName(extra)
		for _, p := range []string{"main", "master", "dev-server", "head", ex} {
			if p == "" {
				continue
			}
			if v := namestest.Variant(p, seed); !protectedName(v, ex) {
				t.Fatalf("protectedName(%q, %q) = false, a spelling of %q", v, ex, p)
			}
		}
		got := protectedName(branch, extra)
		want := false
		for _, p := range append([]string{"main", "master", WorkTargetBranch, "HEAD"}, extra) {
			if p != "" && names.Normalize(branch) == names.Normalize(p) {
				want = true
			}
		}
		if got != want {
			t.Fatalf("protectedName(%q, %q) = %v, want %v", branch, extra, got, want)
		}
	})
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

// The after-delete check catches a protected ref that vanished, puts it back
// and fails loudly; a moved one is reported.
func TestCheckLocalProtected_RestoresAndFailsLoudly(t *testing.T) {
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "i"},
		{"branch", "dev-server"},
		{"checkout", "-q", "--detach"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	ctx := context.Background()
	names := protectedNames(ctx, repo, "")
	before, err := localRefSnapshot(ctx, repo, names)
	if err != nil || before["refs/heads/main"] == "" || before["refs/heads/dev-server"] == "" {
		t.Fatalf("snapshot %v %v", before, err)
	}
	if out, err := exec.Command("git", "-C", repo, "update-ref", "-d", "refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	err = checkLocalProtected(ctx, repo, names, before)
	if err == nil || !strings.Contains(err.Error(), "PROTECTED REF DELETED") || !strings.Contains(err.Error(), "restored") {
		t.Fatalf("checkLocalProtected = %v", err)
	}
	after, _ := localRefSnapshot(ctx, repo, names)
	if after["refs/heads/main"] != before["refs/heads/main"] {
		t.Fatalf("main not restored: %v", after)
	}
	if err := compareSnapshots("origin", map[string]string{"refs/heads/main": "a"}, map[string]string{"refs/heads/main": "b"}, nil); err == nil || !strings.Contains(err.Error(), "PROTECTED REF MOVED") {
		t.Fatalf("moved ref not reported: %v", err)
	}
}

func TestLsRemoteSymref(t *testing.T) {
	out := "ref: refs/heads/main\trefs/heads/fix/sym\nabc\trefs/heads/fix/sym\n"
	if !lsRemoteSymref(out, "fix/sym") || lsRemoteSymref(out, "main") {
		t.Fatal("lsRemoteSymref")
	}
}
