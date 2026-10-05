package security

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
)

// boardEndpointRe matches board-only API endpoint path segments that agents must not call.
var boardEndpointRe = regexp.MustCompile(
	`/(?:ship-review/(?:approve|send-back|reject|delete-branch)|gate-requests/[^/\s'"]+/decide|settings/(?:security-gate|ship-review))`)

// boardBootstrapRe matches the board-session bootstrap URL pattern (board_nonce or board_token
// query parameter). An agent fetching this URL (even via GET) would obtain the board cookie.
var boardBootstrapRe = regexp.MustCompile(`[?&]board_(?:nonce|token)=`)

// httpMutationRe detects POST/mutation indicators inside inline scripts.
var httpMutationRe = regexp.MustCompile(
	`(?i)\bPOST\b|\.post\s*\(|urlopen\s*\(|method\s*[=:]\s*["']\s*POST`)

// staypointTokenFileRe matches reads of the staypoint auth/board token files in inline scripts.
var staypointTokenFileRe = regexp.MustCompile(
	`/\.staypoint/(?:auth_token|board_token)\b|['"](auth_token|board_token)['"]`)

// ptyForgingRe matches PTY-creation calls or pty module imports in inline scripts.
var ptyForgingRe = regexp.MustCompile(
	`(?i)\b(?:import\s+pty\b|pty\.spawn|openpty|os\.openpty|zpty|posix_openpt|forkpty)\s*(?:\(|$)`)

// boardCredentialInScriptRe matches board credential names/values in inline scripts
// that are not already covered by staypointTokenFileRe.
var boardCredentialInScriptRe = regexp.MustCompile(`\bboard_(?:nonce|token)\b`)

// Tier is a command risk class.
type Tier int

const (
	// Green is read-only.
	Green Tier = iota
	// Yellow is a local edit; safe to run under an auto-checkpoint.
	Yellow
	// Red is destructive, secret-touching or exfiltrating; needs human confirmation.
	Red
)

func (t Tier) String() string {
	switch t {
	case Green:
		return "green"
	case Yellow:
		return "yellow"
	default:
		return "red"
	}
}

// Verdict is the classification of a command line.
type Verdict struct {
	Tier    Tier
	Reasons []string
}

func (v *Verdict) raise(t Tier, reason string) {
	if t > v.Tier {
		v.Tier = t
	}
	if t == Red {
		v.Reasons = append(v.Reasons, reason)
	}
}

func (v *Verdict) merge(o Verdict) {
	if o.Tier > v.Tier {
		v.Tier = o.Tier
	}
	v.Reasons = append(v.Reasons, o.Reasons...)
}

// Classifier assigns tiers. Worktree, when non-nil, additionally makes any
// path argument that resolves outside it Red.
type Classifier struct {
	Worktree *Boundary
	Home     string // defaults to the user's home directory
	// CWD is the working directory of the command being classified. When set,
	// classifyGit uses it for bare-push branch resolution. If empty the
	// bare-push check is fail-closed (returns Red when no explicit refspec).
	CWD string
}

// Classify classifies a shell command line. Unparseable input is Red (fail closed).
func (c *Classifier) Classify(line string) Verdict {
	return c.classifyLine(line, 0)
}

// ClassifyArgv classifies an already-split command (no shell involved).
func (c *Classifier) ClassifyArgv(argv []string) Verdict {
	var v Verdict
	c.classifySegment(segment{argv: argv}, &v, 0)
	return v
}

const maxDepth = 8

func (c *Classifier) classifyLine(line string, depth int) Verdict {
	var v Verdict
	if depth > maxDepth {
		v.raise(Red, "command nesting too deep to analyse")
		return v
	}
	segs, subs, err := parseShell(line)
	if err != nil {
		v.raise(Red, "unparseable command: "+err.Error())
		return v
	}
	for _, s := range subs {
		v.merge(c.classifyLine(s, depth+1))
	}
	for i, s := range segs {
		c.classifySegment(s, &v, depth)
		// remote-shell pipe: anything | sh
		if i > 0 && segs[i-1].piped {
			if name := baseCmd(stripPrefixes(s.argv)); shells[name] {
				v.raise(Red, "piping into a shell ("+name+")")
			}
		}
	}
	return v
}

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true}

