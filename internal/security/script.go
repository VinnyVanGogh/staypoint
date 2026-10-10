package security

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Scratch-dir scripts (STA-868).
//
// Agents write throwaway audit scripts under /tmp or their run's scratch dir
// and then run them with `bash x.sh`. Such a call used to be Red ("runs an
// opaque script") and held for the Board even when the script only ran
// du/ls/git status. When the classifier may read files (Classifier.ReadFile)
// and the script sits in a scratch dir, it is judged by its contents instead.
//
// The analysis is an allowlist and fails closed: anything the walker does not
// positively understand keeps the script Red. A script is Yellow only when
//   - every command is a known reader, a narrowly-allowed git/sort/find/...
//     form, or shell syntax; nothing it runs comes from a variable;
//   - every write is to an absolute path strictly inside the script's own
//     directory (no relative paths: the script runs in the caller's cwd);
//   - no variable that changes how commands resolve or run (PATH, IFS,
//     BASH_ENV, GIT_*, ...) is touched, and the call adds no environment.
//
// The bytes judged must be the bytes that run. A judged script is recorded in
// Verdict.Scripts, and the caller (the pre-tool hook) must rewrite the command
// so it executes exactly those bytes (PinCommand: `bash -c '<bytes>' path`).
// If it cannot, the command must be treated as Red.

// maxScriptBytes bounds how much of a script is read and analysed.
const maxScriptBytes = 256 << 10

// JudgedScript is a script whose contents decided the verdict.
type JudgedScript struct {
	Token   string // the script path as written in the command (unquoted)
	Path    string // absolute path
	Interp  string // "" when run as `bash|sh <token>`; else the interpreter for ./x.sh
	Content []byte // the exact bytes that were judged
}

// DefaultScratchDirs are the system temp dirs: /tmp, its macOS target
// /private/tmp, and $TMPDIR.
func DefaultScratchDirs() []string {
	dirs := []string{"/tmp", "/private/tmp"}
	if t := os.TempDir(); t != "" {
		dirs = append(dirs, filepath.Clean(t))
	}
	return dirs
}

func (c *Classifier) scratchDirs() []string {
	if c.ScratchDirs != nil {
		return c.ScratchDirs
	}
	return DefaultScratchDirs()
}

// within reports whether p is root or below it (lexically).
func within(root, p string) bool {
	root, p = filepath.Clean(root), filepath.Clean(p)
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// resolveExisting resolves symlinks in the longest existing prefix of p.
func resolveExisting(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			if rest == "" {
				return r
			}
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// scratchRootFor returns the scratch dir holding p, checked both lexically
// and after resolving symlinks (a link in /tmp must not point elsewhere).
func (c *Classifier) scratchRootFor(p string) string {
	if !filepath.IsAbs(p) {
		return ""
	}
	resolved := resolveExisting(p)
	for _, d := range c.scratchDirs() {
		if d == "" || !within(d, p) {
			continue
		}
		if within(resolveExisting(d), resolved) {
			return d
		}
	}
	return ""
}

func (c *Classifier) inScratch(p string) bool { return c.scratchRootFor(p) != "" }

// inScratchUnder reports whether p is in a scratch dir that itself lies
// strictly inside the sensitive dir d (the run's ~/.staypoint/scratch/<task>).
// A scratch dir that merely contains d (a temp HOME) exempts nothing.
func (c *Classifier) inScratchUnder(d, p string) bool {
	r := c.scratchRootFor(p)
	return r != "" && r != d && within(d, r)
}

// strictlyInside reports whether p is below root (not root itself), both
// lexically and after resolving symlinks.
func strictlyInside(root, p string) bool {
	rr, rp := resolveExisting(root), resolveExisting(p)
	return within(rr, rp) && rr != rp
}

// lineCtx records facts about the whole command line.
type lineCtx struct {
	heredocWrites map[string]*redirect // absolute target -> heredoc written to it
	writes        map[string]bool      // absolute paths written by redirects / tee / touch
	opaqueWriter  string               // first segment that could write anywhere
	scriptRuns    int                  // scripts executed in the line
	dirUncertain  bool                 // a subshell or pushd makes `cd` tracking unreliable
	envTainted    string               // the line changes variables or the environment
	raw           string               // the line as written
}

var subshellRe = regexp.MustCompile(`(^|[^$<>])\(`)

// knownTargetWriters write only to the paths named in their arguments or
// redirects, which lineContext records.
var knownTargetWriters = map[string]bool{
	"mkdir": true, "chmod": true, "touch": true, "tee": true, "cat": true, "echo": true, "printf": true,
}

// envChangers change variables, options or definitions seen by later commands.
var envChangers = map[string]bool{
	"export": true, "env": true, "declare": true, "typeset": true, "local": true, "readonly": true,
	"set": true, "shopt": true, "unset": true, "source": true, ".": true, "alias": true, "enable": true,
	"hash": true, "eval": true, "builtin": true, "trap": true, "ulimit": true,
}

func (c *Classifier) lineContext(line string, segs []segment, subs []string, depth int) *lineCtx {
	lc := &lineCtx{heredocWrites: map[string]*redirect{}, writes: map[string]bool{}, raw: line}
	lc.dirUncertain = subshellRe.MatchString(line)
	plain := &Classifier{Home: c.Home, CWD: c.CWD, ScratchDirs: c.ScratchDirs}
	for _, s := range subs {
		if plain.classifyLine(s, depth+1).Tier != Green {
			lc.opaqueWriter = "$(" + s + ")"
		}
	}
	dir := c.CWD
	for _, s := range segs {
		argv := stripPrefixes(s.argv)
		name := baseCmd(argv)
		if len(argv) != len(s.argv) && lc.envTainted == "" {
			lc.envTainted = "variable assignment " + s.argv[0]
		}
		if envChangers[name] && lc.envTainted == "" {
			lc.envTainted = name
		}
		var docs []*redirect
		for _, r := range s.redirects {
			switch {
			case r.heredoc:
				docs = append(docs, r)
			case isOutputRedirect(r.op) && !harmlessPaths[r.target]:
				lc.noteWrite(dir, r.target)
			}
		}
		if name == "tee" || name == "touch" {
			for _, a := range argv[1:] {
				if !strings.HasPrefix(a, "-") {
					lc.noteWrite(dir, a)
				}
			}
		}
		if len(docs) > 0 && (name == "cat" || name == "tee") {
			for _, t := range outputTargets(s, argv) {
				if p := absIn(dir, t); p != "" {
					lc.heredocWrites[p] = docs[len(docs)-1]
				}
			}
		}
		switch {
		case len(argv) == 0, name == "cd":
		case name == "pushd" || name == "popd":
			lc.dirUncertain = true
		case isScriptCall(argv):
			lc.scriptRuns++
		case knownTargetWriters[name]:
		default:
			var fv Verdict
			pc := *plain
			pc.CWD = dir
			pc.classifySegment(segment{argv: argv}, &fv, depth+1)
			if fv.Tier != Green && lc.opaqueWriter == "" {
				lc.opaqueWriter = strings.Join(argv, " ")
			}
		}
		dir = c.nextDir(s, dir)
	}
	return lc
}

func (lc *lineCtx) noteWrite(dir, target string) {
	if strings.Contains(target, "$") {
		if lc.opaqueWriter == "" {
			lc.opaqueWriter = "write to " + target
		}
		return
	}
	if p := absIn(dir, target); p != "" {
		lc.writes[p] = true
	} else if lc.opaqueWriter == "" {
		lc.opaqueWriter = "write to " + target
	}
}

// outputTargets lists the files a segment writes: output redirects plus, for
// tee, its file arguments.
func outputTargets(s segment, argv []string) []string {
	var out []string
	for _, r := range s.redirects {
		if !r.heredoc && isOutputRedirect(r.op) && !harmlessPaths[r.target] {
			out = append(out, r.target)
		}
	}
	if baseCmd(argv) == "tee" {
		for _, a := range argv[1:] {
			if !strings.HasPrefix(a, "-") {
				out = append(out, a)
			}
		}
	}
	return out
}

// absIn resolves p against dir, or returns "" when that is not possible.
func absIn(dir, p string) string {
	if p == "" || strings.Contains(p, "$") || strings.HasPrefix(p, "~") {
		return ""
	}
	r := resolveAgainst(dir, p)
	if !filepath.IsAbs(r) {
		return ""
	}
	return filepath.Clean(r)
}

// nextDir returns the working directory after segment s ran in dir: `cd x`
// moves it, anything unresolvable makes it unknown ("").
func (c *Classifier) nextDir(s segment, dir string) string {
	argv := stripPrefixes(s.argv)
	if baseCmd(argv) != "cd" {
		return dir
	}
	if len(argv) == 1 {
		return c.home()
	}
	a := c.expandHome(argv[1]) // cd ~, cd ~/x, cd $HOME
	if a == "-" || strings.Contains(a, "$") || strings.HasPrefix(a, "-") {
		return ""
	}
	return absIn(dir, a)
}

// shellScriptArg returns the script a shell is asked to run, skipping its
// option flags. ok is false for -c, -s, -i and for no script at all.
func shellScriptArg(args []string) (string, bool) {
	i, ok := shellScriptIndex(args)
	if !ok {
		return "", false
	}
	return args[i], true
}

func shellScriptIndex(args []string) (int, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if i+1 < len(args) {
				return i + 1, true
			}
			return 0, false
		case a == "-o" || a == "+o":
			i++
		case strings.HasPrefix(a, "-") || strings.HasPrefix(a, "+"):
			if strings.ContainsAny(a, "csi") && !strings.HasPrefix(a, "--") {
				return 0, false
			}
		default:
			return i, true
		}
	}
	return 0, false
}

