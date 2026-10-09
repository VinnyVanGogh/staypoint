package security

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Pure-edit inline Python (task-31dea40b). Agents edit files with
// `python3 - <<'EOF' … s.replace(…) … EOF`. Parsing that body as shell made
// every such command "unparseable" (Red), and the trust and Board-rule text
// backstops matched source code pasted inside the script's string literals
// (http.NewRequest, "/merge"), so each edit was held for the Board.
//
// The check is an allowlist: a script is a pure edit only when it is proven
// to be one, and anything the checker does not recognise is held as before.
//   - argv is python/python3[.N] with no arguments or a single "-" (script on
//     stdin), no prefixes or wrappers, and no other redirect;
//   - exactly one heredoc, with a quoted delimiter (the shell expands
//     nothing in the body);
//   - the code (string literals and comments removed) is ASCII, has no
//     dunder, backslash, ';' or '@', and imports only re, json, textwrap or
//     difflib on lines of their own;
//   - every name is an allowed keyword or builtin, an imported module, or a
//     variable the script binds (never one shadowing a builtin); every
//     attribute is an allowed str/list/dict/file/re/json method;
//   - open() is only ever called as open(PATH[, MODE][, encoding=LIT]) where
//     PATH is a string literal, or a name bound exactly once to one, MODE is
//     a literal file mode, and PATH is a whitespace-free relative path that
//     stays inside cwd after cleaning, names no protected state, and reaches
//     no symlink or multiply-linked file on disk;
//   - no f-string carries a {…} expression.
// Such a script can only read and write files under its working directory;
// callers also require that directory to be the trusted one.

var pythonInterpRe = regexp.MustCompile(`^python(3(\.\d+)?)?$`)

// pureEditAutoAllow is the one switch for relaxing pure-edit heredocs.
// Auto-allow disabled by the Board 2026-10-08 until the typed-tools MCP
// replaces it (task-3b4575f7); every inline script is held.
const pureEditAutoAllow = false

// pureEditHeredoc reports whether the classifier and trust check may relax s
// as a pure edit. It is always false while pureEditAutoAllow is off.
func pureEditHeredoc(s segment, cwd string) bool {
	return pureEditAutoAllow && pureEditHeredocShape(s, cwd)
}

// pureEditHeredocShape reports whether s is a pure-edit Python heredoc run
// from cwd. With cwd "" the on-disk link checks are skipped. It only
// analyses; whether anything is relaxed is pureEditHeredoc's call.
func pureEditHeredocShape(s segment, cwd string) bool {
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
	return pythonPureEdit(doc.body, cwd)
}

const pyModules = `(re|json|textwrap|difflib)`

var (
	pyImportLineRe = regexp.MustCompile(`(?m)^[ \t]*(import|from)\b.*$`)
	pyImportOKRe   = regexp.MustCompile(`^[ \t]*(import[ \t]+` + pyModules + `([ \t]*,[ \t]*` + pyModules + `)*|from[ \t]+` + pyModules + `[ \t]+import[ \t]+[A-Za-z_]\w*([ \t]*,[ \t]*[A-Za-z_]\w*)*)[ \t]*$`)
	pyWordRe       = regexp.MustCompile(`[A-Za-z_]\w*`)
	// A name, optionally as an attribute (".name").
	pyIdentRe = regexp.MustCompile(`(\.[ \t]*)?\b[A-Za-z_]\w*`)
	// Bindings: assignment, augmented assignment, walrus, keyword argument;
	// for-loop targets; with/except "as" targets.
	pyAssignRe = regexp.MustCompile(`\b([A-Za-z_]\w*)[ \t]*(?:[-+*/%&|^:]|//|\*\*|<<|>>)?=[^=]`)
	pyForRe    = regexp.MustCompile(`\bfor\b([^:]*?)\bin\b`)
	pyAsRe     = regexp.MustCompile(`\bas[ \t]+([A-Za-z_]\w*)`)
	// open() as the checker accepts it; literals are "N" placeholders.
	pyOpenRe = regexp.MustCompile(`^open\([ \t]*(?:"(\d+)"|([A-Za-z_]\w*))[ \t]*(?:,[ \t]*"(\d+)"[ \t]*)?(?:,[ \t]*encoding[ \t]*=[ \t]*"\d+"[ \t]*)?\)`)
	// Protected state a pure edit never touches, even inside the worktree:
	// agent and guard state, git internals and hooks, shell profiles, and files
	// tools auto-load or run from a repo. Case-insensitive: APFS is.
	pyPathBadRe = regexp.MustCompile(`(?i)^(/|~|\.\.)|/\.\.(/|$)|\.staypoint|\.claude|\.gemini|\.ssh|LaunchAgents|\.local/bin|\.git/|\.githooks|\.husky|\.agents|\.cursor|\.vscode|(^|/)\.mcp\.json$|(^|/)\.envrc$|\.zshrc|\.zprofile|\.zshenv|\.bashrc|\.bash_profile|\.profile`)
)

func wordSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

var (
	pyKeywordsOK = wordSet(`if elif else for in not and or is None True False assert while break continue pass with as try except finally raise`)
	pyBuiltinsOK = wordSet(`print len range enumerate zip sorted reversed min max sum any all str int float bool list dict set tuple isinstance repr abs round ord chr
		Exception ValueError KeyError IndexError SystemExit AssertionError FileNotFoundError RuntimeError`)
	pyAttrsOK = wordSet(`read readlines write writelines close replace split rsplit splitlines join strip lstrip rstrip startswith endswith
		find rfind index rindex count lower upper partition rpartition append extend insert pop get items keys values update setdefault sort
		sub subn search match fullmatch findall finditer group groups start end span escape compile dumps loads dump load
		dedent indent unified_diff encode decode isdigit isspace M S I X MULTILINE DOTALL IGNORECASE VERBOSE`)
	// Every builtin and keyword: a script may not bind any of them.
	pyReserved = wordSet(`abs aiter all anext any ascii bin bool breakpoint bytearray bytes callable chr classmethod compile complex
		copyright credits delattr dict dir divmod enumerate eval exec exit filter float format frozenset getattr globals hasattr hash
		help hex id input int isinstance issubclass iter len license list locals map max memoryview min next object oct open ord pow
		print property quit range repr reversed round set setattr slice sorted staticmethod str sum super tuple type vars zip
		False None True and as assert async await break class continue def del elif else except finally for from global if import
		in is lambda nonlocal not or pass raise return try while with yield match case type`)
	pyModesOK = wordSet(`r w a x rb wb ab xb rt wt at r+ w+ a+ r+b rb+ w+b wb+ a+b ab+`)
)

// pythonPureEdit applies the rules above to a Python script body.
func pythonPureEdit(body, cwd string) bool {
	code, lits, ok := splitPython(body)
	if !ok {
		return false
	}
	// Python NFKC-normalises identifiers, so fullwidth ｅｘｅｃ is exec: the
	// ASCII word checks below only hold for ASCII code.
	for _, r := range code {
		if r > 127 {
			return false
		}
	}
	if strings.Contains(code, "__") || strings.ContainsAny(code, "\\;@`$") {
		return false
	}
	known := map[string]bool{}
	for _, line := range pyImportLineRe.FindAllString(code, -1) {
		if !pyImportOKRe.MatchString(line) {
			return false
		}
		words := pyWordRe.FindAllString(line, -1)
		isFrom := words[0] == "from"
		for i, w := range words[1:] {
			if w == "import" {
				continue
			}
			if isFrom && i == 0 {
				continue // the module itself is not bound
			}
			if isFrom && !pyAttrsOK[w] {
				return false
			}
			known[w] = true
		}
	}
	code = pyImportLineRe.ReplaceAllString(code, "")

	bound := map[string]int{}
	for _, m := range pyAssignRe.FindAllStringSubmatch(code, -1) {
		bound[m[1]]++
	}
	for _, m := range pyForRe.FindAllStringSubmatch(code, -1) {
		for _, w := range pyWordRe.FindAllString(m[1], -1) {
			bound[w]++
		}
	}
	for _, m := range pyAsRe.FindAllStringSubmatch(code, -1) {
		bound[m[1]]++
	}
	for name := range bound {
		if pyReserved[name] || known[name] {
			return false
		}
	}

	for _, loc := range pyIdentRe.FindAllStringSubmatchIndex(code, -1) {
		name := strings.TrimLeft(code[loc[0]:loc[1]], ". \t")
		if loc[2] >= 0 { // attribute
			if !pyAttrsOK[name] {
				return false
			}
			continue
		}
		if name == "open" {
			if !pyOpenOK(code[loc[1]-len(name):], code, lits, bound, cwd) {
				return false
			}
			continue
		}
		if !pyKeywordsOK[name] && !pyBuiltinsOK[name] && !known[name] && bound[name] == 0 {
			return false
		}
	}
	return true
}

