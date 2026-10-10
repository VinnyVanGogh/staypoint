package security

import (
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Trust exclusions (task-6c1ed91f). While the Board trusts a task, its Red
// requests are auto-approved except:
//   - merges and pushes to protected branches, which always wait, and
//   - deletes outside the run's worktree and scratch dirs, which wait up to a
//     timeout and are then skipped (deferred).
//
// The daemon decides these from the command text itself, never from the
// hook's reasons or anything the client sends. Anything it cannot reason
// about counts as excluded (fail closed): an unparseable line, a dynamic
// path, an unknown working directory, a script it cannot read.

// TrustContext is what AnalyzeForTrust needs about the request.
type TrustContext struct {
	// CWD is the command's working directory ("" when unknown).
	CWD string
	// Allowed are the dirs deletes may touch: the run's worktree and its
	// scratch dirs. A delete must resolve strictly inside one of them.
	Allowed []string
	// Protected are branch names a push may not target (main, dev-server,
	// the repo's target branch, ...).
	Protected map[string]bool
	// Home expands a leading "~/".
	Home string
	// Scripts are the snapshots of the scripts the command runs.
	Scripts []ScriptHash
	// PushTargets returns the branch names a bare `git push` in dir would
	// update (current branch and its push destination). An error means
	// unknown, which counts as protected.
	PushTargets func(dir string) ([]string, error)
}

// TrustFacts is the analysis of one request.
type TrustFacts struct {
	Protected     bool   `json:"protected"`
	ProtectedWhy  string `json:"protected_why,omitempty"`
	DeleteOutside bool   `json:"delete_outside"`
	DeleteWhy     string `json:"delete_why,omitempty"`
}

func (f *TrustFacts) protect(why string) {
	if !f.Protected {
		f.Protected, f.ProtectedWhy = true, why
	}
}

func (f *TrustFacts) deleteOut(why string) {
	if !f.DeleteOutside {
		f.DeleteOutside, f.DeleteWhy = true, why
	}
}

// OpsToolPrefix starts the gate-request cmdline of a typed ops MCP tool call
// (opstools.Call.Canonical, task-7d279c9d).
const OpsToolPrefix = "mcp__staypoint__"

// IsOpsToolCall reports whether a gate-request cmdline is an ops tool call.
func IsOpsToolCall(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), OpsToolPrefix)
}

// TruncatedMarker ends script content that was cut for storage.
const TruncatedMarker = "…(truncated)"

// mergeTextRe and pushTextRe are the text backstop for code the shell parser
// cannot see into (interpreter one-liners, non-shell scripts).
var (
	mergeTextRe  = regexp.MustCompile(`(?i)(\bgh\b[\s\S]*\bmerge\b|/merge\b|/pulls/\d+/merge|ship-review|merge_pull|mergePullRequest|enablePullRequestAutoMerge)`)
	pushTextRe   = regexp.MustCompile(`(?i)\bgit\b[\s\S]*\bpush\b|\bpush\b[\s\S]*\bgit\b`)
	deleteTextRe = regexp.MustCompile(`(?i)(rmtree|os\.remove|os\.unlink|\.unlink\(|unlinkSync|rmSync|rmdirSync|\bfs\.rm\b|fs\.promises\.rm|\brmdir\b|FileUtils\.rm|\bunlink\b|\btruncate\b|\brm\s+-|\brm\s+/|-delete\b|git\s+clean|remove_dir|remove_file|shutil\.move|\bshred\b|--delete\b|\.delete\(\))`)
)

var interpreters = map[string]bool{
	"python": true, "python2": true, "python3": true, "node": true, "nodejs": true, "deno": true, "bun": true,
	"ruby": true, "perl": true, "php": true, "osascript": true, "lua": true, "Rscript": true,
}

// AnalyzeForTrust reports whether a trusted task's request must still wait
// for the Board.
func AnalyzeForTrust(line string, tc TrustContext) TrustFacts {
	var f TrustFacts
	if IsOpsToolCall(line) {
		// The ops MCP server asks the gate only for prod and external
		// writes, so no trust or tev1 may approve one: the decision is the
		// tool's declared effect, not text a rule happens to match.
		f.protect("ops tool prod/external write: only the Board approves it")
		return f
	}
	a := trustAnalyzer{tc: tc, f: &f}
	a.line(line, tc.CWD, 0)
	return f
}

type trustAnalyzer struct {
	tc TrustContext
	f  *TrustFacts
}

func (a *trustAnalyzer) unsure(why string) {
	a.f.protect(why)
	a.f.deleteOut(why)
}

// text applies the backstop to code the parser cannot see into.
func (a *trustAnalyzer) text(code, what string) {
	if mergeTextRe.MatchString(code) {
		a.f.protect(what + " may merge a pull request")
	}
	if pushTextRe.MatchString(code) {
		a.f.protect(what + " may run git push")
	}
	if deleteTextRe.MatchString(code) {
		a.f.deleteOut(what + " may delete files")
	}
}

