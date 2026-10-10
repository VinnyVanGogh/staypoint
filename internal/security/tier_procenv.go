package security

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Board review #4 (task-7d279c9d), defence in depth behind the daemon-held
// run tokens: commands that read another process's environment, and the
// spellings that ran `staypoint` without the classifier seeing the name.

// psValueFlags take a separate value (ps -o pid,etime, ps -p 12).
var psValueFlags = map[string]bool{
	"-o": true, "-O": true, "-p": true, "-U": true, "-u": true, "-G": true,
	"-g": true, "-t": true, "-M": true, "-N": true,
}

// classifyPs makes ps Red when it prints process environments: -E on macOS
// (ps -Eww, ps -E -p PID) and the BSD/Linux `e` modifier (ps eww, ps auxe).
// Another same-uid process's environment holds its run token.
func classifyPs(args []string, v *Verdict) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case psValueFlags[a]:
			i++
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-"):
			if strings.Contains(a, "E") {
				v.raise(Red, "ps -E: prints other processes' environments (run tokens)")
				return
			}
		case i == 0 && isLetters(a) && strings.Contains(a, "e"):
			v.raise(Red, "ps "+a+": the e modifier prints other processes' environments (run tokens)")
			return
		}
	}
	v.raise(Yellow, "")
}

func isLetters(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

// expandBraces expands shell brace alternatives ({a,b}, nested) the way the
// shell will before the command sees the path. Ranges ({1..3}) are left as
// written; they do not spell names. Capped so a crafted token cannot blow up.
func expandBraces(s string, depth int) []string {
	open := strings.IndexByte(s, '{')
	if open < 0 || depth > 8 {
		return []string{s}
	}
	level, close := 0, -1
	var commas []int
	for i := open; i < len(s) && close < 0; i++ {
		switch s[i] {
		case '{':
			level++
		case '}':
			level--
			if level == 0 {
				close = i
			}
		case ',':
			if level == 1 {
				commas = append(commas, i)
			}
		}
	}
	if close < 0 {
		return []string{s}
	}
	pre, post := s[:open], s[close+1:]
	if len(commas) == 0 {
		// {x} is literal; look for braces after it.
		var out []string
		for _, rest := range expandBraces(post, depth+1) {
			out = append(out, pre+s[open:close+1]+rest)
		}
		return out
	}
	var alts []string
	start := open + 1
	for _, cm := range append(commas, close) {
		alts = append(alts, s[start:cm])
		start = cm + 1
	}
	var out []string
	for _, a := range alts {
		out = append(out, expandBraces(pre+a+post, depth+1)...)
		if len(out) > 256 {
			break
		}
	}
	return out
}

// procEnvironRe matches Linux /proc/<pid>/environ (and /proc/self/environ).
var procEnvironRe = regexp.MustCompile(`^/proc/[^/]+/environ$`)

// isStaypointBin reports whether argv0 runs the staypoint binary under
// another name: a symlink or hard link to it (`ln -s $(which staypoint)
// /tmp/sp2; /tmp/sp2 mcp`). Copies are caught when they are made
// (copiesStaypoint).
func (c *Classifier) isStaypointBin(argv0 string) bool {
	if argv0 == "" || strings.ContainsAny(argv0, "$`") {
		return false
	}
	p := c.expandHome(argv0)
	switch {
	case !strings.Contains(p, "/"):
		lp, err := exec.LookPath(p)
		if err != nil {
			return false
		}
		p = lp
	case !filepath.IsAbs(p):
		if c.CWD == "" {
			return false
		}
		p = filepath.Join(c.CWD, p)
	}
	fi, err := os.Stat(p)
	if err != nil {
		return false
	}
	for _, b := range c.staypointBins() {
		if bi, err := os.Stat(b); err == nil && os.SameFile(fi, bi) {
			return true
		}
	}
	return false
}

// staypointBins are the staypoint binaries a link could point at: the
// running hook binary and the staypoint on PATH.
func (c *Classifier) staypointBins() []string {
	if c.StaypointBins != nil {
		return c.StaypointBins
	}
	var out []string
	if self, err := os.Executable(); err == nil && strings.HasPrefix(filepath.Base(self), "staypoint") {
		out = append(out, self)
	}
	if lp, err := exec.LookPath("staypoint"); err == nil {
		out = append(out, lp)
	}
	return out
}

// linkOrCopyCmds make a file under a new name.
var linkOrCopyCmds = map[string]bool{"ln": true, "cp": true, "install": true, "ditto": true, "mv": true, "link": true}

// copiesStaypoint reports whether a link/copy command's arguments name the
// staypoint binary: by name, as a link to it, or through an expansion
// ($(which staypoint), p=$(command -v staypoint); ln -s $p x) on a line that
// mentions staypoint.
func (c *Classifier) copiesStaypoint(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		if staypointWordRe.MatchString(a) || c.isStaypointBin(a) {
			return true
		}
		if strings.ContainsAny(a, "$`") && c.line != nil && staypointWordRe.MatchString(c.line.raw) {
			return true
		}
	}
	return false
}

