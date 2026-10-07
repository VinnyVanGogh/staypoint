package trackgate

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/geminiapproval"
	"github.com/VinnyVanGogh/staypoint/internal/security"
)

const (
	workRepo     = "/home/u/Documents/dev/mansol-portal"
	personalRepo = "/home/u/Documents/dev/hobby"
)

func isWork(p string) bool { return strings.HasPrefix(p, workRepo) }

func openStore(t *testing.T) *sql.DB {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "staypoint.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store.DB()
}

func gateFor(conn *sql.DB, now time.Time) *Gate {
	return &Gate{
		OpenDB:     func() (*sql.DB, func(), error) { return conn, func() {}, nil },
		IsWorkRepo: isWork,
		Now:        func() time.Time { return now },
	}
}

func insertTask(t *testing.T, conn *sql.DB, id, repo, org, status string) {
	t.Helper()
	if _, err := conn.Exec(`INSERT INTO tasks (id, name, repo_path, organization, status) VALUES (?, ?, ?, ?, ?)`,
		id, "t "+id, repo, org, status); err != nil {
		t.Fatalf("insert task: %v", err)
	}
}

func editReq(path string) Request {
	return Request{Client: ClientClaude, ToolName: "Edit", FilePaths: []string{path}, CWD: workRepo, SessionID: "sess-1"}
}

func bashReq(cmd, cwd string) Request {
	return Request{Client: ClientClaude, ToolName: "Bash", Command: cmd, CWD: cwd, SessionID: "sess-1"}
}

func TestWorkRepoNoTask_EditAndCommitBlocked(t *testing.T) {
	g := gateFor(openStore(t), time.Now())
	for _, req := range []Request{
		editReq(workRepo + "/main.go"),
		{Client: ClientClaude, ToolName: "Write", FilePaths: []string{workRepo + "/new.go"}, CWD: workRepo, SessionID: "sess-1"},
		{Client: ClientClaude, ToolName: "MultiEdit", FilePaths: []string{workRepo + "/a.go"}, CWD: workRepo, SessionID: "sess-1"},
		{Client: ClientClaude, ToolName: "NotebookEdit", FilePaths: []string{workRepo + "/n.ipynb"}, CWD: workRepo, SessionID: "sess-1"},
		bashReq("git commit -m wip", workRepo),
		bashReq("git push origin feature", workRepo),
		bashReq("git merge main", workRepo),
		bashReq("git rebase origin/main", workRepo),
		bashReq("gh pr create --fill", workRepo),
		bashReq("gh pr merge 3", workRepo),
		bashReq("echo x > notes.md", workRepo),
		bashReq("echo x | tee notes.md", workRepo),
		bashReq("git -C "+workRepo+" commit -m x", "/tmp"),
	} {
		d := g.Evaluate(req)
		if !d.Block {
			t.Errorf("%s %q %v: want blocked", req.ToolName, req.Command, req.FilePaths)
			continue
		}
		if d.Company != CompanyManagedSolution {
			t.Errorf("company = %q, want Managed Solution", d.Company)
		}
		for _, want := range []string{
			"staypoint task create --org 'Managed Solution' --session sess-1",
			"staypoint task attach <task-id> --session sess-1",
			"not attached to a StayPoint task",
		} {
			if !strings.Contains(d.Reason, want) {
				t.Errorf("block message missing %q:\n%s", want, d.Reason)
			}
		}
	}
}

func TestWorkRepoReadOnlyAllowed(t *testing.T) {
	g := gateFor(openStore(t), time.Now())
	for _, req := range []Request{
		{Client: ClientClaude, ToolName: "Read", FilePaths: []string{workRepo + "/main.go"}, CWD: workRepo},
		{Client: ClientClaude, ToolName: "Grep", CWD: workRepo},
		bashReq("git status && git log -5", workRepo),
		bashReq("go test ./...", workRepo),
		bashReq("echo hi > /tmp/scratch.txt", workRepo), // redirect outside the repo
	} {
		if d := g.Evaluate(req); d.Block {
			t.Errorf("%s %q: want allowed, got block:\n%s", req.ToolName, req.Command, d.Reason)
		}
	}
}

