package security

import (
	"strings"
	"testing"
)

// task-97b4fa02 (Board, Fable review #4 on task-7d279c9d): checkPath was a
// prefix match on the cleaned word, so a sensitive path spelled with a
// glob, a case change, a variable, a cd or a recursive parent was Green or
// Yellow. Each spelling below must be Red, for the token file and for the
// data dir itself. The classifier never reads these paths; Home is a fake.
func TestSensitivePathSpellings(t *testing.T) {
	const home = "/Users/agent"
	c := &Classifier{Home: home, CWD: "/repo"}
	red := []string{
		// The Board's list.
		"cat ~/.st*/auth_token",
		"cat ~/.staypoin?/auth_token",
		"cat $HOME/.STAYPOINT/auth_token",
		"cd ~ && cat .staypoint/auth_token",
		"d=~/.staypoint; cat $d/auth_token",
		"cp -r ~ /tmp/h",
		"tar cf - -C ~ .staypoint",
		"find ~ -name auth_token -exec cat {} +",
		`python3 -c "print(open(__import__('os').path.expanduser('~/.staypoint/auth_token')).read())"`,
		// The data dir itself.
		"ls ~/.st*",
		"ls ~/.STAYPOINT",
		"cd ~ && ls .staypoint",
		"d=~/.staypoint; ls $d",
		"tar cf - -C ~ .",
		// More spellings of the same.
		"cat ~/.[s]taypoint/auth_token",
		"cat ~/[.]staypoint/auth_token",
		"cat ~/.{staypoint,x}/auth_token",
		"cat ~/.{s,}taypoint/auth_token",
		"cat ~/.s{t,}aypoint/auth_token",
		"cat ${HOME}/.Staypoint/AUTH_TOKEN",
		"cat /Users/agent/.StayPoint/auth_token",
		"cat /Users/*/.staypoint/auth_token",
		"cat /U*/a*/.s*/a*",
		"cat ~agent/.staypoint/auth_token",
		"cat ~/x/../.staypoint/auth_token",
		"cat ~/**/auth_token",
		"shopt -s dotglob; cat ~/*/auth_token",
		"shopt -s dotglob; cat ~/?staypoint/auth_token",
		"cd ~; cat .st*/auth_token",
		"cd ~/x && cat ../.staypoint/auth_token",
		"pushd ~ && cat .staypoint/auth_token",
		"cd && cat .staypoint/auth_token",
		"env -C ~ cat .staypoint/auth_token",
		"git -C ~ diff --no-index .staypoint/auth_token /dev/null",
		"d=~; cat $d/.staypoint/auth_token",
		"d=~/.st; cat ${d}aypoint/auth_token",
		"export d=~/.staypoint; cat \"$d\"/auth_token",
		"for d in ~/.staypoint; do cat $d/auth_token; done",
		"cat ~/$f",
		"cat $X/auth_token",
		"cat $X/.st*/x",
		"cat $X/.staypoint/x",
		"read d; cat ${d}point/x",
		"cat \"$(echo ~)\"/.staypoint/auth_token",
		"cd \"$X\" && tar cf - .",
		"cd \"$X\" && cat .staypoint/x",
		"cat ~/.ssh/id_ed25519",
		"cat ~/.S*/id_rsa",
		"cat ~/.aws/credentials",
		"cat /e?c/passwd",
		// Recursive commands over a parent.
		"cp -R ~/ /tmp/h",
		"cp -a $HOME /tmp/h",
		"rsync -a ~/ /tmp/h",
		"zip -r /tmp/h.zip ~",
		"tar czf /tmp/h.tgz ~",
		"ditto ~ /tmp/h",
		"grep -r token ~",
		"grep -rn token /Users/agent",
		"cd ~ && grep -r token .",
		"rg --hidden token ~",
		"rg -uu token ~",
		"find ~ -name '*token*' -exec cat {} \\;",
		"find / -name auth_token -delete",
		"find -L ~ -type f -exec cat {} +",
		"diff -r ~ /tmp/h",
		"cp -r \"$SRC\" /tmp/h",
		"cd \"$X\" && find . -exec cat {} +",
		// find's names fed to a reader; CDPATH and pushd moving the cwd.
		"find ~ -name auth_token | xargs cat",
		"cat $(find ~ -name auth_token)",
		"while read f; do cat \"$f\"; done < <(find ~ -name auth_token)",
		"fd -H auth_token ~ | xargs cat",
		"CDPATH=~ cd .staypoint && cat auth_token",
		"export CDPATH=$HOME; cd .staypoint; cat x",
		"pushd ~ >/dev/null; pushd /tmp; pushd; cat .staypoint/x",
		"pushd ~; pushd /tmp; pushd +1; cat .st*/x",
		"C''DPATH=~ cd .staypoint && cat x",
		`x=CD; declare "${x}PATH=$HOME"; cd .staypoint; cat x`,
		"find ~ -name auth_token > /tmp/l; xargs cat < /tmp/l",
		"timeout 5 find ~ -name auth_token | xargs cat",
		"fd -u auth_token ~",
		// Inline scripts that name it however split.
		`python3 -c "import os; print(open(os.path.expanduser('~/.st'+'aypoint/auth_token')).read())"`,
		`python3 -c "import pathlib; print((pathlib.Path.home()/'.STAYPOINT'/'x').read_text())"`,
		`node -e "require('fs').readFileSync(require('os').homedir()+'/.staypoint/auth_token')"`,
		"python3 <<'EOF'\nimport os\nprint(open(os.path.expanduser('~/.staypoint/auth_token')).read())\nEOF",
	}
	for _, line := range red {
		if v := c.Classify(line); v.Tier != Red {
			t.Errorf("%q: want red, got %s %v", line, v.Tier, v.Reasons)
		}
	}

	notRed := []string{
		"cat ~/Documents/notes.md",
		"ls ~",
		"ls ~/*",
		"du -sh ~/Library/*",
		"cat ~/.zshrc",
		"cat ~/.stayput/x",
		"cp -r src /tmp/x",
		"cp -r ~/Documents/proj /tmp/p",
		"find . -name '*.go' -exec grep -l x {} +",
		"find /repo/internal -name '*.go' -delete",
		"grep -rn staypoint internal/",
		`grep -rn '\.staypoint' internal/ docs/`,
		"grep -rn \"$PATTERN\" .",
		"rg TODO",
		"rg --hidden TODO internal",
		"tar czf /tmp/x.tgz -C /repo .",
		"tar xzf /tmp/x.tgz",
		"echo $PATH",
		"go test ./...",
		"d=/tmp/x; cat $d/out.txt",
		"for f in a b; do cat $f; done",
		`python3 -c "print('github.com/VinnyVanGogh/staypoint')"`,
		"cat $T/*",
		"find . -name '*.go' | xargs grep -l x",
		"cat $(find internal -name '*.md')",
		"cd internal && grep -rn x .",
		"pushd internal && ls; popd",
		"ls \"$TMPDIR\"/staypoint-e2e",
	}
	for _, line := range notRed {
		if v := c.Classify(line); v.Tier == Red {
			t.Errorf("%q: want below red, got red %v", line, v.Reasons)
		}
	}
}

