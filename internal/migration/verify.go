// Package migration — verify.go
// ParseChecks parses a migration SQL file and returns a list of read-only
// schema-presence checks for each DDL/DML object it finds.
// These checks are designed to be run inside a READ ONLY transaction against
// the target Postgres database to confirm the migration was really applied.
package migration

import (
	"fmt"
	"regexp"
	"strings"
)

// safeIdentRe rejects any captured identifier that contains characters outside
// the safe SQL identifier set before it is interpolated into a query.
var safeIdentRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// quoteIdent double-quotes a Postgres identifier and escapes embedded quotes.
// Only identifiers that pass safeIdentRe should reach this function; callers
// must validate first so that a malformed capture falls back rather than
// producing an injection vector.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// safeQualified returns quoteIdent(schema)+"."+quoteIdent(table) after
// validating both parts. Returns ("","") if either part is unsafe.
func safeQualified(schema, table string) (string, string, bool) {
	if !safeIdentRe.MatchString(schema) || !safeIdentRe.MatchString(table) {
		return "", "", false
	}
	return schema, table, true
}

// CheckKind classifies how a Check is evaluated.
type CheckKind string

const (
	KindRegclass           CheckKind = "regclass"            // to_regclass('public.x') IS NOT NULL
	KindColumn             CheckKind = "column"              // information_schema.columns
	KindPolicy             CheckKind = "policy"              // pg_policies
	KindRLS                CheckKind = "rls"                 // pg_class.relrowsecurity
	KindTrigger            CheckKind = "trigger"             // pg_trigger
	KindFunction           CheckKind = "function"            // pg_proc
	KindIndex              CheckKind = "index"               // pg_indexes
	KindRows               CheckKind = "rows"                // row count or key presence
	KindUnchecked          CheckKind = "unchecked"           // not checked (never silently ✓)
)

// Check is one verifiable object extracted from a migration file.
type Check struct {
	Kind        CheckKind `json:"kind"`
	Description string    `json:"description"` // human label
	SQL         string    `json:"sql"`         // read-only SELECT that returns a single boolean column "ok"
}

// CheckResult is the outcome of running a Check.
type CheckResult struct {
	Check
	Passed  bool   `json:"passed"`
	Error   string `json:"error,omitempty"`
}

// regexes for DDL/DML patterns — all case-insensitive, compiled once.
var (
	reCreateTable = regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(\w+(?:\.\w+)?)`)

	reAddColumn = regexp.MustCompile(`(?is)ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(\w+(?:\.\w+)?)\s+ADD\s+(?:COLUMN\s+)?(?:IF\s+NOT\s+EXISTS\s+)?(\w+)\s`)

	// reCreatePolicy matches both unquoted (cv_select) and double-quoted
	// ("Public can view active corporate values") policy names.
	reCreatePolicy = regexp.MustCompile(`(?is)CREATE\s+POLICY\s+("(?:[^"]|"")*"|\w+)\s+ON\s+(\w+(?:\.\w+)?)`)

	reEnableRLS = regexp.MustCompile(`(?is)ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?(\w+(?:\.\w+)?)\s+ENABLE\s+ROW\s+LEVEL\s+SECURITY`)

	reCreateTrigger = regexp.MustCompile(`(?is)CREATE\s+(?:OR\s+REPLACE\s+)?TRIGGER\s+(\w+)\s+(?:\w+\s+){1,3}ON\s+(\w+(?:\.\w+)?)`)

	reCreateFunction = regexp.MustCompile(`(?is)CREATE\s+(?:OR\s+REPLACE\s+)?FUNCTION\s+(\w+(?:\.\w+)?)\s*\(`)

	reCreateIndex = regexp.MustCompile(`(?is)CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?(\w+)\s+ON\s+(\w+(?:\.\w+)?)`)

	reInsertValues = regexp.MustCompile(`(?is)INSERT\s+INTO\s+(\w+(?:\.\w+)?)\s*\(([^)]+)\)\s*VALUES\s*(.+?)(?:;|$)`)

	// reInsertSelectValues handles: INSERT INTO t (cols) SELECT … FROM (VALUES …) AS alias(cols)
	// Groups: 1=table, 2=VALUES block, 3=alias column list.
	reInsertSelectValues = regexp.MustCompile(`(?is)INSERT\s+INTO\s+(\w+(?:\.\w+)?)\s*\([^)]+\)\s*SELECT\s+.+?\bFROM\s*\(\s*VALUES\s*(.+?)\)\s*AS\s+\w+\s*\(([^)]+)\)`)
)