func TestAttachedSessionAllowed(t *testing.T) {
	conn := openStore(t)
	insertTask(t, conn, "task-1", workRepo, CompanyManagedSolution, "active")
	if err := Attach(conn, "sess-1", "task-1", ClientClaude, workRepo); err != nil {
		t.Fatalf("attach: %v", err)
	}
	g := gateFor(conn, time.Now())
	for _, req := range []Request{editReq(workRepo + "/main.go"), bashReq("git commit -m x", workRepo)} {
		if d := g.Evaluate(req); d.Block {
			t.Errorf("attached session blocked:\n%s", d.Reason)
		}
	}
	// A different session is still blocked.
	other := editReq(workRepo + "/main.go")
	other.SessionID = "sess-2"
	if d := g.Evaluate(other); !d.Block {
		t.Error("unattached session sess-2 allowed")
	}

	// Timeline entry.
	var details string
	if err := conn.QueryRow(`SELECT details FROM activity_log WHERE task_id = 'task-1' AND event_type = 'interactive_session_attached'`).Scan(&details); err != nil {
		t.Fatalf("timeline entry: %v", err)
	}
	if !strings.Contains(details, "Interactive session attached") || !strings.Contains(details, "sess-1") {
		t.Errorf("timeline details = %q", details)
	}

	// Once the task is done the attachment no longer counts.
	if _, err := conn.Exec(`UPDATE tasks SET status = 'done' WHERE id = 'task-1'`); err != nil {
		t.Fatal(err)
	}
	if d := g.Evaluate(editReq(workRepo + "/main.go")); !d.Block {
		t.Error("session attached to a done task allowed")
	}
}

func TestAttachedToOtherCompanyTaskBlocked(t *testing.T) {
	for _, tc := range []struct {
		org   string
		block bool
	}{
		{"StayPoint", true},
		{"", true},
		{"Research", true},
		{"MAN", false},
		{"managed solution", false},
		{CompanyManagedSolution, false},
	} {
		conn := openStore(t)
		insertTask(t, conn, "task-1", workRepo, tc.org, "active")
		if err := Attach(conn, "sess-1", "task-1", ClientClaude, workRepo); err != nil {
			t.Fatalf("attach: %v", err)
		}
		d := gateFor(conn, time.Now()).Evaluate(editReq(workRepo + "/main.go"))
		if d.Block != tc.block {
			t.Errorf("org %q: block = %v, want %v (%s)", tc.org, d.Block, tc.block, d.Reason)
		}
		if tc.block && !strings.Contains(d.Reason, "attach a Managed Solution task") {
			t.Errorf("org %q: reason missing guidance:\n%s", tc.org, d.Reason)
		}
	}
}

func TestAttachRejectsUnknownOrInactiveTask(t *testing.T) {
	conn := openStore(t)
	if err := Attach(conn, "s", "task-missing", ClientClaude, ""); err == nil {
		t.Error("attach to missing task succeeded")
	}
	insertTask(t, conn, "task-done", workRepo, CompanyManagedSolution, "done")
	if err := Attach(conn, "s", "task-done", ClientClaude, ""); err == nil {
		t.Error("attach to done task succeeded")
	}
}

func TestPersonalRepoAllowedByDefault(t *testing.T) {
	g := gateFor(openStore(t), time.Now())
	req := Request{Client: ClientClaude, ToolName: "Edit", FilePaths: []string{personalRepo + "/x.go"}, CWD: personalRepo, SessionID: "s"}
	if d := g.Evaluate(req); d.Block {
		t.Errorf("personal repo blocked:\n%s", d.Reason)
	}
	if d := g.Evaluate(bashReq("git commit -m x", personalRepo)); d.Block {
		t.Errorf("personal commit blocked:\n%s", d.Reason)
	}
}

func TestPersonalCompanyToggleOn(t *testing.T) {
	conn := openStore(t)
	insertTask(t, conn, "task-p", personalRepo, "StayPoint", "active")
	if err := SetEnabled(conn, "StayPoint", true); err != nil {
		t.Fatal(err)
	}
	g := gateFor(conn, time.Now())
	d := g.Evaluate(bashReq("git commit -m x", personalRepo))
	if !d.Block || d.Company != "StayPoint" {
		t.Fatalf("want StayPoint gate to block, got %+v", d)
	}
	if !strings.Contains(d.Reason, "--org 'StayPoint'") {
		t.Errorf("message should name the company's org:\n%s", d.Reason)
	}
}

func TestManagedSolutionToggleOff(t *testing.T) {
	conn := openStore(t)
	if err := SetEnabled(conn, CompanyManagedSolution, false); err != nil {
		t.Fatal(err)
	}
	if d := gateFor(conn, time.Now()).Evaluate(editReq(workRepo + "/a.go")); d.Block {
		t.Errorf("gate disabled for Managed Solution but blocked:\n%s", d.Reason)
	}
}

