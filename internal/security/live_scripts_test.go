package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The scripts behind the four 2026-10-06 false positives, verbatim (STA-868).
// Copied into a scratch dir, each must be Yellow: they only run du/ls/git
// reads/comm/sort and write under that dir.
func TestLiveFalsePositiveScriptsAreYellow(t *testing.T) {
	c, dir := scratchClassifier(t)
	for _, name := range []string{"sizes.sh", "todo.sh", "wt-audit.sh"} {
		body, err := os.ReadFile(filepath.Join("testdata", "sta868", name))
		if err != nil {
			t.Fatal(err)
		}
		writeScript(t, dir, name, strings.ReplaceAll(string(body), "/tmp/sta-cleanup", dir))
	}
	for _, line := range []string{
		"bash " + dir + "/todo.sh",
		"bash " + dir + "/sizes.sh > " + dir + "/sizes.txt 2>&1",
		"cd " + dir + " && /opt/homebrew/bin/bash ./wt-audit.sh /Users/x/dev/worktrees/a /Users/x/dev/worktrees/b > wt.tsv",
	} {
		if v := c.Classify(line); v.Tier != Yellow {
			t.Errorf("%q: want yellow, got %s %v", line, v.Tier, v.Reasons)
		}
	}
	heredoc := "mkdir -p " + dir + " && cat > " + dir + "/wt-audit2.sh <<'EOF'\n" + mustRead(t, "wt-audit.sh") + "EOF\nchmod +x " + dir + "/wt-audit2.sh"
	if v := c.Classify(heredoc); v.Tier != Yellow {
		t.Errorf("heredoc write: want yellow, got %s %v", v.Tier, v.Reasons)
	}
}

func mustRead(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "sta868", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