func (a *trustAnalyzer) line(line, cwd string, depth int) {
	if depth > maxDepth {
		a.unsure("command nesting too deep to analyse")
		return
	}
	segs, subs, err := parseShell(line)
	if err != nil {
		a.unsure("unparseable command: " + err.Error())
		return
	}
	for _, s := range subs {
		a.line(s, cwd, depth+1)
	}
	dir := cwd
	for _, s := range segs {
		a.segment(s, dir, cwd, depth)
		dir = a.nextDir(s, dir)
	}
}

// nextDir follows `cd`; pushd/popd and anything dynamic make it unknown.
func (a *trustAnalyzer) nextDir(s segment, dir string) string {
	argv := stripPrefixes(s.argv)
	switch baseCmd(argv) {
	case "cd":
		if len(argv) == 1 {
			return a.tc.Home
		}
		off := len(s.argv) - len(argv)
		p := argv[1]
		if segDyn(s, off+1) || strings.HasPrefix(p, "-") {
			return ""
		}
		if strings.HasPrefix(p, "~/") && a.tc.Home != "" {
			return filepath.Join(a.tc.Home, p[2:])
		}
		return absIn(dir, p)
	case "pushd", "popd":
		return ""
	}
	return dir
}

func (a *trustAnalyzer) segment(s segment, dir, baseDir string, depth int) {
	// A pure-edit Python heredoc run in the command's own cwd only edits
	// files there (task-31dea40b): nothing to hold. Off while
	// pureEditAutoAllow is false (Board, 2026-10-08).
	if a.tc.CWD != "" && dir == baseDir && pureEditHeredoc(s, dir) {
		return
	}
	argv := stripPrefixes(s.argv)
	off := len(s.argv) - len(argv)
	dynAt := func(i int) bool { return segDyn(s, off+i) }

	for _, r := range s.redirects {
		if r.heredoc {
			continue
		}
		// > and >| truncate their target; &> and 2> too. >> appends, >& dups.
		if strings.HasPrefix(r.op, ">") || strings.HasPrefix(r.op, "&>") {
			if strings.HasPrefix(r.op, ">>") || strings.HasPrefix(r.op, "&>>") || (strings.HasPrefix(r.op, ">&") && isDigits(r.target)) || r.target == "-" {
				continue
			}
			if r.target == "/dev/null" || r.target == "/dev/stdout" || r.target == "/dev/stderr" {
				continue
			}
			if r.dyn || r.meta {
				a.f.deleteOut("redirect target is dynamic: " + r.target)
				continue
			}
			a.deletePath(r.target, dir, baseDir, "> truncates")
		}
	}
	if len(argv) == 0 {
		return
	}
	name := baseCmd(argv)
	args := argv[1:]

	// Heredocs fed to a shell are scripts; elsewhere they are data.
	for _, r := range s.redirects {
		if r.heredoc && (shells[name] || interpreters[name]) {
			if shells[name] {
				a.line(r.body, dir, depth+1)
			} else {
				a.text(r.body, name+" heredoc")
			}
		}
	}

	switch {
	case wrappers[name]:
		inner := skipWrapper(name, args)
		if len(inner) == 0 {
			return
		}
		if name == "xargs" {
			// Arguments arrive on stdin: whatever it runs gets unknown paths.
			a.argvUnknownPaths(inner, dir, baseDir, depth)
			return
		}
		// Keep env-style assignments (env GIT_DIR=x git ...) in front of
		// the inner command, where segment() sees them as prefixes.
		a.segment(segment{argv: carryAssigns(s.argv[:off], args[:len(args)-len(inner)], inner)}, dir, baseDir, depth+1)
	case name == "sudo" || name == "doas":
		if inner := skipPrivFlags(args); len(inner) > 0 {
			a.segment(segment{argv: carryAssigns(s.argv[:off], args[:len(args)-len(inner)], inner)}, dir, baseDir, depth+1)
		}
	case shells[name]:
		for i, x := range args {
			if strings.HasPrefix(x, "-") && !strings.HasPrefix(x, "--") && strings.Contains(x, "c") && i+1 < len(args) {
				a.line(args[i+1], dir, depth+1)
				return
			}
		}
		if idx, ok := shellScriptIndex(args); ok {
			a.script(args[idx], dir, depth, true)
		}
	case name == "eval":
		a.line(strings.Join(args, " "), dir, depth+1)
	case name == "source" || name == ".":
		if len(args) > 0 {
			a.script(args[0], dir, depth, true)
		}
	case interpreters[name]:
		a.interpreter(name, args, dir, depth)
	case name == "rm" || name == "rmdir" || name == "unlink" || name == "shred" || name == "srm":
		for _, p := range plainArgs(args, nil) {
			a.deleteArg(p, dynAt, dir, baseDir, name)
		}
	case name == "truncate":
		for _, p := range plainArgs(args, map[string]bool{"-s": true, "--size": true, "-r": true, "--reference": true}) {
			a.deleteArg(p, dynAt, dir, baseDir, name)
		}
	case name == "ssh":
		a.ssh(s, args, depth)
	case name == "tee":
		for _, p := range plainArgs(args, nil) {
			if !hasFlag(args, "-a", "--append") {
				a.deleteArg(p, dynAt, dir, baseDir, "tee truncates")
			}
		}
	case name == "dd":
		for i, x := range args {
			if strings.HasPrefix(x, "of=") {
				a.deleteArg(plainArg{v: x[3:], i: i}, dynAt, dir, baseDir, "dd overwrites")
			}
		}
	case name == "mv" || name == "cp" || name == "ln" || name == "install":
		a.copyLike(name, args, dynAt, dir, baseDir)
	case name == "find":
		a.find(args, dynAt, dir, baseDir, depth)
	case name == "rsync":
		a.rsync(args, dynAt, dir, baseDir)
	case name == "git":
		for _, x := range s.argv[:off] {
			if strings.HasPrefix(x, "GIT_") {
				a.f.protect("git with a GIT_* environment override")
			}
		}
		a.git(args, dir, baseDir, depth)
	case name == "gh":
		a.gh(args)
	case name == "staypoint":
		for _, x := range args {
			if x == "ship-review" || x == "merge" || x == "approve" {
				a.f.protect("staypoint ship-review merges a branch")
			}
		}
	case name == "curl" || name == "wget" || name == "http" || name == "xh":
		for _, x := range args {
			if mergeTextRe.MatchString(x) || boardEndpointRe.MatchString(x) {
				a.f.protect(name + " calls a merge or Board endpoint")
			}
		}
	default:
		if strings.Contains(argv[0], "/") {
			a.script(argv[0], dir, depth, false)
		}
	}
}

