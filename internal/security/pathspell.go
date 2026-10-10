package security

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Sensitive paths may be spelled many ways the shell resolves to the same
// file (task-97b4fa02): globs (~/.st*/x, ~/.staypoin?/x), braces, case
// (APFS is case-insensitive: $HOME/.STAYPOINT/x), a variable set earlier in
// the line (d=~/.staypoint; cat $d/x), a cd or -C before a relative path
// (cd ~ && cat .staypoint/x, tar -C ~ .staypoint) and recursive commands
// over a parent (cp -r ~ /tmp/h, find ~ -exec cat {} +). A token is read as
// a path pattern and matched component by component, case-folded, against
// each sensitive dir. What cannot be resolved fails closed.

// unresolved stands for an expansion whose value is unknown: it may be any
// text, slashes and leading dots included.
const unresolved = "\x00"

// maxBraceAlts caps brace expansion; past it the word is matched with each
// brace group read as a wildcard.
const maxBraceAlts = 64

// dotglobRe: the line may make * and ? match a leading dot (bash dotglob,
// zsh GLOB_DOTS or a (D) qualifier).
var dotglobRe = regexp.MustCompile(`(?i)dotglob|glob_?dots|\(D[^)]*\)|\*\(D`)

// cdpathRe: the line may set CDPATH, by name or through a name built from
// an expansion (declare "${x}PATH=~", printf -v "$n" ...).
var cdpathRe = regexp.MustCompile(`(?i)cdpath|\$\{\(|\b(export|declare|typeset|local|readonly|printf|read|eval|mapfile|readarray|getopts|set)\b[^;&|\n]*[$\{*?\[]`)

// expansionRe matches what parseShell leaves in a word for an expansion:
// $NAME, ${...}, $SUBST (a command substitution), $ANSI, and special
// parameters.
var expansionRe = regexp.MustCompile(`\$(\{[^}]*\}|[A-Za-z_][A-Za-z0-9_]*|[0-9@*#?$!-])`)

// braceExpand expands {a,b} alternatives. A {x..y} sequence or more than
// maxBraceAlts results reads the group as unresolved (fail closed).
func braceExpand(w string) []string {
	out := []string{w}
	for round := 0; round < 16; round++ {
		var next []string
		changed := false
		for _, s := range out {
			open, close, alts := firstBrace(s)
			if open < 0 {
				next = append(next, s)
				continue
			}
			changed = true
			if alts == nil {
				next = append(next, s[:open]+unresolved+s[close+1:])
				continue
			}
			for _, a := range alts {
				next = append(next, s[:open]+a+s[close+1:])
			}
		}
		out = next
		if !changed {
			return out
		}
		if len(out) > maxBraceAlts {
			return []string{regexp.MustCompile(`\{[^{}]*\}`).ReplaceAllString(w, unresolved)}
		}
	}
	return out
}

// firstBrace finds the first brace group with a top-level comma or a ..
// sequence. alts is nil for a sequence.
func firstBrace(s string) (open, close int, alts []string) {
	for i := 0; i < len(s); i++ {
		if s[i] != '{' || (i > 0 && s[i-1] == '$') {
			continue
		}
		depth, start := 0, i+1
		var parts []string
		for j := i; j < len(s); j++ {
			switch s[j] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					body := s[i+1 : j]
					if len(parts) > 0 {
						return i, j, append(parts, s[start:j])
					}
					if strings.Contains(body, "..") {
						return i, j, nil
					}
					j = len(s)
				}
			case ',':
				if depth == 1 {
					parts = append(parts, s[start:j])
					start = j + 1
				}
			}
		}
	}
	return -1, -1, nil
}

// compMatch reports whether one path component pattern may match name,
// case-folded. Without dots a leading wildcard cannot match a leading dot
// (bash and zsh defaults); a bracket class naming a dot may. An unresolved
// expansion matches anything. A pattern that does not compile matches.
func compMatch(pat, name string, dots bool) bool {
	pat, name = strings.ToLower(pat), strings.ToLower(name)
	if strings.Contains(pat, unresolved) {
		pat = strings.ReplaceAll(pat, unresolved, "*")
		dots = true
	}
	if strings.HasPrefix(name, ".") && !dots && pat != "" {
		switch pat[0] {
		case '*', '?':
			return false
		case '[':
			if end := strings.IndexByte(pat[1:], ']'); end >= 0 && !strings.Contains(pat[:end+2], ".") {
				return false
			}
		}
	}
	// The shell negates a class with ! as well as ^.
	pat = strings.ReplaceAll(pat, "[!", "[^")
	ok, err := filepath.Match(pat, name)
	if err != nil {
		// A [ with no ] after it is literal to the shell; a class Go
		// cannot read ([]a], [a-]) may still match: fail closed.
		if k := strings.IndexByte(pat, '['); k >= 0 && strings.IndexByte(pat[k:], ']') > 0 {
			return true
		}
		return strings.ReplaceAll(pat, "[^", "[!") == name
	}
	return ok
}

func hasWild(s string) bool { return strings.ContainsAny(s, "*?["+unresolved) }

// splitPath splits a cleaned absolute path into components.
func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// patternReach reports how an absolute path pattern relates to dir: under
// is true when it may name dir or something below it, parent when it may
// name a directory above dir. A ** component, or one holding an unresolved
// expansion (which may hold slashes), reaches anything below the
// components before it.
func patternReach(pat, dir string, dots bool) (under, parent bool) {
	pc, dc := splitPath(filepath.Clean(pat)), splitPath(dir)
	for i, p := range pc {
		if strings.Contains(p, "**") || strings.Contains(p, unresolved) {
			if i >= len(dc) {
				return true, false
			}
			return compMatch(p, dc[i], true), false
		}
		if i >= len(dc) {
			return true, false
		}
		if !compMatch(p, dc[i], dots) {
			return false, false
		}
	}
	if len(pc) >= len(dc) {
		return true, false
	}
	return false, true
}

