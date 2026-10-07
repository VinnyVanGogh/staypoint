package security

import (
	"strings"
	"testing"
)

func TestStateChanges(t *testing.T) {
	const cwd = "/repo"
	cases := []struct {
		line       string
		wantAction string // substring of the single expected action; "" means none
		wantPath   string
	}{
		{"git status", "", ""},
		{"git log --oneline -5", "", ""},
		{"git diff HEAD~1", "", ""},
		{"ls -la && cat README.md", "", ""},
		{"go test ./...", "", ""},
		{"echo hi > /dev/null", "", ""},
		{"grep foo bar.txt 2>&1", "", ""},
		{"gh pr view 12", "", ""},
		{"gh pr list", "", ""},
		{"git commit -m 'x'", "git commit", "/repo"},
		{"git -C /other push origin main", "git push", "/other"},
		{"git -C sub merge feature", "git merge", "/repo/sub"},
		{"git rebase origin/main", "git rebase", "/repo"},
		{"cd /elsewhere && git commit -am x", "git commit", "/elsewhere"},
		{"gh pr create --title t --body b", "gh pr create", "/repo"},
		{"gh -R owner/x pr merge 4 --squash", "gh pr merge", "/repo"},
		{"echo hi > notes.txt", "redirect into notes.txt", "/repo/notes.txt"},
		{"echo hi >> /tmp/x.log", "redirect into /tmp/x.log", "/tmp/x.log"},
		{"cat a | tee out.txt", "tee into out.txt", "/repo/out.txt"},
		{"env FOO=1 git push", "git push", "/repo"},
		{"bash -c 'git commit -m y'", "git commit", "/repo"},
		{"echo $(git push)", "git push", "/repo"},
		{"sqlite3 ~/.staypoint/staypoint.db 'select 1'", "StayPoint database", "/repo"},
		{"echo 'unterminated", "unparseable", "/repo"},
	}
	for _, c := range cases {
		got := StateChanges(c.line, cwd)
		if c.wantAction == "" {
			if len(got) != 0 {
				t.Errorf("%q: want no state change, got %+v", c.line, got)
			}
			continue
		}
		if len(got) != 1 {
			t.Errorf("%q: want 1 change, got %+v", c.line, got)
			continue
		}
		if !strings.Contains(got[0].Action, c.wantAction) {
			t.Errorf("%q: action %q does not contain %q", c.line, got[0].Action, c.wantAction)
		}
		if len(got[0].Paths) != 1 || got[0].Paths[0] != c.wantPath {
			t.Errorf("%q: paths %v, want [%s]", c.line, got[0].Paths, c.wantPath)
		}
	}
}
