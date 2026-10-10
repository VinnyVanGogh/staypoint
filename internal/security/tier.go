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
	`/(?:ship-review/(?:approve|send-back|reject|delete-branch|push-branch)|gate-requests/[^/\s'"]+/decide|settings/(?:security-gate|ship-review))`)

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

// sensitiveScriptRe matches a sensitive dir or token file named in an
// inline script, however case or string concatenation splits it
// ('.st' + 'aypoint'): the script builds paths the classifier cannot see
// (open(os.path.expanduser(...))). A dot before the name is required, so
// the module path github.com/.../staypoint does not match.
var sensitiveScriptRe = regexp.MustCompile(
	`(?i)\.\W{0,6}s\W{0,6}t\W{0,6}a\W{0,6}y\W{0,6}p\W{0,6}o\W{0,6}i\W{0,6}n\W{0,6}t\b|\.(ssh|aws|gnupg)\b|auth_token|board_token`)

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
	// Scripts were judged by their contents (STA-868). A verdict below Red
	// that lists scripts holds only if the command is rewritten to run
	// exactly these bytes (PinCommand); otherwise treat it as Red.
	Scripts []JudgedScript
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
	v.Scripts = append(v.Scripts, o.Scripts...)
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
	// ReadFile, when set, lets the classifier judge a script that a shell is
	// asked to run (bash x.sh, ./x.sh) by its contents when the script lives in
	// a scratch dir (STA-868). Nil keeps such calls Red ("runs an opaque script").
	//
	// Deprecated as a reader: any non-nil value only enables script reading;
	// scripts are read once through ReadScriptOnce via Snap.
	ReadFile func(path string) ([]byte, error)
	// Snap reads each script once per hook run (shared with ScriptRefs so
	// the judged and the snapshotted bytes are the same). Classify creates
	// one per Classify call when ReadFile is set and Snap is nil.
	Snap *Snapshotter
	// CWDTrusted says CWD is the shell's real working directory (from the
	// hook payload), so a relative script path may be resolved against it.
	CWDTrusted bool
	// ScratchDirs are where an agent's throwaway files live: the run's scratch
	// dir and the system temp dirs. Nil means DefaultScratchDirs().
	ScratchDirs []string
	// PushPolicy is the per-project push restriction, used when PushPolicyFor
	// is nil. When "never", all git push operations are classified Red
	// regardless of refspec. "" means no policy was resolved for this
	// Classifier (not "no row": the resolver maps no row to "branch_only"), so
	// it is treated as "never". Pushes to main/master are Red under every
	// policy.
	PushPolicy string
	// PushPolicyFor, when set, resolves the push policy of the repo a push
	// runs in (after -C and cd), replacing PushPolicy. A push whose repo
	// cannot be modelled (--git-dir, --work-tree, -c) is treated as "never".
	PushPolicyFor func(dir string) string
	// TaskID is the daemon run's task, if any: a plain read of its own
	// handoff files (~/.staypoint/handoffs/<TaskID>/) is not a sensitive
	// path, as in the Board rules (ownHandoffRead).
	TaskID string

	line       *lineCtx // facts about the whole command line being classified
	baseCWD    string   // CWD before any `cd` in the line
	inner      bool     // classifying a wrapper's inner command (env/xargs/find -exec ...)
	cwdFromCd  bool     // CWD was set by an absolute `cd` earlier in the line
	vars       pathVars // variables assigned earlier in the line
	dotglob    bool     // the line may make globs match leading dots
	cdLost     bool     // a cd earlier in the line went somewhere unknown
	ownHandoff bool     // the line only reads the task's own handoff files
	cdpath     bool     // the line may set CDPATH, so a relative cd is unknown
	inSubst    bool     // classifying a $(...) or <(...) body, whose output is used
	pipedOut   bool     // a wrapper's output is piped or written (classifyInner)
}

