// Package testgate decides whether a change about to merge was tested
// (STA-734): which changed files are source, test or exempt, whether the repo
// has CI that runs tests, and which changed lines a CI coverage report shows
// no test touching. It only reads; the ship review flow acts on the result.
package testgate

import (
	"path"
	"sort"
	"strings"
)

// DefaultExemptGlobs are the docs, config, style and asset paths that never
// need a test. A project adds its own on top (ProjectDevConfig.TestExemptGlobs).
var DefaultExemptGlobs = []string{
	// docs
	"*.md", "*.mdx", "*.rst", "*.txt", "*.adoc", "docs/**", "doc/**",
	"LICENSE*", "CHANGELOG*", "AUTHORS*", "CODEOWNERS", ".github/**",
	// config
	"*.yml", "*.yaml", "*.toml", "*.ini", "*.cfg", "*.conf", "*.json",
	"*.lock", "go.mod", "go.sum", ".gitignore", ".gitattributes",
	".editorconfig", ".dockerignore", "Dockerfile", ".env.example", ".nvmrc",
	// style
	"*.css", "*.scss", "*.sass", "*.less",
	// assets
	"*.png", "*.jpg", "*.jpeg", "*.gif", "*.svg", "*.ico", "*.webp", "*.woff", "*.woff2",
}

// testDirs are path segments whose whole subtree counts as test code.
var testDirs = map[string]bool{"tests": true, "__tests__": true, "testdata": true}

// jsExts are the extensions a *.test.* / *.spec.* file may have.
var jsExts = map[string]bool{".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true}

// IsTestFile reports whether a repo-relative path is test code: *_test.go,
// test_*.py / *_test.py / conftest.py, *.test.* / *.spec.* for TS and JS, or
// anything under a tests/, __tests__/ or testdata/ directory.
func IsTestFile(p string) bool {
	p = path.Clean(strings.TrimPrefix(p, "./"))
	segs := strings.Split(p, "/")
	for _, s := range segs[:len(segs)-1] {
		if testDirs[s] {
			return true
		}
	}
	base := segs[len(segs)-1]
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	switch {
	case ext == ".go":
		return strings.HasSuffix(stem, "_test")
	case ext == ".py":
		return strings.HasPrefix(stem, "test_") || strings.HasSuffix(stem, "_test") || stem == "conftest"
	case jsExts[ext]:
		return strings.HasSuffix(stem, ".test") || strings.HasSuffix(stem, ".spec")
	}
	return false
}

// IsExempt reports whether p matches DefaultExemptGlobs or one of extra.
func IsExempt(p string, extra []string) bool {
	p = path.Clean(strings.TrimPrefix(p, "./"))
	for _, g := range DefaultExemptGlobs {
		if globMatch(g, p) {
			return true
		}
	}
	for _, g := range extra {
		if g = strings.TrimSpace(g); g != "" && globMatch(g, p) {
			return true
		}
	}
	return false
}

// globMatch matches a repo-relative path against one exempt glob:
//   - "dir/**" matches everything under dir;
//   - a glob without "/" matches the file's base name at any depth;
//   - anything else matches the whole path with path.Match.
func globMatch(g, p string) bool {
	g = strings.TrimPrefix(strings.TrimSpace(g), "./")
	if prefix, ok := strings.CutSuffix(g, "/**"); ok {
		return p == prefix || strings.HasPrefix(p, prefix+"/")
	}
	if !strings.Contains(g, "/") {
		ok, _ := path.Match(g, path.Base(p))
		return ok
	}
	ok, _ := path.Match(g, p)
	return ok
}

// ParseGlobList splits a newline- or comma-separated glob list, dropping blanks.
func ParseGlobList(s string) []string {
	var out []string
	for _, line := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ',' }) {
		if g := strings.TrimSpace(line); g != "" {
			out = append(out, g)
		}
	}
	return out
}

// Classification splits a change's files three ways. Each list is sorted.
type Classification struct {
	Sources []string `json:"sources"`
	Tests   []string `json:"tests"`
	Exempt  []string `json:"exempt"`
}

// Classify sorts changed files into source, test and exempt. A test file is
// always a test, even under an exempt path, so a test change is never hidden.
func Classify(files []string, extraExempt []string) Classification {
	var c Classification
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		switch {
		case IsTestFile(f):
			c.Tests = append(c.Tests, f)
		case IsExempt(f, extraExempt):
			c.Exempt = append(c.Exempt, f)
		default:
			c.Sources = append(c.Sources, f)
		}
	}
	sort.Strings(c.Sources)
	sort.Strings(c.Tests)
	sort.Strings(c.Exempt)
	return c
}

// UnmatchedSources returns the source files with no matching test change:
//   - Go: any changed *_test.go in the same directory (same package);
//   - everything else: a changed test named after the source's stem, i.e.
//     test_<stem>.*, <stem>_test.*, <stem>.test.*, <stem>.spec.*, or
//     __tests__/<stem>.*, in any directory.
func UnmatchedSources(sources, tests []string) []string {
	goTestDirs := map[string]bool{}
	testStems := map[string]bool{}
	for _, t := range tests {
		t = path.Clean(t)
		base := path.Base(t)
		if strings.HasSuffix(base, "_test.go") {
			goTestDirs[path.Dir(t)] = true
		}
		for _, s := range stemsTestedBy(t) {
			testStems[s] = true
		}
	}
	var out []string
	for _, s := range sources {
		c := path.Clean(s)
		if path.Ext(c) == ".go" {
			if !goTestDirs[path.Dir(c)] {
				out = append(out, s)
			}
			continue
		}
		base := path.Base(c)
		if !testStems[strings.TrimSuffix(base, path.Ext(base))] {
			out = append(out, s)
		}
	}
	return out
}

// stemsTestedBy returns the source stems a test file is named after.
func stemsTestedBy(t string) []string {
	base := path.Base(t)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	var out []string
	if s, ok := strings.CutPrefix(stem, "test_"); ok && s != "" {
		out = append(out, s)
	}
	if s, ok := strings.CutSuffix(stem, "_test"); ok && s != "" {
		out = append(out, s)
	}
	for _, suf := range []string{".test", ".spec"} {
		if s, ok := strings.CutSuffix(stem, suf); ok && s != "" {
			out = append(out, s)
		}
	}
	if path.Base(path.Dir(t)) == "__tests__" {
		out = append(out, stem)
	}
	return out
}
