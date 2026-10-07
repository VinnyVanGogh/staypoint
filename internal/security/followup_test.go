package security

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// STA-868 follow-up review: parser differentials vs bash, and read/hash/pin
// TOCTOU in the hook path.

// ── Parser differentials ────────────────────────────────────────────────────

func TestDifferential_TopLevelStaysRed(t *testing.T) {
	c := &Classifier{Home: t.TempDir()}
	for name, line := range map[string]string{
		"cr before #":           "echo x\r# ; rm -rf ~",
		"cr word":               "ls\rrm -rf ~",
		"ansi-c escaped quote":  "echo $'\\'' ; rm -rf ~ #'",
		"unquoted continuation": "cat <<EOF\nfoo\\\nEOF\nrm -rf ~\nEOF",
		"nested dq in ${}":      "echo \"${x:-\"}\"}\" ; rm -rf ~",
		"sq in ${}":             "echo ${x:-'}'; rm -rf ~; '{'}",
		"subst in ${}":          "echo ${x:-$(rm -rf ~)}",
		"comment in $()":        "echo $( # )\nrm -rf ~\n)",
		"line continuation":     "r\\\nm -rf ~",
		"hash in word":          "echo a#b; rm -rf ~",
		"arith shift heredoc":   "((x = 1<<2))\nrm -rf ~\n2))",
		"case body":             "case x in a) rm -rf ~;; esac",
		"case paren pattern":    "case x in (a) rm -rf ~;; esac",
		"process subst in":      "cat <(rm -rf ~)",
		"process subst out":     "ls >(rm -rf ~)",
		"extglob":               "shopt -s extglob; ls !(rm -rf ~)",
		"locale string":         "echo $\"a\" ; rm -rf ~",
		"backtick in dq":        "echo \"`rm -rf ~`\"",
		"dq escaped quote":      "echo \"a\\\" ; rm -rf ~ ; \\\"\"; rm -rf ~",
		"unterminated ${":       "echo ${x",
	} {
		if v := c.Classify(line); v.Tier != Red {
			t.Errorf("%s: want red, got %s %v", name, v.Tier, v.Reasons)
		}
	}
	// $'a\' ; rm -rf ~ ; b' is one ANSI-C string in bash (\' does not end
	// it): nothing runs, and we now agree.
	if v := c.Classify("echo $'a\\' ; rm -rf ~ ; b'"); v.Tier == Red {
		t.Errorf("ansi-c string: should parse as one word, got %s %v", v.Tier, v.Reasons)
	}
	// A real comment is still a comment, as in bash.
	if v := c.Classify("ls # ; rm -rf ~"); v.Tier != Green {
		t.Errorf("comment: want green, got %s %v", v.Tier, v.Reasons)
	}
}