var greenCmds = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "wc": true, "grep": true, "egrep": true,
	"fgrep": true, "rg": true, "pwd": true, "echo": true, "printf": true, "which": true, "type": true,
	"file": true, "stat": true, "du": true, "df": true, "tree": true, "sort": true, "uniq": true,
	"cut": true, "tr": true, "diff": true, "cmp": true, "jq": true, "true": true, "false": true,
	"date": true, "basename": true, "dirname": true, "realpath": true, "readlink": true, "test": true,
	"[": true, "whoami": true, "uname": true, "id": true, "nl": true, "column": true, "less": true,
	"more": true, "sleep": true, "seq": true, "expr": true, "md5sum": true, "shasum": true, "sha256sum": true,
	"awk": true, "sed": true, "find": true, "xargs": true, "cd": true,
}

var greenGit = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true, "rev-parse": true, "ls-files": true,
	"blame": true, "describe": true, "shortlog": true, "grep": true, "ls-tree": true, "cat-file": true,
	"rev-list": true, "remote": true, "branch": true, "tag": true, "config": true, "fetch": true,
}

var greenGo = map[string]bool{"version": true, "list": true, "vet": true, "env": true, "doc": true}

var alwaysRed = map[string]string{
	"sudo": "privilege escalation", "doas": "privilege escalation", "su": "privilege escalation",
	"nc": "raw network access", "ncat": "raw network access", "netcat": "raw network access",
	"socat": "raw network access", "telnet": "raw network access", "ftp": "raw network access",
	"scp": "remote copy", "sftp": "remote copy", "ssh": "remote shell", "rsync": "remote copy",
	"mkfs": "filesystem format", "dd": "raw disk write", "shutdown": "system power", "reboot": "system power",
	"halt": "system power", "poweroff": "system power", "crontab": "persistent scheduler",
	"launchctl": "service manager", "systemctl": "service manager", "chroot": "chroot",
	"security": "keychain access", "gpg": "keyring access", "openssl": "key material handling",
	// TTY-forging tools: used to defeat stdout-is-a-terminal guards.
	"script":   "TTY-forging tool (bypasses terminal checks)",
	"unbuffer": "TTY-forging tool (bypasses terminal checks)",
	"expect":   "TTY-forging / automation tool (bypasses terminal checks)",
}

// wrapper commands whose real command follows their own flags.
var wrappers = map[string]bool{
	"env": true, "nohup": true, "time": true, "nice": true, "command": true, "exec": true,
	"timeout": true, "stdbuf": true, "ionice": true, "builtin": true, "xargs": true,
}

func baseCmd(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	return strings.ToLower(filepath.Base(argv[0]))
}