// qualifyTable returns a schema-qualified table name.
// If the name already contains a dot, return as-is; otherwise prepend "public.".
func qualifyTable(name string) string {
	if strings.Contains(name, ".") {
		return strings.ToLower(name)
	}
	return "public." + strings.ToLower(name)
}

// schemaTable splits schema and table from a qualified name.
func schemaTable(qualified string) (schema, table string) {
	parts := strings.SplitN(qualified, ".", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "public", parts[0]
}

// ParseChecks parses the migration SQL and returns verification checks.
// The input SQL should be the raw file content (comments are stripped internally).
func ParseChecks(sql string) []Check {
	stripped := stripComments(sql)
	var checks []Check
	seen := map[string]bool{}

	add := func(c Check) {
		if !seen[c.Description] {
			seen[c.Description] = true
			checks = append(checks, c)
		}
	}

	// CREATE TABLE
	for _, m := range reCreateTable.FindAllStringSubmatch(stripped, -1) {
		tbl := qualifyTable(m[1])
		add(Check{
			Kind:        KindRegclass,
			Description: fmt.Sprintf("table %s exists", tbl),
			SQL:         fmt.Sprintf("SELECT to_regclass('%s') IS NOT NULL AS ok", tbl),
		})
	}

	// ALTER TABLE … ADD COLUMN
	for _, m := range reAddColumn.FindAllStringSubmatch(stripped, -1) {
		tbl := qualifyTable(m[1])
		col := strings.ToLower(m[2])
		schema, table := schemaTable(tbl)
		add(Check{
			Kind:        KindColumn,
			Description: fmt.Sprintf("column %s.%s exists", tbl, col),
			SQL: fmt.Sprintf(
				"SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='%s' AND table_name='%s' AND column_name='%s') AS ok",
				schema, table, col,
			),
		})
	}

	// CREATE POLICY
	// pg_policies.policyname stores unquoted names lowercased and quoted names
	// with their original case, so we preserve case for quoted identifiers.
	for _, m := range reCreatePolicy.FindAllStringSubmatch(stripped, -1) {
		var policy string
		if strings.HasPrefix(m[1], `"`) {
			policy = unquoteIdent(m[1])
		} else {
			policy = strings.ToLower(m[1])
		}
		tbl := qualifyTable(m[2])
		schema, table := schemaTable(tbl)
		escapedPolicy := strings.ReplaceAll(policy, "'", "''")
		add(Check{
			Kind:        KindPolicy,
			Description: fmt.Sprintf("policy %s on %s exists", policy, tbl),
			SQL: fmt.Sprintf(
				"SELECT EXISTS(SELECT 1 FROM pg_policies WHERE schemaname='%s' AND tablename='%s' AND policyname='%s') AS ok",
				schema, table, escapedPolicy,
			),
		})
	}

	// ENABLE ROW LEVEL SECURITY
	for _, m := range reEnableRLS.FindAllStringSubmatch(stripped, -1) {
		tbl := qualifyTable(m[1])
		schema, table := schemaTable(tbl)
		add(Check{
			Kind:        KindRLS,
			Description: fmt.Sprintf("RLS enabled on %s", tbl),
			SQL: fmt.Sprintf(
				"SELECT relrowsecurity AS ok FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='%s' AND c.relname='%s'",
				schema, table,
			),
		})
	}

	// CREATE TRIGGER
	for _, m := range reCreateTrigger.FindAllStringSubmatch(stripped, -1) {
		trigger := strings.ToLower(m[1])
		tbl := qualifyTable(m[2])
		_, table := schemaTable(tbl)
		add(Check{
			Kind:        KindTrigger,
			Description: fmt.Sprintf("trigger %s on %s exists", trigger, tbl),
			SQL: fmt.Sprintf(
				"SELECT EXISTS(SELECT 1 FROM pg_trigger t JOIN pg_class c ON c.oid=t.tgrelid WHERE c.relname='%s' AND t.tgname='%s') AS ok",
				table, trigger,
			),
		})
	}

	// CREATE FUNCTION
	for _, m := range reCreateFunction.FindAllStringSubmatch(stripped, -1) {
		fn := strings.ToLower(m[1])
		schema := "public"
		fname := fn
		if idx := strings.Index(fn, "."); idx >= 0 {
			schema = fn[:idx]
			fname = fn[idx+1:]
		}
		add(Check{
			Kind:        KindFunction,
			Description: fmt.Sprintf("function %s.%s exists", schema, fname),
			SQL: fmt.Sprintf(
				"SELECT EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='%s' AND p.proname='%s') AS ok",
				schema, fname,
			),
		})
	}

	// CREATE INDEX
	for _, m := range reCreateIndex.FindAllStringSubmatch(stripped, -1) {
		idx := strings.ToLower(m[1])
		tbl := qualifyTable(m[2])
		schema, table := schemaTable(tbl)
		add(Check{
			Kind:        KindIndex,
			Description: fmt.Sprintf("index %s on %s exists", idx, tbl),
			SQL: fmt.Sprintf(
				"SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname='%s' AND tablename='%s' AND indexname='%s') AS ok",
				schema, table, idx,
			),
		})
	}

	// INSERT INTO … VALUES (seed data check — expected rows)
	for _, m := range reInsertValues.FindAllStringSubmatch(stripped, -1) {
		tbl := qualifyTable(m[1])
		colsRaw := m[2]
		valuesBlock := m[3]

		// Count VALUES tuples
		tuples := countValueTuples(valuesBlock)
		if tuples == 0 {
			continue
		}

		// Try to find a key column (first column named key/id/name)
		keyCol := pickKeyColumn(colsRaw)
		if keyCol != "" && safeIdentRe.MatchString(keyCol) {
			// Extract string values for the key column
			keys := extractKeyValues(colsRaw, keyCol, valuesBlock)
			if len(keys) > 0 {
				inList := buildInList(keys)
				schema, table := schemaTable(tbl)
				if s, tb, ok := safeQualified(schema, table); ok {
					add(Check{
						Kind:        KindRows,
						Description: fmt.Sprintf("seed rows in %s (%s IN (%s))", tbl, keyCol, truncateList(inList, 60)),
						SQL: fmt.Sprintf(
							"SELECT COUNT(*)>=%d AS ok FROM %s.%s WHERE %s IN (%s)",
							len(keys), quoteIdent(s), quoteIdent(tb), quoteIdent(keyCol), inList,
						),
					})
					continue
				}
			}
		}

		// Fallback: row-count check
		schema, table := schemaTable(tbl)
		if s, tb, ok := safeQualified(schema, table); ok {
			add(Check{
				Kind:        KindRows,
				Description: fmt.Sprintf("at least %d rows in %s", tuples, tbl),
				SQL: fmt.Sprintf(
					"SELECT COUNT(*) >= %d AS ok FROM %s.%s",
					tuples, quoteIdent(s), quoteIdent(tb),
				),
			})
		}
	}

	// INSERT INTO … SELECT … FROM (VALUES …) AS alias(cols)
	// Seed rows written with the SELECT/VALUES form (e.g. with WHERE NOT EXISTS guard).
	for _, m := range reInsertSelectValues.FindAllStringSubmatch(stripped, -1) {
		tbl := qualifyTable(m[1])
		valuesBlock := m[2]
		colsRaw := m[3]

		tuples := countValueTuples(valuesBlock)
		if tuples == 0 {
			continue
		}

		keyCol := pickKeyColumn(colsRaw)
		if keyCol != "" && safeIdentRe.MatchString(keyCol) {
			keys := extractKeyValues(colsRaw, keyCol, valuesBlock)
			if len(keys) > 0 {
				inList := buildInList(keys)
				schema, table := schemaTable(tbl)
				if s, tb, ok := safeQualified(schema, table); ok {
					add(Check{
						Kind:        KindRows,
						Description: fmt.Sprintf("seed rows in %s (%s IN (%s))", tbl, keyCol, truncateList(inList, 60)),
						SQL: fmt.Sprintf(
							"SELECT COUNT(*)>=%d AS ok FROM %s.%s WHERE %s IN (%s)",
							len(keys), quoteIdent(s), quoteIdent(tb), quoteIdent(keyCol), inList,
						),
					})
					continue
				}
			}
		}

		// Fallback: row-count check
		schema, table := schemaTable(tbl)
		if s, tb, ok := safeQualified(schema, table); ok {
			add(Check{
				Kind:        KindRows,
				Description: fmt.Sprintf("at least %d rows in %s", tuples, tbl),
				SQL: fmt.Sprintf(
					"SELECT COUNT(*) >= %d AS ok FROM %s.%s",
					tuples, quoteIdent(s), quoteIdent(tb),
				),
			})
		}
	}

	return checks
}

// BuildVerificationQuery combines all checks into a single SQL that returns one
// row per check with columns: description TEXT, ok BOOLEAN.
// It wraps the entire query in a read-only transaction guard comment.
func BuildVerificationQuery(checks []Check) string {
	if len(checks) == 0 {
		return "SELECT NULL::text AS description, NULL::boolean AS ok WHERE false;"
	}

	var parts []string
	for _, c := range checks {
		escaped := strings.ReplaceAll(c.Description, "'", "''")
		if c.Kind == KindUnchecked {
			parts = append(parts, fmt.Sprintf(
				"SELECT '%s'::text AS description, NULL::boolean AS ok",
				escaped,
			))
			continue
		}
		// Wrap as: SELECT '<desc>' AS description, (SELECT ... AS ok) AS ok
		inner := c.SQL
		// The inner SQL must produce one row with column "ok"
		parts = append(parts, fmt.Sprintf(
			"SELECT '%s'::text AS description, (%s) AS ok",
			escaped, inner,
		))
	}

	return strings.Join(parts, "\nUNION ALL\n") + ";"
}

// countValueTuples counts the number of VALUES tuples (groups of parentheses) in raw.
func countValueTuples(raw string) int {
	count := 0
	depth := 0
	for _, ch := range raw {
		if ch == '(' {
			if depth == 0 {
				count++
			}
			depth++
		} else if ch == ')' {
			depth--
		}
	}
	return count
}

// pickKeyColumn returns the first column name that looks like a natural key.
func pickKeyColumn(colsRaw string) string {
	cols := splitColumns(colsRaw)
	for _, c := range cols {
		lower := strings.ToLower(strings.TrimSpace(c))
		if lower == "key" || lower == "name" || lower == "slug" || lower == "code" || lower == "title" {
			return lower
		}
	}
	for _, c := range cols {
		lower := strings.ToLower(strings.TrimSpace(c))
		if strings.HasSuffix(lower, "_id") || lower == "id" {
			return lower
		}
	}
	return ""
}

// splitColumns splits a comma-separated column list, ignoring whitespace.
func splitColumns(raw string) []string {
	var cols []string
	for _, c := range strings.Split(raw, ",") {
		c = strings.TrimSpace(c)
		// strip any type casts or qualifiers
		if idx := strings.Index(c, "::"); idx >= 0 {
			c = c[:idx]
		}
		c = strings.Trim(c, `"`)
		if c != "" {
			cols = append(cols, c)
		}
	}
	return cols
}

// extractKeyValues finds the values for keyCol in the VALUES block.
func extractKeyValues(colsRaw, keyCol, valuesBlock string) []string {
	cols := splitColumns(colsRaw)
	keyIdx := -1
	for i, c := range cols {
		if strings.ToLower(strings.TrimSpace(c)) == keyCol {
			keyIdx = i
			break
		}
	}
	if keyIdx < 0 {
		return nil
	}

	var keys []string
	// parse tuples: (val1, val2, …)
	depth := 0
	start := -1
	var tupleTokens []string
	for i, ch := range valuesBlock {
		if ch == '(' {
			if depth == 0 {
				start = i + 1
			}
			depth++
		} else if ch == ')' {
			depth--
			if depth == 0 && start >= 0 {
				inner := valuesBlock[start:i]
				tupleTokens = append(tupleTokens, inner)
				start = -1
			}
		}
	}

	for _, tuple := range tupleTokens {
		vals := splitValues(tuple)
		if keyIdx < len(vals) {
			v := strings.TrimSpace(vals[keyIdx])
			// Unquote string literals
			if strings.HasPrefix(v, "'") && strings.HasSuffix(v, "'") {
				v = v[1 : len(v)-1]
				keys = append(keys, v)
			}
		}
	}
	return keys
}

// splitValues splits a tuple body by commas (respects nested parens and quotes).
func splitValues(raw string) []string {
	var vals []string
	var cur strings.Builder
	depth := 0
	inStr := false
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		if ch == '\'' && !inStr {
			inStr = true
			cur.WriteByte(ch)
		} else if ch == '\'' && inStr {
			// check for escaped quote
			if i+1 < len(raw) && raw[i+1] == '\'' {
				cur.WriteByte(ch)
				cur.WriteByte(raw[i+1])
				i++
			} else {
				inStr = false
				cur.WriteByte(ch)
			}
		} else if ch == '(' && !inStr {
			depth++
			cur.WriteByte(ch)
		} else if ch == ')' && !inStr {
			depth--
			cur.WriteByte(ch)
		} else if ch == ',' && depth == 0 && !inStr {
			vals = append(vals, cur.String())
			cur.Reset()
		} else {
			cur.WriteByte(ch)
		}
	}
	if cur.Len() > 0 {
		vals = append(vals, cur.String())
	}
	return vals
}

// buildInList builds a quoted SQL IN list from string keys.
func buildInList(keys []string) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = "'" + strings.ReplaceAll(k, "'", "''") + "'"
	}
	return strings.Join(parts, ", ")
}

func truncateList(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