func hasFlag(args []string, flags ...string) bool {
	for _, a := range args {
		for _, f := range flags {
			if a == f {
				return true
			}
		}
	}
	return false
}

// sshCommandOptions run a command on this machine.
// HostName and ProxyJump change where the command runs, so they count too.
var sshCommandOptions = map[string]bool{"proxycommand": true, "localcommand": true, "knownhostscommand": true,
	"permitlocalcommand": true, "proxyusefdpass": true, "remotecommand": true, "match": true, "include": true,
	"hostname": true, "proxyjump": true}

// sshValueLetters are the ssh options that take a value (ssh's getopt
// string "B:b:c:D:E:e:F:I:i:J:L:l:m:O:o:P:p:Q:R:S:W:w:").
var sshValueLetters = map[byte]bool{'B': true, 'b': true, 'c': true, 'D': true, 'E': true, 'e': true, 'F': true,
	'I': true, 'i': true, 'J': true, 'L': true, 'l': true, 'm': true, 'O': true, 'o': true, 'P': true, 'p': true,
	'Q': true, 'R': true, 'S': true, 'W': true, 'w': true}

// ssh: SSH is allowed under trust, but a remote command may still merge or
// push to a protected branch, an option can run a local command, and an
// ssh to this machine is local execution.
func (a *trustAnalyzer) ssh(s segment, args []string, depth int) {
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		x := args[i]
		if x == "--" {
			i++
			break
		}
		for c := 1; c < len(x); c++ {
			if !sshValueLetters[x[c]] {
				continue
			}
			v := x[c+1:]
			if v == "" && i+1 < len(args) {
				i++
				v = args[i]
			}
			switch x[c] {
			case 'o':
				fields := strings.FieldsFunc(v, func(r rune) bool { return r == '=' || r == ' ' || r == '\t' })
				if len(fields) == 0 {
					a.unsure("ssh -o without an option")
				} else if key := strings.ToLower(fields[0]); sshCommandOptions[key] {
					a.unsure("ssh option " + key + " runs a command")
				}
			case 'F':
				a.unsure("ssh with a custom config file")
			case 'J':
				a.unsure("ssh -J jump host")
			}
			break
		}
	}
	if i >= len(args) {
		return
	}
	host := args[i]
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	rc := strings.Join(args[i+1:], " ")
	if rc == "" {
		// A login shell fed from stdin runs a script we cannot see.
		for _, r := range s.redirects {
			if strings.HasPrefix(r.op, "<") {
				a.unsure("ssh runs a script from stdin")
			}
		}
		return
	}
	if isLocalHost(host) {
		a.line(rc, "", depth+1)
		return
	}
	if numericHost(host) {
		// An address form we do not normalise (127.1, 0x7f000001) may be
		// this machine: analyse it as local and as remote.
		a.line(rc, "", depth+1)
	}
	if mergeTextRe.MatchString(rc) {
		a.f.protect("ssh remote command may merge a pull request")
	}
	if pushTextRe.MatchString(rc) {
		a.f.protect("ssh remote command may run git push")
	}
	for _, r := range s.redirects {
		if strings.HasPrefix(r.op, "<") {
			a.unsure("ssh remote command reads stdin, which may be a script")
		}
	}
}