var staypointWordRe = regexp.MustCompile(`(?i)(^|[/\s($` + "`" + `])staypoint($|[\s)` + "`" + `])`)

// agentCLIs register MCP servers (claude mcp add x -- staypoint mcp).
var agentCLIs = map[string]bool{"claude": true, "gemini": true, "agy": true, "codex": true, "cursor": true, "code": true}

// registersStaypointMCP reports an agent CLI's `mcp` subcommand whose
// arguments start `staypoint mcp` outside the harness.
func registersStaypointMCP(name string, args []string) bool {
	if !agentCLIs[name] {
		return false
	}
	hasMCP := false
	for _, a := range args {
		if a == "mcp" {
			hasMCP = true
		}
	}
	return hasMCP && staypointMCPRe.MatchString(strings.Join(args, " "))
}

// staypointMCPRe also spans JSON: {"command":"staypoint","args":["mcp"]}.
var staypointMCPRe = regexp.MustCompile(`(?i)\bstaypoint\b.{0,40}?\bmcp\b`)

// lineEnvEdits returns the environment edits a segment makes for the
// segments after it on the same line: bare assignments (HOME=/x; ...),
// export/declare/typeset/readonly/local, unset, and source/. (which can set
// anything). They are checked like env VAR=x when a later segment runs
// staypoint.
func lineEnvEdits(s segment) []string {
	argv := stripPrefixes(s.argv)
	if len(argv) == 0 {
		return s.argv
	}
	switch baseCmd(argv) {
	case "export", "declare", "typeset", "readonly", "local", "unset":
		var out []string
		for _, a := range argv[1:] {
			switch {
			case strings.HasPrefix(a, "-"):
			case isAssign(a):
				out = append(out, a)
			default:
				out = append(out, a+"=")
			}
		}
		return out
	case "source", ".":
		return []string{"-source"}
	case "eval", "set", "alias", "hash", "read", "mapfile", "readarray", "getopts", "enable", "trap", "builtin", "command":
		// Can set or export variables, or change what a later name runs,
		// in ways not modelled here: fail closed for a later staypoint.
		return []string{"-opaque:" + baseCmd(argv)}
	case "printf":
		for _, a := range argv[1:] {
			if a == "-v" {
				return []string{"-opaque:printf -v"}
			}
		}
	}
	return nil
}

// reachesHomeWhole reports a copy/archive command, or find -exec, given the
// home directory (or one above it) as a whole: `cp -r ~ /tmp/h`,
// `tar cf - -C ~ .staypoint`, `find ~ -name x -exec cat {} +` read the data
// dir without naming it.
func (c *Classifier) reachesHomeWhole(name string, args []string) bool {
	switch name {
	case "cp", "tar", "zip", "ditto", "cpio", "pax", "bsdtar", "gtar", "7z", "7za", "rsync", "scp", "rclone":
	case "find":
		exec := false
		for _, a := range args {
			if a == "-exec" || a == "-execdir" || a == "-ok" || a == "-okdir" {
				exec = true
			}
		}
		if !exec {
			return false
		}
	default:
		return false
	}
	home := c.home()
	if home == "" {
		return false
	}
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--directory="):
			a = strings.TrimPrefix(a, "--directory=")
		case strings.HasPrefix(a, "-C") && len(a) > 2: // tar -C~
			a = a[2:]
		case strings.HasPrefix(a, "-") || a == "":
			continue
		}
		p := c.expandHome(a)
		if !filepath.IsAbs(p) {
			if c.CWD == "" || !filepath.IsAbs(c.CWD) {
				continue
			}
			p = filepath.Join(c.CWD, p) // cd ~/x && cp -r .. /tmp/h
		}
		p = filepath.Clean(p)
		if p == "/" || strings.HasPrefix(home+"/", p+"/") {
			return true
		}
	}
	return false
}
