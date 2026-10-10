package orchestrator

import "testing"

// task-7d279c9d: ops tool calls show their declared effect in the timeline.
func TestOpsToolTitleShowsEffect(t *testing.T) {
	cases := map[string][2]string{
		"restart": {"mcp__staypoint__dev_host_run", `{"host":"mansol-dev","action":"restart","service":"mansol-web"}`},
		"read":    {"mcp__staypoint__dev_host_run", `{"host":"mansol-dev","action":"git_status"}`},
		"prod":    {"mcp__staypoint__pr_merge", `{"pr":12,"base":"main"}`},
		"dev":     {"mcp__staypoint__pr_merge", `{"pr":12,"base":"dev-server"}`},
		"query":   {"mcp__staypoint__staypoint_query", `{"query":"handoffs"}`},
	}
	want := map[string]string{
		"restart": "dev_host_run [dev_write] host=mansol-dev action=restart service=mansol-web",
		"read":    "dev_host_run [read] host=mansol-dev action=git_status",
		"prod":    "pr_merge [prod_write] pr=12 base=main",
		"dev":     "pr_merge [dev_write] pr=12 base=dev-server",
		"query":   "staypoint_query [read] query=handoffs",
	}
	for k, c := range cases {
		title, cmd := extractToolMeta(c[0], c[1], "")
		if title != want[k] || cmd != "" {
			t.Errorf("%s: title %q cmd %q, want %q", k, title, cmd, want[k])
		}
	}
	// Other staypoint tools keep their old titles.
	if title, _ := extractToolMeta("mcp__staypoint__staypoint_status", `{}`, ""); title != "mcp__staypoint__staypoint_status" {
		t.Errorf("non-ops tool title %q", title)
	}
}