// isLocalHost reports whether an ssh target is this machine: a loopback or
// unspecified address, localhost, or this host's name. (An ~/.ssh/config
// alias pointing here is not visible.)
func isLocalHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if i := strings.IndexByte(h, '%'); i >= 0 {
		h = h[:i]
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	if name, err := os.Hostname(); err == nil {
		name = strings.ToLower(strings.TrimSuffix(name, "."))
		short, _, _ := strings.Cut(name, ".")
		if h == name || h == short || h == short+".local" {
			return true
		}
	}
	return false
}

// copyLike checks mv (sources removed, destination overwritten) and
// cp/ln/install (destination overwritten). Short options are walked
// getopt-style, so -t in any cluster form (-tDIR, -ft DIR, -ft/etc) is found.
func (a *trustAnalyzer) copyLike(name string, args []string, dynAt func(int) bool, dir, baseDir string) {
	what := name + " overwrites"
	shortValue := map[byte]bool{'t': true, 'S': true, 'm': true, 'o': true, 'g': true}
	longValue := map[string]bool{"--target-directory": true, "--suffix": true, "--mode": true, "--owner": true, "--group": true}
	targetSet := false
	target := func(v string, i int) {
		targetSet = true
		if i >= 0 {
			a.deleteArg(plainArg{v: v, i: i}, dynAt, dir, baseDir, what)
		} else {
			a.deletePath(v, dir, baseDir, what)
		}
	}
	var pos []plainArg
	for i := 0; i < len(args); i++ {
		x := args[i]
		switch {
		case x == "--":
			for j := i + 1; j < len(args); j++ {
				pos = append(pos, plainArg{v: args[j], i: j})
			}
			i = len(args)
		case len(pos) > 0 && strings.HasPrefix(x, "-"):
			// An option after an operand: GNU reads it as an option, BSD as a
			// file. Do not guess which.
			a.f.deleteOut(name + ": option after an operand: " + x)
		case strings.HasPrefix(x, "--"):
			k, v, hasV := strings.Cut(x, "=")
			if k == "--strip-program" || strings.HasPrefix("--strip-program", k) && len(k) > 8 {
				a.unsure(name + " --strip-program runs a program")
			}
			if !longValue[k] {
				// Only exact, known flags pass; abbreviations and unknown
				// options resolve against tables we do not model.
				if !copyLikeFlags[k] {
					a.f.deleteOut(name + ": unrecognised long option " + k)
				}
				continue
			}
			if !hasV {
				if i+1 >= len(args) {
					a.f.deleteOut(name + " " + k + " without a value")
					continue
				}
				i++
				v = args[i]
			}
			if k == "--target-directory" {
				if hasV {
					target(v, -1)
				} else {
					target(v, i)
				}
			}
		case len(x) > 1 && x[0] == '-':
			for c := 1; c < len(x); c++ {
				if !shortValue[x[c]] {
					continue
				}
				v, vi := x[c+1:], -1
				if v == "" {
					if i+1 >= len(args) {
						a.f.deleteOut(name + " -" + string(x[c]) + " without a value")
						break
					}
					i++
					v, vi = args[i], i
				}
				if x[c] == 't' {
					target(v, vi)
				}
				break
			}
		default:
			pos = append(pos, plainArg{v: x, i: i})
		}
	}
	if name == "mv" {
		for _, p := range pos {
			a.deleteArg(p, dynAt, dir, baseDir, "mv")
		}
		return
	}
	if len(pos) > 0 && !targetSet {
		a.deleteArg(pos[len(pos)-1], dynAt, dir, baseDir, what)
	}
}

