package main

import (
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/config"
)

// task-7d279c9d: a shell command an ops MCP tool replaces is denied with a
// pointer to the tool, never held for the Board; other commands still reach
// the Board gate.
func TestPreToolHookRedirectsToOpsTools(t *testing.T) {
	oldCfg, oldConn := cfg, gateDaemonConn
	t.Cleanup(func() { cfg, gateDaemonConn = oldCfg, oldConn })
	t.Setenv("STAYPOINT_TASK_ID", "")
	cfg = config.DefaultConfig()
	cfg.Gates.Hosts = config.HostClasses{Dev: []string{"mansol-dev"}, Prod: []string{"mansol-prod"}}
	daemonAsked := false
	gateDaemonConn = func() (string, string) {
		daemonAsked = true
		return "", ""
	}

	run := func(cmd string) (string, string) {
		select {
		case out := <-runPreToolHook(t, cmd):
			return hookDecision(t, out)
		case <-time.After(15 * time.Second):
			t.Fatalf("hook held %q", cmd)
		}
		return "", ""
	}

	for cmd, tool := range map[string]string{
		"ssh -o ConnectTimeout=10 mansol-dev 'cd /var/www/mansol_apps && git pull --ff-only'": "dev_host_run",
		"cat ~/.staypoint/handoffs/task-1/notes.md":                                           "staypoint_query",
		"cat > /tmp/comment.md <<'EOF'\nDone.\nEOF":                                           "task_comment",
		"gh pr merge 42 --merge":                                                              "pr_merge",
	} {
		daemonAsked = false
		decision, reason := run(cmd)
		if decision != "block" || !strings.Contains(reason, "mcp__staypoint__"+tool) || !strings.HasPrefix(reason, "denied, not run") {
			t.Errorf("%q: decision=%q reason=%q, want deny pointing at %s", cmd, decision, reason, tool)
		}
		if daemonAsked {
			t.Errorf("%q reached the Board gate", cmd)
		}
	}

	// ssh to a prod host is not redirected: it still goes to the Board gate
	// (here unreachable, so blocked fail-closed).
	daemonAsked = false
	decision, reason := run("ssh mansol-prod 'sudo systemctl restart web'")
	if decision != "block" || strings.Contains(reason, "dev_host_run") || !daemonAsked {
		t.Fatalf("prod ssh: decision=%q reason=%q daemonAsked=%v", decision, reason, daemonAsked)
	}
}
