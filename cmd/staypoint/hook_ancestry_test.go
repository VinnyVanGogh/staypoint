package main

import "testing"

func TestNestedAgentUnderDaemon(t *testing.T) {
	type row = struct {
		ppid int
		name string
	}
	table := map[int]row{
		10: {1, "staypointd"},
		20: {10, "claude"},  // the run's agent
		30: {20, "zsh"},     // its Bash tool
		40: {30, "python3"}, // os.environ.pop + os.system
		50: {40, "claude"},  // nested agent, no task ID
		60: {10, "claude"},  // daemon helper started directly by staypointd
		70: {1, "iTerm2"},
		80: {70, "claude"}, // interactive session
		90: {80, "claude"}, // nested under an interactive session
	}
	cases := []struct {
		pid  int
		want bool
	}{
		{50, true},  // nested agent under a run
		{40, false}, // only the run's agent above it
		{20, false}, // the run's agent itself
		{60, false}, // daemon helper: one agent CLI
		{80, false}, // interactive, not under the daemon
		{90, false}, // nested but not under the daemon
		{999, false},
	}
	for _, c := range cases {
		if got := nestedAgentUnderDaemon(table, c.pid); got != c.want {
			t.Errorf("pid %d: got %v, want %v", c.pid, got, c.want)
		}
	}
}
