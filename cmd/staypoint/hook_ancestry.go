package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Nested-agent guard (task-859a5234). A daemon-run agent can start another
// agent CLI with STAYPOINT_TASK_ID dropped (os.environ.pop, env={}, tmux …),
// and that child's hook would then skip the Board rules and the edit gate.
// Regexes cannot stop every way of dropping an env var, so the hook looks at
// its own process ancestry instead: under staypointd, a chain with two agent
// CLIs (the run's agent and one it started) and no task ID is a nested agent,
// and every tool call it makes is refused. Daemon helpers that staypointd
// starts directly (one agent CLI in the chain) are not affected.

var agentCLINames = map[string]bool{
	"claude": true, "gemini": true, "agy": true, "codex": true,
	"cursor-agent": true, "aider": true,
}

// psTable returns pid -> (ppid, command basename) for every process, or nil
// when ps fails (the guard then does nothing: it is an extra layer).
func psTable() map[int]struct {
	ppid int
	name string
} {
	out, err := exec.Command("/bin/ps", "-A", "-o", "pid=,ppid=,comm=").Output()
	if err != nil {
		return nil
	}
	t := map[int]struct {
		ppid int
		name string
	}{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		t[pid] = struct {
			ppid int
			name string
		}{ppid, filepath.Base(strings.Join(f[2:], " "))}
	}
	return t
}

// nestedAgentUnderDaemon reports whether pid's ancestry contains staypointd
// with at least two agent CLIs below it.
func nestedAgentUnderDaemon(table map[int]struct {
	ppid int
	name string
}, pid int) bool {
	agents := 0
	for i := 0; i < 128 && pid > 1; i++ {
		p, ok := table[pid]
		if !ok {
			return false
		}
		switch {
		case p.name == "staypointd":
			return agents >= 2
		case agentCLINames[p.name]:
			agents++
		}
		pid = p.ppid
	}
	return false
}

// orphanedNestedAgent is the hook's check: no task ID in its own env, but its
// ancestry is a nested agent under the daemon.
func orphanedNestedAgent() bool {
	if os.Getenv("STAYPOINT_TASK_ID") != "" {
		return false
	}
	t := psTable()
	return t != nil && nestedAgentUnderDaemon(t, os.Getppid())
}

const nestedAgentReason = "StayPoint: this agent was started inside a StayPoint run without the run's task ID (a nested agent, or STAYPOINT_TASK_ID was dropped). Its tool calls are refused; do the work in the parent run instead."
