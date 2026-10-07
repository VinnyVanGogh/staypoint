package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/security"
)

// STA-868: a verdict that rests on a script's contents is only honoured when
// the command can be rewritten to run exactly the judged bytes.

func pinFixture(t *testing.T, body string) (*security.Classifier, string, string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "s.sh")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return &security.Classifier{Home: t.TempDir(), CWD: dir, ReadFile: os.ReadFile, ScratchDirs: []string{dir}}, dir, p
}

func TestPinJudged_RewritesToJudgedBytes(t *testing.T) {
	c, _, p := pinFixture(t, "ls -la\n")
	cmd := "bash " + p + " > /dev/null"
	v := c.Classify(cmd)
	pinned := pinJudged(cmd, &v)
	if v.Tier != security.Yellow || pinned != "bash -c 'ls -la\n' "+p+" > /dev/null" {
		t.Fatalf("pinned %q tier %s %v", pinned, v.Tier, v.Reasons)
	}
	out := pinnedHookOutput(json.RawMessage(`{"command":"`+cmd+`","description":"d","timeout":5}`), pinned)
	var got struct {
		HookSpecificOutput struct {
			HookEventName      string         `json:"hookEventName"`
			PermissionDecision string         `json:"permissionDecision"`
			UpdatedInput       map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	h := got.HookSpecificOutput
	if h.HookEventName != "PreToolUse" || h.PermissionDecision != "" || h.UpdatedInput["command"] != pinned ||
		h.UpdatedInput["description"] != "d" || h.UpdatedInput["timeout"] != float64(5) {
		t.Fatalf("hook output: %s", out)
	}
}

func TestPinJudged_UnpinnableIsRed(t *testing.T) {
	c, _, p := pinFixture(t, "ls\n")
	// The script path occurs twice, so the rewrite would be ambiguous.
	cmd := "bash " + p + " && cat " + p
	v := c.Classify(cmd)
	if v.Tier != security.Yellow {
		t.Fatalf("setup: %s %v", v.Tier, v.Reasons)
	}
	if pinned := pinJudged(cmd, &v); pinned != "" || v.Tier != security.Red ||
		!strings.Contains(strings.Join(v.Reasons, ";"), "cannot be pinned") {
		t.Fatalf("want red unpinnable, got %q %s %v", pinned, v.Tier, v.Reasons)
	}
}

func TestSnapshotScripts_PinsHeldCommand(t *testing.T) {
	_, dir, p := pinFixture(t, "git push origin HEAD\n")
	scripts, pinned := snapshotScripts("bash "+p, dir)
	if len(scripts) != 1 || scripts[0].Content != "git push origin HEAD\n" || pinned != "bash -c 'git push origin HEAD\n' "+p {
		t.Fatalf("snapshot %+v pinned %q", scripts, pinned)
	}
	// Edited after the snapshot: the approved command still runs the old bytes.
	if err := os.WriteFile(p, []byte("rm -rf ~\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(pinned, "rm -rf") {
		t.Fatal("pinned command depends on the file")
	}
	if s, pn := snapshotScripts("ls", dir); s != nil || pn != "" {
		t.Fatalf("no scripts: %+v %q", s, pn)
	}
}