var safeShellOptions = map[string]bool{"pipefail": true, "errexit": true, "nounset": true, "xtrace": true, "noclobber": true}

// strictShellFlags reports whether every flag before the script is one that
// cannot change what or how the script runs (no rc files, login, -i, --).
func strictShellFlags(flags []string) bool {
	for i := 0; i < len(flags); i++ {
		a := flags[i]
		switch {
		case a == "-o" || a == "+o":
			if i+1 >= len(flags) || !safeShellOptions[flags[i+1]] {
				return false
			}
			i++
		case a == "--norc" || a == "--noprofile" || a == "--posix":
		case len(a) > 1 && (a[0] == '-' || a[0] == '+') && strings.Trim(a[1:], "euxvn") == "":
		default:
			return false
		}
	}
	return true
}

// shellNoExec reports whether argv is a shell told only to read a script
// file (-n, -o noexec: a syntax check), so the script does not run. Not
// with -c (later args are $0...), -s, or -i (interactive shells ignore -n).
func shellNoExec(argv []string) bool {
	if len(argv) == 0 || !shells[baseCmd(argv)] {
		return false
	}
	args := argv[1:]
	end, ok := shellScriptIndex(args)
	if !ok {
		return false
	}
	noexec := false
	for i := 0; i < end; i++ {
		a := args[i]
		switch {
		case a == "-o" || a == "+o":
			// zsh reads option names loosely (+o NO_EXEC, -o exec), so
			// anything but -o noexec or a harmless option fails closed.
			if i+1 >= end {
				return false
			}
			switch v := args[i+1]; {
			case a == "-o" && v == "noexec":
				noexec = true
			case !safeShellOptions[v]:
				return false
			}
			i++
		case len(a) > 1 && a[0] == '+':
			// +ox noexec: o takes the next word, which may undo -n.
			if strings.ContainsAny(a[1:], "oO") {
				return false
			}
			if strings.ContainsRune(a[1:], 'n') {
				noexec = false
			}
		case strings.HasPrefix(a, "--"):
			// --login and --rcfile read startup files.
			if a != "--norc" && a != "--noprofile" && a != "--posix" {
				return false
			}
		case len(a) > 1 && a[0] == '-':
			// o in a cluster (-no exec) takes the next word as its option.
			if strings.ContainsAny(a[1:], "csilo") {
				return false
			}
			if strings.ContainsRune(a[1:], 'n') {
				noexec = true
			}
		}
	}
	return noexec
}

// shellReadsInput reports whether a shell's flags make it read commands from
// somewhere other than stdin (so a heredoc on stdin is not the whole script).
func shellReadsInput(args []string) bool {
	_, ok := shellScriptArg(args)
	return ok
}

// isScriptCall reports whether argv runs a script file: `bash x.sh` or a
// direct `./x.sh` / `/path/x.sh`.
func isScriptCall(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	if shells[baseCmd(argv)] {
		_, ok := shellScriptArg(argv[1:])
		return ok
	}
	return strings.Contains(argv[0], "/") && !greenCmds[baseCmd(argv)]
}

// classifyHeredocs classifies the heredoc bodies on segment s. A body fed to
// a shell is that shell's script; a body that cat/tee writes into a scratch
// dir (or prints) is plain data; any other body is classified as commands, as
// the parser did before it understood heredocs. It reports whether a shell's
// script came from a heredoc.
func (c *Classifier) classifyHeredocs(s segment, name string, args []string, v *Verdict, depth int) bool {
	var docs []*redirect
	for _, r := range s.redirects {
		if r.heredoc {
			docs = append(docs, r)
		}
	}
	if len(docs) == 0 {
		return false
	}
	switch {
	case shells[name]:
		for _, d := range docs {
			v.merge(c.classifyLine(d.body, depth+1))
		}
		return true
	case (name == "cat" || name == "tee") && c.dataSink(s):
		return false
	default:
		for _, d := range docs {
			v.merge(c.classifyLine(d.body, depth+1))
		}
		return false
	}
}