func TestFailClosedWhenDBUnreadable(t *testing.T) {
	g := &Gate{
		OpenDB:     func() (*sql.DB, func(), error) { return nil, nil, errors.New("disk I/O error") },
		IsWorkRepo: isWork,
	}
	d := g.Evaluate(editReq(workRepo + "/a.go"))
	if !d.Block || !strings.Contains(d.Reason, "unreadable") {
		t.Fatalf("want fail-closed block, got %+v", d)
	}
	// Personal repos stay open: their default is off.
	if d := g.Evaluate(bashReq("git commit -m x", personalRepo)); d.Block {
		t.Error("personal repo blocked when DB unreadable")
	}

	// DB opens but the schema is missing (queries fail): still closed.
	broken, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer broken.Close()
	g2 := gateFor(broken, time.Now())
	if d := g2.Evaluate(editReq(workRepo + "/a.go")); !d.Block {
		t.Error("work repo allowed with unreadable schema")
	}
}

func TestDaemonRunWithTaskIDAllowed(t *testing.T) {
	g := &Gate{
		// Even with no DB at all, STAYPOINT_TASK_ID passes.
		OpenDB:     func() (*sql.DB, func(), error) { return nil, nil, errors.New("unreachable") },
		IsWorkRepo: isWork,
	}
	req := editReq(workRepo + "/a.go")
	req.TaskID = "task-daemon"
	if d := g.Evaluate(req); d.Block {
		t.Errorf("daemon run blocked:\n%s", d.Reason)
	}
}

func approveOverride(t *testing.T, conn *sql.DB, company string, minutes int, decidedAt time.Time) string {
	t.Helper()
	gr, err := security.CreateGateRequest(conn, OverrideCmdline(company, minutes), []string{"test"}, OverrideRunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`UPDATE security_gate_requests SET status = 'approved', decided_at = ? WHERE id = ?`,
		decidedAt.UTC().Format(time.RFC3339Nano), gr.ID); err != nil {
		t.Fatal(err)
	}
	return gr.ID
}

func TestBoardOverrideAllowsUntilExpiry(t *testing.T) {
	conn := openStore(t)
	approved := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	approveOverride(t, conn, CompanyManagedSolution, 30, approved)

	if d := gateFor(conn, approved.Add(29*time.Minute)).Evaluate(editReq(workRepo + "/a.go")); d.Block {
		t.Errorf("blocked inside the override window:\n%s", d.Reason)
	}
	if d := gateFor(conn, approved.Add(31*time.Minute)).Evaluate(editReq(workRepo + "/a.go")); !d.Block {
		t.Error("allowed after the override expired")
	}
}

func TestOverrideRequiresApprovalAndMatchingCompany(t *testing.T) {
	conn := openStore(t)
	now := time.Now()
	// Pending (not approved by the Board): no effect.
	if _, err := security.CreateGateRequest(conn, OverrideCmdline(CompanyManagedSolution, 60), nil, OverrideRunID); err != nil {
		t.Fatal(err)
	}
	// Approved, but for another company.
	approveOverride(t, conn, "StayPoint", 60, now)
	// Approved Red-tier request with override-looking text but a different run_id.
	gr, _ := security.CreateGateRequest(conn, OverrideCmdline(CompanyManagedSolution, 60), nil, "sess-1")
	_, _ = security.DecideGateRequest(conn, gr.ID, true)

	if d := gateFor(conn, now).Evaluate(editReq(workRepo + "/a.go")); !d.Block {
		t.Error("allowed without a matching approved override")
	}
}

func TestParseOverrideCmdline(t *testing.T) {
	c, m, ok := ParseOverrideCmdline(OverrideCmdline("Managed Solution", 45))
	if !ok || c != "Managed Solution" || m != 45 {
		t.Fatalf("round trip = %q %d %v", c, m, ok)
	}
	for _, bad := range []string{
		"staypoint gate override --minutes 0 --company \"X\"",
		"staypoint gate override --minutes 9999 --company \"X\"",
		"git push origin main",
		OverrideCmdline("X", 10) + " && rm -rf /",
	} {
		if _, _, ok := ParseOverrideCmdline(bad); ok {
			t.Errorf("parsed %q", bad)
		}
	}
}

