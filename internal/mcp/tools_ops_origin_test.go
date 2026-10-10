package mcp

import (
	"context"
	"strings"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/opstools"
)

// Board review #2 H1: an MCP server the harness did not start for this
// task (no ops data dir, STAYPOINT_TASK_ID unset or swapped, a missing or
// replayed run token) runs no ops tool at all.
func TestOpsToolsRefuseOutsideTheirRun(t *testing.T) {
	_, database := setupTestDB(t)
	own, err := meshContext.CreateTask(database, "own", t.TempDir(), "main", "personal")
	if err != nil {
		t.Fatal(err)
	}
	other, err := meshContext.CreateTask(database, "other", t.TempDir(), "main", "personal")
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	head := strings.Repeat("e", 40)
	calls := map[string]map[string]any{
		"dev_host_run":      {"host": "mansol-dev", "action": "restart", "service": "mansol-web"},
		"dev_deploy_verify": {"repo": "o/r", "sha": "abc1234", "page_checks": []string{"/a=b"}},
		"staypoint_query":   {"query": "task"},
		"task_comment":      {"text": "hi"},
		"task_doc":          {"key": "notes", "text": "hi"},
		"pr_body":           {"repo": repo, "pr": 9, "text": "x"},
		"pr_merge":          {"repo": repo, "pr": 5, "base": "dev-server"},
	}
	refusedAll := func(t *testing.T, s *Server, rec *recorder, want string) {
		t.Helper()
		for name, args := range calls {
			res := callOps(t, s, name, args)
			if !res.IsError || !strings.Contains(resultText(res), want) {
				t.Errorf("%s: err=%v %s (want %q)", name, res.IsError, resultText(res), want)
			}
		}
		if len(rec.cmds) != 0 {
			t.Errorf("refused calls ran processes: %+v", rec.cmds)
		}
	}
	newServer := func(t *testing.T, rec *recorder, opts ...Option) *Server {
		s := NewServer(append([]Option{WithDB(database), withRunner(rec.run), WithApprover(func(context.Context, ApprovalRequest) (bool, string) {
			return true, ""
		})}, opts...)...)
		t.Cleanup(func() { s.Close() })
		return s
	}

	// tokens stands in for the daemon's registry; check asks it, the way
	// staypoint mcp asks the daemon over HTTP.
	tokens := opstools.NewRunTokens()
	check := WithRunCheck(registryCheck(tokens))
	issue := func(t *testing.T, taskID string) string {
		t.Helper()
		tok, revoke, err := tokens.Issue(taskID, "run-"+taskID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(revoke)
		return tok
	}

	t.Run("no trusted data dir", func(t *testing.T) {
		cfg := opsConfig(t)
		t.Setenv("STAYPOINT_TASK_ID", own.ID)
		t.Setenv(opstools.RunTokenEnv, issue(t, own.ID))
		rec := &recorder{view: prViewJSON("o/r", 5, "dev-server", opstools.TaskBranch(own.ID), head, "OPEN")}
		refusedAll(t, newServer(t, rec, WithConfig(cfg), check), rec, "ops tools are off")
	})

	t.Run("no daemon to check with", func(t *testing.T) {
		cfg := opsConfig(t)
		t.Setenv("STAYPOINT_TASK_ID", own.ID)
		t.Setenv(opstools.RunTokenEnv, issue(t, own.ID))
		rec := &recorder{}
		refusedAll(t, newServer(t, rec, WithConfig(cfg), WithOpsDataDir(cfg.DataDir)), rec, "ops tools are off")
	})

	t.Run("STAYPOINT_TASK_ID unset", func(t *testing.T) {
		cfg := opsConfig(t)
		t.Setenv("STAYPOINT_TASK_ID", "")
		t.Setenv(opstools.RunTokenEnv, issue(t, own.ID))
		rec := &recorder{}
		refusedAll(t, newServer(t, rec, WithConfig(cfg), WithOpsDataDir(cfg.DataDir), check), rec, "no STAYPOINT_TASK_ID")
	})

	t.Run("no run token", func(t *testing.T) {
		cfg := opsConfig(t)
		issue(t, own.ID) // the run is live, but this server was not given its token
		t.Setenv("STAYPOINT_TASK_ID", own.ID)
		t.Setenv(opstools.RunTokenEnv, "")
		rec := &recorder{}
		refusedAll(t, newServer(t, rec, WithConfig(cfg), WithOpsDataDir(cfg.DataDir), check), rec, opstools.RunTokenEnv)
	})

	// Replay from a different task: the agent of `other` sets
	// STAYPOINT_TASK_ID to `own` but only holds its own token.
	t.Run("another task's token", func(t *testing.T) {
		cfg := opsConfig(t)
		issue(t, own.ID)
		t.Setenv("STAYPOINT_TASK_ID", own.ID)
		t.Setenv(opstools.RunTokenEnv, issue(t, other.ID))
		rec := &recorder{}
		refusedAll(t, newServer(t, rec, WithConfig(cfg), WithOpsDataDir(cfg.DataDir), check), rec, "does not belong to task "+own.ID)
	})

	// Board review #4: a token is good only while its run lasts.
	t.Run("token of a run that ended", func(t *testing.T) {
		cfg := opsConfig(t)
		tok, revoke, err := tokens.Issue(own.ID, "run-ended")
		if err != nil {
			t.Fatal(err)
		}
		revoke()
		t.Setenv("STAYPOINT_TASK_ID", own.ID)
		t.Setenv(opstools.RunTokenEnv, tok)
		rec := &recorder{}
		refusedAll(t, newServer(t, rec, WithConfig(cfg), WithOpsDataDir(cfg.DataDir), check), rec, "not a live run's token")
	})

	// Board review #4: a token no daemon issued (forged, or minted from a
	// key file the first version kept on disk) checks out for no task.
	t.Run("forged token", func(t *testing.T) {
		cfg := opsConfig(t)
		issue(t, own.ID)
		t.Setenv("STAYPOINT_TASK_ID", own.ID)
		t.Setenv(opstools.RunTokenEnv, strings.Repeat("ab", 32))
		rec := &recorder{}
		refusedAll(t, newServer(t, rec, WithConfig(cfg), WithOpsDataDir(cfg.DataDir), check), rec, "not a live run's token")
	})

	t.Run("non-ops tools are unaffected", func(t *testing.T) {
		t.Setenv("STAYPOINT_TASK_ID", "")
		t.Setenv(opstools.RunTokenEnv, "")
		s := newServer(t, &recorder{})
		if res := callOps(t, s, "staypoint_task_list", map[string]any{}); res.IsError {
			t.Fatalf("task list: %s", resultText(res))
		}
	})
}

// Board review #2 M1: a base retargeted to main while a dev merge waits is
// caught by the API read right before the merge, not only after it.
func TestPRMergeRechecksBaseBeforeMerge(t *testing.T) {
	repo := t.TempDir()
	head := strings.Repeat("c", 40)
	view := prViewJSON("o/r", 5, "dev-server", "feature", head, "OPEN")
	for name, live := range map[string]string{
		"base now main": "main " + head + " open",
		"head moved":    "dev-server " + strings.Repeat("f", 40) + " open",
		"closed":        "dev-server " + head + " closed",
		"unreadable":    "{}",
	} {
		t.Run(name, func(t *testing.T) {
			rec := &recorder{view: view, live: live}
			s := NewServer(opsRun(t, opsConfig(t)), withRunner(rec.run))
			defer s.Close()
			res := callOps(t, s, "pr_merge", map[string]any{"repo": repo, "pr": 5, "base": "dev-server"})
			if !res.IsError || rec.ran("gh", "merge") {
				t.Fatalf("merged despite live %q: %s", live, resultText(res))
			}
		})
	}
	// The read goes to the REST API of the repo the gate saw.
	rec := &recorder{views: []string{view, view, prViewJSON("o/r", 5, "dev-server", "feature", head, "MERGED")}}
	s := NewServer(opsRun(t, opsConfig(t)), withRunner(rec.run))
	defer s.Close()
	if res := callOps(t, s, "pr_merge", map[string]any{"repo": repo, "pr": 5, "base": "dev-server"}); res.IsError {
		t.Fatalf("dev merge: %s", resultText(res))
	}
	var order []string
	for _, c := range rec.cmds {
		if c.Name == "gh" {
			order = append(order, c.Args[0]+" "+c.Args[1])
		}
	}
	if got := strings.Join(order, ", "); got != "pr view, pr view, api repos/o/r/pulls/5, pr merge, pr view" {
		t.Fatalf("gh calls: %s", got)
	}
}
