package mcp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
)

// STA-689: staypoint_undo's `git clean -f -X -d` ran on the MCP context, which
// has no deadline, so gitexec capped it at the short default git timeout. A
// large ignored tree could be cut off mid-clean. The clean gets its own,
// longer deadline.
func TestUndoCleanIgnoredOutlivesDefaultGitTimeout(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	gitDir := setupTestGitRepo(t)
	if err := os.WriteFile(filepath.Join(gitDir, ".gitignore"), []byte("*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, gitDir, "git", "add", ".gitignore")
	runCmd(t, gitDir, "git", "commit", "-m", "ignore logs")

	s := NewServer(WithWorkDir(gitDir))
	defer s.Close()
	callTool(t, s, "staypoint_checkpoint", map[string]any{"message": "before", "session_id": "sess-clean"})

	junk := filepath.Join(gitDir, "junk.log")
	if err := os.WriteFile(junk, []byte("ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Every git call goes to real git, except the forced clean, which takes
	// longer than the default git timeout.
	bin := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = clean ] && [ \"$2\" = -f ]; then sleep 3; fi\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(gitexec.TimeoutEnv, "2s")

	callTool(t, s, "staypoint_undo", map[string]any{"keep_untracked": true, "clean_ignored": true})

	if _, err := os.Stat(junk); !os.IsNotExist(err) {
		t.Errorf("ignored file still present after undo with clean_ignored (stat err = %v): clean was cut off", err)
	}
}

func callTool(t *testing.T, s *Server, name string, args map[string]any) {
	t.Helper()
	rawArgs, _ := json.Marshal(args)
	params, _ := json.Marshal(CallToolParams{Name: name, Arguments: rawArgs})
	res := parseToolCallResult(t, sendRequest(t, s, Request{
		JSONRPC: "2.0",
		ID:      makeRawID("call-" + name),
		Method:  "tools/call",
		Params:  params,
	}))
	if res.IsError {
		t.Fatalf("%s returned error: %s", name, res.Content[0].Text)
	}
}