// Board decision 2026-10-06: agy never writes code in a work repo, even when
// attached, in a daemon run, or under a Board override.
func TestGeminiCodeWritesDeniedInWorkRepoEvenWhenAttached(t *testing.T) {
	conn := openStore(t)
	insertTask(t, conn, "task-1", workRepo, CompanyManagedSolution, "active")
	if err := Attach(conn, "conv-1", "task-1", ClientGemini, workRepo); err != nil {
		t.Fatal(err)
	}
	approveOverride(t, conn, CompanyManagedSolution, 60, time.Now())
	g := gateFor(conn, time.Now())
	for _, req := range []Request{
		{Client: ClientGemini, ToolName: "code_action", FilePaths: []string{workRepo + "/a.go"}, CWD: workRepo, SessionID: "conv-1"},
		{Client: ClientGemini, ToolName: "write_to_file", FilePaths: []string{workRepo + "/b.go"}, CWD: workRepo, SessionID: "conv-1", TaskID: "task-1"},
		{Client: ClientGemini, ToolName: "run_command", Command: "git commit -m x", CWD: workRepo, SessionID: "conv-1"},
		{Client: ClientGemini, ToolName: "run_command", Command: "echo x > main.go", CWD: workRepo, SessionID: "conv-1"},
	} {
		d := g.Evaluate(req)
		if !d.Block {
			t.Errorf("agy %s %q not denied", req.ToolName, req.Command)
			continue
		}
		if !strings.Contains(d.Reason, "claude --work") || !strings.Contains(d.Reason, "never writes code") {
			t.Errorf("agy denial message:\n%s", d.Reason)
		}
	}
	// Doc files fall through to the tracking rules: attached -> allowed.
	doc := Request{Client: ClientGemini, ToolName: "write_to_file", FilePaths: []string{workRepo + "/docs/notes.md"}, CWD: workRepo, SessionID: "conv-1"}
	if d := g.Evaluate(doc); d.Block {
		t.Errorf("attached agy doc write denied:\n%s", d.Reason)
	}
}

func TestGeminiToolsGated(t *testing.T) {
	g := gateFor(openStore(t), time.Now())
	// Unattached doc write in a work repo: tracking gate message for agy.
	d := g.Evaluate(Request{Client: ClientGemini, ToolName: "write_to_file", FilePaths: []string{workRepo + "/README.md"}, CWD: workRepo, SessionID: "conv-1"})
	if !d.Block || !strings.Contains(d.Reason, "--session conv-1 --client gemini") {
		t.Errorf("unattached agy doc write: %+v", d)
	}
	// Outside any git repo (personalRepo here has no .git): not repo code,
	// so the personal-repo Gemini code gate does not apply.
	if d := g.Evaluate(Request{Client: ClientGemini, ToolName: "code_action", FilePaths: []string{personalRepo + "/a.go"}, CWD: personalRepo}); d.Block {
		t.Errorf("agy write outside a git repo blocked:\n%s", d.Reason)
	}
	for _, req := range []Request{
		{Client: ClientGemini, ToolName: "view_file", FilePaths: []string{workRepo + "/a.go"}, CWD: workRepo},
		{Client: ClientGemini, ToolName: "run_command", Command: "git status", CWD: workRepo},
	} {
		if d := g.Evaluate(req); d.Block {
			t.Errorf("agy read-only %s blocked", req.ToolName)
		}
	}
}