func isAssign(s string) bool {
	i := strings.IndexByte(s, '=')
	if i <= 0 {
		return false
	}
	for j, r := range s[:i] {
		if !(r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (j > 0 && r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// stripPrefixes drops leading VAR=x assignments.
func stripPrefixes(argv []string) []string {
	for len(argv) > 0 && isAssign(argv[0]) {
		argv = argv[1:]
	}
	return argv
}

func (c *Classifier) classifySegment(s segment, v *Verdict, depth int) {
	argv := stripPrefixes(s.argv)

	for _, r := range s.redirects {
		if r.target != "" && !strings.HasPrefix(r.op, "<") && !strings.HasPrefix(r.op, ">&") {
			if r.target != "/dev/null" {
				v.raise(Yellow, "")
			}
		}
		c.checkPath(r.target, v)
	}
	if len(argv) == 0 {
		return
	}
	name := baseCmd(argv)
	args := argv[1:]

	if why, ok := alwaysRed[name]; ok {
		v.raise(Red, name+": "+why)
	}
	if strings.HasPrefix(name, "mkfs.") {
		v.raise(Red, name+": filesystem format")
	}
	for _, a := range args {
		c.checkPath(a, v)
	}

	switch {
	case wrappers[name]:
		v.raise(Yellow, "")
		if inner := skipWrapper(name, args); len(inner) > 0 {
			c.classifyInner(inner, v, depth)
		}
		return
	case shells[name]:
		v.raise(Yellow, "")
		for i, a := range args {
			if strings.HasPrefix(a, "-") && strings.Contains(a, "c") && !strings.HasPrefix(a, "--") && i+1 < len(args) {
				v.merge(c.classifyLine(args[i+1], depth+1))
				return
			}
		}
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			v.raise(Red, name+": interactive or opaque shell")
		} else {
			v.raise(Red, name+": runs an opaque script")
		}
		return
	case name == "eval":
		v.merge(c.classifyLine(strings.Join(args, " "), depth+1))
		v.raise(Yellow, "")
		return
	case name == "rm":
		v.raise(Yellow, "")
		for _, a := range args {
			if a == "--recursive" || (strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsAny(a, "rR")) {
				v.raise(Red, "rm with recursive flag")
			}
		}
	case name == "git":
		c.classifyGit(args, v)
	case name == "gh":
		c.classifyGh(args, v)
	case name == "staypoint":
		c.classifyStaypoint(args, v)
	case name == "curl" || name == "wget":
		c.classifyFetch(name, args, v)
	case name == "python" || name == "python2" || name == "python3":
		c.classifyScriptInterp(name, args, []string{"-c"}, v)
	case name == "node" || name == "nodejs":
		c.classifyScriptInterp(name, args, []string{"-e", "--eval"}, v)
	case name == "ruby" || name == "perl" || name == "php":
		c.classifyScriptInterp(name, args, []string{"-e"}, v)
	case name == "go":
		if len(args) > 0 && greenGo[args[0]] {
			return
		}
		v.raise(Yellow, "")
	case name == "find":
		for i, a := range args {
			switch a {
			case "-exec", "-execdir", "-ok", "-okdir":
				v.raise(Yellow, "")
				if i+1 < len(args) {
					c.classifyInner(trimUntil(args[i+1:], ";", "+"), v, depth)
				}
			case "-delete":
				v.raise(Yellow, "")
			}
		}
	case name == "sed":
		for _, a := range args {
			if strings.HasPrefix(a, "-i") || a == "--in-place" || strings.HasPrefix(a, "--in-place=") {
				v.raise(Yellow, "")
			}
		}
	case greenCmds[name]:
		// green unless redirected (handled above)
	default:
		v.raise(Yellow, "")
	}
}

func (c *Classifier) classifyInner(argv []string, v *Verdict, depth int) {
	if depth > maxDepth {
		v.raise(Red, "command nesting too deep to analyse")
		return
	}
	c.classifySegment(segment{argv: argv}, v, depth+1)
}

func trimUntil(a []string, stops ...string) []string {
	for i, s := range a {
		for _, st := range stops {
			if s == st {
				return a[:i]
			}
		}
	}
	return a
}

// wrapperValueFlags lists, per wrapper, flags that consume a separate value.
var wrapperValueFlags = map[string][]string{
	"env":     {"-u", "-C", "-S"},
	"timeout": {"-k", "-s", "--kill-after", "--signal"},
	"nice":    {"-n"},
	"ionice":  {"-c", "-n", "-p"},
	"xargs":   {"-I", "-n", "-P", "-L", "-s", "-d", "-E"},
	"stdbuf":  {"-i", "-o", "-e"},
}

// skipWrapper returns the argv of the command a wrapper launches.
func skipWrapper(name string, args []string) []string {
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case isAssign(a):
			i++
		case strings.HasPrefix(a, "-"):
			i++
			for _, f := range wrapperValueFlags[name] {
				if a == f {
					i++
					break
				}
			}
		default:
			if name == "timeout" {
				name = "" // first positional is the duration
				i++
				continue
			}
			return args[i:]
		}
	}
	return nil
}

func (c *Classifier) classifyGit(args []string, v *Verdict) {
	// Skip global options, tracking any -C dir changes for bare-push resolution.
	// -c config-override is skipped; --git-dir/--work-tree can't be modelled → fail-closed.
	effectiveDir := c.CWD
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		switch {
		case args[i] == "-C":
			i++
			if i < len(args) {
				effectiveDir = resolveGitWorkDir(effectiveDir, args[i])
			}
		case strings.HasPrefix(args[i], "-C"):
			effectiveDir = resolveGitWorkDir(effectiveDir, args[i][2:])
		case args[i] == "--git-dir" || args[i] == "--work-tree":
			// Can't model effective repo — fail-closed for bare-push detection.
			effectiveDir = ""
			i++ // consume value
		case strings.HasPrefix(args[i], "--git-dir=") || strings.HasPrefix(args[i], "--work-tree="):
			effectiveDir = ""
		case args[i] == "-c":
			// -c can redirect push destination (remote.*.push, push.default);
			// force bare-push detection fail-closed when present.
			effectiveDir = ""
			i++ // skip key=value token
		}
		i++
	}
	if i >= len(args) {
		return
	}
	sub, rest := args[i], args[i+1:]
	has := func(flags ...string) bool {
		for _, a := range rest {
			for _, f := range flags {
				if a == f || strings.HasPrefix(a, f+"=") {
					return true
				}
			}
		}
		return false
	}
	shortFlag := func(ch byte) bool {
		for _, a := range rest {
			if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.IndexByte(a, ch) > 0 {
				return true
			}
		}
		return false
	}
	switch sub {
	case "reset":
		if has("--hard", "--merge", "--keep") {
			v.raise(Red, "git reset --hard discards work")
		}
		v.raise(Yellow, "")
	case "clean":
		if has("--force") || shortFlag('f') {
			v.raise(Red, "git clean -f deletes untracked files")
		}
		v.raise(Yellow, "")
	case "push":
		if has("--force", "--force-with-lease", "--mirror", "--delete", "--prune", "--all", "--tags") || shortFlag('f') || shortFlag('d') {
			v.raise(Red, "git push rewrites or deletes remote history")
		}
		for _, a := range rest {
			if strings.HasPrefix(a, "+") || strings.HasPrefix(a, ":") && len(a) > 1 {
				v.raise(Red, "git push rewrites or deletes remote history")
			}
		}
		if pushTargetsMain(rest) {
			v.raise(Red, "git push targets main/master; Board approval required")
		}
		if barePushTargetsMain(effectiveDir, rest) {
			v.raise(Red, "git push targets main/master; Board approval required")
		}
		v.raise(Yellow, "")
	case "config":
		// git config --get is read-only; writes are local edits, global writes touch dotfiles
		if !has("--get", "--get-all", "--list", "-l") {
			v.raise(Yellow, "")
		}
		if has("--global", "--system") {
			v.raise(Yellow, "")
		}
	case "branch", "tag", "remote":
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") || a == "-d" || a == "-D" || a == "-m" || a == "-M" || a == "add" ||
				a == "--delete" || a == "set-url" || a == "remove" {
				if a != "-v" && a != "-a" && a != "-r" && a != "--list" && a != "-l" {
					v.raise(Yellow, "")
				}
			}
		}
	case "fetch":
		// network read, updates refs only
	default:
		if !greenGit[sub] {
			v.raise(Yellow, "")
		}
	}
}

