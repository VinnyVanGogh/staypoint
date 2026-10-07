// Package geminiguard enforces the Board rule that Gemini never writes code,
// in any repo (STA-856, revised 2026-10-06). Gemini may edit documentation and
// other non-code files; anything else it changes in a turn is reverted to the
// daemon's pre-turn checkpoint and the run is failed.
//
// The package is a leaf (stdlib + gitexec + security) so the orchestrator can
// use it without importing router, which would create an import cycle.
package geminiguard

import (
	"path"
	"strings"
)

// DocExtensions are the non-code file extensions Gemini may write anywhere:
// prose, markdown slide decks and office documents.
var DocExtensions = []string{
	".md", ".mdx", ".txt", ".rst", ".adoc",
	".pdf", ".docx", ".pptx", ".key", ".odt", ".odp",
}

// DocDirs are top-level directories Gemini may write in. Under docs/ and doc/
// a file that is code by type (CodeExtensions, CodeBasenames) is still code;
// ".staypoint" holds StayPoint plan artifacts and is allowed whole.
var DocDirs = []string{"docs", "doc", ".staypoint"}

// CodeExtensions are always code, even inside docs/ or doc/: config, SQL,
// scripts, notebooks and source files.
var CodeExtensions = []string{
	".yaml", ".yml", ".json", ".toml", ".env", ".ini", ".cfg", ".conf",
	".sql", ".sh", ".bash", ".zsh", ".fish", ".ps1", ".bat", ".cmd",
	".ipynb",
	".go", ".py", ".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx", ".rb", ".rs",
	".java", ".kt", ".swift", ".c", ".h", ".cc", ".cpp", ".hpp", ".cs", ".php",
	".lua", ".pl", ".scala", ".dart", ".vue", ".svelte", ".css", ".scss",
	".html", ".htm", ".tf", ".hcl", ".gradle", ".mk",
}

// CodeBasenames are file names that are code whatever their extension:
// build files, Dockerfiles and dependency manifests that use .txt.
var CodeBasenames = []string{
	"dockerfile", "containerfile", "makefile", "gnumakefile", "justfile",
	"cmakelists.txt", "requirements.txt", "constraints.txt", "dev-requirements.txt",
}

// CodeDirs are top-level directories that are code whatever the file type:
// CI workflows and repo automation.
var CodeDirs = []string{".github", ".gitlab", ".circleci", ".buildkite"}

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

func isCodeFile(p string) bool {
	base := strings.ToLower(path.Base(p))
	for _, b := range CodeBasenames {
		if base == b || strings.HasPrefix(base, b+".") {
			return true
		}
	}
	if strings.HasPrefix(base, "requirements") && strings.HasSuffix(base, ".txt") {
		return true
	}
	if base == ".env" || strings.HasPrefix(base, ".env.") {
		return true
	}
	ext := strings.ToLower(path.Ext(p))
	for _, e := range CodeExtensions {
		if ext == e {
			return true
		}
	}
	return false
}

// IsDocPath reports whether a repo-relative path is a non-code file Gemini may
// write: a DocExtensions file anywhere (except code-named files such as
// requirements.txt), anything under .staypoint/, or a non-code file under a
// top-level docs/ or doc/. Config, SQL, scripts, Dockerfiles, notebooks,
// tests and source are code everywhere, and everything under CI directories
// (.github/ ...) is code. Absolute paths, paths escaping the repo and
// anything under .git are never docs.
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
	for _, d := range CodeDirs {
		if strings.HasPrefix(p, d+"/") {
			return false
		}
	}
	if strings.HasPrefix(p, ".staypoint/") {
		return true
	}
	if isCodeFile(p) {
		return false
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

// IsRepoPath reports whether p is a plain repo-relative path: not absolute,
// not escaping the repo, and not under .git. It is the whole allowlist for a
// Board-approved Gemini code run.
func IsRepoPath(p string) bool {
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
	return true
}

// IsGeminiProvider reports whether an adapter provider key names a Gemini CLI.
func IsGeminiProvider(p string) bool {
	p = strings.ToLower(strings.TrimSpace(p))
	return p == "gemini" || p == "agy" || strings.HasPrefix(p, "gemini-")
}
