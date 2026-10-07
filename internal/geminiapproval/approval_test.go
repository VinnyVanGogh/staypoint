package geminiapproval_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/geminiapproval"
	"github.com/VinnyVanGogh/staypoint/internal/security"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "staypoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store.DB()
}

// isWork treats any path containing "mansol" as a work repo.
func isWork(p string) bool { return strings.Contains(p, "mansol") }

func TestScopeCmdlineRoundTrip(t *testing.T) {
	for _, s := range []geminiapproval.Scope{
		{TaskID: "task-1234abcd", Repo: "/Users/x/dev/my app"},
		{SessionID: "conv-9f8e", Repo: `/tmp/a "quoted" repo`},
	} {
		got, ok := geminiapproval.Parse(s.Cmdline())
		if !ok || got != s {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", s.Cmdline(), got, ok, s)
		}
	}
	for _, bad := range []string{
		"staypoint gate gemini-code --task t1",
		`staypoint gate gemini-code --task t1; rm -rf / --repo "/x"`,
		`staypoint gate override --minutes 5 --company "x"`,
	} {
		if _, ok := geminiapproval.Parse(bad); ok {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

func TestRequestRefusesWorkRepos(t *testing.T) {
	conn := openDB(t)
	_, err := geminiapproval.Request(conn, geminiapproval.Scope{TaskID: "t1", Repo: "/dev/mansol-app"}, isWork)
	if !errors.Is(err, geminiapproval.ErrWorkRepo) {
		t.Fatalf("work repo request: err = %v, want ErrWorkRepo", err)
	}
	if _, err := geminiapproval.Request(conn, geminiapproval.Scope{TaskID: "t1", SessionID: "s", Repo: "/dev/app"}, isWork); err == nil {
		t.Error("a scope with both task and session was accepted")
	}
	if _, err := geminiapproval.Request(conn, geminiapproval.Scope{TaskID: "t1", Repo: "/dev/app"}, nil); err == nil {
		t.Error("nil classifier must fail closed")
	}
}

func TestTaskGrantLifecycleOneRunPerApproval(t *testing.T) {
	conn := openDB(t)
	repo := "/dev/personal-app"
	g, err := geminiapproval.LatestTaskGrant(conn, "t1", repo, isWork)
	if err != nil || g.State != geminiapproval.StateNone {
		t.Fatalf("fresh: %+v %v", g, err)
	}
	gr, err := geminiapproval.Request(conn, geminiapproval.Scope{TaskID: "t1", Repo: repo}, isWork)
	if err != nil {
		t.Fatal(err)
	}
	if g, _ := geminiapproval.LatestTaskGrant(conn, "t1", repo, isWork); g.State != geminiapproval.StatePending || g.GateID != gr.ID {
		t.Fatalf("pending: %+v", g)
	}
	// Another task's or another repo's approval does not count.
	if g, _ := geminiapproval.LatestTaskGrant(conn, "t2", repo, isWork); g.State != geminiapproval.StateNone {
		t.Errorf("other task sees %+v", g)
	}
	if g, _ := geminiapproval.LatestTaskGrant(conn, "t1", "/dev/other", isWork); g.State != geminiapproval.StateNone {
		t.Errorf("other repo sees %+v", g)
	}
	if _, err := security.DecideGateRequest(conn, gr.ID, true); err != nil {
		t.Fatal(err)
	}
	if g, _ := geminiapproval.LatestTaskGrant(conn, "t1", repo, isWork); g.State != geminiapproval.StateApproved {
		t.Fatalf("approved: %+v", g)
	}
	if err := geminiapproval.Consume(conn, gr.ID, "t1", "run-1"); err != nil {
		t.Fatal(err)
	}
	if err := geminiapproval.Consume(conn, gr.ID, "t1", "run-2"); err == nil {
		t.Fatal("an approval was consumed twice")
	}
	if g, _ := geminiapproval.LatestTaskGrant(conn, "t1", repo, isWork); g.State != geminiapproval.StateNone {
		t.Fatalf("after one run the next needs a new approval, got %+v", g)
	}
}

func TestForgedWorkRepoApprovalGrantsNothing(t *testing.T) {
	conn := openDB(t)
	now := time.Now()
	for _, s := range []geminiapproval.Scope{
		{TaskID: "t1", Repo: "/dev/mansol-app"},
		{SessionID: "conv1", Repo: "/dev/mansol-app"},
	} {
		if _, err := conn.Exec(`INSERT INTO security_gate_requests (id, cmdline, run_id, status, created_at, decided_at) VALUES (?, ?, ?, 'approved', ?, ?)`,
			"forged-"+s.TaskID+s.SessionID, s.Cmdline(), geminiapproval.RunID, now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := geminiapproval.LatestTaskGrant(conn, "t1", "/dev/mansol-app", isWork); !errors.Is(err, geminiapproval.ErrWorkRepo) {
		t.Errorf("forged task approval: err = %v, want ErrWorkRepo", err)
	}
	if ok, _ := geminiapproval.SessionApproved(conn, "conv1", "/dev/mansol-app", isWork, now); ok {
		t.Error("forged session approval granted a work repo")
	}
}

func TestSessionApprovalScopeAndExpiry(t *testing.T) {
	conn := openDB(t)
	repo := "/dev/personal-app"
	gr, err := geminiapproval.Request(conn, geminiapproval.Scope{SessionID: "conv1", Repo: repo}, isWork)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if ok, _ := geminiapproval.SessionApproved(conn, "conv1", repo, isWork, now); ok {
		t.Fatal("pending request approved the session")
	}
	if _, err := security.DecideGateRequest(conn, gr.ID, true); err != nil {
		t.Fatal(err)
	}
	if ok, err := geminiapproval.SessionApproved(conn, "conv1", repo, isWork, now); !ok || err != nil {
		t.Fatalf("approved session: %v %v", ok, err)
	}
	if ok, _ := geminiapproval.SessionApproved(conn, "conv1", repo+"/sub", isWork, now); !ok {
		t.Error("a subdirectory of the approved repo should be covered")
	}
	if ok, _ := geminiapproval.SessionApproved(conn, "conv2", repo, isWork, now); ok {
		t.Error("another conversation was approved")
	}
	if ok, _ := geminiapproval.SessionApproved(conn, "conv1", "/dev/other", isWork, now); ok {
		t.Error("another repo was approved")
	}
	if ok, _ := geminiapproval.SessionApproved(conn, "conv1", repo, isWork, now.Add((geminiapproval.MaxSessionHours+1)*time.Hour)); ok {
		t.Error("approval did not expire")
	}
}
