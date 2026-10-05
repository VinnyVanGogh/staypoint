// Package migration provides helpers for detecting, reading, and risk-checking
// DB migration files within a task's changed file set.
package migration

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
)

// DefaultGlobs are the migration file patterns used when no project config overrides them.
var DefaultGlobs = []string{
	"supabase/migrations/*.sql",
	"prisma/migrations/**/migration.sql",
	"migrations/*.sql",
	"db/migrate/*",
}

// File holds a detected migration file with its content and risk analysis.
type File struct {
	Path                string   `json:"path"`
	SQL                 string   `json:"sql"`
	RiskStatements      []string `json:"risk_statements"`
	IdempotentReCreates []string `json:"idempotent_recreates,omitempty"`
	AdditiveOnly        bool     `json:"additive_only"`
	// ReadError is set when the file content could not be read; SQL is then
	// empty and the file must not be treated as safe.
	ReadError string `json:"read_error,omitempty"`
	// AppliedBy/AppliedAt come from the latest "migration_applied" activity
	// for this path. Verified stays false until a schema check exists (STA-564).
	AppliedBy string `json:"applied_by,omitempty"`
	AppliedAt string `json:"applied_at,omitempty"`
	Verified  bool   `json:"verified"`
}

// destructivePatterns is compiled once at startup.
var destructivePatterns = []*regexp.Regexp{
	// DROP TABLE / VIEW / INDEX / SEQUENCE / FUNCTION / etc.
	regexp.MustCompile(`(?i)\bDROP\s+\w`),
	// TRUNCATE
	regexp.MustCompile(`(?i)\bTRUNCATE\b`),
	// DELETE without WHERE
	regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+\S+\s*(?:;|$)`),
	// UPDATE without WHERE (bare statement ending in ; with no WHERE clause on the same line)
	regexp.MustCompile(`(?i)\bUPDATE\s+\S+\s+SET\b[^;]*;`),
	// ALTER TABLE … DROP COLUMN
	regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bDROP\s+COLUMN\b`),
	// ALTER TABLE … ALTER COLUMN … TYPE
	regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bALTER\s+COLUMN\b.*\bTYPE\b`),
	// ALTER TABLE … SET NOT NULL (risky on existing populated tables)
	regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bSET\s+NOT\s+NULL\b`),
	// DISABLE ROW LEVEL SECURITY
	regexp.MustCompile(`(?i)\bDISABLE\s+ROW\s+LEVEL\s+SECURITY\b`),
	// DROP POLICY
	regexp.MustCompile(`(?i)\bDROP\s+POLICY\b`),
}

// stripComments removes -- line comments and /* */ block comments from sql.
// Limitation: it does not skip inside string literals ('…', "…", $tag$…$tag$),
// so a literal that contains -- or /* is partially stripped. This is a
// parser-differential that may produce false negatives (a destructive statement
// missed inside a literal). That is acceptable here because this function is
// used only for informational warnings; it never executes SQL and a missed
// warning is not exploitable.
func stripComments(sql string) string {
	var b strings.Builder
	i := 0
	for i < len(sql) {
		// Line comment
		if i+1 < len(sql) && sql[i] == '-' && sql[i+1] == '-' {
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			continue
		}
		// Block comment
		if i+1 < len(sql) && sql[i] == '/' && sql[i+1] == '*' {
			i += 2
			for i+1 < len(sql) && !(sql[i] == '*' && sql[i+1] == '/') {
				i++
			}
			i += 2 // skip */
			continue
		}
		b.WriteByte(sql[i])
		i++
	}
	return b.String()
}

// sqlIdentRE matches either a double-quoted SQL identifier (allowing spaces and
// special characters inside the quotes) or an unquoted word identifier.
const sqlIdentRE = `(?:"[^"]*"|\w+)`

// idempotentDropRe matches DROP/CREATE POLICY/TRIGGER and captures the name (and
// for policies the table ref, since policy names are scoped to a table).
var (
	reDropPolicyIFE  = regexp.MustCompile(`(?i)\bDROP\s+POLICY\s+IF\s+EXISTS\s+(` + sqlIdentRE + `)\s+ON\s+((?:\w+\.)?\w+)`)
	reDropTriggerIFE = regexp.MustCompile(`(?i)\bDROP\s+TRIGGER\s+IF\s+EXISTS\s+(` + sqlIdentRE + `)`)
	reCreatePolicyN  = regexp.MustCompile(`(?i)\bCREATE\s+POLICY\s+(` + sqlIdentRE + `)\s+ON\s+((?:\w+\.)?\w+)`)
	reCreateTriggerN = regexp.MustCompile(`(?i)\bCREATE\s+(?:OR\s+REPLACE\s+)?TRIGGER\s+(` + sqlIdentRE + `)`)
)

