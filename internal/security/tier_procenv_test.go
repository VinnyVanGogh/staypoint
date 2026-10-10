package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Board review #4 (task-7d279c9d): every bypass the review listed, plus the
// everyday commands that must stay below Red.
func TestBoardReview4Bypasses(t *testing.T) {
	home := t.TempDir()
	dd := filepath.Join(home, ".staypoint")
	if err := os.MkdirAll(dd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dd, "auth_token"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A stand-in staypoint binary and links to it.
	bin := filepath.Join(t.TempDir(), "staypoint")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	links := t.TempDir()
	sym, hard := filepath.Join(links, "sp2"), filepath.Join(links, "sp3")
	if err := os.Symlink(bin, sym); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(bin, hard); err != nil {
		t.Fatal(err)
	}
	c := &Classifier{Home: home, StaypointBins: []string{bin}}

	red := map[string]string{
		// Paths to the data dir the string match missed.
		"cat ~/.st*/auth_token":             "sensitive path",
		"cat ~/.staypoin?/auth_token":       "sensitive path",
		"cat $HOME/.STAYPOINT/auth_token":   "sensitive path",
		"cat ~/.StayPoint/auth_token":       "sensitive path",
		"cd ~ && cat .staypoint/auth_token": "sensitive path",
		"cd $HOME; cat .staypoint/x":        "sensitive path",
		"d=~/.staypoint; cat $d/auth_token": "sensitive path",
		"cp -r ~ /tmp/h":                    "whole home",
		"tar cf - -C ~ .staypoint":          "whole home",
		"find ~ -name x -exec cat {} +":     "whole home",
		"find / -name x -exec cat {} \\;":   "whole home",
		"cat /proc/1234/environ":            "process environment",
		"cat /proc/self/environ":            "process environment",
		// Another process's environment (its run token).
		"ps -Eww":      "ps -E",
		"ps -E -p 123": "ps -E",
		"ps -axE":      "ps -E",
		"ps eww 12":    "e modifier",
		"ps auxe":      "e modifier",
		// The daemon's memory, where run tokens live.
		"lldb -p 4242":        "debugger",
		"dtrace -p 4242 -n x": "tracer",
		// staypoint under another name or another environment.
		"s=staypoint; $s mcp":                                            "computed at run time",
		"$(which staypoint) mcp":                                         "computed at run time",
		"ln -s $(which staypoint) /tmp/sp2":                              "links or copies",
		"p=$(command -v staypoint); cp $p /tmp/sp":                       "links or copies",
		"cp /usr/local/bin/staypoint /tmp/x":                             "links or copies",
		sym + " mcp":                                                     "staypoint mcp",
		hard + " mcp":                                                    "staypoint mcp",
		"env -u STAYPOINT_TASK_ID " + sym + " status":                    "staypoint with env -u",
		"echo mcp | xargs staypoint":                                     "xargs staypoint",
		"export STAYPOINT_TASK_ID=x; staypoint status":                   "STAYPOINT_TASK_ID overridden",
		"export HOME=/tmp/x && staypoint status":                         "HOME overridden",
		"HOME=/tmp/x; staypoint status":                                  "HOME overridden",
		"unset STAYPOINT_TASK_ID; staypoint status":                      "STAYPOINT_TASK_ID overridden",
		"export PATH=/tmp/bin:$PATH; staypoint status":                   "PATH overridden",
		"source ./x.env; staypoint status":                               "source",
		"claude mcp add x -- staypoint mcp":                              "registers",
		"gemini mcp add sp staypoint mcp":                                "registers",
		`claude mcp add-json x '{"command":"staypoint","args":["mcp"]}'`: "registers",
	}
	for line, want := range red {
		v := c.Classify(line)
		if v.Tier != Red || !strings.Contains(strings.Join(v.Reasons, "; "), want) {
			t.Errorf("%q = %s %v, want Red (%s)", line, v.Tier, v.Reasons, want)
		}
	}

	notRed := []string{
		"staypoint status", "staypoint task list", "ps aux", "ps -o pid,etime -p 1", "ps -ef",
		"ls ~", "find . -name x", "find ~ -name '*.md'", "cp a b", "cp -r src dst", "ln -s a b",
		"git commit -m 'staypoint mcp fix'", "export FOO=1; staypoint status", "cd ~/x && ls",
		"tar czf out.tgz ./build", "cat docs/staypoint.md", "claude --version", "cd ~ && cat notes.txt",
	}
	for _, line := range notRed {
		if v := c.Classify(line); v.Tier == Red {
			t.Errorf("%q = Red %v, want below Red", line, v.Reasons)
		}
	}
}