// dataSink reports whether a cat/tee segment only prints its input or writes
// it under a scratch dir.
func (c *Classifier) dataSink(s segment) bool {
	targets := outputTargets(s, stripPrefixes(s.argv))
	if len(targets) == 0 {
		return !s.piped
	}
	for _, t := range targets {
		p := absIn(c.CWD, t)
		if p == "" || !c.inScratch(p) {
			return false
		}
	}
	return true
}

// shebangRe lists the interpreters a directly-executed script may name.
var shebangRe = regexp.MustCompile(`^#!\s*(/bin/bash|/bin/sh|/usr/bin/env bash|/usr/bin/env sh|/opt/homebrew/bin/bash|/usr/local/bin/bash)\s*$`)

// ShebangInterp returns the interpreter a directly-executed script's first
// line names, when it is one we analyse (bash or sh); "" otherwise.
func ShebangInterp(content []byte) string {
	first := string(content)
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	m := shebangRe.FindStringSubmatch(first)
	if m == nil {
		return ""
	}
	return m[1]
}

// contentProblem reports bytes we refuse to analyse: our parser and bash
// would read them differently.
func contentProblem(b []byte) string {
	switch {
	case len(b) > maxScriptBytes:
		return "too large to analyse"
	case strings.IndexByte(string(b), 0) >= 0:
		return "contains a NUL byte"
	case strings.IndexByte(string(b), '\r') >= 0:
		return "contains a carriage return"
	case !utf8.Valid(b):
		return "is not valid UTF-8"
	}
	return ""
}

// scriptCall is what classifySegment knows about one script invocation.
type scriptCall struct {
	name     string // shell name, or the script's base name for ./x.sh
	token    string // script path as written
	tokenDyn bool   // the path contains an expansion
	direct   bool   // ./x.sh rather than bash x.sh
	flags    []string
	prefixed bool // VAR=x before the command
}

// scriptVerdict judges a script call by the script's contents. It returns
// false (the caller keeps its old verdict) when contents cannot be used.
func (c *Classifier) scriptVerdict(call scriptCall, v *Verdict, depth int) bool {
	if c.Snap == nil || c.inner || depth > maxDepth || call.tokenDyn {
		return false
	}
	if !call.direct && (call.name != "bash" && call.name != "sh" || !strictShellFlags(call.flags)) {
		return false
	}
	lc := c.line
	if lc == nil {
		lc = &lineCtx{}
	}
	abs := call.token
	if !filepath.IsAbs(abs) {
		// A relative script resolves against the shell's cwd at run time: only
		// trust our idea of it when it came from the hook payload or an
		// absolute cd earlier in the line.
		if lc.dirUncertain || c.CWD == "" || !(c.CWDTrusted || c.cwdFromCd) {
			return false
		}
		abs = absIn(c.CWD, call.token)
	}
	if abs == "" || strings.ContainsAny(call.token, "$`*?[{~") {
		return false
	}
	abs = filepath.Clean(abs)
	if c.scratchRootFor(abs) == "" {
		return false
	}
	label := call.name + ": script " + abs
	realPath := abs
	if call.prefixed || lc.envTainted != "" {
		v.raise(Red, label+" runs with a changed environment ("+firstNonEmptyStr(lc.envTainted, "prefix assignment")+")")
		return true
	}
	var content []byte
	if hd := lc.heredocWrites[abs]; hd != nil {
		if !hd.quoted {
			v.raise(Red, label+" is written by an unquoted heredoc in the same command (expansions run)")
			return true
		}
		if lc.scriptRuns > 1 || lc.opaqueWriter != "" {
			v.raise(Red, label+" may be changed by another part of the same command")
			return true
		}
		content = []byte(hd.body)
	} else {
		if lc.writes[abs] || lc.opaqueWriter != "" || lc.scriptRuns > 1 {
			v.raise(Red, label+" may be changed by another part of the same command")
			return true
		}
		// One read through one descriptor; where the file really is comes
		// from that descriptor, not from another lookup of the path.
		snap := c.Snap.Read(abs)
		switch {
		case errors.Is(snap.Err, ErrTooLarge):
			v.raise(Red, label+" too large to analyse")
			return true
		case snap.Err != nil:
			return false // missing, symlink swapped in, FIFO, device: opaque
		}
		if c.scratchRootFor(snap.Real) == "" {
			v.raise(Red, label+" resolves outside the scratch dirs ("+snap.Real+")")
			return true
		}
		realPath = snap.Real
		content = snap.Data
	}
	if p := contentProblem(content); p != "" {
		v.raise(Red, label+" "+p)
		return true
	}
	interp := ""
	if call.direct {
		if interp = ShebangInterp(content); interp == "" {
			v.raise(Red, label+": no bash/sh shebang, cannot analyse")
			return true
		}
	}
	confine := scriptConfine(realPath)
	ok, detail := c.scriptReadOnly(string(content), confine, depth+1)
	if ok {
		v.raise(Yellow, "")
		v.Scripts = append(v.Scripts, JudgedScript{Token: call.token, Path: abs, Interp: interp, Content: content})
		return true
	}
	v.raise(Red, fmt.Sprintf("%s is not provably read-only (%s)", label, detail))
	inner := (&Classifier{Home: c.Home, ScratchDirs: c.ScratchDirs}).classifyLine(string(content), depth+1)
	for _, r := range inner.Reasons {
		v.Reasons = append(v.Reasons, "script: "+r)
	}
	return true
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// scriptConfine is the only directory a script may write in: its own
// directory, unless that is a shared system temp dir itself ("" = no writes).
func scriptConfine(abs string) string {
	dir := resolveExisting(filepath.Dir(abs))
	for _, d := range DefaultScratchDirs() {
		if resolveExisting(d) == dir {
			return ""
		}
	}
	return dir
}

// ── Script walker ───────────────────────────────────────────────────────────

// shellKeywordPrefix are words that may precede a command in a script.
var shellKeywordPrefix = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "do": true, "while": true,
	"until": true, "!": true, "{": true, "time": true,
}

// shellSyntaxOnly are words that form a whole segment of script syntax.
var shellSyntaxOnly = map[string]bool{
	"fi": true, "done": true, "esac": true, "}": true, ";;": true, "case": true, "in": true, "function": true,
}

// scriptBuiltins change only shell state (variables are checked separately).
var scriptBuiltins = map[string]bool{
	"set": true, "local": true, "declare": true, "typeset": true, "export": true, "readonly": true,
	"read": true, "shift": true, "return": true, "exit": true, "break": true, "continue": true,
	"unset": true, ":": true, "true": true, "false": true, "[[": true, "]]": true, "wait": true,
	"getopts": true, "shopt": true, "echo": true, "printf": true, "test": true, "[": true,
}