// numericHost reports a host that is an address but not one ParseIP reads
// (inet_aton short, decimal, hex or octal forms).
func numericHost(host string) bool {
	h := strings.ToLower(strings.Trim(host, "[]"))
	if h == "" || net.ParseIP(h) != nil {
		return false
	}
	for _, r := range h {
		if !(r >= '0' && r <= '9' || r == '.' || r == 'x' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return strings.ContainsAny(h, "0123456789")
}

// copyLikeFlags are flag-only long options of mv/cp/ln/install (GNU).
var copyLikeFlags = map[string]bool{
	"--archive": true, "--attributes-only": true, "--backup": true, "--copy-contents": true, "--debug": true,
	"--dereference": true, "--force": true, "--interactive": true, "--link": true, "--no-clobber": true,
	"--no-dereference": true, "--no-target-directory": true, "--one-file-system": true, "--parents": true,
	"--preserve": true, "--no-preserve": true, "--recursive": true, "--remove-destination": true,
	"--strip-trailing-slashes": true, "--symbolic-link": true, "--symbolic": true, "--update": true,
	"--verbose": true, "--help": true, "--version": true, "--logical": true, "--physical": true,
	"--relative": true, "--directory": true, "--compare": true, "--preserve-timestamps": true,
	"--preserve-context": true, "--context": true, "--reflink": true, "--sparse": true, "--exchange": true, "--keep-directory-symlink": true,
}

// carryAssigns puts the VAR=x prefixes of the outer command and of a
// wrapper's own arguments in front of the command it runs, so env overrides
// (GIT_DIR=..., env GIT_SSH_COMMAND=...) stay visible.
func carryAssigns(outer, wrapperArgs, inner []string) []string {
	out := append([]string{}, outer...)
	for _, x := range wrapperArgs {
		if isAssign(x) {
			out = append(out, x)
		}
	}
	return append(out, inner...)
}

// gitConfigReadFlags are value-less git config options that may precede a
// read action.
var gitConfigReadFlags = map[string]bool{"--local": true, "--global": true, "--system": true, "--worktree": true,
	"-z": true, "--null": true, "--name-only": true, "--show-origin": true, "--show-scope": true,
	"--includes": true, "--no-includes": true, "--bool": true, "--int": true, "--path": true, "--bool-or-int": true}

// gitBuiltins are git subcommands; anything else may be an alias that runs
// an arbitrary command (git config alias.x '!git push origin main').
var gitBuiltins = map[string]bool{
	"add": true, "am": true, "apply": true, "archive": true, "bisect": true, "blame": true, "branch": true,
	"bundle": true, "cat-file": true, "checkout": true, "cherry": true, "cherry-pick": true, "clean": true,
	"clone": true, "commit": true, "config": true, "count-objects": true, "describe": true, "diff": true,
	"fetch": true, "for-each-ref": true, "format-patch": true, "fsck": true, "gc": true, "grep": true,
	"help": true, "init": true, "log": true, "ls-files": true, "ls-remote": true, "ls-tree": true,
	"merge": true, "merge-base": true, "mv": true, "name-rev": true, "notes": true, "pull": true, "push": true,
	"range-diff": true, "rebase": true, "reflog": true, "remote": true, "reset": true, "restore": true,
	"rev-list": true, "rev-parse": true, "revert": true, "rm": true, "shortlog": true, "show": true,
	"show-ref": true, "sparse-checkout": true, "stash": true, "status": true, "submodule": true,
	"subtree": true, "switch": true, "symbolic-ref": true, "tag": true, "update-index": true,
	"update-ref": true, "version": true, "worktree": true, "lfs": true,
}

// skipPrivFlags returns the command sudo/doas runs.
func skipPrivFlags(args []string) []string {
	value := map[string]bool{"-u": true, "-g": true, "-C": true, "-h": true, "-p": true, "-U": true, "-r": true, "-t": true, "-D": true}
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--":
			return args[i+1:]
		case strings.HasPrefix(args[i], "-"):
			if value[args[i]] {
				i++
			}
		case isAssign(args[i]):
		default:
			return args[i:]
		}
	}
	return nil
}

type plainArg struct {
	v string
	i int // index in args
}

// plainArgs returns the non-flag arguments; valueFlags consume the next one.
func plainArgs(args []string, valueFlags map[string]bool) []plainArg {
	var out []plainArg
	dashdash := false
	for i := 0; i < len(args); i++ {
		x := args[i]
		if !dashdash {
			if x == "--" {
				dashdash = true
				continue
			}
			if strings.HasPrefix(x, "-") && x != "-" {
				if valueFlags[x] {
					i++
				}
				continue
			}
		}
		out = append(out, plainArg{v: x, i: i})
	}
	return out
}

func (a *trustAnalyzer) deleteArg(p plainArg, dynAt func(int) bool, dir, baseDir, what string) {
	if dynAt(p.i + 1) {
		a.f.deleteOut(what + " path is dynamic: " + p.v)
		return
	}
	a.deletePath(p.v, dir, baseDir, what)
}

