// Package geminiguard enforces the Board rule that Gemini never writes code in
// a work repo (STA-856). Gemini may edit documentation there; anything else it
// changes in a turn is reverted to the daemon's pre-turn checkpoint and the run
// is failed.
//
// The package is a leaf (stdlib + gitexec + security) so the orchestrator can
// use it without importing router, which would create an import cycle.
package geminiguard

import (
	"path"
	"strings"
)

// DocExtensions are file extensions Gemini may write in a work repo.
var DocExtensions = []string{".md", ".mdx", ".txt", ".rst", ".adoc"}

// DocDirs are top-level directories whose whole contents Gemini may write in a
// work repo. ".staypoint" holds plan artifacts.
var DocDirs = []string{"docs", "doc", ".staypoint"}

// Allowlist is the human-readable allowlist, for the gates page (STA-816).
func Allowlist() []string {
	out := make([]string, 0, len(DocExtensions)+len(DocDirs))
	for _, e := range DocExtensions {
		out = append(out, "*"+e)
	}
	for _, d := range DocDirs {
		out = append(out, d+"/**")
	}
	return out
}

// IsDocPath reports whether a repo-relative path is documentation Gemini may
// write in a work repo: a *.md/*.mdx/*.txt/*.rst/*.adoc file anywhere, or
// anything under a top-level docs/, doc/ or .staypoint/ directory. Absolute
// paths, paths escaping the repo and anything under .git are never docs.
func IsDocPath(p string) bool {
	p = strings.ReplaceAll(p, "\\", "/")
	if p == "" || strings.HasPrefix(p, "/") {
		return false
	}
	p = path.Clean(p)
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".git" {
			return false
		}
	}
	for _, d := range DocDirs {
		if strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	ext := strings.ToLower(path.Ext(p))
	for _, e := range DocExtensions {
		if ext == e {
			return true
		}
	}
	return false
}

// IsGeminiProvider reports whether an adapter provider key names a Gemini CLI.
func IsGeminiProvider(p string) bool {
	p = strings.ToLower(strings.TrimSpace(p))
	return p == "gemini" || p == "agy" || strings.HasPrefix(p, "gemini-")
}