// pureReaders read files and print; no flag or argument makes them write a
// file or run another program, so any arguments are fine.
var pureReaders = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "wc": true, "pwd": true, "which": true,
	"stat": true, "du": true, "df": true, "cut": true, "tr": true, "diff": true, "cmp": true,
	"basename": true, "dirname": true, "realpath": true, "readlink": true, "whoami": true,
	"uname": true, "id": true, "nl": true, "column": true, "sleep": true, "seq": true, "expr": true,
	"md5sum": true, "shasum": true, "sha256sum": true, "comm": true, "join": true, "paste": true,
	"tac": true, "rev": true, "fold": true, "grep": true, "egrep": true, "fgrep": true, "date": true,
	"jq": true, "true": true, "false": true, "test": true, "[": true, "echo": true,
}

// sysBinDirs may prefix a command name (/usr/bin/git).
var sysBinDirs = map[string]bool{
	"/bin": true, "/usr/bin": true, "/usr/local/bin": true, "/opt/homebrew/bin": true, "/usr/sbin": true, "/sbin": true,
}

var (
	arrayAssignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\[[^\]]*\]\+?=`)
	funcDefRe     = regexp.MustCompile(`(?m)^\s*(?:function\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*\(\s*\)`)
	funcKwRe      = regexp.MustCompile(`(?m)^\s*function\s+([A-Za-z_][A-Za-z0-9_]*)`)
	cmdNameRe     = regexp.MustCompile(`^[A-Za-z0-9_.+:\[\]-]+$`)
	// dangerVarRe: variables that change how commands resolve or run.
	dangerVarRe = regexp.MustCompile(`^(PATH|IFS|BASH_ENV|ENV|CDPATH|GLOBIGNORE|BASHOPTS|SHELLOPTS|PS4|` +
		`PROMPT_COMMAND|HOME|TMPDIR|PWD|PAGER|MANPAGER|LESSOPEN|LESSCLOSE|EDITOR|VISUAL|BROWSER|SHELL|` +
		`(?:GIT|DYLD|LD|BASH|SSH|SUDO|RIPGREP|JQ|GREP)_\w*)$`)
	assignExpRe = regexp.MustCompile(`\$\{\w+:?[=?]`) // ${X=..} / ${X:=..} assign; ${X?..} aborts
	refRe       = regexp.MustCompile(`\$\{([A-Za-z_]\w*)([%#]{1,2}[^}$` + "`" + `]*)?\}|\$([A-Za-z_]\w*)`)
	arithRe     = regexp.MustCompile(`\(\(|\$\[|(^|[;&|\s])let\s|\[\[[^\]]*\s-(eq|ne|lt|le|gt|ge)\s|\$\{\w+:[^}]*\$`)
	assocDeclRe = regexp.MustCompile(`(?m)\b(?:declare|local|typeset)\s+-A\s+([A-Za-z_]\w*)`)
	subscriptRe = regexp.MustCompile(`([A-Za-z_]\w*)\[[^\]]*\$[^\]]*\]`)
	braceExpRe  = regexp.MustCompile(`\{[^{}]*(,|\.\.)[^{}]*\}`)
	sedUnsafeRe = regexp.MustCompile(`[wWe]`)
	awkUnsafeRe = regexp.MustCompile(`system|getline|[|>@]`)
	safeValueRe = regexp.MustCompile(`^[A-Za-z0-9._/@+:%^=,][A-Za-z0-9._/@+:%^=,-]*$`)
)

// trustedEnv are variables whose values come from the trusted process
// environment and are absolute paths or plain names.
var trustedEnv = map[string]bool{"HOME": true, "TMPDIR": true, "USER": true, "PWD": true}

// scriptVar is what is known statically about a variable.
type scriptVar struct {
	literal string   // its value, when assigned one literal (prologue)
	known   bool     // literal is meaningful
	values  []string // possible values of a literal for-loop variable
	trusted bool     // value is derived only from trusted env (begins with /)
}

// scriptState is what scriptReadOnly tracks while walking a script.
type scriptState struct {
	c       *Classifier
	confine string
	funcs   map[string]bool
	vars    map[string]scriptVar
	content string
	depth   int
}

// scriptReadOnly reports whether content runs only read-only commands and
// writes only strictly inside confine. detail names the first offending part.
func (c *Classifier) scriptReadOnly(content, confine string, depth int) (bool, string) {
	if assignExpRe.MatchString(content) {
		return false, "assigns through ${VAR=...} expansion"
	}
	// Arithmetic contexts evaluate variable contents as expressions, and an
	// array subscript inside one can run $(...): x='a[$(rm -rf ~)]'; ((x)).
	if arithRe.MatchString(content) {
		return false, "uses arithmetic evaluation"
	}
	assoc := map[string]bool{}
	for _, m := range assocDeclRe.FindAllStringSubmatch(content, -1) {
		assoc[m[1]] = true
	}
	for _, m := range subscriptRe.FindAllStringSubmatch(content, -1) {
		if !assoc[m[1]] {
			return false, "indexes array " + m[1] + " with an expansion (arithmetic)"
		}
	}
	st := &scriptState{c: c, confine: confine, funcs: map[string]bool{}, vars: map[string]scriptVar{},
		content: content, depth: depth}
	for _, m := range funcDefRe.FindAllStringSubmatch(content, -1) {
		st.funcs[m[1]] = true
	}
	for _, m := range funcKwRe.FindAllStringSubmatch(content, -1) {
		st.funcs[m[1]] = true
	}
	st.prologue()
	return st.walk(content)
}

// bareCount counts occurrences of identifier name that are not a plain
// $name or ${name} read: each is a possible (re)assignment.
func (st *scriptState) bareCount(name string) int {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
	n := 0
	for _, loc := range re.FindAllStringIndex(st.content, -1) {
		s, e := loc[0], loc[1]
		switch {
		case s >= 1 && st.content[s-1] == '$':
		case s >= 2 && st.content[s-2:s] == "${" && e < len(st.content) && st.content[e] == '}':
		default:
			n++
		}
	}
	return n
}

// prologue records the variables assigned at the top of the script before
// any other command (comments, blank lines and `set` options may precede):
// those assignments always run first. A variable counts only when it is
// never assigned again anywhere.
func (st *scriptState) prologue() {
	for _, raw := range strings.Split(st.content, "\n") {
		t := strings.TrimSpace(raw)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if strings.ContainsAny(t, ";&|<>`()") {
			return
		}
		if t == "set" || strings.HasPrefix(t, "set ") {
			continue
		}
		segs, subs, err := parseShell(t)
		if err != nil || len(segs) != 1 || len(subs) > 0 || len(segs[0].redirects) > 0 {
			return
		}
		s := segs[0]
		for _, a := range s.argv {
			if !isAssign(a) {
				return
			}
		}
		for i, a := range s.argv {
			name, val := a[:strings.IndexByte(a, '=')], a[strings.IndexByte(a, '=')+1:]
			if st.bareCount(name) != 1 {
				continue
			}
			switch {
			case !s.dyn[i] && !s.meta[i] && val != "" && !strings.ContainsAny(val, " \t*?[{~"):
				st.vars[name] = scriptVar{literal: val, known: true}
			case s.dyn[i] && st.trustedValue(val):
				st.vars[name] = scriptVar{trusted: true}
			}
		}
	}
}

// trustedValue reports whether an expansion-only value begins with a
// reference to a trusted variable, so it is an absolute path.
func (st *scriptState) trustedValue(val string) bool {
	if strings.Contains(val, "$SUBST") || strings.Contains(val, "`") {
		return false
	}
	loc := refRe.FindStringSubmatchIndex(val)
	if loc == nil || loc[0] != 0 {
		return false
	}
	var name string
	if loc[2] >= 0 {
		name = val[loc[2]:loc[3]]
	} else {
		name = val[loc[6]:loc[7]]
	}
	v := st.vars[name]
	if !trustedEnv[name] && !v.trusted && !(v.known && strings.HasPrefix(v.literal, "/")) {
		return false
	}
	rest := refRe.ReplaceAllString(val, "")
	return !strings.Contains(rest, "$")
}

// safeArg reports whether argument i of s cannot become an option or extra
// words at run time: literal with no glob/brace, or expansions of variables
// with statically safe values.
func (st *scriptState) safeArg(s segment, i int) bool {
	a := s.argv[i]
	if segMeta(s, i) || braceExpRe.MatchString(a) {
		return false
	}
	if !segDyn(s, i) {
		return true
	}
	if strings.Contains(a, "$SUBST") || strings.Contains(a, "`") {
		return false
	}
	ok := true
	for _, m := range refRe.FindAllStringSubmatchIndex(a, -1) {
		name := ""
		if m[2] >= 0 {
			name = a[m[2]:m[3]]
		} else {
			name = a[m[6]:m[7]]
		}
		v := st.vars[name]
		switch {
		case trustedEnv[name], v.trusted:
		case v.known && safeValueRe.MatchString(v.literal):
		case len(v.values) > 0:
		default:
			ok = false
		}
	}
	if strings.Contains(refRe.ReplaceAllString(a, ""), "$") {
		return false
	}
	if strings.HasPrefix(a, "-") {
		return false
	}
	return ok
}

// concrete returns the run-time value of a write target, when it is fully
// known: a literal, or literal prologue variables and $HOME.
func (st *scriptState) concrete(tok string, dyn bool) (string, bool) {
	if !dyn {
		return tok, true
	}
	if strings.Contains(tok, "$SUBST") || strings.Contains(tok, "`") {
		return "", false
	}
	ok := true
	out := refRe.ReplaceAllStringFunc(tok, func(m string) string {
		sm := refRe.FindStringSubmatch(m)
		if sm[2] != "" {
			ok = false // ${X%...} forms are not evaluated
			return ""
		}
		name := sm[1] + sm[3]
		if v := st.vars[name]; v.known {
			return v.literal
		}
		if name == "HOME" && st.c.home() != "" && st.bareCount("HOME") == 0 {
			return st.c.home()
		}
		ok = false
		return ""
	})
	if !ok || strings.Contains(out, "$") {
		return "", false
	}
	return out, true
}

// writable reports whether the script may write tok: an absolute path,
// strictly inside the confine dir, symlinks resolved, no .. or globs.
func (st *scriptState) writable(tok string, dyn, meta bool) bool {
	if !dyn && harmlessPaths[tok] {
		return true
	}
	if st.confine == "" || meta {
		return false
	}
	p, ok := st.concrete(tok, dyn)
	if !ok || p == "" || !filepath.IsAbs(p) || strings.ContainsAny(p, " \t\n*?[]{}~") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return false
		}
	}
	if strings.HasPrefix(p, "/dev/") {
		return harmlessPaths[p]
	}
	return strictlyInside(st.confine, filepath.Clean(p))
}

func segDyn(s segment, i int) bool {
	if s.dyn == nil || i >= len(s.dyn) {
		return strings.ContainsAny(s.argv[i], "$`")
	}
	return s.dyn[i]
}

func segUdyn(s segment, i int) bool {
	if s.udyn == nil || i >= len(s.udyn) {
		return strings.ContainsAny(s.argv[i], "$`")
	}
	return s.udyn[i]
}

func segMeta(s segment, i int) bool {
	if s.meta == nil || i >= len(s.meta) {
		return strings.ContainsAny(s.argv[i], "*?[{~")
	}
	return s.meta[i]
}

func (st *scriptState) walk(content string) (bool, string) {
	if st.depth > maxDepth {
		return false, "nesting too deep"
	}
	segs, subs, err := parseShell(content)
	if err != nil {
		return false, "unparseable: " + err.Error()
	}
	for _, s := range subs {
		inner := *st
		inner.depth++
		if ok, d := inner.walk(s); !ok {
			return false, d
		}
	}
	for _, s := range segs {
		if ok, d := st.segment(s); !ok {
			return false, d
		}
	}
	return true, ""
}

// sub returns segment s from argv index off on (keeping the flag slices).
func sub(s segment, off int) segment {
	out := segment{argv: s.argv[off:]}
	if s.dyn != nil && off <= len(s.dyn) {
		out.dyn, out.udyn, out.meta = s.dyn[off:], s.udyn[off:], s.meta[off:]
	}
	return out
}

func (st *scriptState) segment(s segment) (bool, string) {
	// Redirects first: every write must stay strictly inside the confine.
	for _, r := range s.redirects {
		switch {
		case strings.HasPrefix(r.target, "/dev/tcp") || strings.HasPrefix(r.target, "/dev/udp"):
			return false, "opens a network connection via " + r.target
		case r.heredoc:
		case isOutputRedirect(r.op) || strings.Contains(r.op, ">") && !isDigits(r.target) && r.target != "-":
			if !st.writable(r.target, r.dyn, r.meta) {
				return false, "writes " + r.target + " outside the script's own directory"
			}
		}
	}

	// Assigning a variable that changes how commands run is never allowed.
	for _, a := range s.argv {
		if isAssign(a) || arrayAssignRe.MatchString(a) {
			if n := assignName(a); dangerVarRe.MatchString(n) {
				return false, "assigns " + n + " (changes how commands run)"
			}
		}
	}
	// Pure assignments (A=1, A[k]=v) only set variables.
	all := len(s.argv) > 0
	for _, a := range s.argv {
		if !isAssign(a) && !arrayAssignRe.MatchString(a) {
			all = false
			break
		}
	}
	if all {
		return true, ""
	}
	if len(stripPrefixes(s.argv)) != len(s.argv) {
		return false, "runs " + strings.Join(s.argv, " ") + " with a changed environment"
	}
	off := 0
	for off < len(s.argv) && shellKeywordPrefix[s.argv[off]] {
		off++
	}
	cs := sub(s, off)
	if len(cs.argv) == 0 {
		return true, ""
	}
	// After a keyword, a pure assignment (then x=1) is still just state.
	allAssign := true
	for _, a := range cs.argv {
		if !isAssign(a) && !arrayAssignRe.MatchString(a) {
			allAssign = false
		}
	}
	if allAssign {
		return true, ""
	}
	for _, r := range s.redirects {
		if r.heredoc {
			n := baseCmd(cs.argv)
			if !pureReaders[n] && n != "read" && n != "while" {
				return false, n + " reads a heredoc as input"
			}
		}
	}
	return st.command(cs)
}

// command checks one simple command (argv[0] is the command).
func (st *scriptState) command(s segment) (bool, string) {
	argv := s.argv
	line := strings.Join(argv, " ")
	name := argv[0]
	if shellSyntaxOnly[name] {
		return true, ""
	}
	if segDyn(s, 0) || (segMeta(s, 0) && name != "[" && name != "[[") {
		return false, "command name comes from an expansion or glob: " + argv[0]
	}
	if strings.Contains(name, "/") {
		if !sysBinDirs[filepath.Dir(name)] || filepath.Clean(name) != name {
			return false, "runs " + name + " (not a system binary)"
		}
		name = filepath.Base(name)
	}
	if !cmdNameRe.MatchString(name) {
		return false, "unrecognised command " + name
	}
	args := sub(s, 1)

	// Sensitive paths are never read or written.
	var fv Verdict
	for _, a := range args.argv {
		st.c.checkPath(a, &fv)
	}
	if fv.Tier == Red {
		return false, line + ": " + strings.Join(fv.Reasons, "; ")
	}

	allSafe := func(from int) (bool, string) {
		for i := from; i < len(args.argv); i++ {
			if !st.safeArg(args, i) {
				return false, line + " (argument " + args.argv[i] + " could become an option)"
			}
		}
		return true, ""
	}
	hasFlag := func(bad ...string) bool {
		for _, a := range args.argv {
			for _, b := range bad {
				if a == b || strings.HasPrefix(a, b+"=") || (len(b) == 2 && b[0] == '-' && b[1] != '-' &&
					strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a[1:], rune(b[1]))) {
					return true
				}
			}
		}
		return false
	}

	switch {
	case shellSyntaxOnly[name] || st.funcs[name]:
		// Function bodies are walked where they are defined.
		return true, ""
	case name == "for":
		return st.forLoop(args)
	case name == "select":
		return false, "select is interactive"
	case name == "cd":
		return true, ""
	case name == "printf":
		if hasFlag("-v") || (len(args.argv) > 0 && segDyn(args, 0)) {
			return false, line + " (printf -v or a dynamic format)"
		}
		return true, ""
	case scriptBuiltins[name]:
		dynOK := map[string]bool{"echo": true, "test": true, "[": true, "[[": true, "]]": true,
			"exit": true, "return": true, "shift": true, "wait": true, "break": true, "continue": true}
		for i, a := range args.argv {
			if segDyn(args, i) && !dynOK[name] {
				return false, line + " (dynamic argument to " + name + ")"
			}
			switch name {
			case "export", "declare", "typeset", "local", "readonly", "read", "unset", "getopts":
				if n := assignName(a); dangerVarRe.MatchString(n) {
					return false, line + " (touches " + n + ")"
				}
			}
		}
		return true, ""
	case pureReaders[name]:
		return true, ""
	case name == "git":
		return st.git(args)
	case name == "sort":
		if hasFlag("-o", "--output", "-T", "--temporary-directory", "--compress-program") {
			return false, line + " (sort writes a file or runs a program)"
		}
		return allSafe(0)
	case name == "uniq":
		if len(positionals(args.argv)) > 1 {
			return false, line + " (uniq writes its second argument)"
		}
		return allSafe(0)
	case name == "file":
		if hasFlag("-C", "--compile") {
			return false, line + " (file -C writes a file)"
		}
		return allSafe(0)
	case name == "tree":
		if hasFlag("-o", "--output") {
			return false, line + " (tree -o writes a file)"
		}
		return allSafe(0)
	case name == "rg":
		if hasFlag("--pre", "--pre-glob") {
			return false, line + " (rg --pre runs a program)"
		}
		return allSafe(0)
	case name == "find":
		for _, a := range args.argv {
			if strings.HasPrefix(a, "-exec") || strings.HasPrefix(a, "-ok") || a == "-delete" ||
				strings.HasPrefix(a, "-fprint") || a == "-fls" {
				return false, line + " (find runs, deletes or writes)"
			}
		}
		return allSafe(0)
	case name == "sed":
		if hasFlag("-i", "--in-place", "-f", "--file") {
			return false, line + " (sed edits in place or reads a script file)"
		}
		for i, a := range args.argv {
			if segDyn(args, i) || segMeta(args, i) {
				return false, line + " (dynamic sed argument)"
			}
			if !strings.HasPrefix(a, "-") && sedUnsafeRe.MatchString(a) {
				return false, line + " (sed program may use w/e)"
			}
		}
		return true, ""
	case name == "awk" || name == "gawk" || name == "nawk":
		if hasFlag("-f", "-i", "--file", "--include", "-l", "--load") {
			return false, line + " (awk reads a program file)"
		}
		for i, a := range args.argv {
			if segDyn(args, i) {
				return false, line + " (dynamic awk argument)"
			}
			if !strings.HasPrefix(a, "-") && awkUnsafeRe.MatchString(a) {
				return false, line + " (awk program may write or run commands)"
			}
		}
		return true, ""
	case name == "xargs":
		inner := skipWrapper("xargs", args.argv)
		if len(inner) == 0 {
			return true, "" // xargs alone runs echo
		}
		n := inner[0]
		if strings.Contains(n, "/") || !pureReaders[n] || n == "test" || n == "[" {
			return false, line + " (xargs feeds unseen arguments to " + n + ")"
		}
		return true, ""
	case name == "env":
		if len(args.argv) > 0 {
			return false, line + " (env changes the environment of a command)"
		}
		return true, ""
	case name == "time" || name == "nice" || name == "timeout" || name == "command" || name == "stdbuf" || name == "exec":
		innerArgv := skipWrapper(name, args.argv)
		if name == "command" && len(args.argv) > 0 && (args.argv[0] == "-v" || args.argv[0] == "-V") {
			return true, ""
		}
		if len(innerArgv) == 0 {
			if name == "exec" && len(args.argv) == 0 {
				return true, "" // exec with only redirects
			}
			return false, line
		}
		return st.command(sub(s, len(argv)-len(innerArgv)))
	case name == "mkdir" || name == "touch" || name == "rm" || name == "rmdir" || name == "chmod" || name == "tee":
		start := 0
		var pos []int
		for i, a := range args.argv {
			if !strings.HasPrefix(a, "-") || segDyn(args, i) {
				pos = append(pos, i)
			}
		}
		if name == "chmod" && len(pos) > 0 {
			if segDyn(args, pos[0]) {
				return false, line + " (dynamic chmod mode)"
			}
			pos = pos[1:]
		}
		if len(pos) == 0 && name != "tee" {
			return false, line + " (no target)"
		}
		_ = start
		for _, i := range pos {
			if !st.writable(args.argv[i], segDyn(args, i), segMeta(args, i)) {
				return false, line + " (writes " + args.argv[i] + " outside the script's own directory)"
			}
		}
		return true, ""
	case name == "mktemp":
		for _, a := range args.argv {
			if a != "-d" && a != "-q" && a != "-u" {
				return false, line + " (mktemp with a path)"
			}
		}
		return true, ""
	}
	return false, "runs " + name + " (not on the read-only list)"
}

// forLoop records a for loop's variable: its values when every item is a
// literal word that cannot be an option.
func (st *scriptState) forLoop(args segment) (bool, string) {
	if len(args.argv) < 2 || args.argv[1] != "in" {
		return true, "" // `for x` / C-style: variable stays unknown
	}
	name := args.argv[0]
	if dangerVarRe.MatchString(name) {
		return false, "for loop assigns " + name
	}
	var vals []string
	for i := 2; i < len(args.argv); i++ {
		if segDyn(args, i) || segMeta(args, i) || !safeValueRe.MatchString(args.argv[i]) {
			delete(st.vars, name)
			return true, ""
		}
		vals = append(vals, args.argv[i])
	}
	if st.bareCount(name) == 1 && len(vals) > 0 {
		st.vars[name] = scriptVar{values: vals}
	} else {
		delete(st.vars, name)
	}
	return true, ""
}

// gitReadSubs are git subcommands that only read (with the checks below).
var gitReadSubs = map[string]bool{
	"status": true, "log": true, "show": true, "diff": true, "rev-parse": true, "rev-list": true,
	"ls-files": true, "ls-tree": true, "ls-remote": true, "for-each-ref": true, "merge-base": true,
	"symbolic-ref": true, "describe": true, "cat-file": true, "name-rev": true, "count-objects": true,
	"shortlog": true, "blame": true, "worktree": true, "branch": true, "remote": true, "config": true,
	"tag": true, "grep": true,
}

// git allows read-only git: -C <dir> as the only global option, a read
// subcommand, no option that writes a file or runs a program, and no
// argument that could turn into one at run time.
func (st *scriptState) git(args segment) (bool, string) {
	line := "git " + strings.Join(args.argv, " ")
	i := 0
	for i < len(args.argv) && strings.HasPrefix(args.argv[i], "-") && !segDyn(args, i) {
		switch a := args.argv[i]; {
		case a == "-C":
			if i+1 >= len(args.argv) || segUdyn(args, i+1) && !st.safeArg(args, i+1) || segMeta(args, i+1) {
				return false, line + " (unquoted or globbed -C dir)"
			}
			i += 2
		case a == "--no-pager" || a == "-P" || a == "--no-optional-locks":
			i++
		default:
			return false, line + " (git global option " + a + ")"
		}
	}
	if i >= len(args.argv) || segDyn(args, i) {
		return false, line + " (no literal subcommand)"
	}
	sub := args.argv[i]
	rest := segment{argv: args.argv[i+1:]}
	if args.dyn != nil {
		rest.dyn, rest.udyn, rest.meta = args.dyn[i+1:], args.udyn[i+1:], args.meta[i+1:]
	}
	if !gitReadSubs[sub] {
		return false, line + " (git " + sub + " is not read-only)"
	}
	for j, a := range rest.argv {
		if !st.safeArg(rest, j) {
			return false, line + " (argument " + a + " could become an option)"
		}
		for _, bad := range []string{"--output", "--upload-pack", "--exec", "--receive-pack", "--ext-diff",
			"--textconv", "--open-files-in-pager", "--config-env", "--edit", "--file", "--add", "--unset",
			"--replace-all", "--rename-section", "--remove-section", "--delete", "--move", "--copy",
			"--set-upstream-to", "--unset-upstream", "--create-reflog", "--force"} {
			if a == bad || strings.HasPrefix(a, bad+"=") {
				return false, line + " (" + bad + ")"
			}
		}
		if strings.HasPrefix(a, "-O") || (sub == "ls-remote" && a == "-u") {
			return false, line + " (runs a program)"
		}
	}
	pos := positionals(rest.argv)
	switch sub {
	case "branch", "tag":
		if len(pos) > 0 {
			return false, line + " (git " + sub + " with a name creates or changes it)"
		}
		for _, a := range rest.argv {
			if a == "-d" || a == "-D" || a == "-m" || a == "-M" || a == "-c" || a == "-C" || a == "-f" || a == "-u" {
				return false, line + " (changes branches/tags)"
			}
		}
	case "remote":
		if len(rest.argv) > 1 || len(rest.argv) == 1 && rest.argv[0] != "-v" {
			return false, line + " (git remote may change remotes)"
		}
	case "config":
		get := false
		for _, a := range rest.argv {
			if a == "--get" || a == "--get-all" || a == "--list" || a == "-l" || a == "--get-regexp" {
				get = true
			}
		}
		if !get {
			return false, line + " (git config write)"
		}
	case "symbolic-ref":
		if len(pos) > 1 {
			return false, line + " (git symbolic-ref write)"
		}
	case "worktree":
		if len(rest.argv) == 0 || rest.argv[0] != "list" {
			return false, line + " (git worktree change)"
		}
	}
	return true, ""
}

// assignName is the variable name in NAME=v, NAME+=v, NAME[k]=v, or NAME.
func assignName(a string) string {
	if i := strings.IndexAny(a, "=[+"); i >= 0 {
		return a[:i]
	}
	return a
}

func positionals(args []string) []string {
	var out []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			out = append(out, a)
		}
	}
	return out
}