// deletePath marks the request when p is not strictly inside an allowed dir.
// A relative path must be inside for both the tracked dir and the line's
// starting dir (a cd may sit in a subshell the parser flattens).
func (a *trustAnalyzer) deletePath(p, dir, baseDir, what string) {
	if p == "" {
		return
	}
	if strings.ContainsAny(p, "$`") || (strings.HasPrefix(p, "~") && !strings.HasPrefix(p, "~/")) {
		a.f.deleteOut(what + " path is dynamic: " + p)
		return
	}
	if strings.HasPrefix(p, "~/") {
		if a.tc.Home == "" {
			a.f.deleteOut(what + " path under unknown home: " + p)
			return
		}
		p = filepath.Join(a.tc.Home, p[2:])
	}
	// A glob deletes what it matches under its fixed prefix.
	if i := strings.IndexAny(p, "*?[{"); i >= 0 {
		p = p[:i]
		if j := strings.LastIndex(p, "/"); j >= 0 {
			p = p[:j+1]
		} else {
			p = "."
		}
		if p == "" || p == "/" {
			a.f.deleteOut(what + " glob at filesystem root")
			return
		}
	}
	dirs := []string{dir}
	if !filepath.IsAbs(p) && baseDir != dir {
		dirs = append(dirs, baseDir)
	}
	for _, d := range dirs {
		abs := p
		if !filepath.IsAbs(abs) {
			if d == "" {
				a.f.deleteOut(what + " relative path with unknown working dir: " + p)
				return
			}
			abs = filepath.Join(d, p)
		}
		if !a.insideAllowed(filepath.Clean(abs)) {
			a.f.deleteOut(what + " outside the task worktree: " + abs)
			return
		}
	}
}

// insideAllowed: strictly below an allowed dir, both lexically and with
// symlinks resolved (a link in the worktree must not point elsewhere).
func (a *trustAnalyzer) insideAllowed(abs string) bool {
	resolved := resolveExisting(abs)
	for _, root := range a.tc.Allowed {
		if root == "" {
			continue
		}
		root = filepath.Clean(root)
		rroot := resolveExisting(root)
		lex := abs != root && within(root, abs)
		res := resolved != rroot && (within(rroot, resolved) || within(root, resolved))
		if lex && res {
			return true
		}
	}
	return false
}

// argvUnknownPaths handles a command whose arguments are not all visible
// (xargs): a delete command is outside, any other command is analysed as is.
func (a *trustAnalyzer) argvUnknownPaths(argv []string, dir, baseDir string, depth int) {
	switch baseCmd(argv) {
	case "rm", "rmdir", "unlink", "shred", "truncate", "srm":
		a.f.deleteOut(baseCmd(argv) + " with arguments from stdin")
	}
	a.segment(segment{argv: argv}, dir, baseDir, depth+1)
}

func (a *trustAnalyzer) find(args []string, dynAt func(int) bool, dir, baseDir string, depth int) {
	var roots []plainArg
	i := 0
	for ; i < len(args); i++ {
		x := args[i]
		if strings.HasPrefix(x, "-") || x == "(" || x == "!" {
			if x == "-H" || x == "-L" || x == "-P" {
				continue
			}
			break
		}
		roots = append(roots, plainArg{v: x, i: i})
	}
	deletes := false
	for j := i; j < len(args); j++ {
		switch args[j] {
		case "-delete":
			deletes = true
		case "-exec", "-execdir", "-ok", "-okdir":
			if j+1 < len(args) {
				a.argvUnknownPaths(trimUntil(args[j+1:], ";", "+"), dir, baseDir, depth)
			}
		}
	}
	if !deletes {
		return
	}
	if len(roots) == 0 {
		roots = []plainArg{{v: ".", i: -2}}
	}
	for _, r := range roots {
		if r.i >= 0 && dynAt(r.i+1) {
			a.f.deleteOut("find -delete root is dynamic: " + r.v)
			continue
		}
		// find deletes below its root (it cannot remove "." itself).
		a.deletePath(filepath.Join(r.v, "x"), dir, baseDir, "find -delete")
	}
}

func (a *trustAnalyzer) rsync(args []string, dynAt func(int) bool, dir, baseDir string) {
	del := false
	for _, x := range args {
		if strings.HasPrefix(x, "--delete") || x == "--remove-source-files" {
			del = true
		}
	}
	if !del {
		return
	}
	pos := plainArgs(args, map[string]bool{"-e": true, "--rsh": true, "--exclude": true, "--include": true, "--filter": true, "-f": true})
	if len(pos) == 0 {
		a.f.deleteOut("rsync --delete with no visible destination")
		return
	}
	for _, p := range pos {
		if strings.Contains(p.v, ":") {
			a.f.deleteOut("rsync deletes on a remote: " + p.v)
			return
		}
	}
	// --remove-source-files deletes sources; --delete prunes the destination.
	for _, p := range pos {
		if dynAt(p.i + 1) {
			a.f.deleteOut("rsync path is dynamic: " + p.v)
			return
		}
		a.deletePath(p.v, dir, baseDir, "rsync --delete")
	}
}