// gitRepo makes a temp dir with a .git entry (a personal repo).
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func approveGeminiSession(t *testing.T, conn *sql.DB, session, repo string, decidedAt time.Time) {
	t.Helper()
	s := geminiapproval.Scope{SessionID: session, Repo: repo}
	if _, err := conn.Exec(`INSERT INTO security_gate_requests (id, cmdline, run_id, status, created_at, decided_at) VALUES (?, ?, ?, 'approved', ?, ?)`,
		"gc-"+session+"-"+filepath.Base(repo), s.Cmdline(), geminiapproval.RunID,
		decidedAt.UTC().Format(time.RFC3339Nano), decidedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
}

// Board addition (2026-10-06): interactive agy in a personal repo may not
// write code without a Touch ID approval for its conversationId.
func TestGeminiPersonalRepoCodeNeedsSessionApproval(t *testing.T) {
	conn := openStore(t)
	repo := gitRepo(t)
	now := time.Now()
	g := gateFor(conn, now)
	code := []Request{
		{Client: ClientGemini, ToolName: "write_to_file", FilePaths: []string{repo + "/main.go"}, CWD: repo, SessionID: "conv-7"},
		{Client: ClientGemini, ToolName: "replace_file_content", FilePaths: []string{repo + "/config.yaml"}, CWD: repo, SessionID: "conv-7"},
		{Client: ClientGemini, ToolName: "run_command", Command: "echo x > main.go", CWD: repo, SessionID: "conv-7"},
	}
	for _, req := range code {
		d := g.Evaluate(req)
		if !d.Block || !strings.Contains(d.Reason, "staypoint gate gemini-code --session conv-7") || !strings.Contains(d.Reason, "never writes code") {
			t.Errorf("unapproved %s: %+v", req.ToolName, d)
		}
	}
	// Non-code files pass without approval.
	for _, f := range []string{"README.md", "docs/guide.md", "deck.pptx"} {
		if d := g.Evaluate(Request{Client: ClientGemini, ToolName: "write_to_file", FilePaths: []string{filepath.Join(repo, f)}, CWD: repo, SessionID: "conv-7"}); d.Block {
			t.Errorf("doc write %s blocked:\n%s", f, d.Reason)
		}
	}
	// Daemon runs are guarded after each turn instead.
	if d := g.Evaluate(Request{Client: ClientGemini, ToolName: "write_to_file", FilePaths: []string{repo + "/main.go"}, CWD: repo, TaskID: "task-1"}); d.Block {
		t.Errorf("daemon-run write blocked by the interactive gate:\n%s", d.Reason)
	}
	// Claude is unaffected.
	if d := g.Evaluate(Request{Client: ClientClaude, ToolName: "Write", FilePaths: []string{repo + "/main.go"}, CWD: repo, SessionID: "s"}); d.Block {
		t.Errorf("claude write blocked:\n%s", d.Reason)
	}

	approveGeminiSession(t, conn, "conv-7", repo, now.Add(-time.Hour))
	for _, req := range code {
		if d := g.Evaluate(req); d.Block {
			t.Errorf("approved %s still blocked:\n%s", req.ToolName, d.Reason)
		}
	}
	// Another conversation is not covered.
	other := code[0]
	other.SessionID = "conv-8"
	if d := g.Evaluate(other); !d.Block {
		t.Error("approval leaked to another conversation")
	}
	// Expired after MaxSessionHours.
	late := gateFor(conn, now.Add((geminiapproval.MaxSessionHours+1)*time.Hour))
	if d := late.Evaluate(code[0]); !d.Block {
		t.Error("approval did not expire")
	}
}

func TestGeminiWorkRepoForgedApprovalStillDenied(t *testing.T) {
	conn := openStore(t)
	approveGeminiSession(t, conn, "conv-1", workRepo, time.Now())
	g := gateFor(conn, time.Now())
	d := g.Evaluate(Request{Client: ClientGemini, ToolName: "write_to_file", FilePaths: []string{workRepo + "/a.go"}, CWD: workRepo, SessionID: "conv-1"})
	if !d.Block || !strings.Contains(d.Reason, "never writes code") {
		t.Fatalf("work repo with forged approval: %+v", d)
	}
}

// task-7c9df5b6: in a work repo Gemini may read anything and write docs; code
// writes are denied with a reason that names the path. The doc rule matches
// the post-turn guard (geminiguard.IsDocPath) on the repo-relative path.
func TestGeminiWorkRepoDocRuleAndReason(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	g := &Gate{
		OpenDB:     func() (*sql.DB, func(), error) { return openStore(t), func() {}, nil },
		IsWorkRepo: func(p string) bool { return strings.HasPrefix(p, repo) },
	}
	write := func(rel string) Request {
		return Request{Client: ClientGemini, ToolName: "write_to_file", FilePaths: []string{filepath.Join(repo, rel)}, CWD: repo, SessionID: "conv-1", TaskID: "task-1"}
	}
	for _, rel := range []string{"README.md", "notes.txt", "docs/guide.md", "docs/diagram.png", "sub/CHANGELOG.rst"} {
		if d := g.Evaluate(write(rel)); d.Block {
			t.Errorf("doc write %s denied:\n%s", rel, d.Reason)
		}
	}
	for _, rel := range []string{"app.py", "requirements.txt", "docs/build.sh", ".github/workflows/ci.md", "Dockerfile"} {
		d := g.Evaluate(write(rel))
		want := "Gemini may not write code in work repos: " + filepath.Join(repo, rel)
		if !d.Block || !strings.HasPrefix(d.Reason, want) {
			t.Errorf("code write %s: block=%v reason=%q, want prefix %q", rel, d.Block, d.Reason, want)
		}
	}
	for _, req := range []Request{
		{Client: ClientGemini, ToolName: "view_file", FilePaths: []string{filepath.Join(repo, "app.py")}, CWD: repo},
		{Client: ClientGemini, ToolName: "list_dir", FilePaths: []string{repo}, CWD: repo},
		{Client: ClientGemini, ToolName: "grep_search", CWD: repo},
		{Client: ClientGemini, ToolName: "run_command", Command: "echo hi", CWD: repo},
		{Client: ClientGemini, ToolName: "run_command", Command: "cat app.py", CWD: repo},
	} {
		if d := g.Evaluate(req); d.Block {
			t.Errorf("read-only %s %q denied:\n%s", req.ToolName, req.Command, d.Reason)
		}
	}
}
