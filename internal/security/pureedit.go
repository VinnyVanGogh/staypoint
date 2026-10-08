package security

import (
	"regexp"
	"strings"
)

// Pure-edit inline Python (task-31dea40b). Agents edit files with
// `python3 - <<'EOF' … s.replace(…) … EOF`. Parsing that body as shell made
// every such command "unparseable" (Red), and the trust and Board-rule text
// backstops matched source code pasted inside the script's string literals
// (http.NewRequest, "/merge"), so each edit was held for the Board.
//
// A segment is a pure edit only when every one of these holds; anything else
// is analysed exactly as before:
//   - argv is python/python3[.N] with no arguments or a single "-" (script on
//     stdin), no prefixes or wrappers, and no other redirect;
//   - exactly one heredoc, with a quoted delimiter (the shell expands
//     nothing in the body);
//   - with string literals and comments removed, the code imports only
//     allow-listed modules (no aliasing), never names a process, network,
//     dynamic-code, delete/move or environment primitive, and has no dunder;
//   - no f-string carries a {…} expression (that is code inside a literal);
//   - no path-like literal is absolute, home-relative, climbs out (..), or
//     names StayPoint or agent guard state.
// Such a script can only read and write files relative to its working
// directory; callers also require that directory to be the trusted one.

var pythonInterpRe = regexp.MustCompile(`^python(3(\.\d+)?)?$`)

// pureEditHeredoc reports whether s is a pure-edit Python heredoc.
func pureEditHeredoc(s segment) bool {
	if len(s.argv) == 0 || len(stripPrefixes(s.argv)) != len(s.argv) {
		return false
	}
	if !pythonInterpRe.MatchString(s.argv[0]) {
		return false
	}
	for i, a := range s.argv {
		if i < len(s.dyn) && s.dyn[i] {
			return false
		}
		if i > 0 && a != "-" {
			return false
		}
	}
	if len(s.argv) > 2 {
		return false
	}
	var doc *redirect
	for _, r := range s.redirects {
		if !r.heredoc || doc != nil {
			return false
		}
		doc = r
	}
	if doc == nil || !doc.quoted {
		return false
	}
	return pythonPureEdit(doc.body)
}

var (
	pyImportRe   = regexp.MustCompile(`(?m)^\s*(import|from)\s+(.*)$`)
	pyImportOKRe = regexp.MustCompile(`^(import\s+(re|json|sys|pathlib|textwrap|collections|itertools|functools|string|difflib|os|os\.path)(\s*,\s*(re|json|sys|pathlib|textwrap|collections|itertools|functools|string|difflib|os|os\.path))*\s*(;.*)?|from\s+(pathlib|os\.path|collections|itertools|functools|textwrap|string|difflib|typing)\s+import\s+[\w\s,()]+|from\s+os\s+import\s+path)\s*$`)
	// Primitives a pure edit never names in code. str.replace is fine;
	// Path.replace/rename/unlink move or delete files.
	pyBannedRe  = regexp.MustCompile(`\b(subprocess|system|popen|exec\w*|spawn\w*|fork\w*|kill\w*|remove\w*|unlink|rmdir|removedirs|rename\w*|renames|symlink\w*|hardlink_to|link|chmod|chown|chdir|chroot|environ|putenv|unsetenv|getenv|shutil|pty|socket|ssl|urllib|http|httpx|requests|aiohttp|ftplib|smtplib|telnetlib|webbrowser|ctypes|cffi|multiprocessing|threading|asyncio|signal|importlib|eval|compile|getattr|setattr|delattr|globals|locals|vars|breakpoint|input|sqlite3|pickle|marshal|shelve|tempfile|glob|walk|scandir|expanduser|expandvars|home|abspath|realpath|resolve|sep|modules|mkfifo|mknod|truncate|utime|open_code)\b|__`)
	pyAsRe      = regexp.MustCompile(`\bas\b`)
	pyReplaceRe = regexp.MustCompile(`\.replace\s*\(`)
	// Path-like literal: no whitespace, absolute/home/parent, or protected.
	pyPathBadRe = regexp.MustCompile(`^(/|~|\.\.)|/\.\.(/|$)|\.staypoint|\.claude|\.gemini|\.ssh|LaunchAgents|\.local/bin|\.git/|\.zshrc|\.bashrc|\.profile`)
)