// resolveGitWorkDir resolves a git -C argument against the current effective
// dir, mirroring git's cumulative -C application. Returns "" when base is
// empty and rel is relative, so the fail-closed path triggers downstream.
func resolveGitWorkDir(base, rel string) string {
	if rel == "" {
		return base
	}
	if filepath.IsAbs(rel) {
		return rel
	}
	if base == "" {
		return ""
	}
	return filepath.Join(base, rel)
}

// pushValueFlags are git push options that consume the next token as a value.
// Values must not be counted as refspec positionals.
var pushValueFlags = map[string]bool{
	"-o": true, "--push-option": true,
	"--receive-pack": true, "--exec": true, "--repo": true,
}

// barePushTargetsMain reports whether a bare `git push` (no explicit refspec)
// targets main/master by resolving the current branch in dir.
// Returns true (fail-closed) when dir is empty or branch resolution fails —
// a git push should always happen inside a real repo.
// When rest contains 2+ positionals the first is remote and second is a
// refspec — pushTargetsMain already classified it, so we return false.
func barePushTargetsMain(dir string, rest []string) bool {
	var positionals []string
	for j := 0; j < len(rest); j++ {
		a := rest[j]
		if strings.HasPrefix(a, "-") {
			// Skip flag value when flag takes a separate argument and has no = form.
			if !strings.Contains(a, "=") && pushValueFlags[a] {
				j++
			}
			continue
		}
		positionals = append(positionals, a)
	}
	if len(positionals) >= 2 {
		return false
	}
	if dir == "" {
		return true
	}
	// symbolic-ref works on unborn branches; rev-parse --abbrev-ref fails there.
	out, err := gitexec.Command(context.Background(), "-C", dir, "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		return true
	}
	return isMainRef(strings.TrimSpace(string(out)))
}