// sensitiveNames are the last components of the home sensitive dirs; a
// path whose root is unknown is held when a component may spell one.
var sensitiveNames = []string{".staypoint", ".ssh", ".aws", ".gnupg"}

// sensitiveFiles are StayPoint credential files, held by name wherever the
// path's root is unknown.
var sensitiveFiles = []string{"auth_token", "board_token"}

// unknownRootTouches reports whether a path whose root is unknown (it
// starts with an unresolved expansion, or is relative to an unknown dir)
// may name a sensitive dir: some component, other than one that is only
// an expansion, may spell a sensitive dir or credential file.
func unknownRootTouches(pat string, names []string, dots bool) bool {
	for _, p := range strings.Split(pat, "/") {
		if p == "" || p == unresolved || p == "." || p == ".." {
			continue
		}
		for _, n := range names {
			if compMatch(p, n, dots) {
				return true
			}
		}
		// A file name must be spelled at least in part: * alone is any file.
		if strings.Trim(p, "*?"+unresolved) == "" {
			continue
		}
		for _, n := range sensitiveFiles {
			if compMatch(p, n, dots) {
				return true
			}
		}
	}
	return false
}

// pathVars tracks variables assigned earlier in a command line, so
// d=~/.staypoint; cat $d/x reads as the path it is. A name mapped to
// unresolved was set to something unknown (read, for, a substitution).
type pathVars map[string]string

func (pv pathVars) clone() pathVars {
	out := make(pathVars, len(pv))
	for k, v := range pv {
		out[k] = v
	}
	return out
}

// varBinders set a variable from input or a list: read x, for x in ...
var varBinders = map[string]bool{"read": true, "for": true, "select": true, "getopts": true, "mapfile": true, "readarray": true, "printf": true}

// noteAssignments records the variables segment s sets for the rest of
// the line: bare assignments and export/declare/local/readonly/typeset.
// Anything else that binds a name makes it unknown.
func (c *Classifier) noteAssignments(s segment, vars pathVars) {
	argv := dropKeywords(s.argv)
	set := func(a string) {
		i := strings.IndexByte(a, '=')
		name := strings.TrimSuffix(a[:i], "+")
		val, _ := c.expandWord(a[i+1:], vars)
		if strings.HasSuffix(a[:i], "+") {
			val = vars[name] + val
		}
		vars[name] = val
	}
	rest := stripPrefixes(argv)
	if len(rest) == 0 {
		for _, a := range argv {
			if isAssign(a) || isAppendAssign(a) {
				set(a)
			}
		}
		return
	}
	name := baseCmd(rest)
	switch {
	case exportLike[name]:
		for _, a := range rest[1:] {
			if isAssign(a) || isAppendAssign(a) {
				set(a)
			}
		}
	case name == "for" && len(rest) > 2 && rest[2] == "in":
		// for p in a b c: p is one of the words, read as a brace group.
		var items []string
		for _, a := range rest[3:] {
			w, _ := c.expandWord(a, vars)
			if strings.ContainsAny(w, "{},") {
				vars[rest[1]] = unresolved
				return
			}
			items = append(items, w)
		}
		switch len(items) {
		case 0:
			vars[rest[1]] = ""
		case 1:
			vars[rest[1]] = items[0]
		default:
			vars[rest[1]] = "{" + strings.Join(items, ",") + "}"
		}
	case varBinders[name] || name == "unset":
		for _, a := range rest[1:] {
			if !strings.HasPrefix(a, "-") && a != "in" {
				vars[a] = unresolved
			}
		}
	}
}

func isAppendAssign(s string) bool {
	i := strings.Index(s, "+=")
	return i > 0 && isAssign(s[:i]+"="+s[i+2:])
}

// expandWord resolves ~, $HOME, $PWD and variables tracked in vars in a
// parsed word. Expansions it cannot resolve become unresolved; known
// reports none were left.
func (c *Classifier) expandWord(w string, vars pathVars) (out string, known bool) {
	home := c.home()
	switch {
	case w == "~" || strings.HasPrefix(w, "~/"):
		if home != "" {
			w = home + w[1:]
		}
	case w == "~+" || strings.HasPrefix(w, "~+/"):
		if c.CWD != "" {
			w = c.CWD + w[2:]
		} else {
			w = unresolved + w[2:]
		}
	case strings.HasPrefix(w, "~"):
		// ~user, ~-: another home or the old dir.
		end := strings.IndexByte(w, '/')
		if end < 0 {
			end = len(w)
		}
		user := w[1:end]
		if user != "" && user != "-" && taskIDRe.MatchString(user) && home != "" {
			w = filepath.Join(filepath.Dir(home), user) + w[end:]
		} else {
			w = unresolved + w[end:]
		}
	}
	known = true
	out = expansionRe.ReplaceAllStringFunc(w, func(m string) string {
		name := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(m, "$"), "{"), "}")
		switch {
		case name == "HOME" && home != "":
			return home
		case name == "PWD" && c.CWD != "":
			return c.CWD
		}
		if v, ok := vars[name]; ok {
			if strings.Contains(v, unresolved) {
				known = false
			}
			return v
		}
		known = false
		return unresolved
	})
	if strings.ContainsAny(out, "$`") {
		known = false
	}
	return out, known
}
