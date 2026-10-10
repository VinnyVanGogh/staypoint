package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/config"
	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/opstools"
)

// recorder is a fake opstools.Runner that records every command and answers
// gh pr view with view.
type recorder struct {
	mu   sync.Mutex
	cmds []opstools.Cmd
	view string
	// views, when set, answer successive gh pr view calls (the last repeats).
	views []string
	// origin answers git remote get-url origin.
	origin string
	// live answers pr_merge's gh api read ("base head state"); "" derives
	// it from the last gh pr view served.
	live     string
	lastView string
}

func (r *recorder) run(ctx context.Context, c opstools.Cmd) opstools.Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, c)
	if c.Name == "gh" && len(c.Args) > 1 && c.Args[1] == "view" {
		v := r.view
		if len(r.views) > 0 {
			v = r.views[0]
			if len(r.views) > 1 {
				r.views = r.views[1:]
			}
		}
		r.lastView = v
		return opstools.Result{Stdout: v, Output: v}
	}
	if c.Name == "gh" && len(c.Args) > 1 && c.Args[0] == "api" && strings.Contains(c.Args[1], "/pulls/") {
		out := r.live
		if out == "" {
			var v opstools.PRView
			_ = json.Unmarshal([]byte(r.lastView), &v)
			out = v.BaseRefName + " " + v.HeadRefOid + " " + strings.ToLower(v.State)
		}
		return opstools.Result{Stdout: out + "\n", Output: out + "\n"}
	}
	if c.Name == "git" && strings.Join(c.Args, " ") == "remote get-url origin" {
		return opstools.Result{Stdout: r.origin + "\n", Output: r.origin + "\n"}
	}
	return opstools.Result{Output: "SECRET_KEY=fixture-leak-123\nok\n"}
}

func (r *recorder) ran(name, sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.cmds {
		if c.Name == name && len(c.Args) > 1 && c.Args[1] == sub {
			return true
		}
	}
	return false
}

// withRunner replaces how ops tools run processes.
func withRunner(r opstools.Runner) Option {
	return func(s *Server) { s.runner = r }
}

func opsConfig(t *testing.T) *config.Config {
	cfg := config.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.Gates.Hosts = config.HostClasses{Dev: []string{"mansol-dev"}}
	cfg.Gates.Ops.DevHosts = map[string]config.DevHostConfig{
		"mansol-dev": {HostName: "10.0.0.5", SSHConfig: filepath.Join(cfg.DataDir, "ssh_config"), AppDir: "/var/www/mansol_apps", Services: []string{"mansol-web"}},
	}
	cfg.Gates.Ops.OwnRepos = []string{"o/r"}
	return cfg
}

