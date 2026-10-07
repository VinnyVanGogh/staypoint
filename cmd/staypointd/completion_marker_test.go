package main

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeStreamCLI stands in for claude and agy: it logs each spawn like fakeCLI,
// then prints claudeOut when spawned as claude (--print) and agyOut otherwise.
// Spawns whose args contain failOn exit 1 before any output.
func fakeStreamCLI(t *testing.T, failOn, claudeOut, agyOut string) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "spawns.log")
	bin = filepath.Join(dir, "fakecli.sh")
	abs := func(name string) string {
		p, err := filepath.Abs(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	script := "#!/bin/sh\nprintf '%s|%s\\n' \"${CLAUDE_CONFIG_DIR}\" \"$(echo \"$*\" | tr '\\n' ' ')\" >> \"" + logPath + "\"\n"
	if failOn != "" {
		script += "case \"$*\" in *" + failOn + "*) exit 1;; esac\n"
	}
	script += "case \"$*\" in *--print*) cat '" + abs(claudeOut) + "';; *) cat '" + abs(agyOut) + "';; esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logPath
}

// STA-840: the harness must see [[TASK_COMPLETE]] in every provider's final
// answer, through the production wake path and the real stream parsers.
// The fixtures are real agy / Claude stream-json shapes, trimmed.
func TestWake_CompletionMarkerDetectedForEveryProvider(t *testing.T) {
	cases := []struct {
		name, kind, failOn string
		claudeOut, agyOut  string
		wantSpawns         int
		wantDetected       bool
	}{
		// planning routes Gemini-first since PR #213.
		{"gemini planning run", "planning", "", "claude_marker_not_final.ndjson", "agy_task_complete.ndjson", 1, true},
		{"claude coding run", "coding", "", "claude_task_complete.ndjson", "agy_marker_not_final.ndjson", 1, true},
		// Gemini dies before output, the same turn falls over to Claude.
		{"gemini to claude fallback", "planning", "gemini-3.8-flash", "claude_task_complete.ndjson", "agy_task_complete.ndjson", 2, true},
		// Marker only in tool output, echoed input or prose: not a completion.
		{"gemini marker not final", "planning", "", "claude_task_complete.ndjson", "agy_marker_not_final.ndjson", 1, false},
		{"claude marker not final", "coding", "", "claude_marker_not_final.ndjson", "agy_task_complete.ndjson", 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bin, logPath := fakeStreamCLI(t, c.failOn, c.claudeOut, c.agyOut)
			r := runWakeBin(t, personalRepo(t), c.kind, openPacer(), bin, logPath)
			if len(r.spawns) != c.wantSpawns {
				t.Fatalf("spawns = %q, want %d", r.spawns, c.wantSpawns)
			}
			if got := r.interceptorComments > 0; got != c.wantDetected {
				t.Errorf("completion detected = %v, want %v (interceptor comments: %d)", got, c.wantDetected, r.interceptorComments)
			}
		})
	}
}