// isMainRef reports whether a git ref name is a protected default branch.
func isMainRef(ref string) bool {
	ref = strings.TrimPrefix(ref, "refs/heads/")
	return ref == "main" || ref == "master"
}

// pushTargetsMain reports whether any refspec in the push arg list targets main/master.
// The first non-flag positional is the remote; subsequent ones are refspecs.
func pushTargetsMain(args []string) bool {
	var positionals []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			positionals = append(positionals, a)
		}
	}
	// index 0 is the remote (if present); refspecs start at index 1.
	for _, ref := range positionals[min(1, len(positionals)):] {
		if i := strings.LastIndex(ref, ":"); i >= 0 {
			if isMainRef(ref[i+1:]) {
				return true
			}
		} else if isMainRef(ref) {
			return true
		}
	}
	return false
}

// ghValueFlags are gh global flags that consume the next token (so the token
// after them is a value, not a subcommand).
var ghValueFlags = map[string]bool{"-R": true, "--repo": true, "--hostname": true}

func (c *Classifier) classifyGh(args []string, v *Verdict) {
	// Skip global flags (and their values) to find the subcommand.
	// --flag=value form is a single token; --flag value form consumes two.
	i := 0
	for i < len(args) {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			break
		}
		i++
		if ghValueFlags[a] {
			i++ // skip the separate value token
		}
	}
	if i >= len(args) {
		v.raise(Yellow, "")
		return
	}
	sub := args[i]
	rest := args[i+1:]
	switch sub {
	case "pr":
		// Fail closed: any positional "merge" token under gh pr is a merge action.
		for _, a := range rest {
			if !strings.HasPrefix(a, "-") && a == "merge" {
				v.raise(Red, "gh pr merge lands on the default branch; Board approval required")
				return
			}
		}
		v.raise(Yellow, "")
	case "api":
		// Any body-submitting flag means the call mutates state — Red immediately.
		// For the method, only GET and HEAD are safe reads; anything else (POST, PUT,
		// PATCH, DELETE, or any unrecognized/custom method) is Red (fail-closed on unknown).
		// Default when no method flag is present: gh api defaults to GET when no body
		// flags, so we allow that through; body flags above would have already raised Red.
		ghAPIRed := func(reason string) {
			v.raise(Red, reason)
		}
		apiMethodSeen := false
		for j, a := range rest {
			lower := strings.ToLower(a)
			switch {
			// -f/-F/--field/--raw-field/--input (any attached or separate form) → POST body
			case a == "-f" || a == "-F",
				strings.HasPrefix(lower, "--field"),
				strings.HasPrefix(lower, "--raw-field"),
				lower == "--input",
				strings.HasPrefix(lower, "--input="):
				ghAPIRed("gh api submits data; Board approval required")
				return
			// -X METHOD or --method METHOD (separate value token)
			case (a == "-X" || strings.EqualFold(a, "--method")) && j+1 < len(rest):
				m := strings.ToUpper(rest[j+1])
				if m != "GET" && m != "HEAD" {
					ghAPIRed("gh api mutating or unrecognized method; Board approval required")
					return
				}
				apiMethodSeen = true
			// -XMETHOD or -X=METHOD (attached, with or without =)
			case strings.HasPrefix(lower, "-x") && len(a) > 2:
				raw := a[2:]
				m := strings.ToUpper(strings.TrimPrefix(raw, "="))
				if m != "GET" && m != "HEAD" {
					ghAPIRed("gh api mutating or unrecognized method; Board approval required")
					return
				}
				apiMethodSeen = true
			// --method=METHOD (attached with =)
			case strings.HasPrefix(lower, "--method="):
				m := strings.ToUpper(a[strings.Index(a, "=")+1:])
				if m != "GET" && m != "HEAD" {
					ghAPIRed("gh api mutating or unrecognized method; Board approval required")
					return
				}
				apiMethodSeen = true
			}
			// Merge endpoint by URL
			if !strings.HasPrefix(a, "-") && strings.Contains(lower, "/merge") {
				ghAPIRed("gh api targets a merge endpoint; Board approval required")
				return
			}
		}
		// If no explicit method: gh api defaults to GET when no body flags, which is safe.
		// If method was explicitly set and passed the GET/HEAD check above, allow through.
		_ = apiMethodSeen
		v.raise(Yellow, "")
	default:
		v.raise(Yellow, "")
	}
}