// unquoteIdent strips surrounding double-quotes from a SQL identifier.
// Unquoted identifiers are returned unchanged.
func unquoteIdent(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// policyKey returns the lookup key for a policy keyed on unquoted-lowercased name
// and table so that same-named policies on different tables are not conflated.
func policyKey(name, table string) string {
	return "policy:" + strings.ToLower(unquoteIdent(name)) + ":" + strings.ToLower(table)
}

// idempotentDropKeys returns a set of policy/trigger keys
// where the DROP … IF EXISTS is followed by a matching CREATE in the same file.
func idempotentDropKeys(stripped string) map[string]bool {
	created := map[string]bool{}
	for _, m := range reCreatePolicyN.FindAllStringSubmatch(stripped, -1) {
		created[policyKey(m[1], m[2])] = true
	}
	for _, m := range reCreateTriggerN.FindAllStringSubmatch(stripped, -1) {
		created["trigger:"+strings.ToLower(unquoteIdent(m[1]))] = true
	}
	keys := map[string]bool{}
	for _, m := range reDropPolicyIFE.FindAllStringSubmatch(stripped, -1) {
		k := policyKey(m[1], m[2])
		if created[k] {
			keys[k] = true
		}
	}
	for _, m := range reDropTriggerIFE.FindAllStringSubmatch(stripped, -1) {
		k := "trigger:" + strings.ToLower(unquoteIdent(m[1]))
		if created[k] {
			keys[k] = true
		}
	}
	return keys
}

// CheckRisk scans sql for destructive or locking patterns.
// Returns destructive snippets, idempotent re-create snippets, and whether any
// genuinely destructive risks were found. DROP POLICY/TRIGGER IF EXISTS that
// are immediately re-created with the same name in the same file are classified
// as idempotent re-creates, not destructive.
func CheckRisk(sql string) (risks []string, idempotentReCreates []string, hasRisk bool) {
	stripped := stripComments(sql)
	idKeys := idempotentDropKeys(stripped)
	lines := strings.Split(stripped, "\n")
	seen := map[string]bool{}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		for _, pat := range destructivePatterns {
			if pat.FindStringIndex(trimmed) != nil {
				snippet := trimmed
				if len(snippet) > 80 {
					snippet = snippet[:80] + "…"
				}
				if seen[snippet] {
					continue
				}
				seen[snippet] = true
				// Check if this DROP POLICY/TRIGGER IF EXISTS is an idempotent re-create.
				if m := reDropPolicyIFE.FindStringSubmatch(trimmed); m != nil {
					if idKeys[policyKey(m[1], m[2])] {
						idempotentReCreates = append(idempotentReCreates, snippet)
						continue
					}
				}
				if m := reDropTriggerIFE.FindStringSubmatch(trimmed); m != nil {
					if idKeys["trigger:"+strings.ToLower(unquoteIdent(m[1]))] {
						idempotentReCreates = append(idempotentReCreates, snippet)
						continue
					}
				}
				risks = append(risks, snippet)
			}
		}
	}
	return risks, idempotentReCreates, len(risks) > 0
}

// matchGlob reports whether path matches any of the globs.
// Each glob is matched against the path using filepath.Match; the full path
// and the path with each leading directory component stripped are tried so that
// globs like "supabase/migrations/*.sql" match relative paths from the repo root.
func matchGlob(path string, globs []string) bool {
	for _, g := range globs {
		if ok, _ := filepath.Match(g, path); ok {
			return true
		}
		// Try matching just the file name for simple glob patterns like "*.sql"
		if ok, _ := filepath.Match(g, filepath.Base(path)); ok {
			return true
		}
		// Try prefix-stripped paths: for "supabase/migrations/*.sql" vs "supabase/migrations/foo.sql"
		// filepath.Match already handles this when g has no **, but Glob-style ** is not supported natively.
		// We handle ** by matching against progressively shorter path suffixes.
		if strings.Contains(g, "**") {
			if matchDoubleGlob(path, g) {
				return true
			}
		}
	}
	return false
}

// matchDoubleGlob handles simple ** patterns by splitting on ** and testing prefix/suffix.
func matchDoubleGlob(path, glob string) bool {
	parts := strings.SplitN(glob, "**", 2)
	if len(parts) != 2 {
		return false
	}
	prefix, suffix := parts[0], parts[1]
	if prefix != "" && !strings.HasPrefix(path, prefix) {
		return false
	}
	remainder := path
	if prefix != "" {
		remainder = path[len(prefix):]
	}
	if suffix == "" {
		return true
	}
	// suffix starts with "/" – match it at any directory depth
	suf := strings.TrimPrefix(suffix, "/")
	ok, _ := filepath.Match(suf, filepath.Base(remainder))
	if ok {
		return true
	}
	// walk subdirs
	return strings.HasSuffix(remainder, strings.TrimPrefix(suffix, "/"))
}

// Detect filters filePaths to those matching any of globs.
func Detect(filePaths, globs []string) []string {
	var matched []string
	for _, p := range filePaths {
		if matchGlob(p, globs) {
			matched = append(matched, p)
		}
	}
	SortByTimestamp(matched)
	return matched
}

// SortByTimestamp sorts migration paths lexicographically (timestamp prefixes sort naturally).
func SortByTimestamp(paths []string) {
	sort.Strings(paths)
}

// ReadContent reads the current content of a migration file from workDir.
func ReadContent(workDir, relPath string) (string, error) {
	full := filepath.Join(workDir, relPath)
	b, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ReadContentAtRef reads relPath as committed on ref (e.g. the task branch)
// without needing a checkout, so it works after the task worktree is pruned.
func ReadContentAtRef(ctx context.Context, repoPath, ref, relPath string) (string, error) {
	out, err := gitexec.Command(ctx, "-C", repoPath, "show", ref+":"+filepath.ToSlash(relPath)).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