func firstOr(a []string, d string) string {
	if len(a) > 0 {
		return a[0]
	}
	return d
}

// rebaseGitDir maps a git working dir derived from the tracked cwd onto the
// line's original cwd.
func rebaseGitDir(dir, tracked, base string) string {
	if dir == "" || tracked == "" {
		return base
	}
	if within(tracked, dir) {
		rel, err := filepath.Rel(tracked, dir)
		if err == nil {
			return filepath.Join(base, rel)
		}
	}
	return dir
}

// ── Pinning: run exactly the judged bytes ──────────────────────────────────

// PinScript is a script to be run from fixed bytes.
type PinScript struct {
	Token   string // path as written in the command
	Interp  string // "" for `bash|sh <token>`; else interpreter for ./x.sh
	Content []byte
}

// shellQuote single-quotes s for bash and zsh. Pieces between apostrophes
// are quoted separately and joined with \' so the result never contains ”
// (which zsh's RC_QUOTES would read as a literal apostrophe inside a string).
func shellQuote(s string) string {
	parts := strings.Split(s, "'")
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteString(`\'`)
		}
		if p != "" {
			b.WriteString("'" + p + "'")
		}
	}
	if b.Len() == 0 {
		return "''"
	}
	return b.String()
}

func isWordBoundary(b byte) bool {
	return strings.IndexByte(" \t\n;&|()<>", b) >= 0
}