// pyOpenOK checks one open( call at the start of rest.
func pyOpenOK(rest, code string, lits []string, bound map[string]int, cwd string) bool {
	m := pyOpenRe.FindStringSubmatch(rest)
	if m == nil {
		return false
	}
	lit := func(idx string) (string, bool) {
		n, err := strconv.Atoi(idx)
		if err != nil || n < 0 || n >= len(lits) {
			return "", false
		}
		return lits[n], true
	}
	pathIdx := m[1]
	if pathIdx == "" {
		name := m[2]
		if bound[name] != 1 {
			return false
		}
		def := regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(name) + `[ \t]*=[ \t]*"(\d+)"[ \t]*$`).FindStringSubmatch(code)
		if def == nil {
			return false
		}
		pathIdx = def[1]
	}
	p, ok := lit(pathIdx)
	if !ok || !safeRelPath(p, cwd) {
		return false
	}
	if m[3] != "" {
		mode, ok := lit(m[3])
		if !ok || !pyModesOK[mode] {
			return false
		}
	}
	return true
}

// safeRelPath reports whether p is a whitespace-free relative path that stays
// under cwd and names no protected state. With cwd set, it also refuses a
// path that reaches a symlink, a non-directory parent, or an existing file
// that is not a regular singly-linked file.
func safeRelPath(p, cwd string) bool {
	if p == "" || strings.HasPrefix(p, "~") || filepath.IsAbs(p) || pyPathBadRe.MatchString(p) {
		return false
	}
	for _, r := range p {
		if r <= ' ' || r == '\\' || r > 126 {
			return false
		}
	}
	clean := filepath.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || pyPathBadRe.MatchString(strings.ToLower(clean)) { // APFS folds case
		return false
	}
	if cwd == "" {
		return true
	}
	parts := strings.Split(clean, "/")
	cur := cwd
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return true // open() cannot create the missing directories
		}
		if err != nil || fi.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if i < len(parts)-1 {
			if !fi.IsDir() {
				return false
			}
			continue
		}
		return fi.Mode().IsRegular() && singleLink(fi)
	}
	return false
}

// splitPython returns body's code with each string literal replaced by a
// "N" placeholder (N indexes lits) and comments removed. ok is false when the
// body cannot be split (unterminated literal) or an f-string holds an
// expression.
func splitPython(body string) (code string, lits []string, ok bool) {
	var out []rune
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
			out = out[:len(out)-(i-j)] // the prefix is part of the literal
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
			out = append(out, []rune(`"`+strconv.Itoa(len(lits))+`"`)...)
			lits = append(lits, lit)
			i = end + len([]rune(q))
			continue
		}
		out = append(out, c)
		i++
	}
	return string(out), lits, true
}

func isIdentRune(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// No heredoc body (Python or cat/tee data) is stripped before the Board-rule
// text check: with auto-allow off nothing is relaxed, so stripping could only
// hide text from the Board rules. AnalyzeBoardRules sees the full command.
