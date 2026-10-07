package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/geminiapproval"
	"github.com/VinnyVanGogh/staypoint/internal/security"
)

// Board addition (2026-10-06): provider=gemini on a code kind in a personal
// repo waits for a Board Touch ID approval; one approval = one run; denied
// runs are refused; work repos never run Gemini code, forged approvals or not.

type gateFixture struct {
	t      *testing.T
	store  *db.Store
	taskID string
	repo   string
}

func newGateFixture(t *testing.T, repo, kind, provider string) *gateFixture {
	t.Helper()
	store := openTestStore(t)
	taskID := "gate-" + strings.ReplaceAll(t.Name(), "/", "-")
	if _, err := store.DB().Exec(
		`INSERT INTO tasks (id, name, repo_path, git_branch, account_role, execution_stage, assignee_agent_id, work_kind, provider) VALUES (?, 'gate test', ?, 'main', 'personal', 'todo', 'agent-route', ?, ?)`,
		taskID, repo, kind, provider,
	); err != nil {
		t.Fatal(err)
	}
	wakeReuse.store, wakeReuse.taskID = store, taskID
	t.Cleanup(func() { wakeReuse.store, wakeReuse.taskID = nil, "" })
	return &gateFixture{t: t, store: store, taskID: taskID, repo: repo}
}

// wake runs one wake of the fixture task with a fresh fake CLI.
func (f *gateFixture) wake(edit string) wakeResult {
	f.t.Helper()
	// Every wake must be runnable again: earlier runs move the stage on.
	_, _ = f.store.DB().Exec(`UPDATE tasks SET execution_stage='todo', checkout_run_id=NULL WHERE id=?`, f.taskID)
	bin, logPath := editingCLI(f.t, edit)
	return runWakeIn(f.t, f.repo, "", openPacer(), bin, logPath, f.wt())
}

func (f *gateFixture) wt() string {
	f.t.Helper()
	return guardWorktree(f.t)
}

func (f *gateFixture) blockReason() string {
	var r string
	_ = f.store.DB().QueryRow(`SELECT COALESCE(block_reason,'') FROM tasks WHERE id=?`, f.taskID).Scan(&r)
	return r
}