// PinCommand rewrites line so each script runs from the given bytes instead
// of re-reading its file: `bash x.sh a` becomes `bash -c '<bytes>' x.sh a`
// ($0 and the arguments are unchanged) and `./x.sh a` becomes
// `<interp> -c '<bytes>' ./x.sh a`. Each token must occur exactly once in the
// line, as a whole word, or the line is refused.
func PinCommand(line string, pins []PinScript) (string, error) {
	type span struct {
		start, end int
		repl       string
	}
	var spans []span
	for _, p := range pins {
		if p.Token == "" || strings.Count(line, p.Token) != 1 {
			return "", fmt.Errorf("script %s does not occur exactly once in the command", p.Token)
		}
		if prob := contentProblem(p.Content); prob != "" {
			return "", fmt.Errorf("script %s %s", p.Token, prob)
		}
		s := strings.Index(line, p.Token)
		e := s + len(p.Token)
		// Include surrounding quotes, if any.
		if s > 0 && e < len(line) && (line[s-1] == '\'' || line[s-1] == '"') && line[e] == line[s-1] {
			s--
			e++
		}
		if (s > 0 && !isWordBoundary(line[s-1])) || (e < len(line) && !isWordBoundary(line[e])) {
			return "", fmt.Errorf("script %s is not a separate word in the command", p.Token)
		}
		repl := "-c " + shellQuote(string(p.Content)) + " " + line[s:e]
		if p.Interp != "" {
			repl = p.Interp + " " + repl
		}
		spans = append(spans, span{s, e, repl})
	}
	for i := range spans {
		for j := range spans {
			if i != j && spans[i].start < spans[j].end && spans[j].start < spans[i].end {
				return "", fmt.Errorf("overlapping scripts")
			}
		}
	}
	out := line
	// Apply right to left so earlier offsets stay valid.
	for len(spans) > 0 {
		k := 0
		for i := range spans {
			if spans[i].start > spans[k].start {
				k = i
			}
		}
		sp := spans[k]
		out = out[:sp.start] + sp.repl + out[sp.end:]
		spans = append(spans[:k], spans[k+1:]...)
	}
	return out, nil
}