func TestDifferential_ParserTokens(t *testing.T) {
	// $'\'' is one word in bash; the rest of the line is commands.
	segs, _, err := parseShell("echo $'\\'' ; rm -rf ~ #'")
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 2 || segs[1].argv[0] != "rm" {
		t.Fatalf("segments: %+v", segs)
	}
	if !segs[0].dyn[1] {
		t.Fatal("$'...' must be dynamic")
	}
	for _, bad := range []string{"a\rb", "cat <<EOF\nx\\\nEOF\n", "echo ${a:-'x'}", "echo \"${a:-\"x\"}\""} {
		if _, _, err := parseShell(bad); err == nil {
			t.Errorf("%q: want parse error", bad)
		}
	}
	// Plain ${...} forms still parse.
	for _, ok := range []string{"echo ${HOME%/}", "echo \"${a:-b}\" ${#x} ${x/a/b}", "cat <<'EOF'\nx\\\nEOF\n"} {
		if _, _, err := parseShell(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
}

func TestDifferential_ScriptBodiesStayRed(t *testing.T) {
	assertRed(t, map[string]string{
		"ansi-c quote":          "echo $'\\'' ; rm -rf ~ #'\n",
		"unquoted continuation": "cat <<EOF\nfoo\\\nEOF\nrm -rf ~\nEOF\n",
		"nested dq in ${}":      "echo \"${x:-\"}\"}\" ; rm -rf ~\n",
		"comment in $()":        "echo $( # )\nrm -rf ~\n)\n",
		"line continuation":     "r\\\nm -rf ~\n",
		"cr":                    "ls\r# ; rm -rf ~\n",
		"process subst":         "cat <(rm -rf ~)\n",
		"extglob":               "shopt -s extglob\nls !(rm -rf ~)\n",
	})
}

// ── TOCTOU: one read, one descriptor, same bytes everywhere ────────────────

func TestReadOnce_NoFollowAndRegularOnly(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	target := filepath.Join(dir, "real.sh")
	if err := os.WriteFile(target, []byte("ls\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.sh")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// A symlink in the final component at open time (swapped in after the
	// path was resolved) must not be followed.
	if _, _, err := readResolved(link); err == nil {
		t.Fatal("open followed a symlink")
	}
	data, real, err := ReadScriptOnce(link)
	if err != nil || string(data) != "ls\n" || real != target {
		t.Fatalf("read: %q %q %v", data, real, err)
	}
	fifo := filepath.Join(dir, "f.sh")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	done := make(chan error, 1)
	go func() { _, _, err := ReadScriptOnce(fifo); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegular) {
			t.Fatalf("fifo: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reading a FIFO blocked")
	}
	big := filepath.Join(dir, "big.sh")
	if err := os.WriteFile(big, make([]byte, maxScriptBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadScriptOnce(big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("large: %v", err)
	}
}

func TestReadOnce_RealPathDecidesScratch(t *testing.T) {
	c, dir := scratchClassifier(t)
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	writeScript(t, outside, "s.sh", "ls\n")
	if err := os.Symlink(outside, filepath.Join(dir, "d")); err != nil {
		t.Fatal(err)
	}
	v := c.Classify("bash " + dir + "/d/s.sh")
	if v.Tier != Red || len(v.Scripts) != 0 {
		t.Fatalf("script whose opened file is outside scratch: %s %v", v.Tier, v.Reasons)
	}
}

func TestSnapshotter_ReadsOncePerRun(t *testing.T) {
	_, dir := scratchClassifier(t)
	p := writeScript(t, dir, "s.sh", "ls\n")
	reads := 0
	snap := &Snapshotter{m: map[string]Snapshot{}, read: func(path string) ([]byte, string, error) {
		reads++
		if reads > 1 {
			return []byte("rm -rf ~\n"), path, nil // a second read would see the swap
		}
		return ReadScriptOnce(path)
	}}
	c := &Classifier{Home: t.TempDir(), Snap: snap, ScratchDirs: []string{dir}}
	line := "bash " + p
	v := c.Classify(line)
	refs := ScriptRefs(line, "", snap, 0)
	if reads != 1 {
		t.Fatalf("script read %d times", reads)
	}
	if v.Tier != Yellow || len(v.Scripts) != 1 || len(refs) != 1 ||
		string(v.Scripts[0].Content) != "ls\n" || string(refs[0].Full) != "ls\n" {
		t.Fatalf("judged and snapshotted bytes differ: %+v %+v", v.Scripts, refs)
	}
}

func TestCWD_RelativeScriptNeedsTrustedCWD(t *testing.T) {
	_, dir := scratchClassifier(t)
	writeScript(t, dir, "s.sh", "ls\n")
	untrusted := &Classifier{Home: t.TempDir(), CWD: dir, ReadFile: os.ReadFile, ScratchDirs: []string{dir}}
	if v := untrusted.Classify("bash ./s.sh"); v.Tier != Red {
		t.Fatalf("relative script against a guessed cwd: want red, got %s", v.Tier)
	}
	if v := untrusted.Classify("cd " + dir + " && bash ./s.sh"); v.Tier != Yellow {
		t.Fatalf("after an absolute cd: want yellow, got %s %v", v.Tier, v.Reasons)
	}
	if v := untrusted.Classify("cd s && bash ./s.sh"); v.Tier != Red {
		t.Fatalf("after a relative cd: want red, got %s", v.Tier)
	}
	trusted := *untrusted
	trusted.CWDTrusted = true
	if v := trusted.Classify("bash ./s.sh"); v.Tier != Yellow {
		t.Fatalf("payload cwd: want yellow, got %s %v", v.Tier, v.Reasons)
	}
}

// The pinned command, run by a real shell, executes exactly the judged bytes
// and never reads the script file again.
func TestPinned_ExecutesExactBytes(t *testing.T) {
	c, dir := scratchClassifier(t)
	out := filepath.Join(dir, "out.txt")
	marker := filepath.Join(dir, "marker")
	content := "# it's ''quoted'' \\ $HOME `x` ü\nprintf '%s' \"$BASH_EXECUTION_STRING\" > " + out + "\n"
	p := writeScript(t, dir, "s.sh", content)
	line := "bash " + p + " arg"
	v := c.Classify(line)
	if v.Tier != Yellow || len(v.Scripts) != 1 {
		t.Fatalf("classify: %s %v", v.Tier, v.Reasons)
	}
	pinned, err := PinCommand(line, PinsFromVerdict(v))
	if err != nil {
		t.Fatal(err)
	}
	if hasEmptyQuotedPiece(pinned) {
		t.Fatalf("pinned command contains '' (zsh RC_QUOTES would misread it): %s", pinned)
	}
	// Swap the file for something destructive after judging.
	if err := os.WriteFile(p, []byte("touch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	shells := [][]string{{"/bin/bash", "-c"}, {"/bin/sh", "-c"}}
	if z, err := exec.LookPath("zsh"); err == nil {
		shells = append(shells, []string{z, "-f", "-c"}, []string{z, "-f", "-o", "rcquotes", "-c"})
	}
	for _, sh := range shells {
		_ = os.Remove(out)
		cmd := exec.Command(sh[0], append(sh[1:], pinned)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir}
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v %s", sh, err, b)
		}
		got, err := os.ReadFile(out)
		if err != nil {
			t.Fatalf("%v: no output: %v", sh, err)
		}
		if string(got) != content {
			t.Fatalf("%v executed different bytes:\n%q\nwant\n%q", sh, got, content)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("%v ran the swapped file", sh)
		}
	}
}

func TestShellQuote_NoEmptyQuotes(t *testing.T) {
	for _, s := range []string{"", "'", "''", "a'b", "'a'", "it's", "\\'"} {
		q := shellQuote(s)
		if s != "" && hasEmptyQuotedPiece(q) {
			t.Errorf("%q -> %q contains ''", s, q)
		}
		out, err := exec.Command("/bin/sh", "-c", "printf '%s' "+q).Output()
		if err != nil || string(out) != s {
			t.Errorf("%q -> %q -> %q %v", s, q, out, err)
		}
	}
}

// hasEmptyQuotedPiece reports a ” that is not an escaped \' followed by an
// opening quote: inside a quoted string zsh's RC_QUOTES reads ” as a quote.
func hasEmptyQuotedPiece(q string) bool {
	for i := 0; i+1 < len(q); i++ {
		if q[i] == '\'' && q[i+1] == '\'' && (i == 0 || q[i-1] != '\\') {
			return true
		}
	}
	return false
}
