package security

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Scratch-dir scripts (STA-868).
//
// Agents write throwaway audit scripts under /tmp or their run's scratch dir
// and then run them with `bash x.sh`. Such a call used to be Red ("runs an
// opaque script") and held for the Board even when the script only ran
// du/ls/git status. When the classifier may read files (Classifier.ReadFile)
// and the script sits in a scratch dir, it is judged by its contents instead:
// a script made only of read-only commands that writes only under that
// scratch dir is Yellow; anything else stays Red with the offending line.
//
// The contents judged must be the contents that run. A command line that
// could rewrite the script before running it (another writer, a second
// script, an unquoted heredoc) is not trusted and stays Red.

// maxScriptBytes bounds how much of a script is read and analysed.
const maxScriptBytes = 256 << 10

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

// insideRoot reports whether p stays inside root after resolving symlinks.
func insideRoot(root, p string) bool {
	return within(root, p) && within(resolveExisting(root), resolveExisting(p))
}

// lineCtx records what a whole command line writes, so a script's on-disk
// contents are trusted only when nothing else in the line can change them.
type lineCtx struct {
	heredocWrites map[string]*redirect // absolute target -> heredoc written to it
	writes        map[string]bool      // absolute paths written by redirects / tee / touch
	opaqueWriter  string               // first segment that could write anywhere
	scriptRuns    int                  // scripts executed in the line
	dirUncertain  bool                 // a subshell or pushd makes `cd` tracking unreliable
}

var subshellRe = regexp.MustCompile(`(^|[^$<>])\(`)

// knownTargetWriters write only to the paths named in their arguments or
// redirects, which lineContext records.
var knownTargetWriters = map[string]bool{
	"mkdir": true, "chmod": true, "touch": true, "tee": true, "cat": true, "echo": true, "printf": true,
}

func (c *Classifier) lineContext(line string, segs []segment, subs []string, depth int) *lineCtx {
	lc := &lineCtx{heredocWrites: map[string]*redirect{}, writes: map[string]bool{}}
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
	a := argv[1]
	if a == "-" || strings.Contains(a, "$") || strings.HasPrefix(a, "-") {
		return ""
	}
	return absIn(dir, a)
}

// shellScriptArg returns the script a shell is asked to run, skipping its
// option flags. ok is false for -c, -s, -i and for no script at all.
func shellScriptArg(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", false
		case a == "-o" || a == "+o":
			i++
		case strings.HasPrefix(a, "-") || strings.HasPrefix(a, "+"):
			if strings.ContainsAny(a, "csi") && !strings.HasPrefix(a, "--") {
				return "", false
			}
		default:
			return a, true
		}
	}
	return "", false
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

// scriptVerdict judges `name path` by the script's contents. It returns false
// (caller keeps its old verdict) when contents cannot be used: no ReadFile,
// not in a scratch dir, or unreadable.
func (c *Classifier) scriptVerdict(name, path string, v *Verdict, depth int) bool {
	if c.ReadFile == nil || depth > maxDepth {
		return false
	}
	lc := c.line
	if lc == nil {
		lc = &lineCtx{}
	}
	abs := path
	if !filepath.IsAbs(abs) {
		if lc.dirUncertain || c.CWD == "" {
			return false
		}
		abs = absIn(c.CWD, path)
	}
	if abs == "" || strings.Contains(path, "$") {
		return false
	}
	abs = filepath.Clean(abs)
	root := c.scratchRootFor(abs)
	if root == "" {
		return false
	}
	var content string
	if hd := lc.heredocWrites[abs]; hd != nil {
		if !hd.quoted {
			v.raise(Red, name+": script "+abs+" is written by an unquoted heredoc in the same command (expansions run)")
			return true
		}
		if lc.scriptRuns > 1 || lc.opaqueWriter != "" {
			v.raise(Red, name+": script "+abs+" may be changed by another part of the same command")
			return true
		}
		content = hd.body
	} else {
		if lc.writes[abs] || lc.opaqueWriter != "" || lc.scriptRuns > 1 {
			v.raise(Red, name+": script "+abs+" may be changed by another part of the same command")
			return true
		}
		data, err := c.ReadFile(resolveExisting(abs))
		if err != nil {
			return false
		}
		if len(data) > maxScriptBytes {
			v.raise(Red, name+": script "+abs+" too large to analyse")
			return true
		}
		content = string(data)
	}
	ok, detail := c.scriptReadOnly(content, filepath.Dir(abs), root, depth+1)
	if ok {
		v.raise(Yellow, "")
		return true
	}
	v.raise(Red, fmt.Sprintf("%s: script %s is not read-only (%s)", name, abs, detail))
	inner := (&Classifier{Home: c.Home, CWD: filepath.Dir(abs), ScratchDirs: c.ScratchDirs}).classifyLine(content, depth+1)
	for _, r := range inner.Reasons {
		v.Reasons = append(v.Reasons, "script: "+r)
	}
	return true
}

