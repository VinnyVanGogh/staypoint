package testgate

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// ── CI workflows ────────────────────────────────────────────────────────────

// testCommandRe matches a shell command that runs a test suite.
var testCommandRe = regexp.MustCompile(`(?:^|[\s;&|(` + "`" + `"'])(?:` + strings.Join([]string{
	`go\s+test\b`, `gotestsum\b`,
	`(?:python3?\s+-m\s+)?pytest\b`, `python3?\s+-m\s+unittest\b`, `tox\b`, `nox\b`,
	`(?:npm|pnpm|yarn|bun)\s+(?:run\s+)?test\b`, `npm\s+t\b`,
	`(?:npx\s+|pnpm\s+exec\s+|yarn\s+)?(?:vitest|jest|mocha|ava)\b`, `(?:npx\s+)?playwright\s+test\b`,
	`cargo\s+(?:test|nextest)\b`, `make\s+(?:test|check)\b`,
	`(?:mvn|\./mvnw)\s+(?:\S+\s+)*(?:test|verify)\b`, `(?:gradle|\./gradlew)\s+(?:\S+\s+)*(?:test|check)\b`,
	`(?:bundle\s+exec\s+)?(?:rspec|rake\s+test)\b`, `dotnet\s+test\b`, `deno\s+test\b`,
	`phpunit\b`, `mix\s+test\b`, `ctest\b`, `swift\s+test\b`,
}, "|") + `)`)

// WorkflowRunsTests reports whether a GitHub Actions workflow file contains
// a command that runs tests. Commented-out lines are ignored.
func WorkflowRunsTests(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") {
			continue
		}
		if testCommandRe.MatchString(t) {
			return true
		}
	}
	return false
}

// ── diff ────────────────────────────────────────────────────────────────────

var hunkRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// ParseChangedLines reads `git diff -U0` output and returns, per new-side
// file path, the line numbers the change added or modified. Deleted files and
// pure deletions contribute nothing.
func ParseChangedLines(diff string) map[string][]int {
	out := map[string][]int{}
	cur := ""
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ "):
			p := strings.TrimPrefix(line, "+++ ")
			if p == "/dev/null" {
				cur = ""
			} else {
				cur = strings.TrimPrefix(p, "b/")
			}
		case strings.HasPrefix(line, "@@ ") && cur != "":
			m := hunkRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			start, _ := strconv.Atoi(m[1])
			n := 1
			if m[2] != "" {
				n, _ = strconv.Atoi(m[2])
			}
			for i := 0; i < n; i++ {
				out[cur] = append(out[cur], start+i)
			}
		}
	}
	return out
}

