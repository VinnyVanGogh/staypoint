package mcp

import (
	"context"
	"encoding/json"
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
}

func (r *recorder) run(ctx context.Context, c opstools.Cmd) opstools.Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, c)
	if c.Name == "gh" && len(c.Args) > 1 && c.Args[1] == "view" {
		return opstools.Result{Stdout: r.view, Output: r.view}
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
		"mansol-dev": {AppDir: "/var/www/mansol_apps", Services: []string{"mansol-web"}},
	}
	return cfg
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
	s := NewServer(WithConfig(opsConfig(t)), withRunner(rec.run))
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
	if !rec.ran("ssh", "-o") {
		t.Fatalf("restart did not run ssh: %+v", rec.cmds)
	}
}

func TestPRMergeGatesOnRealBase(t *testing.T) {
	repo := t.TempDir()
	head := strings.Repeat("c", 40)
	view := func(base string) string {
		return `{"baseRefName":"` + base + `","headRefOid":"` + head + `","state":"OPEN"}`
	}

	t.Run("main without approver is refused", func(t *testing.T) {
		rec := &recorder{view: view("main")}
		s := NewServer(WithConfig(opsConfig(t)), withRunner(rec.run))
		defer s.Close()
		res := callOps(t, s, "pr_merge", map[string]any{"repo": repo, "pr": 5, "base": "main"})
		if !res.IsError || !strings.Contains(resultText(res), "prod_write") || rec.ran("gh", "merge") {
			t.Fatalf("prod merge without Board: err=%v ran=%v %s", res.IsError, rec.ran("gh", "merge"), resultText(res))
		}
	})

	t.Run("main asks the Board and merges only when approved", func(t *testing.T) {
		for _, approve := range []bool{false, true} {
			rec := &recorder{view: view("main")}
			var asked []ApprovalRequest
			approver := func(ctx context.Context, req ApprovalRequest) (bool, string) {
				asked = append(asked, req)
				return approve, "Board denied"
			}
			s := NewServer(WithConfig(opsConfig(t)), withRunner(rec.run), WithApprover(approver))
			res := callOps(t, s, "pr_merge", map[string]any{"repo": repo, "pr": 5, "base": "main"})
			s.Close()
			if len(asked) != 1 || asked[0].Call.Effect != opstools.ProdWrite || !strings.Contains(asked[0].Reason, "night rule") {
				t.Fatalf("approver asked %+v", asked)
			}
			if !strings.Contains(asked[0].Call.Canonical(), "head="+head) {
				t.Fatalf("canonical not pinned to head: %s", asked[0].Call.Canonical())
			}
			if rec.ran("gh", "merge") != approve || res.IsError == approve {
				t.Fatalf("approve=%v merged=%v isError=%v %s", approve, rec.ran("gh", "merge"), res.IsError, resultText(res))
			}
		}
	})

	t.Run("dev-server merges unattended", func(t *testing.T) {
		rec := &recorder{view: view("dev-server")}
		called := false
		s := NewServer(WithConfig(opsConfig(t)), withRunner(rec.run), WithApprover(func(context.Context, ApprovalRequest) (bool, string) {
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
		s := NewServer(WithConfig(opsConfig(t)), withRunner(rec.run))
		defer s.Close()
		res := callOps(t, s, "pr_merge", map[string]any{"repo": repo, "pr": 5, "base": "dev-server"})
		if !res.IsError || rec.ran("gh", "merge") {
			t.Fatalf("base lie merged: %s", resultText(res))
		}
	})
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
	s := NewServer(WithDB(database), WithConfig(cfg))
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
	s := NewServer(WithDB(database), WithConfig(opsConfig(t)))
	defer s.Close()

	text := "Deploy notes with `backticks`, $(not run) and 'quotes'\n- item"
	if res := callOps(t, s, "task_comment", map[string]any{"text": text}); res.IsError {
		t.Fatalf("comment: %s", resultText(res))
	}
	comments, _ := meshContext.GetTaskComments(database, own.ID)
	if len(comments) != 1 || comments[0].Message != text || comments[0].Author != "agent" {
		t.Fatalf("comment stored as %+v", comments)
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

func TestPRBodyPassesTextOnStdin(t *testing.T) {
	rec := &recorder{}
	repo := t.TempDir()
	s := NewServer(WithConfig(opsConfig(t)), withRunner(rec.run))
	defer s.Close()
	body := "## Summary\n$(touch /tmp/pwned) `x`"
	res := callOps(t, s, "pr_body", map[string]any{"repo": repo, "pr": 9, "text": body})
	if res.IsError || len(rec.cmds) != 1 {
		t.Fatalf("pr_body: %s", resultText(res))
	}
	c := rec.cmds[0]
	if c.Name != "gh" || strings.Join(c.Args, " ") != "pr edit 9 --body-file -" || string(c.Stdin) != body || c.Dir != repo {
		t.Fatalf("pr_body ran %+v", c)
	}
}