// shellKeywordPrefix are words that may precede a command in a script.
var shellKeywordPrefix = map[string]bool{
	"if": true, "then": true, "else": true, "elif": true, "do": true, "while": true,
	"until": true, "!": true, "{": true, "time": true,
}

// shellSyntaxOnly are words that form a whole segment of script syntax.
var shellSyntaxOnly = map[string]bool{
	"fi": true, "done": true, "esac": true, "}": true, ";;": true, "for": true, "case": true,
	"select": true, "in": true, "function": true,
}

// scriptBuiltins change only shell state.
var scriptBuiltins = map[string]bool{
	"set": true, "local": true, "declare": true, "typeset": true, "export": true, "readonly": true,
	"read": true, "shift": true, "return": true, "exit": true, "break": true, "continue": true,
	"unset": true, ":": true, "true": true, "false": true, "[[": true, "]]": true, "wait": true,
	"getopts": true, "shopt": true, "umask": true, "echo": true, "printf": true, "test": true, "[": true,
}

// scriptWrappers run the command after their own flags.
var scriptWrappers = map[string]bool{
	"time": true, "nice": true, "timeout": true, "command": true, "env": true, "stdbuf": true,
	"xargs": true, "exec": true,
}

var (
	arrayAssignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\[[^\]]*\]\+?=`)
	funcDefRe     = regexp.MustCompile(`(?m)^\s*(?:function\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*\(\s*\)`)
	funcKwRe      = regexp.MustCompile(`(?m)^\s*function\s+([A-Za-z_][A-Za-z0-9_]*)`)
	varRefRe      = regexp.MustCompile(`\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?`)
	sedWriteRe    = regexp.MustCompile(`(^|[/;}\s])[gpIiMm0-9]*[we](\s|$)`)
	awkUnsafeRe   = regexp.MustCompile(`system|getline|[|>]`)
	scriptInterp  = map[string]bool{
		"python": true, "python2": true, "python3": true, "node": true, "nodejs": true,
		"ruby": true, "perl": true, "php": true, "osascript": true,
	}
)

// scriptState is what scriptReadOnly tracks while walking a script.
type scriptState struct {
	c     *Classifier
	root  string
	funcs map[string]bool
	vars  map[string]string
	depth int
}

// scriptReadOnly reports whether content runs only read-only commands and
// writes only under root. detail names the first offending command.
func (c *Classifier) scriptReadOnly(content, dir, root string, depth int) (bool, string) {
	st := &scriptState{c: c, root: root, funcs: map[string]bool{}, vars: map[string]string{}, depth: depth}
	for _, m := range funcDefRe.FindAllStringSubmatch(content, -1) {
		st.funcs[m[1]] = true
	}
	for _, m := range funcKwRe.FindAllStringSubmatch(content, -1) {
		st.funcs[m[1]] = true
	}
	return st.walk(content, dir)
}

func (st *scriptState) walk(content, dir string) (bool, string) {
	if st.depth > maxDepth {
		return false, "nesting too deep"
	}
	segs, subs, err := parseShell(content)
	if err != nil {
		return false, "unparseable: " + err.Error()
	}
	for _, s := range subs {
		inner := &scriptState{c: st.c, root: st.root, funcs: st.funcs, vars: st.vars, depth: st.depth + 1}
		if ok, d := inner.walk(s, dir); !ok {
			return false, d
		}
	}
	for _, s := range segs {
		ok, d, next := st.segment(s, dir)
		if !ok {
			return false, d
		}
		dir = next
	}
	return true, ""
}

// expand substitutes variables assigned literally earlier in the script.
func (st *scriptState) expand(p string) string {
	return varRefRe.ReplaceAllStringFunc(p, func(m string) string {
		name := varRefRe.FindStringSubmatch(m)[1]
		if v, ok := st.vars[name]; ok {
			return v
		}
		return m
	})
}

// writable reports whether the script may write path p (relative to dir).
func (st *scriptState) writable(dir, p string) bool {
	p = st.c.expandHome(st.expand(p))
	if harmlessPaths[p] {
		return true
	}
	if strings.Contains(p, "$") {
		return false
	}
	abs := absIn(dir, p)
	return abs != "" && insideRoot(st.root, abs)
}

func (st *scriptState) recordAssign(a string) {
	i := strings.IndexByte(a, '=')
	if i <= 0 {
		return
	}
	name, val := a[:i], a[i+1:]
	if strings.Contains(val, "$SUBST") || strings.Contains(st.expand(val), "$") {
		delete(st.vars, name)
		return
	}
	st.vars[name] = st.expand(val)
}

func (st *scriptState) segment(s segment, dir string) (bool, string, string) {
	argv := s.argv
	// Pure assignments set shell variables used by later write targets.
	// Array element assignments (A[k]=v) only change shell state.
	all := len(argv) > 0
	for _, a := range argv {
		if !isAssign(a) && !arrayAssignRe.MatchString(a) {
			all = false
			break
		}
	}
	if all {
		for _, a := range argv {
			if isAssign(a) {
				st.recordAssign(a)
			}
		}
		argv = nil // redirects below are still checked
	}
	argv = stripPrefixes(argv)
	for len(argv) > 0 && shellKeywordPrefix[argv[0]] {
		argv = argv[1:]
	}
	name := baseCmd(argv)
	for _, r := range s.redirects {
		switch {
		case r.heredoc:
			if shells[name] || scriptInterp[name] {
				return false, name + " runs a heredoc script", dir
			}
		case isOutputRedirect(r.op):
			if !st.writable(dir, r.target) {
				return false, "writes " + r.target + " outside " + st.root, dir
			}
		}
	}
	if len(argv) == 0 {
		return true, "", dir
	}
	args := argv[1:]
	line := strings.Join(argv, " ")
	switch {
	case shellSyntaxOnly[name], st.funcs[name]:
		return true, "", dir
	case name == "cd":
		return true, "", st.c.nextDir(segment{argv: []string{"cd", st.expand(firstOr(args, ""))}}, dir)
	case scriptBuiltins[name]:
		switch name {
		case "export", "local", "declare", "typeset", "readonly":
			for _, a := range args {
				if isAssign(a) {
					st.recordAssign(a)
				}
			}
		case "read":
			for _, a := range args {
				if !strings.HasPrefix(a, "-") {
					delete(st.vars, a)
				}
			}
		}
		return true, "", dir
	case scriptWrappers[name]:
		inner := skipWrapper(name, args)
		if len(inner) == 0 {
			return true, "", dir
		}
		return st.segment(segment{argv: inner}, dir)
	case name == "mkdir" || name == "touch" || name == "rm" || name == "rmdir" || name == "chmod" || name == "tee":
		pos := positionals(args)
		if name == "chmod" && len(pos) > 0 {
			pos = pos[1:]
		}
		if len(pos) == 0 && name != "tee" {
			return false, line + " (no target)", dir
		}
		for _, p := range pos {
			if !st.writable(dir, p) {
				return false, line + " (outside " + st.root + ")", dir
			}
		}
		return true, "", dir
	case name == "mktemp":
		for _, p := range positionals(args) {
			if !st.writable(dir, p) {
				return false, line + " (outside " + st.root + ")", dir
			}
		}
		return true, "", dir
	case name == "cp" || name == "mv":
		pos := positionals(args)
		if len(pos) < 2 {
			return false, line, dir
		}
		check := pos[len(pos)-1:]
		if name == "mv" {
			check = pos
		}
		for _, p := range check {
			if !st.writable(dir, p) {
				return false, line + " (outside " + st.root + ")", dir
			}
		}
		return true, "", dir
	case name == "sort":
		for i, a := range args {
			var out string
			switch {
			case a == "-o" && i+1 < len(args):
				out = args[i+1]
			case strings.HasPrefix(a, "--output="):
				out = strings.TrimPrefix(a, "--output=")
			case strings.HasPrefix(a, "-o") && len(a) > 2:
				out = a[2:]
			}
			if out != "" && !st.writable(dir, out) {
				return false, line + " (outside " + st.root + ")", dir
			}
		}
	case name == "sed":
		for _, a := range args {
			if strings.HasPrefix(a, "-i") || strings.HasPrefix(a, "--in-place") {
				return false, line + " (edits files in place)", dir
			}
			if !strings.HasPrefix(a, "-") && sedWriteRe.MatchString(a) {
				return false, line + " (sed w/e command)", dir
			}
		}
	case name == "awk" || name == "gawk" || name == "nawk":
		for _, a := range args {
			if !strings.HasPrefix(a, "-") && awkUnsafeRe.MatchString(a) {
				return false, line + " (awk may write or run commands)", dir
			}
		}
	case name == "find":
		for _, a := range args {
			if strings.HasPrefix(a, "-fprint") || a == "-fls" {
				return false, line + " (find writes a file)", dir
			}
		}
	}
	var fv Verdict
	pc := &Classifier{Home: st.c.Home, CWD: dir, ScratchDirs: st.c.ScratchDirs}
	pc.classifySegment(segment{argv: argv}, &fv, st.depth)
	if fv.Tier != Green {
		if len(fv.Reasons) > 0 {
			return false, line + ": " + strings.Join(fv.Reasons, "; "), dir
		}
		return false, line + " (not read-only)", dir
	}
	return true, "", dir
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

// ScriptRef is a script file a command line runs.
type ScriptRef struct {
	Path string `json:"path"`
	// SHA256 of the contents at the time it was read ("" when unreadable).
	SHA256 string `json:"sha256"`
	// Trusted is false when the same command line could change the file
	// before it runs, so its current contents say nothing.
	Trusted bool `json:"trusted"`
	// Content is the (possibly truncated) text, for reviewers. Not stored.
	Content string `json:"-"`
}

// ScriptRefs lists the script files that line runs (bash x.sh, ./x.sh),
// resolved against cwd, with their current sha256 read via readFile. It is
// what an allow rule pins (STA-868).
func ScriptRefs(line, cwd string, readFile func(string) ([]byte, error), contentLimit int) []ScriptRef {
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
			if isScriptCall(argv) {
				p := argv[0]
				if shells[baseCmd(argv)] {
					p, _ = shellScriptArg(argv[1:])
				}
				abs := p
				if !filepath.IsAbs(abs) {
					abs = ""
					if !lc.dirUncertain {
						abs = absIn(dir, p)
					}
				}
				ref := ScriptRef{Path: p}
				if abs != "" {
					ref.Path = filepath.Clean(abs)
					ref.Trusted = !lc.writes[ref.Path] && lc.heredocWrites[ref.Path] == nil &&
						lc.opaqueWriter == "" && lc.scriptRuns <= 1
					if readFile != nil {
						if data, err := readFile(resolveExisting(ref.Path)); err == nil {
							sum := sha256.Sum256(data)
							ref.SHA256 = hex.EncodeToString(sum[:])
							if contentLimit > 0 {
								if len(data) > contentLimit {
									data = data[:contentLimit]
								}
								ref.Content = string(data)
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