// Classify classifies a shell command line. Unparseable input is Red (fail closed).
func (c *Classifier) Classify(line string) Verdict {
	cc := *c
	if c.ReadFile != nil && c.Snap == nil {
		cc.Snap = NewSnapshotter() // fresh reads for this command line
	}
	cc.ownHandoff = c.TaskID != "" && ownHandoffRead(line, c.TaskID)
	// An inherited CDPATH makes every relative cd target unknown.
	cc.cdpath = c.cdpath || os.Getenv("CDPATH") != ""
	return cc.classifyLine(line, 0)
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
	lineDots := c.dotglob || dotglobRe.MatchString(line)
	cdpath := c.cdpath || cdpathRe.MatchString(unquoter.Replace(line))
	for _, s := range subs {
		sc := *c
		sc.dotglob, sc.cdpath, sc.inSubst = lineDots, cdpath, true
		v.merge(sc.classifyLine(s, depth+1))
	}
	lc := c.lineContext(line, segs, subs, depth)
	dir := c.CWD
	fromCd := c.cwdFromCd
	vars := c.vars.clone()
	cdLost := c.cdLost
	for i, s := range segs {
		cc := *c
		cc.CWD, cc.line, cc.cwdFromCd = dir, lc, fromCd
		cc.vars, cc.dotglob, cc.cdLost, cc.cdpath = vars, lineDots, cdLost, cdpath
		if cc.baseCWD == "" {
			cc.baseCWD = c.CWD
		}
		cc.classifySegment(s, &v, depth)
		cc.noteAssignments(s, vars)
		if a := stripPrefixes(s.argv); baseCmd(a) == "cd" {
			fromCd = len(a) == 2 && filepath.IsAbs(a[1]) && !segDyn(s, len(s.argv)-len(a)+1)
		}
		dir = cc.nextDir(s, dir)
		if dirChangers[baseCmd(stripPrefixes(dropKeywords(s.argv)))] && dir == "" {
			cdLost = true
		}
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
	"comm": true, "join": true, "paste": true, "tac": true, "rev": true, "fold": true,
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
	"ls-remote": true, "for-each-ref": true, "merge-base": true, "name-rev": true, "count-objects": true,
	"cherry": true, "symbolic-ref": true,
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
		if r.heredoc {
			continue // target is the delimiter word, not a path
		}
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
	// A pure-edit Python heredoc only reads and writes files relative to its
	// working directory (task-31dea40b); when that is the trusted cwd, it is
	// an ordinary edit, not a script to hold. Off while pureEditAutoAllow is
	// false (Board, 2026-10-08): pureEditHeredoc never matches.
	if c.CWDTrusted && !c.cwdFromCd && c.CWD != "" && pureEditHeredoc(s, c.CWD) {
		v.raise(Yellow, "")
		return
	}
	name := baseCmd(argv)
	args := argv[1:]
	shellFedByHeredoc := c.classifyHeredocs(s, name, args, v, depth)

	if why, ok := alwaysRed[name]; ok {
		v.raise(Red, name+": "+why)
	}
	if strings.HasPrefix(name, "mkfs.") {
		v.raise(Red, name+": filesystem format")
	}
	bases := c.argBases(name, args)
	if c.cdpath && dirChangers[name] {
		for i := range bases {
			bases[i] = unresolved // CDPATH picks where a relative target is
		}
	}
	for i, a := range args {
		c.checkPathIn(a, bases[i], v)
	}
	// find's or fd's list of names, piped on or substituted into a command
	// line, feeds whatever reads it (find ~ -name x | xargs cat).
	feeds := s.piped || c.pipedOut || c.inSubst || writesFile(s)
	if recursiveOver(name, args) || ((name == "find" || name == "fd" || name == "fdfind") && feeds) {
		c.checkRecursive(name, args, bases, v)
	}

	switch {
	case wrappers[name]:
		v.raise(Yellow, "")
		if inner := skipWrapper(name, args); len(inner) > 0 {
			wc := *c
			wc.pipedOut = c.pipedOut || s.piped || writesFile(s)
			wc.classifyInner(inner, v, depth)
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
		idx, ok := shellScriptIndex(args)
		off := len(s.argv) - len(argv)
		call := scriptCall{name: name, prefixed: off > 0}
		if ok {
			call.token, call.flags = args[idx], args[:idx]
			call.tokenDyn = segDyn(s, off+1+idx) || segMeta(s, off+1+idx)
		}
		switch {
		case ok && c.scriptVerdict(call, v, depth):
		case ok:
			v.raise(Red, name+": runs an opaque script")
		case shellFedByHeredoc && !shellReadsInput(args):
			// body already classified as the script
		default:
			v.raise(Red, name+": interactive or opaque shell")
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
		off := len(s.argv) - len(argv)
		call := scriptCall{name: name, token: argv[0], direct: true, prefixed: off > 0,
			tokenDyn: segDyn(s, off) || segMeta(s, off)}
		if strings.Contains(argv[0], "/") && c.scriptVerdict(call, v, depth) {
			return
		}
		v.raise(Yellow, "")
	}
}

func (c *Classifier) classifyInner(argv []string, v *Verdict, depth int) {
	if depth > maxDepth {
		v.raise(Red, "command nesting too deep to analyse")
		return
	}
	ic := *c
	ic.inner = true
	ic.classifySegment(segment{argv: argv}, v, depth+1)
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
		// Per-project push policy (STA-562): "never" means agents must not
		// push. Anything but a known permissive policy is "never". The
		// main/master and force/delete checks below apply under every policy.
		if c.pushPolicyAt(effectiveDir) == "never" {
			v.raise(Red, "git push denied: project push_policy is 'never'; the Board pushes after Ship Review")
			return
		}
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
		} else if c.baseCWD != "" && c.baseCWD != c.CWD && barePushTargetsMain(rebaseGitDir(effectiveDir, c.CWD, c.baseCWD), rest) {
			// A `cd` earlier in the line may sit in a subshell we cannot see, so
			// the push is checked from the original directory too.
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
	case "worktree":
		if len(rest) == 0 || rest[0] != "list" {
			v.raise(Yellow, "")
		}
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

// pushPolicyAt returns the push policy for a push run in dir: "branch_only"
// or "pr" when the project allows agent pushes, else "never". With
// PushPolicyFor set, a dir that cannot be modelled ("") is "never", and when a
// `cd` earlier in the line may sit in a subshell the original directory's
// policy must allow the push too.
func (c *Classifier) pushPolicyAt(dir string) string {
	norm := func(p string) string {
		if p == "branch_only" || p == "pr" {
			return p
		}
		return "never"
	}
	if c.PushPolicyFor == nil {
		return norm(c.PushPolicy)
	}
	if dir == "" {
		return "never"
	}
	p := norm(c.PushPolicyFor(dir))
	if p != "never" && c.baseCWD != "" && c.baseCWD != c.CWD {
		alt := rebaseGitDir(dir, c.CWD, c.baseCWD)
		if alt == "" || norm(c.PushPolicyFor(alt)) == "never" {
			return "never"
		}
	}
	return p
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
	if sensitiveScriptRe.MatchString(script) {
		v.raise(Red, "inline script names a sensitive path (~/.staypoint, ~/.ssh, ~/.aws, ~/.gnupg or a token file)")
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
func (c *Classifier) checkPath(tok string, v *Verdict) { c.checkPathIn(tok, "", v) }

// checkPathIn is checkPath for a word that a -C or --directory before it
// makes relative to base ("" for the working directory).
func (c *Classifier) checkPathIn(tok, base string, v *Verdict) {
	cands := []string{tok}
	if i := strings.IndexByte(tok, '='); i > 0 {
		cands = append(cands, tok[i+1:])
	}
	for _, cand := range cands {
		if cand == "" || strings.Contains(cand, "://") || harmlessPaths[cand] {
			continue
		}
		c.checkSensitive(cand, base, v)
		exp := c.expandHome(cand)
		if !looksLikePath(cand) && exp == cand {
			continue
		}
		if c.Worktree != nil {
			if err := c.Worktree.Check(exp); err != nil {
				v.raise(Red, "path outside worktree: "+cand)
			}
		}
	}
}

// checkSensitive raises Red when word may name a sensitive dir or
// something in it, however it is spelled (pathspell.go). The run's own
// scratch dir (under ~/.staypoint/scratch) and, for a line that only reads
// them, its own handoff files are exempt when named literally.
func (c *Classifier) checkSensitive(word, base string, v *Verdict) {
	for _, alt := range c.pathAlts(word) {
		abs, unknown := c.resolvePattern(alt, base)
		if (unknown || strings.Contains(abs, unresolved)) && unknownRootTouches(abs, c.dotglob) {
			v.raise(Red, "may touch a sensitive path: "+word+" (cannot resolve where it points)")
			return
		}
		if unknown {
			continue
		}
		literal := !hasWild(abs)
		clean := filepath.Clean(abs)
		for _, d := range c.sensitiveDirs() {
			if under, _ := patternReach(abs, d, c.dotglob); !under {
				continue
			}
			if literal && (c.inScratchUnder(d, clean) || c.ownHandoffPath(clean)) {
				continue
			}
			v.raise(Red, "touches sensitive path "+d)
		}
	}
}

// pathAlts expands a word's ~, variables and braces into the path
// patterns it may name.
func (c *Classifier) pathAlts(word string) []string {
	w, _ := c.expandWord(word, c.vars)
	return braceExpand(w)
}

// resolvePattern returns the absolute path pattern an expanded word (one of
// pathAlts) names. unknown is true when its root cannot be known: it starts
// with an unresolved expansion, or is relative to a directory that is not
// known.
func (c *Classifier) resolvePattern(w, base string) (abs string, unknown bool) {
	switch {
	case strings.HasPrefix(w, "/"):
		return w, false
	case strings.HasPrefix(w, unresolved):
		return w, true
	}
	dir := base
	if dir == "" {
		dir = c.CWD
	}
	if dir == "" || strings.HasPrefix(dir, unresolved) {
		return w, true
	}
	return dir + "/" + w, false
}

// ownHandoffPath reports a literal path in the task's own handoff dir, on a
// line that only reads such files (ownHandoffRead).
func (c *Classifier) ownHandoffPath(clean string) bool {
	h := c.home()
	if !c.ownHandoff || h == "" || !taskIDRe.MatchString(c.TaskID) {
		return false
	}
	own := filepath.Join(h, ".staypoint", "handoffs", c.TaskID)
	return clean == own || strings.HasPrefix(clean, own+"/")
}

// chdirTakers take -C<dir> glued as a directory to work in.
var chdirTakers = map[string]bool{"tar": true, "bsdtar": true, "gtar": true, "git": true, "make": true, "gmake": true, "env": true, "go": true}

// argBases returns, for each argument, the directory a -C, --directory or
// --chdir before it makes relative paths start from ("" for the working
// directory; unresolved when it cannot be resolved).
func (c *Classifier) argBases(name string, args []string) []string {
	bases := make([]string, len(args))
	base := ""
	set := func(d string) {
		alts := c.pathAlts(d)
		abs, unknown := c.resolvePattern(alts[0], base)
		if unknown || len(alts) > 1 {
			abs = unresolved
		}
		base = abs
	}
	for i := 0; i < len(args); i++ {
		bases[i] = base
		a := args[i]
		switch {
		case a == "-C" || a == "--directory" || a == "--chdir" || a == "--cwd":
			if i+1 < len(args) {
				bases[i+1] = base
				i++
				set(args[i])
			}
		case strings.HasPrefix(a, "--directory=") || strings.HasPrefix(a, "--chdir=") || strings.HasPrefix(a, "--cwd="):
			set(a[strings.IndexByte(a, '=')+1:])
		case strings.HasPrefix(a, "-C") && len(a) > 2 && chdirTakers[name]:
			set(strings.TrimPrefix(a[2:], "="))
		}
	}
	return bases
}

// recursiveOver reports a command that reads, copies, archives or changes
// whole trees under its path operands, hidden dirs included.
func recursiveOver(name string, args []string) bool {
	short := func(chars string) bool {
		for _, a := range args {
			if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsAny(a[1:], chars) {
				return true
			}
		}
		return false
	}
	long := func(names ...string) bool {
		for _, a := range args {
			for _, n := range names {
				if a == n || strings.HasPrefix(a, n+"=") {
					return true
				}
			}
		}
		return false
	}
	switch name {
	case "cp", "rsync", "scp":
		return short("rRa") || long("--recursive", "--archive")
	case "tar", "bsdtar", "gtar", "ditto", "pax", "cpio", "7z", "7za", "7zz":
		return true
	case "zip":
		return short("rR") || long("--recurse-paths")
	case "diff":
		return short("r") || long("--recursive")
	case "chmod", "chown", "chgrp", "chflags":
		return short("R") || long("--recursive")
	case "grep", "egrep", "fgrep", "zgrep", "ggrep":
		for i, a := range args {
			switch {
			case a == "--recursive" || a == "--dereference-recursive" || a == "--directories=recurse" || a == "--recurse":
				return true
			case (a == "-d" || a == "--directories") && i+1 < len(args) && args[i+1] == "recurse":
				return true
			case len(a) > 1 && a[0] == '-' && a[1] != '-':
				for _, ch := range a[1:] {
					if ch == 'r' || ch == 'R' {
						return true
					}
					// A value follows: the rest of the word is data.
					if strings.ContainsRune("efmABCdD", ch) {
						break
					}
				}
			}
		}
	case "rg", "ag":
		u := 0
		for _, a := range args {
			if a == "--hidden" || a == "--unrestricted" || a == "--all-types" && name == "ag" {
				return true
			}
			if len(a) > 1 && a[0] == '-' && a[1] != '-' {
				if strings.ContainsRune(a[1:], '.') {
					return true
				}
				u += strings.Count(a[1:], "u")
			}
		}
		return u >= 2 || (name == "ag" && u >= 1)
	case "find":
		for _, a := range args {
			switch a {
			case "-exec", "-execdir", "-ok", "-okdir", "-delete", "-fprint", "-fprint0", "-fprintf", "-fls":
				return true
			}
		}
	case "fd", "fdfind":
		return short("xXHu") || long("--exec", "--exec-batch", "--hidden", "--unrestricted", "--no-ignore")
	}
	return false
}

// recursiveValueOpts are options of recursive commands whose value is the
// next word; a short cluster ending in one of the letters takes it too.
var recursiveValueOpts = map[string]string{
	"grep": "efmABCdD", "egrep": "efmABCdD", "fgrep": "efmABCdD", "zgrep": "efmABCdD", "ggrep": "efmABCdD",
	"rg": "efgtTABCmMj", "ag": "GgABCm", "tar": "fCbT", "bsdtar": "fCbT", "gtar": "fCbT", "zip": "bnti",
	"cp": "", "rsync": "efB", "diff": "xXSIF", "chmod": "", "chown": "", "chgrp": "",
}

// optTakesValue reports an option of a recursive command whose value is
// the next word. Long options take one unless written --opt=value, except
// a few flags we know.
func optTakesValue(name, a string) bool {
	if strings.HasPrefix(a, "--") {
		if strings.Contains(a, "=") {
			return false
		}
		switch a {
		case "--regexp", "--file", "--include", "--exclude", "--exclude-dir", "--glob", "--type", "--type-not",
			"--max-count", "--context", "--after-context", "--before-context", "--directories", "--devices",
			"--exclude-from", "--files-from", "--rsh", "--filter":
			return true
		}
		return false
	}
	letters := recursiveValueOpts[name]
	return letters != "" && len(a) > 1 && strings.ContainsRune(letters, rune(a[len(a)-1]))
}

// findLeadFlags are find options before its starting points.
var findLeadFlags = map[string]bool{"-H": true, "-L": true, "-P": true, "-E": true, "-X": true, "-s": true, "-x": true, "-d": true, "-O0": true, "-O1": true, "-O2": true, "-O3": true}

// checkRecursive raises Red when a recursive command's path operand may be
// a parent of a sensitive dir (cp -r ~ /tmp/h, find ~ -exec cat {} +), or
// cannot be resolved.
func (c *Classifier) checkRecursive(name string, args, bases []string, v *Verdict) {
	type operand struct{ word, base string }
	var ops []operand
	if name == "find" {
		i := 0
		for i < len(args) && findLeadFlags[args[i]] {
			i++
		}
		for ; i < len(args) && !strings.HasPrefix(args[i], "-") && args[i] != "(" && args[i] != "!"; i++ {
			ops = append(ops, operand{args[i], bases[i]})
		}
		if len(ops) == 0 {
			ops = append(ops, operand{".", ""})
		}
	} else {
		searcher := name == "rg" || name == "ag" || name == "fd" || name == "fdfind" || strings.HasSuffix(name, "grep")
		patGiven := false
		for i := 0; i < len(args); i++ {
			a := args[i]
			switch {
			case a == "" || a == "-" || a == "--":
			case strings.HasPrefix(a, "-"):
				// The value after an option that takes one is not a tree:
				// a pattern, a count, a glob or the archive file.
				if searcher && (a == "-e" || a == "-f" || a == "--regexp" || a == "--file") {
					patGiven = true
				}
				if optTakesValue(name, a) {
					i++
				}
			case searcher && !patGiven:
				patGiven = true // the pattern
			default:
				ops = append(ops, operand{a, bases[i]})
			}
		}
		if len(ops) == 0 && searcher {
			ops = append(ops, operand{".", ""})
		}
	}
	for _, op := range ops {
		for _, alt := range c.pathAlts(op.word) {
			abs, unknown := c.resolvePattern(alt, op.base)
			if unknown {
				// A relative operand in a directory never known (no cwd) is
				// left alone; one a cd or -C moved somewhere unknown is not.
				if strings.HasPrefix(abs, unresolved) || c.cdLost || op.base != "" {
					v.raise(Red, name+" recurses into "+op.word+", which cannot be resolved (it may hold a sensitive path)")
					return
				}
				continue
			}
			for _, d := range c.sensitiveDirs() {
				if _, parent := patternReach(abs, d, true); parent {
					v.raise(Red, name+" recurses into "+op.word+", which holds sensitive path "+d)
					return
				}
			}
		}
	}
}