func (a *trustAnalyzer) git(args []string, dir, baseDir string, depth int) {
	eff := dir
	cfgOverride := false
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		switch {
		case args[i] == "-C":
			i++
			if i < len(args) {
				eff = resolveGitWorkDir(eff, args[i])
			}
		case strings.HasPrefix(args[i], "-C"):
			eff = resolveGitWorkDir(eff, args[i][2:])
		case args[i] == "--git-dir" || args[i] == "--work-tree" || args[i] == "--namespace":
			eff, cfgOverride = "", true
			i++
		case strings.HasPrefix(args[i], "--git-dir=") || strings.HasPrefix(args[i], "--work-tree=") || strings.HasPrefix(args[i], "--namespace="):
			eff, cfgOverride = "", true
		case args[i] == "-c" || strings.HasPrefix(args[i], "-c") || strings.HasPrefix(args[i], "--config-env"):
			// A config override can set a command (alias.*, core.sshCommand,
			// core.hooksPath, credential.helper, ...): fail closed.
			a.f.protect("git -c / --config-env override")
			if args[i] == "-c" || args[i] == "--config-env" {
				i++
			}
			cfgOverride = true
		case strings.HasPrefix(args[i], "--exec-path"):
			a.f.protect("git --exec-path override")
			cfgOverride = true
		}
		i++
	}
	if i >= len(args) {
		return
	}
	sub, rest := args[i], args[i+1:]
	if !gitBuiltins[sub] {
		a.f.protect("git " + sub + ": not a built-in command (may be an alias)")
		return
	}
	switch sub {
	case "config":
		// A config write can set an alias, hook path, helper or push
		// destination that a later command uses: hold every write.
		// Read only when a read action comes before the first positional
		// (`git config core.hooksPath list` sets the value "list").
		read := false
	cfg:
		for j := 0; j < len(rest); j++ {
			switch x := rest[j]; {
			case x == "--get" || x == "--get-all" || x == "--get-regexp" || x == "--get-urlmatch" || x == "--list" || x == "-l":
				read = true
				break cfg
			case x == "--file" || x == "-f" || x == "--blob" || x == "--type":
				if j+1 >= len(rest) || strings.HasPrefix(rest[j+1], "-") {
					break cfg
				}
				j++
			case strings.HasPrefix(x, "-"):
				if !gitConfigReadFlags[x] {
					break cfg // unknown option: treat as a write
				}
			default:
				read = x == "get" || x == "list"
				break cfg
			}
		}
		if !read {
			a.f.protect("git config write under trust")
		}
	case "push":
		a.gitPush(rest, eff, cfgOverride)
	case "submodule":
		// submodule [opts] foreach [opts] <cmd> runs a command in each
		// submodule, a dir we do not track.
		j := 0
		for j < len(rest) && strings.HasPrefix(rest[j], "-") {
			j++
		}
		if j < len(rest) && rest[j] == "foreach" {
			j++
			for j < len(rest) && strings.HasPrefix(rest[j], "-") {
				j++
			}
			if j < len(rest) {
				a.line(strings.Join(rest[j:], " "), "", depth+1)
			}
		}
	case "rebase":
		for k, x := range rest {
			if x == "-x" || x == "--exec" {
				if k+1 < len(rest) {
					a.line(rest[k+1], eff, depth+1)
				}
			} else if strings.HasPrefix(x, "--exec=") {
				a.line(strings.TrimPrefix(x, "--exec="), eff, depth+1)
			} else if strings.HasPrefix(x, "-x") && len(x) > 2 {
				a.line(x[2:], eff, depth+1)
			}
		}
	case "bisect":
		if len(rest) > 1 && rest[0] == "run" {
			a.segment(segment{argv: rest[1:]}, eff, eff, depth+1)
		}
	case "clean":
		force := false
		for _, x := range rest {
			if x == "--force" || (len(x) > 1 && x[0] == '-' && x[1] != '-' && strings.Contains(x, "f")) {
				force = true
			}
		}
		if !force {
			return
		}
		if eff == "" {
			a.f.deleteOut("git clean in an unknown repo dir")
			return
		}
		a.deletePath(filepath.Join(eff, "x"), eff, eff, "git clean")
		for _, p := range plainArgs(rest, map[string]bool{"-e": true, "--exclude": true}) {
			a.deletePath(p.v, eff, eff, "git clean")
		}
	case "rm":
		for _, p := range plainArgs(rest, nil) {
			if eff == "" {
				a.f.deleteOut("git rm in an unknown repo dir")
				return
			}
			a.deletePath(p.v, eff, eff, "git rm")
		}
	case "worktree":
		if len(rest) > 0 && (rest[0] == "remove" || rest[0] == "prune") {
			a.f.deleteOut("git worktree " + rest[0])
		}
	case "branch":
		// Deleting or renaming a protected branch locally is not a push; a
		// later push is checked on its own.
	case "subtree":
		if len(rest) > 0 && rest[0] == "push" {
			a.f.protect("git subtree push")
		}
	}
}

var gitPushValueFlags = map[string]bool{"-o": true, "--push-option": true, "--receive-pack": true, "--exec": true, "--repo": true}