// classifyScriptInterp checks scripting-language interpreter calls (python, node, ruby …).
// inlineFlags lists flags that take an inline script as their next argument (e.g. -c, -e).
// It inspects the inline script for board API mutations and staypoint token file reads.
func (c *Classifier) classifyScriptInterp(name string, args []string, inlineFlags []string, v *Verdict) {
	v.raise(Yellow, "")
	for i, a := range args {
		for _, flag := range inlineFlags {
			if a == flag && i+1 < len(args) {
				if classifyInlineScript(args[i+1], v) {
					return
				}
			}
		}
	}
}

// classifyStaypoint classifies `staypoint <sub> …` calls.
// `staypoint board …` is always Red: even with TTY-gating the board subcommand
// contains credentials that agents must never access.
func (c *Classifier) classifyStaypoint(args []string, v *Verdict) {
	if len(args) > 0 && args[0] == "board" {
		v.raise(Red, "staypoint board: accesses board credentials; agents cannot self-approve (use the Board UI)")
		return
	}
	v.raise(Yellow, "")
}

// isMutatingMethod reports whether a HTTP method string is a mutating operation.
func isMutatingMethod(m string) bool {
	switch strings.ToUpper(m) {
	case "POST", "PUT", "PATCH", "DELETE":
		return true
	}
	return false
}

func (c *Classifier) classifyFetch(name string, args []string, v *Verdict) {
	v.raise(Yellow, "")
	for i, a := range args {
		lower := strings.ToLower(a)
		switch {
		case a == "-d" || strings.HasPrefix(lower, "--data") || a == "-F" || strings.HasPrefix(lower, "--form") ||
			a == "-T" || a == "--upload-file" || lower == "--json" ||
			strings.HasPrefix(lower, "--post-") || lower == "--body-data" || lower == "--body-file":
			v.raise(Red, name+": uploads data (possible exfiltration)")
		// -X POST / --request POST  (separate token)
		case (a == "-X" || strings.EqualFold(a, "--request")) && i+1 < len(args):
			if isMutatingMethod(args[i+1]) {
				v.raise(Red, name+": mutating HTTP method (possible exfiltration)")
			}
		// -XPOST / -X=POST  (combined, with or without =)
		case strings.HasPrefix(lower, "-x") && len(a) > 2:
			if isMutatingMethod(strings.TrimPrefix(a[2:], "=")) {
				v.raise(Red, name+": mutating HTTP method (possible exfiltration)")
			}
		// --request=POST (attached with =)
		case strings.HasPrefix(lower, "--request="):
			if isMutatingMethod(a[strings.Index(a, "=")+1:]) {
				v.raise(Red, name+": mutating HTTP method (possible exfiltration)")
			}
		// wget --method=POST / wget --method POST
		case name == "wget" && strings.EqualFold(a, "--method") && i+1 < len(args):
			if isMutatingMethod(args[i+1]) {
				v.raise(Red, name+": mutating HTTP method (possible exfiltration)")
			}
		case name == "wget" && strings.HasPrefix(lower, "--method="):
			if isMutatingMethod(a[strings.Index(a, "=")+1:]) {
				v.raise(Red, name+": mutating HTTP method (possible exfiltration)")
			}
		// wget --post-data / --post-file (already partially caught by --post- prefix above,
		// kept explicit for clarity and to ensure --post-data without = is caught)
		case name == "wget" && (lower == "--post-data" || lower == "--post-file"):
			v.raise(Red, name+": uploads data (possible exfiltration)")
		case a == "-K" || a == "--config" || (a == "-i" && name == "wget"):
			v.raise(Red, name+": reads request definition from a file")
		// curl -c / --cookie-jar: writes cookies to a file.
		// An agent using this flag against the local daemon would save the board session
		// cookie for later use in a mutating request. Also detect short-option clusters
		// that contain 'c' (e.g. -sc, -vc), since curl supports combined short options.
		case name == "curl" && (strings.EqualFold(a, "--cookie-jar") || strings.HasPrefix(lower, "--cookie-jar=")):
			v.raise(Red, name+": writes session cookies to a file (possible credential theft)")
		case name == "curl" && strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a, 'c'):
			v.raise(Red, name+": -c flag writes session cookies to a file (possible credential theft)")
		default:
			// Non-flag argument: check for board-only endpoints or board bootstrap URLs.
			// An agent fetching a board bootstrap URL (even via GET) would steal the
			// board session cookie, granting approve/decide rights.
			if !strings.HasPrefix(a, "-") {
				if boardEndpointRe.MatchString(a) {
					v.raise(Red, name+": URL targets a board-only endpoint (agents cannot call these)")
				}
				if boardBootstrapRe.MatchString(a) {
					v.raise(Red, name+": URL contains board bootstrap credential (board_nonce/board_token)")
				}
			}
		}
	}
}