// opsRun configures the server with cfg and makes this test a harness run
// of STAYPOINT_TASK_ID (task-ops-test when unset): it issues the run token
// the ops tools check (Board review #2 H1).
func opsRun(t *testing.T, cfg *config.Config) Option {
	t.Helper()
	taskID := os.Getenv("STAYPOINT_TASK_ID")
	if taskID == "" {
		taskID = "task-ops-test"
		t.Setenv("STAYPOINT_TASK_ID", taskID)
	}
	tokens := opstools.NewRunTokens()
	tok, revoke, err := tokens.Issue(taskID, "run-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(revoke)
	t.Setenv(opstools.RunTokenEnv, tok)
	return func(s *Server) {
		WithConfig(cfg)(s)
		WithOpsDataDir(cfg.DataDir)(s)
		WithRunCheck(registryCheck(tokens))(s)
	}
}

// registryCheck checks tokens against r in-process, the way the daemon's
// /api/ops/run-token/check does.
func registryCheck(r *opstools.RunTokens) RunChecker {
	return func(_ context.Context, taskID, token string) error {
		_, err := r.Check(taskID, token)
		return err
	}
}

func callOps(t *testing.T, s *Server, name string, args any) ToolCallResult {
	t.Helper()
	raw, _ := json.Marshal(args)
	params, _ := json.Marshal(CallToolParams{Name: name, Arguments: raw})
	resp := sendRequest(t, s, Request{JSONRPC: "2.0", ID: makeRawID(1), Method: "tools/call", Params: params})
	return parseToolCallResult(t, resp)
}

func resultText(r ToolCallResult) string {
	if len(r.Content) == 0 {
		return ""
	}
	return r.Content[0].Text
}

func TestOpsToolsListed(t *testing.T) {
	s := NewServer()
	defer s.Close()
	found := map[string]bool{}
	for _, tool := range s.getToolsList() {
		found[tool.Name] = true
	}
	for _, name := range []string{"dev_host_run", "dev_deploy_verify", "staypoint_query", "task_comment", "task_doc", "pr_body", "pr_merge"} {
		if !found[name] {
			t.Errorf("tools/list lacks %s", name)
		}
	}
}

func TestDevHostRunValidatesAndRedacts(t *testing.T) {
	rec := &recorder{}
	s := NewServer(opsRun(t, opsConfig(t)), withRunner(rec.run))
	defer s.Close()

	for _, bad := range []map[string]any{
		{"host": "mansol-prod", "action": "git_status"},
		{"host": "mansol-dev", "action": "cat_file", "path": "../../etc/passwd"},
		{"host": "mansol-dev", "action": "restart", "service": "sshd"},
		{"host": "mansol-dev", "action": "git_status", "command": "rm -rf /"},
	} {
		if res := callOps(t, s, "dev_host_run", bad); !res.IsError {
			t.Errorf("dev_host_run %v ran: %s", bad, resultText(res))
		}
	}
	if len(rec.cmds) != 0 {
		t.Fatalf("refused calls reached ssh: %+v", rec.cmds)
	}

	res := callOps(t, s, "dev_host_run", map[string]any{"host": "mansol-dev", "action": "restart", "service": "mansol-web"})
	text := resultText(res)
	if res.IsError || !strings.Contains(text, "effect=dev_write") || strings.Contains(text, "fixture-leak") {
		t.Fatalf("restart result: %s", text)
	}
	if !rec.ran("ssh", "-F") {
		t.Fatalf("restart did not run ssh: %+v", rec.cmds)
	}
}

func TestPRMergeGatesOnRealBase(t *testing.T) {
	repo := t.TempDir()
	head := strings.Repeat("c", 40)
	view := func(base string) string {
		return prViewJSON("o/r", 5, base, "feature", head, "OPEN")
	}

	t.Run("main without approver is refused", func(t *testing.T) {
		rec := &recorder{view: view("main")}
		s := NewServer(opsRun(t, opsConfig(t)), withRunner(rec.run))
		defer s.Close()
		res := callOps(t, s, "pr_merge", map[string]any{"repo": repo, "pr": 5, "base": "main"})
		if !res.IsError || !strings.Contains(resultText(res), "prod_write") || rec.ran("gh", "merge") {
			t.Fatalf("prod merge without Board: err=%v ran=%v %s", res.IsError, rec.ran("gh", "merge"), resultText(res))
		}
	})

	t.Run("main asks the Board and merges only when approved", func(t *testing.T) {
		for _, approve := range []bool{false, true} {
			rec := &recorder{views: []string{view("main"), view("main"), prViewJSON("o/r", 5, "main", "feature", head, "MERGED")}}
			var asked []ApprovalRequest
			approver := func(ctx context.Context, req ApprovalRequest) (bool, string) {
				asked = append(asked, req)
				return approve, "Board denied"
			}
			s := NewServer(opsRun(t, opsConfig(t)), withRunner(rec.run), WithApprover(approver))
			res := callOps(t, s, "pr_merge", map[string]any{"repo": repo, "pr": 5, "base": "main"})
			s.Close()
			if len(asked) != 1 || asked[0].Call.Effect != opstools.ProdWrite || !strings.Contains(asked[0].Reason, "night rule") || !strings.Contains(asked[0].Reason, "GitHub repo o/r") {
				t.Fatalf("approver asked %+v", asked)
			}
			if !strings.Contains(asked[0].Call.Canonical(), "head="+head) || !strings.Contains(asked[0].Call.Canonical(), "gh_repo=o/r") {
				t.Fatalf("canonical not pinned to repo and head: %s", asked[0].Call.Canonical())
			}
			if rec.ran("gh", "merge") != approve || res.IsError == approve {
				t.Fatalf("approve=%v merged=%v isError=%v %s", approve, rec.ran("gh", "merge"), res.IsError, resultText(res))
			}
		}
	})

	t.Run("dev-server merges unattended", func(t *testing.T) {
		rec := &recorder{views: []string{view("dev-server"), view("dev-server"), prViewJSON("o/r", 5, "dev-server", "feature", head, "MERGED")}}
		called := false
		s := NewServer(opsRun(t, opsConfig(t)), withRunner(rec.run), WithApprover(func(context.Context, ApprovalRequest) (bool, string) {
			called = true
			return false, "no"
		}))
		defer s.Close()
		res := callOps(t, s, "pr_merge", map[string]any{"repo": repo, "pr": 5, "base": "dev-server"})
		if res.IsError || called || !rec.ran("gh", "merge") || !strings.Contains(resultText(res), "effect=dev_write") {
			t.Fatalf("dev merge: err=%v called=%v %s", res.IsError, called, resultText(res))
		}
	})

	t.Run("declaring dev-server for a main PR is refused", func(t *testing.T) {
		rec := &recorder{view: view("main")}
		s := NewServer(opsRun(t, opsConfig(t)), withRunner(rec.run))
		defer s.Close()
		res := callOps(t, s, "pr_merge", map[string]any{"repo": repo, "pr": 5, "base": "dev-server"})
		if !res.IsError || rec.ran("gh", "merge") {
			t.Fatalf("base lie merged: %s", resultText(res))
		}
	})

	t.Run("remote repointed after approval is refused", func(t *testing.T) {
		// The Board approved o/r#5; by merge time the checkout's remote
		// resolves PR #5 in another repo.
		rec := &recorder{views: []string{view("main"), prViewJSON("attacker/r", 5, "main", "feature", head, "OPEN")}}
		s := NewServer(opsRun(t, opsConfig(t)), withRunner(rec.run), WithApprover(func(context.Context, ApprovalRequest) (bool, string) { return true, "" }))
		defer s.Close()
		res := callOps(t, s, "pr_merge", map[string]any{"repo": repo, "pr": 5, "base": "main"})
		if !res.IsError || rec.ran("gh", "merge") || !strings.Contains(resultText(res), "changed while held") {
			t.Fatalf("repointed remote merged: %s", resultText(res))
		}
	})

	t.Run("merge targets the approved GitHub repo explicitly", func(t *testing.T) {
		rec := &recorder{views: []string{view("dev-server"), view("dev-server"), prViewJSON("o/r", 5, "dev-server", "feature", head, "MERGED")}}
		s := NewServer(opsRun(t, opsConfig(t)), withRunner(rec.run))
		defer s.Close()
		if res := callOps(t, s, "pr_merge", map[string]any{"repo": repo, "pr": 5, "base": "dev-server"}); res.IsError {
			t.Fatalf("dev merge: %s", resultText(res))
		}
		var merge []string
		for _, c := range rec.cmds {
			if c.Name == "gh" && c.Args[1] == "merge" {
				merge = c.Args
			}
		}
		if got := strings.Join(merge, " "); got != "pr merge 5 --repo o/r --merge --match-head-commit "+head {
			t.Fatalf("merge argv %q", got)
		}
	})

	t.Run("base swapped during the merge is reported", func(t *testing.T) {
		rec := &recorder{views: []string{view("dev-server"), view("dev-server"), prViewJSON("o/r", 5, "main", "feature", head, "MERGED")}}
		s := NewServer(opsRun(t, opsConfig(t)), withRunner(rec.run))
		defer s.Close()
		res := callOps(t, s, "pr_merge", map[string]any{"repo": repo, "pr": 5, "base": "dev-server"})
		if !res.IsError || !strings.Contains(resultText(res), "base changed") {
			t.Fatalf("base swap not reported: %s", resultText(res))
		}
	})
}

func prViewJSON(ghRepo string, pr int, base, headRef, head, state string) string {
	return fmt.Sprintf(`{"url":"https://github.com/%s/pull/%d","baseRefName":%q,"headRefName":%q,"headRefOid":%q,"state":%q}`, ghRepo, pr, base, headRef, head, state)
}

func TestStaypointQueryOwnTaskOnly(t *testing.T) {
	_, database := setupTestDB(t)
	own, err := meshContext.CreateTask(database, "own", "/tmp/repo", "main", "personal")
	if err != nil {
		t.Fatal(err)
	}
	other, err := meshContext.CreateTask(database, "other", "/tmp/repo", "main", "personal")
	if err != nil {
		t.Fatal(err)
	}
	cfg := opsConfig(t)
	hdir := filepath.Join(cfg.DataDir, "handoffs", own.ID)
	if err := os.MkdirAll(hdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hdir, "plan.md"), []byte("plan\nAPI_KEY=fixture-handoff-key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hosts", filepath.Join(hdir, "link.md")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STAYPOINT_TASK_ID", own.ID)
	s := NewServer(WithDB(database), opsRun(t, cfg))
	defer s.Close()

	if res := callOps(t, s, "staypoint_query", map[string]any{"query": "task", "task_id": other.ID}); !res.IsError {
		t.Fatalf("read another task: %s", resultText(res))
	}
	if res := callOps(t, s, "staypoint_query", map[string]any{"query": "select * from tasks"}); !res.IsError {
		t.Fatal("free-form query accepted")
	}
	for _, name := range []string{"../other/plan.md", "link.md", ".hidden"} {
		if res := callOps(t, s, "staypoint_query", map[string]any{"query": "handoff", "name": name}); !res.IsError {
			t.Errorf("handoff %q read: %s", name, resultText(res))
		}
	}
	res := callOps(t, s, "staypoint_query", map[string]any{"query": "handoff", "name": "plan.md"})
	if res.IsError || !strings.Contains(resultText(res), "plan") || strings.Contains(resultText(res), "fixture-handoff-key") {
		t.Fatalf("own handoff: %s", resultText(res))
	}
	res = callOps(t, s, "staypoint_query", map[string]any{"query": "handoffs"})
	if res.IsError || !strings.Contains(resultText(res), "plan.md") || strings.Contains(resultText(res), "link.md") {
		t.Fatalf("handoffs list: %s", resultText(res))
	}
	if res := callOps(t, s, "staypoint_query", map[string]any{"query": "gate_requests"}); res.IsError {
		t.Fatalf("gate_requests: %s", resultText(res))
	}
}

func TestTaskCommentAndDoc(t *testing.T) {
	_, database := setupTestDB(t)
	own, err := meshContext.CreateTask(database, "own", "/tmp/repo", "main", "personal")
	if err != nil {
		t.Fatal(err)
	}
	other, err := meshContext.CreateTask(database, "other", "/tmp/repo", "main", "personal")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("STAYPOINT_TASK_ID", own.ID)
	s := NewServer(WithDB(database), opsRun(t, opsConfig(t)))
	defer s.Close()

	// A card waiting for the Board that a Board comment would supersede.
	if _, err := database.Exec(`INSERT INTO task_interactions (task_id, interaction_kind, payload, status, idempotency_key, supersede_on_comment)
		VALUES (?, 'ask_user_questions', '{}', 'pending', 'k1', 1)`, own.ID); err != nil {
		t.Fatal(err)
	}

	text := "Deploy notes with `backticks`, $(not run) and 'quotes'\n- item"
	if res := callOps(t, s, "task_comment", map[string]any{"text": text}); res.IsError {
		t.Fatalf("comment: %s", resultText(res))
	}
	comments, _ := meshContext.GetTaskComments(database, own.ID)
	if len(comments) != 1 || comments[0].Message != text || comments[0].Author != meshContext.AgentCommentAuthor {
		t.Fatalf("comment stored as %+v", comments)
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM task_interactions WHERE task_id = ?`, own.ID).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("agent comment superseded a Board card: status=%q err=%v", status, err)
	}
	if res := callOps(t, s, "task_comment", map[string]any{"text": "x", "task_id": other.ID}); !res.IsError {
		t.Fatal("commented on another task")
	}
	if res := callOps(t, s, "task_comment", map[string]any{"text": "  "}); !res.IsError {
		t.Fatal("empty comment accepted")
	}
	if res := callOps(t, s, "task_doc", map[string]any{"key": "../plan", "text": "x"}); !res.IsError {
		t.Fatal("bad doc key accepted")
	}
	if res := callOps(t, s, "task_doc", map[string]any{"key": "plan", "text": "v1"}); res.IsError {
		t.Fatalf("doc: %s", resultText(res))
	}
	d, err := meshContext.GetLatestTaskDocument(database, own.ID, "plan")
	if err != nil || d == nil || d.Content != "v1" {
		t.Fatalf("doc stored as %+v %v", d, err)
	}
}

func TestPRBodyScopedToTaskRepo(t *testing.T) {
	_, database := setupTestDB(t)
	taskRepo := t.TempDir()
	own, err := meshContext.CreateTask(database, "own", taskRepo, "main", "personal")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("STAYPOINT_TASK_ID", own.ID)
	head := strings.Repeat("d", 40)
	repo := t.TempDir()
	body := "## Summary\n$(touch /tmp/pwned) `x`"
	edits := func(rec *recorder) []opstools.Cmd {
		var out []opstools.Cmd
		for _, c := range rec.cmds {
			if c.Name == "gh" && c.Args[1] == "edit" {
				out = append(out, c)
			}
		}
		return out
	}

	t.Run("own PR runs unattended, text on stdin", func(t *testing.T) {
		rec := &recorder{view: prViewJSON("o/r", 9, "main", opstools.TaskBranch(own.ID), head, "OPEN"), origin: "git@github.com:o/r.git"}
		asked := false
		s := NewServer(WithDB(database), opsRun(t, opsConfig(t)), withRunner(rec.run), WithApprover(func(context.Context, ApprovalRequest) (bool, string) {
			asked = true
			return false, "no"
		}))
		defer s.Close()
		res := callOps(t, s, "pr_body", map[string]any{"repo": repo, "pr": 9, "text": body})
		e := edits(rec)
		if res.IsError || asked || len(e) != 1 || !strings.Contains(resultText(res), "effect=dev_write") {
			t.Fatalf("pr_body: asked=%v %s", asked, resultText(res))
		}
		if strings.Join(e[0].Args, " ") != "pr edit 9 --repo o/r --body-file -" || string(e[0].Stdin) != body || e[0].Dir != repo {
			t.Fatalf("pr_body ran %+v", e[0])
		}
		// The task's repo comes from its configured checkout, not the agent's dir.
		var originDir string
		for _, c := range rec.cmds {
			if c.Name == "git" {
				originDir = c.Dir
			}
		}
		if originDir != taskRepo {
			t.Fatalf("task repo resolved from %q, want %q", originDir, taskRepo)
		}
	})

	for _, c := range []struct{ name, view, origin string }{
		{"another repo", prViewJSON("other/r", 9, "main", "feature", head, "OPEN"), "https://github.com/o/r.git"},
		{"release PR from main", prViewJSON("o/r", 9, "prod", "main", head, "OPEN"), "https://github.com/o/r.git"},
		// Board review #2 #7/L1: only this task's own PR is a dev write.
		{"another task's PR in the task repo", prViewJSON("o/r", 9, "main", "staypoint/task-other", head, "OPEN"), "https://github.com/o/r.git"},
		{"feature PR in the task repo", prViewJSON("o/r", 9, "main", "feature", head, "OPEN"), "https://github.com/o/r.git"},
		// The agent repointed the shared checkout's origin at its own repo.
		{"repointed task origin", prViewJSON("attacker/r", 9, "main", "feature", head, "OPEN"), "https://github.com/attacker/r.git"},
	} {
		t.Run(c.name+" needs the Board", func(t *testing.T) {
			for _, approve := range []bool{false, true} {
				rec := &recorder{view: c.view, origin: c.origin}
				var asked []ApprovalRequest
				s := NewServer(WithDB(database), opsRun(t, opsConfig(t)), withRunner(rec.run), WithApprover(func(_ context.Context, req ApprovalRequest) (bool, string) {
					asked = append(asked, req)
					return approve, "Board denied"
				}))
				res := callOps(t, s, "pr_body", map[string]any{"repo": repo, "pr": 9, "text": body})
				s.Close()
				if len(asked) != 1 || asked[0].Call.Effect != opstools.ExternalWrite || !strings.Contains(asked[0].Call.Canonical(), "text_sha256=") {
					t.Fatalf("approver asked %+v", asked)
				}
				if (len(edits(rec)) == 1) != approve || res.IsError == approve {
					t.Fatalf("approve=%v edits=%d %s", approve, len(edits(rec)), resultText(res))
				}
			}
		})
	}

	t.Run("no task repo is external", func(t *testing.T) {
		t.Setenv("STAYPOINT_TASK_ID", "")
		rec := &recorder{view: prViewJSON("o/r", 9, "main", "feature", head, "OPEN")}
		s := NewServer(opsRun(t, opsConfig(t)), withRunner(rec.run))
		defer s.Close()
		if res := callOps(t, s, "pr_body", map[string]any{"repo": repo, "pr": 9, "text": body}); !res.IsError || len(edits(rec)) != 0 {
			t.Fatalf("pr_body without a task ran: %s", resultText(res))
		}
	})
}