// PinsFromVerdict returns the pins for scripts the verdict judged.
func PinsFromVerdict(v Verdict) []PinScript {
	out := make([]PinScript, 0, len(v.Scripts))
	for _, s := range v.Scripts {
		out = append(out, PinScript{Token: s.Token, Interp: s.Interp, Content: s.Content})
	}
	return out
}

// ScriptRef is a script file a command line runs.
type ScriptRef struct {
	Path  string `json:"path"`
	Token string `json:"token,omitempty"`
	// SHA256 of the contents at the time it was read ("" when unreadable).
	SHA256 string `json:"sha256"`
	// Trusted is false when the same command line could change the file
	// before it runs, so its current contents say nothing.
	Trusted bool `json:"trusted"`
	// Direct is true for ./x.sh (the shebang picks the interpreter).
	Direct bool `json:"direct,omitempty"`
	// Content is the (possibly truncated) text. Not stored.
	Content string `json:"-"`
	// Full is the whole file as read (for pinning). Not stored.
	Full []byte `json:"-"`
}

// PinsFromRefs returns pins for every script ref, or an error when one
// cannot be pinned (unreadable, or a direct script without a bash/sh shebang).
func PinsFromRefs(refs []ScriptRef) ([]PinScript, error) {
	out := make([]PinScript, 0, len(refs))
	for _, r := range refs {
		if r.Full == nil || r.SHA256 == "" {
			return nil, fmt.Errorf("script %s unreadable", r.Path)
		}
		interp := ""
		if r.Direct {
			if interp = ShebangInterp(r.Full); interp == "" {
				return nil, fmt.Errorf("script %s has no bash/sh shebang", r.Path)
			}
		}
		out = append(out, PinScript{Token: r.Token, Interp: interp, Content: r.Full})
	}
	return out, nil
}