// The tier check exempts a plain read of the run's own handoff files, as
// the Board rules do (ownHandoffRead); gate 49c95281 held one.
func TestSensitivePathOwnHandoffRead(t *testing.T) {
	c := &Classifier{Home: "/Users/agent", CWD: "/repo", TaskID: "task-T1"}
	for _, line := range []string{
		"cat ~/.staypoint/handoffs/task-T1/latest.md",
		"head -50 /Users/agent/.staypoint/handoffs/task-T1/notes.md",
		"ls ~/.staypoint/handoffs/task-T1/",
	} {
		if v := c.Classify(line); v.Tier == Red {
			t.Errorf("%q: own handoff read held: %v", line, v.Reasons)
		}
	}
	for _, line := range []string{
		"cat ~/.staypoint/handoffs/task-OTHER/latest.md",
		"cat ~/.staypoint/handoffs/task-T1/../../auth_token",
		"cat ~/.staypoint/handoffs/task-T1/latest.md ~/.st*/auth_token",
		"cat ~/.staypoint/handoffs/task-T1/latest.md ~/.STAYPOINT/auth_token",
		"cat ~/.staypoint/handoffs/task-T1/*/../../auth_token",
		"cat ~/.staypoint/handoffs/task-T?/latest.md",
		"cp ~/.staypoint/handoffs/task-T1/latest.md /tmp/x",
		"cat ~/.staypoint/handoffs/task-T1/latest.md > /tmp/x",
		"cat ~/.staypoint/handoffs/task-T1/latest.md; cat ~/.staypoint/auth_token",
	} {
		if v := c.Classify(line); v.Tier != Red {
			t.Errorf("%q: want red, got %s %v", line, v.Tier, v.Reasons)
		}
	}
	other := &Classifier{Home: "/Users/agent", CWD: "/repo"}
	if v := other.Classify("cat ~/.staypoint/handoffs/task-T1/latest.md"); v.Tier != Red {
		t.Errorf("no task: want red, got %s", v.Tier)
	}
}

func TestBraceExpand(t *testing.T) {
	got := strings.Join(braceExpand("a{b,c{d,e}}f"), " ")
	if got != "abf acdf acef" {
		t.Errorf("braceExpand: %q", got)
	}
	if alts := braceExpand("x{1..9}"); len(alts) != 1 || !strings.Contains(alts[0], unresolved) {
		t.Errorf("sequence: %q", alts)
	}
	if alts := braceExpand("${HOME}/x"); len(alts) != 1 || alts[0] != "${HOME}/x" {
		t.Errorf("${...} is not a brace group: %q", alts)
	}
}