func (a *trustAnalyzer) gitPush(rest []string, dir string, cfgOverride bool) {
	var pos []string
	for j := 0; j < len(rest); j++ {
		x := rest[j]
		if strings.HasPrefix(x, "-") {
			switch {
			case x == "--all" || x == "--mirror" || x == "--branches":
				a.f.protect("git push " + x + " includes protected branches")
				return
			case !strings.Contains(x, "=") && gitPushValueFlags[x]:
				j++
			}
			continue
		}
		pos = append(pos, x)
	}
	if len(pos) >= 2 {
		for _, ref := range pos[1:] {
			a.pushRef(ref, dir)
		}
		return
	}
	// Bare push: the current branch's push destination.
	if cfgOverride {
		a.f.protect("bare git push with -c / --git-dir: destination unknown")
		return
	}
	a.bareTargets(dir, "bare git push")
}

func (a *trustAnalyzer) bareTargets(dir, what string) {
	if dir == "" || a.tc.PushTargets == nil {
		a.f.protect(what + ": destination unknown")
		return
	}
	targets, err := a.tc.PushTargets(dir)
	if err != nil || len(targets) == 0 {
		a.f.protect(what + ": destination unknown")
		return
	}
	for _, t := range targets {
		if a.isProtected(t) {
			a.f.protect(what + " targets " + t)
			return
		}
	}
}

func (a *trustAnalyzer) pushRef(ref, dir string) {
	if strings.ContainsAny(ref, "$`*") {
		a.f.protect("git push refspec is dynamic or a pattern: " + ref)
		return
	}
	ref = strings.TrimPrefix(ref, "+")
	dst := ref
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		dst = ref[i+1:]
		if dst == "" {
			a.f.protect("git push matching refspec ':'")
			return
		}
	}
	if dst == "HEAD" || strings.HasPrefix(dst, "HEAD") || dst == "@" {
		a.bareTargets(dir, "git push HEAD")
		return
	}
	if a.isProtected(dst) {
		a.f.protect("git push targets " + dst)
	}
}

func (a *trustAnalyzer) isProtected(ref string) bool {
	ref = strings.TrimPrefix(ref, "refs/heads/")
	return a.tc.Protected[ref] || isMainRef(ref)
}

func (a *trustAnalyzer) gh(args []string) {
	for _, x := range args {
		lx := strings.ToLower(x)
		if lx == "merge" || strings.Contains(lx, "/merge") || strings.Contains(lx, "merge_pull") ||
			strings.Contains(lx, "mergepullrequest") || strings.Contains(lx, "automerge") {
			a.f.protect("gh merges a pull request")
			return
		}
	}
	// gh api graphql -f query=... and other bodies: check the whole text.
	if mergeTextRe.MatchString(strings.Join(args, " ")) {
		a.f.protect("gh may merge a pull request")
	}
}

func (a *trustAnalyzer) interpreter(name string, args []string, dir string, depth int) {
	inline := false
	for i, x := range args {
		if (x == "-c" || x == "-e" || x == "--eval" || x == "-E" || x == "-r") && i+1 < len(args) {
			a.text(args[i+1], name+" code")
			inline = true
		}
	}
	if inline {
		return
	}
	for _, p := range plainArgs(args, map[string]bool{"-m": true, "-W": true, "-X": true}) {
		a.script(p.v, dir, depth, false)
		return
	}
	// No script and no inline code: reads stdin, which we cannot see.
	a.unsure(name + " reads code from stdin")
}

// script analyses a script the command runs, from the hook's snapshot.
func (a *trustAnalyzer) script(path, dir string, depth int, asShell bool) {
	abs := path
	if !filepath.IsAbs(abs) {
		abs = absIn(dir, path)
	}
	var content string
	found := false
	for _, s := range a.tc.Scripts {
		// Only a pinned snapshot is what will run; an unpinned script can
		// change between this analysis and its execution.
		if s.Trusted && (s.Path == path || (abs != "" && filepath.Clean(s.Path) == abs)) {
			content, found = s.Content, true
			break
		}
	}
	if !found || content == "" || strings.HasSuffix(content, TruncatedMarker) {
		a.unsure("runs a script whose pinned, full contents were not captured: " + path)
		return
	}
	isShell := asShell || strings.HasSuffix(path, ".sh") || strings.HasSuffix(path, ".bash") || strings.HasSuffix(path, ".zsh")
	if strings.HasPrefix(content, "#!") {
		first := strings.SplitN(content, "\n", 2)[0]
		isShell = isShell || strings.Contains(first, "sh")
		for interp := range interpreters {
			if strings.Contains(first, interp) {
				isShell = false
			}
		}
	}
	if isShell {
		a.line(content, dir, depth+1)
		return
	}
	a.text(content, "script "+path)
}