func (c *Classifier) home() string {
	if c.Home != "" {
		return c.Home
	}
	h, _ := os.UserHomeDir()
	return h
}

// sensitiveDirs are locations whose reads and writes always need confirmation.
func (c *Classifier) sensitiveDirs() []string {
	dirs := []string{"/etc", "/private/etc"}
	if h := c.home(); h != "" {
		for _, d := range []string{".ssh", ".aws", ".gnupg", ".staypoint"} {
			dirs = append(dirs, filepath.Join(h, d))
		}
	}
	return dirs
}

// classifyInlineScript checks an inline script body (from -c / -e / --eval) for:
//   - reads of staypoint token files
//   - HTTP POST mutations to board-only API endpoints
//   - PTY-forging calls that bypass terminal guards
//   - board bootstrap credential patterns (board_nonce / board_token in URLs)
//
// Returns true if a Red verdict was raised.
func classifyInlineScript(script string, v *Verdict) bool {
	if staypointTokenFileRe.MatchString(script) {
		v.raise(Red, "inline script reads staypoint auth/board token file")
		return true
	}
	if boardEndpointRe.MatchString(script) && httpMutationRe.MatchString(script) {
		v.raise(Red, "inline script calls board-only API endpoint (agents cannot self-approve)")
		return true
	}
	if ptyForgingRe.MatchString(script) {
		v.raise(Red, "inline script creates or imports a pseudo-TTY (bypasses terminal guards)")
		return true
	}
	if boardBootstrapRe.MatchString(script) || boardCredentialInScriptRe.MatchString(script) {
		v.raise(Red, "inline script references board bootstrap credential (board_nonce/board_token)")
		return true
	}
	return false
}

func (c *Classifier) expandHome(p string) string {
	h := c.home()
	switch {
	case h == "":
		return p
	case p == "~" || strings.HasPrefix(p, "~/"):
		return filepath.Join(h, p[1:])
	case p == "$HOME" || strings.HasPrefix(p, "$HOME/"):
		return filepath.Join(h, p[len("$HOME"):])
	case p == "${HOME}" || strings.HasPrefix(p, "${HOME}/"):
		return filepath.Join(h, p[len("${HOME}"):])
	}
	return p
}

var harmlessPaths = map[string]bool{"/dev/null": true, "/dev/stdout": true, "/dev/stderr": true, "/dev/stdin": true, "/dev/tty": true}

func looksLikePath(s string) bool {
	return s == "~" || s == ".." || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~/") ||
		strings.HasPrefix(s, "$HOME") || strings.HasPrefix(s, "${HOME}") ||
		strings.HasPrefix(s, "../") || strings.Contains(s, "/../") || strings.HasSuffix(s, "/..")
}

// checkPath escalates for sensitive locations and (when a worktree is set) escapes.
func (c *Classifier) checkPath(tok string, v *Verdict) {
	cands := []string{tok}
	if i := strings.IndexByte(tok, '='); i > 0 {
		cands = append(cands, tok[i+1:])
	}
	for _, cand := range cands {
		if cand == "" || strings.Contains(cand, "://") || harmlessPaths[cand] {
			continue
		}
		exp := c.expandHome(cand)
		if !looksLikePath(cand) && exp == cand {
			continue
		}
		clean := filepath.Clean(exp)
		for _, d := range c.sensitiveDirs() {
			if clean == d || strings.HasPrefix(clean, d+string(filepath.Separator)) {
				v.raise(Red, "touches sensitive path "+d)
			}
		}
		if c.Worktree != nil {
			if err := c.Worktree.Check(exp); err != nil {
				v.raise(Red, "path outside worktree: "+cand)
			}
		}
	}
}