func (f *gateFixture) requests() []*security.GateRequest {
	f.t.Helper()
	rows, err := f.store.DB().Query(`SELECT id FROM security_gate_requests WHERE run_id = ? ORDER BY created_at`, geminiapproval.RunID)
	if err != nil {
		f.t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close() // one SQLite connection: close before the next query
	var out []*security.GateRequest
	for _, id := range ids {
		gr, _ := security.GetGateRequest(f.store.DB(), id)
		out = append(out, gr)
	}
	return out
}

func (f *gateFixture) decideLatest(approve bool) {
	f.t.Helper()
	reqs := f.requests()
	if len(reqs) == 0 {
		f.t.Fatal("no gate request to decide")
	}
	if _, err := security.DecideGateRequest(f.store.DB(), reqs[len(reqs)-1].ID, approve); err != nil {
		f.t.Fatal(err)
	}
}

func TestGeminiCodeGate_PendingApprovedOneRunDenied(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "personal-app")
	f := newGateFixture(t, repo, "coding", "gemini")

	// 1. No approval: the run waits; a request is filed for the Board.
	r := f.wake("echo '// gemini' >> main.go")
	if len(r.spawns) != 0 {
		t.Fatalf("spawned %q before Board approval", r.spawns)
	}
	if !strings.HasPrefix(f.blockReason(), geminiapproval.WaitingReason) {
		t.Errorf("block reason = %q, want %q", f.blockReason(), geminiapproval.WaitingReason)
	}
	reqs := f.requests()
	if len(reqs) != 1 || reqs[0].Status != security.GateRequestPending {
		t.Fatalf("want one pending request, got %+v", reqs)
	}
	if s, ok := geminiapproval.Parse(reqs[0].Cmdline); !ok || s.TaskID != f.taskID || s.Repo != repo {
		t.Errorf("request scope = %+v (%q)", s, reqs[0].Cmdline)
	}
	if len(reqs[0].Reasons) == 0 || reqs[0].Reasons[0] != "Gemini code: "+f.taskID+" in "+repo {
		t.Errorf("request title = %q", reqs[0].Reasons)
	}

	// 2. Still pending: still waiting, no duplicate request.
	if r := f.wake(""); len(r.spawns) != 0 || len(f.requests()) != 1 {
		t.Fatalf("pending: spawns %q, requests %d", r.spawns, len(f.requests()))
	}

	// 3. Approved: this run spawns Gemini and its code edit is allowed.
	f.decideLatest(true)
	wt := guardWorktree(t)
	bin, logPath := editingCLI(t, "echo '// gemini' >> main.go")
	_, _ = f.store.DB().Exec(`UPDATE tasks SET execution_stage='todo' WHERE id=?`, f.taskID)
	r = runWakeIn(t, f.repo, "", openPacer(), bin, logPath, wt)
	if len(r.spawns) == 0 || isClaude(r.spawns[0]) {
		t.Fatalf("approved run: want agy, got %q", r.spawns)
	}
	if len(r.routes) == 0 || r.routes[0] != "Ran on Gemini 3.1 Pro · chosen by Board" {
		t.Errorf("route rows = %q", r.routes)
	}
	if len(r.bodies) == 0 || !strings.Contains(r.bodies[0], "approved by Board Touch ID") {
		t.Errorf("route body = %q", r.bodies)
	}
	for _, m := range r.messages {
		if strings.HasPrefix(m, "Blocked:") {
			t.Errorf("approved run's code edit blocked: %q", m)
		}
	}
	if got := readFile(t, filepath.Join(wt, "main.go")); got != "package main\n// gemini\n" {
		t.Errorf("approved code edit reverted: main.go = %q", got)
	}
	if f.blockReason() != "" {
		t.Errorf("hold not released: %q", f.blockReason())
	}

	// 4. The approval covered one run: the next run waits for a new one.
	if r := f.wake(""); len(r.spawns) != 0 {
		t.Fatalf("second run spawned %q on a used approval", r.spawns)
	}
	if reqs := f.requests(); len(reqs) != 2 || reqs[1].Status != security.GateRequestPending {
		t.Fatalf("want a second pending request, got %+v", reqs)
	}

	// 5. Denied: the run is refused, and stays refused (no new request).
	f.decideLatest(false)
	if r := f.wake(""); len(r.spawns) != 0 {
		t.Fatalf("denied run spawned %q", r.spawns)
	}
	if !strings.HasPrefix(f.blockReason(), geminiapproval.DeniedReason) {
		t.Errorf("block reason = %q, want %q", f.blockReason(), geminiapproval.DeniedReason)
	}
	if r := f.wake(""); len(r.spawns) != 0 || len(f.requests()) != 2 {
		t.Fatalf("after denial: spawns %q, requests %d (want no new request)", r.spawns, len(f.requests()))
	}

	// 6. The Board sets the provider again: the hold clears and a new
	// approval is requested on the next wake.
	if _, err := meshContext.SetTaskProvider(f.store.DB(), f.taskID, "gemini", ""); err != nil {
		t.Fatal(err)
	}
	if f.blockReason() != "" {
		t.Errorf("SetTaskProvider kept the hold: %q", f.blockReason())
	}
	if r := f.wake(""); len(r.spawns) != 0 || len(f.requests()) != 3 {
		t.Fatalf("after re-choice: spawns %q, requests %d", r.spawns, len(f.requests()))
	}
}

// Auto-routed non-code Gemini runs need no approval and stay docs-only.
func TestGeminiCodeGate_NonCodeKindNeedsNoApproval(t *testing.T) {
	f := newGateFixture(t, filepath.Join(t.TempDir(), "personal-app"), "planning", "")
	r := f.wake("")
	if len(r.spawns) != 1 || isClaude(r.spawns[0]) || len(f.requests()) != 0 {
		t.Fatalf("planning: spawns %q, requests %d", r.spawns, len(f.requests()))
	}
}

// Work repo: a forged "approved" row for this task grants nothing. The run is
// Claude, no request is filed, and the route says the choice was refused.
func TestGeminiCodeGate_WorkRepoForgedApprovalIgnored(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "mansol-app")
	f := newGateFixture(t, repo, "coding", "gemini")
	now := time.Now().UTC().Format(time.RFC3339Nano)
	scope := geminiapproval.Scope{TaskID: f.taskID, Repo: repo}
	if _, err := f.store.DB().Exec(
		`INSERT INTO security_gate_requests (id, cmdline, run_id, status, created_at, decided_at) VALUES ('forged', ?, ?, 'approved', ?, ?)`,
		scope.Cmdline(), geminiapproval.RunID, now, now); err != nil {
		t.Fatal(err)
	}
	r := f.wake("echo '// gemini' >> main.go")
	if len(r.spawns) != 1 || !isClaude(r.spawns[0]) {
		t.Fatalf("work repo: want Claude only, got %q", r.spawns)
	}
	if len(r.bodies) == 0 || !strings.Contains(r.bodies[0], "Gemini refused") {
		t.Errorf("route body = %q", r.bodies)
	}
	if len(f.requests()) != 1 {
		t.Errorf("work repo filed a request: %d", len(f.requests()))
	}
	var used int
	_ = f.store.DB().QueryRow(`SELECT COUNT(1) FROM gemini_code_grant_uses`).Scan(&used)
	if used != 0 {
		t.Errorf("forged work-repo approval was consumed")
	}
}