// pythonPureEdit applies the rules above to a Python script body.
func pythonPureEdit(body string) bool {
	code, lits, ok := splitPython(body)
	if !ok {
		return false
	}
	// Path.replace moves files; only str.replace on a value is allowed, and
	// that cannot be told apart cheaply, so a .replace( whose receiver is a
	// Path(...) call is rejected below via the banned "resolve"/"Path(" mix:
	// reject any Path(...).replace( explicitly.
	if strings.Contains(code, "Path(") && pyReplaceRe.MatchString(code) && strings.Contains(code, ").replace(") {
		return false
	}
	if pyBannedRe.MatchString(code) {
		return false
	}
	for _, m := range pyImportRe.FindAllString(code, -1) {
		stmt := strings.TrimSpace(m)
		if pyAsRe.MatchString(stmt) || !pyImportOKRe.MatchString(stmt) {
			return false
		}
	}
	for _, l := range lits {
		if strings.ContainsAny(l, " \t\n") {
			continue // prose or source text, not a path
		}
		if pyPathBadRe.MatchString(l) {
			return false
		}
	}
	return true
}

// splitPython returns body's code with string literals and comments replaced
// by placeholders, and the literal contents. ok is false when the body cannot
// be split (unterminated literal) or an f-string holds an expression.
func splitPython(body string) (code string, lits []string, ok bool) {
	var b strings.Builder
	rs := []rune(body)
	for i := 0; i < len(rs); {
		c := rs[i]
		if c == '#' {
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
			continue
		}
		if c == '\'' || c == '"' {
			// Prefix letters (r, b, u, f and combinations) directly before.
			j := i
			for j > 0 && strings.ContainsRune("rRbBuUfF", rs[j-1]) {
				j--
			}
			if j > 0 && (isIdentRune(rs[j-1])) {
				j = i // part of an identifier, not a prefix
			}
			prefix := strings.ToLower(string(rs[j:i]))
			isF := strings.Contains(prefix, "f")
			raw := strings.Contains(prefix, "r")
			q := string(c)
			triple := i+2 < len(rs) && rs[i+1] == c && rs[i+2] == c
			if triple {
				q = strings.Repeat(string(c), 3)
			}
			start := i + len([]rune(q))
			end := -1
			for k := start; k < len(rs); k++ {
				if rs[k] == '\\' && !raw {
					k++
					continue
				}
				if rs[k] == '\\' && raw {
					k++ // raw strings still can't end on an escaped quote
					continue
				}
				if !triple && rs[k] == '\n' {
					return "", nil, false
				}
				if strings.HasPrefix(string(rs[k:]), q) {
					end = k
					break
				}
			}
			if end < 0 {
				return "", nil, false
			}
			lit := string(rs[start:end])
			if isF && strings.ContainsRune(lit, '{') {
				return "", nil, false
			}
			lits = append(lits, lit)
			b.WriteString(`""`)
			i = end + len([]rune(q))
			continue
		}
		b.WriteRune(c)
		i++
	}
	return b.String(), lits, true
}

func isIdentRune(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// dataHeredocToRelative reports whether s is `cat`/`tee` writing one quoted
// heredoc into relative, non-protected files (`cat > f <<'EOF'`,
// `cat >> f <<'EOF'`, `tee f <<'EOF'`): the body is file content, not code.
func dataHeredocToRelative(s segment) bool {
	if len(s.argv) == 0 || (s.argv[0] != "cat" && s.argv[0] != "tee") || s.piped {
		return false
	}
	var targets []string
	docs := 0
	for _, r := range s.redirects {
		switch {
		case r.heredoc:
			if !r.quoted {
				return false
			}
			docs++
		case r.op == ">" || r.op == ">>":
			if r.dyn || r.meta {
				return false
			}
			targets = append(targets, r.target)
		default:
			return false
		}
	}
	for i, a := range s.argv[1:] {
		if s.argv[0] == "cat" || strings.HasPrefix(a, "-") || (i+1 < len(s.dyn) && s.dyn[i+1]) {
			if s.argv[0] == "cat" || a != "-a" {
				return false
			}
			continue
		}
		targets = append(targets, a)
	}
	if docs != 1 || len(targets) == 0 {
		return false
	}
	for _, t := range targets {
		if t == "" || pyPathBadRe.MatchString(t) {
			return false
		}
	}
	return true
}

// stripPureEditBodies returns line with the bodies of pure-edit Python
// heredocs removed, for the text-based Board-rule backstop. A line that does
// not parse is returned unchanged.
func stripPureEditBodies(line string) string {
	segs, _, err := parseShell(line)
	if err != nil {
		return line
	}
	for _, s := range segs {
		if !pureEditHeredoc(s) && !dataHeredocToRelative(s) {
			continue
		}
		for _, r := range s.redirects {
			if r.body != "" {
				line = strings.Replace(line, r.body, "", 1)
			}
		}
	}
	return line
}