// ScriptRefs lists the script files that line runs (bash x.sh, ./x.sh),
// resolved against cwd, with their sha256 read once through snap (nil: no
// reads). It is what an allow rule pins (STA-868).
func ScriptRefs(line, cwd string, snap *Snapshotter, contentLimit int) []ScriptRef {
	c := &Classifier{CWD: cwd}
	var out []ScriptRef
	var walk func(line, cwd string, depth int)
	walk = func(line, cwd string, depth int) {
		if depth > maxDepth {
			return
		}
		segs, subs, err := parseShell(line)
		if err != nil {
			return
		}
		for _, s := range subs {
			walk(s, cwd, depth+1)
		}
		lc := c.lineContext(line, segs, subs, depth)
		dir := cwd
		for _, s := range segs {
			argv := stripPrefixes(s.argv)
			for _, r := range s.redirects {
				if r.heredoc && shells[baseCmd(argv)] {
					walk(r.body, dir, depth+1)
				}
			}
			if isScriptCall(argv) && !shellNoExec(argv) {
				p := argv[0]
				direct := !shells[baseCmd(argv)]
				if !direct {
					p, _ = shellScriptArg(argv[1:])
				}
				abs := p
				if !filepath.IsAbs(abs) {
					abs = ""
					if !lc.dirUncertain {
						abs = absIn(dir, p)
					}
				}
				ref := ScriptRef{Path: p, Token: p, Direct: direct}
				if abs != "" {
					ref.Path = filepath.Clean(abs)
					ref.Trusted = !lc.writes[ref.Path] && lc.heredocWrites[ref.Path] == nil &&
						lc.opaqueWriter == "" && lc.scriptRuns <= 1
					if snap != nil {
						if sn := snap.Read(ref.Path); sn.Err == nil {
							data := sn.Data
							sum := sha256.Sum256(data)
							ref.SHA256 = hex.EncodeToString(sum[:])
							ref.Full = data
							shown := data
							if contentLimit > 0 && len(shown) > contentLimit {
								shown = shown[:contentLimit]
							}
							if contentLimit != 0 {
								ref.Content = string(shown)
							}
						}
					}
				}
				out = append(out, ref)
			}
			dir = c.nextDir(s, dir)
		}
	}
	walk(line, cwd, 0)
	return out
}