// FormatLines renders line numbers as compact ranges: "10-12, 40".
func FormatLines(lines []int) string {
	if len(lines) == 0 {
		return ""
	}
	s := append([]int(nil), lines...)
	sort.Ints(s)
	var parts []string
	for i := 0; i < len(s); {
		j := i
		for j+1 < len(s) && s[j+1] <= s[j]+1 {
			j++
		}
		if s[j] == s[i] {
			parts = append(parts, strconv.Itoa(s[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", s[i], s[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

// ── coverage reports ────────────────────────────────────────────────────────

// Coverage report formats.
const (
	FormatGo        = "go"
	FormatCobertura = "cobertura"
	FormatLCOV      = "lcov"
)

// FuncCoverage is one function's line span and its hit count (0 = no test
// ran it).
type FuncCoverage struct {
	Name  string `json:"name"`
	Start int    `json:"start"`
	End   int    `json:"end"`
	Hits  int    `json:"hits"`
}

// FileCoverage is one file of a coverage report. Lines holds only the
// instrumented lines; a line absent from it is not code (blank, comment).
type FileCoverage struct {
	Lines map[int]int
	// Funcs comes from the report (LCOV, Cobertura). Go profiles carry no
	// function data; FindUncovered derives it from the source.
	Funcs []FuncCoverage
}

// Coverage is a parsed report, keyed by the paths the report uses.
type Coverage struct {
	Format string
	Files  map[string]*FileCoverage
	// from records which of the merged reports listed each path; nil for a
	// single report. See lookup.
	from map[string]map[int]bool
}

// reportsOf returns the indexes of the merged reports that list p.
func (c *Coverage) reportsOf(p string) map[int]bool {
	if r := c.from[p]; len(r) > 0 {
		return r
	}
	return map[int]bool{0: true}
}

// ErrNotCoverage is returned for a file that is not a known report format.
var ErrNotCoverage = errors.New("not a Go coverprofile, Cobertura XML or LCOV report")

// ParseCoverage parses a Go coverprofile, Cobertura coverage.xml or LCOV
// lcov.info, detected from the content.
func ParseCoverage(name string, data []byte) (*Coverage, error) {
	trim := bytes.TrimSpace(data)
	switch {
	case bytes.HasPrefix(trim, []byte("mode:")):
		return parseGoProfile(trim)
	case bytes.Contains(trim, []byte("<coverage")):
		return parseCobertura(trim)
	case bytes.Contains(trim, []byte("SF:")) && bytes.Contains(trim, []byte("end_of_record")):
		return parseLCOV(trim)
	}
	return nil, fmt.Errorf("%s: %w", name, ErrNotCoverage)
}

func (c *Coverage) file(p string) *FileCoverage {
	fc := c.Files[p]
	if fc == nil {
		fc = &FileCoverage{Lines: map[int]int{}}
		c.Files[p] = fc
	}
	return fc
}

func setMax(m map[int]int, line, hits int) {
	if old, ok := m[line]; !ok || hits > old {
		m[line] = hits
	}
}

// parseGoProfile reads `go test -coverprofile` output:
// "import/path/file.go:startLine.col,endLine.col numStmts count".
func parseGoProfile(data []byte) (*Coverage, error) {
	cov := &Coverage{Format: FormatGo, Files: map[string]*FileCoverage{}}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		colon := strings.LastIndex(line, ":")
		if colon < 0 {
			continue
		}
		file, rest := line[:colon], line[colon+1:]
		f := strings.Fields(rest)
		if len(f) != 3 {
			continue
		}
		span := strings.SplitN(f[0], ",", 2)
		if len(span) != 2 {
			continue
		}
		startLine, err1 := strconv.Atoi(strings.SplitN(span[0], ".", 2)[0])
		endLine, err2 := strconv.Atoi(strings.SplitN(span[1], ".", 2)[0])
		count, err3 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil || err3 != nil || endLine < startLine {
			continue
		}
		fc := cov.file(file)
		for l := startLine; l <= endLine; l++ {
			setMax(fc.Lines, l, count)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return cov, nil
}

type xmlLine struct {
	Number int `xml:"number,attr"`
	Hits   int `xml:"hits,attr"`
}

type xmlCobertura struct {
	Classes []struct {
		Filename string `xml:"filename,attr"`
		Methods  []struct {
			Name  string    `xml:"name,attr"`
			Lines []xmlLine `xml:"lines>line"`
		} `xml:"methods>method"`
		Lines []xmlLine `xml:"lines>line"`
	} `xml:"packages>package>classes>class"`
}

func parseCobertura(data []byte) (*Coverage, error) {
	var doc xmlCobertura
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse Cobertura XML: %w", err)
	}
	// Clover (PHPUnit --coverage-clover) also has a <coverage> root but no
	// packages>package>classes; reading it as an empty Cobertura report
	// would claim every changed line is covered.
	if len(doc.Classes) == 0 {
		return nil, fmt.Errorf("Cobertura XML with no classes: %w", ErrNotCoverage)
	}
	cov := &Coverage{Format: FormatCobertura, Files: map[string]*FileCoverage{}}
	for _, cl := range doc.Classes {
		if cl.Filename == "" {
			continue
		}
		fc := cov.file(path.Clean(cl.Filename))
		for _, l := range cl.Lines {
			setMax(fc.Lines, l.Number, l.Hits)
		}
		for _, m := range cl.Methods {
			if len(m.Lines) == 0 {
				continue
			}
			fn := FuncCoverage{Name: m.Name, Start: m.Lines[0].Number, End: m.Lines[0].Number}
			for _, l := range m.Lines {
				fn.Start = min(fn.Start, l.Number)
				fn.End = max(fn.End, l.Number)
				fn.Hits = max(fn.Hits, l.Hits)
				setMax(fc.Lines, l.Number, l.Hits)
			}
			fc.Funcs = append(fc.Funcs, fn)
		}
	}
	return cov, nil
}

// parseLCOV reads SF / FN / FNDA / DA records. FN carries only a start line
// (or start,end in LCOV 2), so a function without an end runs to the line
// before the next function, or to the file's last instrumented line.
func parseLCOV(data []byte) (*Coverage, error) {
	cov := &Coverage{Format: FormatLCOV, Files: map[string]*FileCoverage{}}
	var fc *FileCoverage
	var fns []FuncCoverage
	hasEnd := map[string]bool{}
	hits := map[string]int{}
	flush := func() {
		if fc == nil {
			return
		}
		sort.SliceStable(fns, func(i, j int) bool { return fns[i].Start < fns[j].Start })
		last := 0
		for l := range fc.Lines {
			last = max(last, l)
		}
		for i := range fns {
			fns[i].Hits = hits[fns[i].Name]
			if hasEnd[fns[i].Name] {
				continue
			}
			if i+1 < len(fns) {
				fns[i].End = fns[i+1].Start - 1
			} else {
				fns[i].End = max(last, fns[i].Start)
			}
		}
		fc.Funcs = append(fc.Funcs, fns...)
		fc, fns, hasEnd, hits = nil, nil, map[string]bool{}, map[string]int{}
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		key, val, _ := strings.Cut(line, ":")
		switch key {
		case "SF":
			flush()
			fc = cov.file(path.Clean(val))
		case "FN":
			if fc == nil {
				continue
			}
			parts := strings.Split(val, ",")
			if len(parts) < 2 {
				continue
			}
			start, err := strconv.Atoi(parts[0])
			if err != nil {
				continue
			}
			fn := FuncCoverage{Name: parts[len(parts)-1], Start: start, End: start}
			if len(parts) >= 3 {
				if end, err := strconv.Atoi(parts[1]); err == nil {
					fn.End = end
					hasEnd[fn.Name] = true
				}
			}
			fns = append(fns, fn)
		case "FNDA":
			if n, name, ok := strings.Cut(val, ","); ok {
				h, _ := strconv.Atoi(n)
				hits[name] = max(hits[name], h)
			}
		case "DA":
			if fc == nil {
				continue
			}
			parts := strings.Split(val, ",")
			if len(parts) < 2 {
				continue
			}
			l, err1 := strconv.Atoi(parts[0])
			h, err2 := strconv.Atoi(parts[1])
			if err1 == nil && err2 == nil {
				setMax(fc.Lines, l, h)
			}
		case "end_of_record":
			flush()
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return cov, nil
}

// MergeCoverage combines reports (several CI jobs, several languages). A
// line hit in any report counts as hit.
func MergeCoverage(covs ...*Coverage) *Coverage {
	out := &Coverage{Files: map[string]*FileCoverage{}, from: map[string]map[int]bool{}}
	var formats []string
	for i, c := range covs {
		if c == nil {
			continue
		}
		if !strings.Contains(","+strings.Join(formats, ",")+",", ","+c.Format+",") {
			formats = append(formats, c.Format)
		}
		for p, fc := range c.Files {
			if out.from[p] == nil {
				out.from[p] = map[int]bool{}
			}
			out.from[p][i] = true
			out.file(p).add(fc)
		}
	}
	out.Format = strings.Join(formats, ",")
	return out
}

// Lookup finds the report entry for a repo-relative path. Reports name files
// by import path (Go), absolute runner path (LCOV) or source-relative path
// (Cobertura), so the entry whose path shares the longest suffix wins. It
// returns nil when the report has no entry or the match is ambiguous; see
// lookup.
func (c *Coverage) Lookup(rel string) *FileCoverage {
	fc, _ := c.lookup(rel)
	return fc
}

// lookup is Lookup that also reports an ambiguous match. A tie on the
// longest suffix is only ever between entries ending in "/"+rel. Entries of
// one report are always different files (util.go in two packages, or
// src/index.ts and packages/web/src/index.ts), so such a tie is ambiguous.
// When each tied entry comes from a different report, it is the same file
// uploaded by several CI jobs under different runner paths
// (/home/runner/... and /Users/runner/...), and the entries are merged.
func (c *Coverage) lookup(rel string) (fc *FileCoverage, ambiguous bool) {
	if c == nil {
		return nil, false
	}
	rel = path.Clean(strings.TrimPrefix(rel, "./"))
	if fc, ok := c.Files[rel]; ok {
		return fc, false
	}
	var best []string
	bestLen := 0
	for p := range c.Files {
		n := 0
		switch {
		case strings.HasSuffix(p, "/"+rel):
			n = len(rel)
		case strings.HasSuffix(rel, "/"+p):
			n = len(p)
		default:
			continue
		}
		switch {
		case n > bestLen:
			best, bestLen = []string{p}, n
		case n == bestLen:
			best = append(best, p)
		}
	}
	switch len(best) {
	case 0:
		return nil, false
	case 1:
		return c.Files[best[0]], false
	}
	seen := map[int]bool{}
	for _, p := range best {
		for r := range c.reportsOf(p) {
			if seen[r] {
				return nil, true
			}
			seen[r] = true
		}
	}
	sort.Strings(best)
	merged := &FileCoverage{Lines: map[int]int{}}
	for _, p := range best {
		merged.add(c.Files[p])
	}
	return merged, false
}

// add folds src into fc: a line or function hit in either counts as hit.
// Functions are matched by name and start line, so one listed by two
// reports isn't reported as uncovered by the report that missed it, while
// two same-named methods in one file stay apart.
func (fc *FileCoverage) add(src *FileCoverage) {
	for l, h := range src.Lines {
		setMax(fc.Lines, l, h)
	}
	for _, f := range src.Funcs {
		i := slices.IndexFunc(fc.Funcs, func(g FuncCoverage) bool { return g.Name == f.Name && g.Start == f.Start })
		if i < 0 {
			fc.Funcs = append(fc.Funcs, f)
			continue
		}
		m := &fc.Funcs[i]
		m.End, m.Hits = max(m.End, f.End), max(m.Hits, f.Hits)
	}
}

// exts returns the file extensions the report covers.
func (c *Coverage) exts() map[string]bool {
	out := map[string]bool{}
	for p := range c.Files {
		out[path.Ext(p)] = true
	}
	return out
}

// GoFuncRanges returns the functions declared in a Go source file with
// their line spans. Methods are named "(*T).M" / "T.M".
func GoFuncRanges(src []byte) []FuncCoverage {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	var out []FuncCoverage
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		name := fd.Name.Name
		if fd.Recv != nil && len(fd.Recv.List) > 0 {
			name = recvName(fd.Recv.List[0].Type) + "." + name
		}
		out = append(out, FuncCoverage{Name: name, Start: fset.Position(fd.Pos()).Line, End: fset.Position(fd.End()).Line})
	}
	return out
}

func recvName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return "(*" + recvName(t.X) + ")"
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return recvName(t.X)
	case *ast.IndexListExpr:
		return recvName(t.X)
	}
	return "?"
}

// UncoveredFile is a changed source file whose changed lines no test runs,
// per the coverage report.
type UncoveredFile struct {
	Path string `json:"path"`
	// Lines are changed, instrumented lines with zero hits.
	Lines []int `json:"lines,omitempty"`
	// Functions contain a changed line and have zero hits overall.
	Functions []string `json:"functions,omitempty"`
	// NotInReport is set for a file in a language the report covers that the
	// report does not list at all (e.g. a Go package with no tests).
	NotInReport bool `json:"not_in_report,omitempty"`
	// Ambiguous is set when several report entries could be this file, so
	// its coverage is unknown. It never blocks a merge.
	Ambiguous bool `json:"ambiguous,omitempty"`
}

// FindUncovered checks each changed file against the report. src returns a
// file's content at the head commit; it is only needed for Go reports, which
// carry no function spans. Files in languages the report doesn't cover are
// skipped: the report says nothing about them either way.
func FindUncovered(changed map[string][]int, cov *Coverage, src func(path string) []byte) []UncoveredFile {
	if cov == nil {
		return nil
	}
	exts := cov.exts()
	paths := make([]string, 0, len(changed))
	for p := range changed {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var out []UncoveredFile
	for _, p := range paths {
		lines := changed[p]
		fc, ambiguous := cov.lookup(p)
		if ambiguous {
			out = append(out, UncoveredFile{Path: p, Ambiguous: true})
			continue
		}
		if fc == nil {
			if exts[path.Ext(p)] {
				out = append(out, UncoveredFile{Path: p, NotInReport: true})
			}
			continue
		}
		u := UncoveredFile{Path: p}
		for _, l := range lines {
			if h, ok := fc.Lines[l]; ok && h == 0 {
				u.Lines = append(u.Lines, l)
			}
		}
		sort.Ints(u.Lines)
		funcs := fc.Funcs
		if len(funcs) == 0 && path.Ext(p) == ".go" && src != nil {
			if b := src(p); b != nil {
				funcs = GoFuncRanges(b)
				for i := range funcs {
					funcs[i].Hits = 0
					for l := funcs[i].Start; l <= funcs[i].End; l++ {
						funcs[i].Hits = max(funcs[i].Hits, fc.Lines[l])
					}
				}
			}
		}
		seen := map[string]bool{}
		for _, fn := range funcs {
			if fn.Hits != 0 || seen[fn.Name] {
				continue
			}
			for _, l := range lines {
				if l >= fn.Start && l <= fn.End {
					u.Functions = append(u.Functions, fn.Name)
					seen[fn.Name] = true
					break
				}
			}
		}
		if len(u.Lines) > 0 || len(u.Functions) > 0 {
			out = append(out, u)
		}
	}
	return out
}
