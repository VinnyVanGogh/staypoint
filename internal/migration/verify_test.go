package migration_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"

	_ "github.com/lib/pq"

	"github.com/VinnyVanGogh/staypoint/internal/migration"
)

// ------------- Parser tests -----------------------------------------------

// corporateValuesMigration mirrors the sample migration referenced in STA-564.
const corporateValuesMigration = `-- 20261003120000_corporate_values.sql
CREATE TABLE IF NOT EXISTS corporate_values (
    id   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    key  text NOT NULL UNIQUE,
    label text NOT NULL,
    sort_order int NOT NULL DEFAULT 0
);

ALTER TABLE corporate_values ENABLE ROW LEVEL SECURITY;

CREATE POLICY cv_select ON corporate_values FOR SELECT USING (true);

CREATE INDEX idx_cv_sort ON corporate_values (sort_order);

INSERT INTO corporate_values (key, label, sort_order) VALUES
    ('integrity', 'Integrity', 1),
    ('collaboration', 'Collaboration', 2),
    ('excellence', 'Excellence', 3);
`

func TestParseChecks_CorporateValues(t *testing.T) {
	checks := migration.ParseChecks(corporateValuesMigration)
	if len(checks) == 0 {
		t.Fatal("expected checks, got none")
	}

	kindsSeen := map[migration.CheckKind]bool{}
	for _, c := range checks {
		kindsSeen[c.Kind] = true
		if c.SQL == "" && c.Kind != migration.KindUnchecked {
			t.Errorf("check %q has empty SQL", c.Description)
		}
	}

	required := []migration.CheckKind{
		migration.KindRegclass,
		migration.KindRLS,
		migration.KindPolicy,
		migration.KindIndex,
		migration.KindRows,
	}
	for _, k := range required {
		if !kindsSeen[k] {
			t.Errorf("expected check kind %q, not found in %v", k, checks)
		}
	}
}

func TestParseChecks_CreateTableQualified(t *testing.T) {
	sql := `CREATE TABLE public.sites (id uuid PRIMARY KEY);`
	checks := migration.ParseChecks(sql)
	found := false
	for _, c := range checks {
		if c.Kind == migration.KindRegclass && strings.Contains(c.SQL, "public.sites") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected regclass check for public.sites, got %v", checks)
	}
}

func TestParseChecks_AddColumn(t *testing.T) {
	sql := `ALTER TABLE profiles ADD COLUMN avatar_url text;`
	checks := migration.ParseChecks(sql)
	found := false
	for _, c := range checks {
		if c.Kind == migration.KindColumn && strings.Contains(c.SQL, "avatar_url") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected column check for avatar_url, got %v", checks)
	}
}

func TestParseChecks_CreateFunction(t *testing.T) {
	sql := `CREATE OR REPLACE FUNCTION update_updated_at() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.updated_at = now(); RETURN NEW; END $$;`
	checks := migration.ParseChecks(sql)
	found := false
	for _, c := range checks {
		if c.Kind == migration.KindFunction && strings.Contains(c.SQL, "update_updated_at") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected function check for update_updated_at, got %v", checks)
	}
}

func TestParseChecks_NoFalsePositiveInComments(t *testing.T) {
	sql := `-- CREATE TABLE ignored (id uuid);
/* CREATE POLICY also_ignored ON foo FOR SELECT USING (true); */
CREATE TABLE real_table (id uuid PRIMARY KEY);`
	checks := migration.ParseChecks(sql)
	for _, c := range checks {
		if strings.Contains(c.Description, "ignored") || strings.Contains(c.Description, "also_ignored") {
			t.Errorf("commented-out DDL produced a check: %v", c)
		}
	}
	found := false
	for _, c := range checks {
		if strings.Contains(c.Description, "real_table") {
			found = true
		}
	}
	if !found {
		t.Errorf("real_table check not produced")
	}
}

func TestBuildVerificationQuery_Empty(t *testing.T) {
	q := migration.BuildVerificationQuery(nil)
	if !strings.Contains(q, "WHERE false") {
		t.Errorf("expected no-op query for empty checks, got: %s", q)
	}
}

func TestBuildVerificationQuery_Roundtrip(t *testing.T) {
	checks := migration.ParseChecks(corporateValuesMigration)
	q := migration.BuildVerificationQuery(checks)
	if q == "" {
		t.Fatal("expected non-empty verification query")
	}
	// Should have UNION ALL
	if len(checks) > 1 && !strings.Contains(q, "UNION ALL") {
		t.Errorf("expected UNION ALL for multiple checks, got: %s", q)
	}
}

// ------------- Postgres integration tests ---------------------------------
// These run only when TEST_PG_DSN is set or a local PG is available.

func testPGDSN(t *testing.T) string {
	t.Helper()
	if dsn := os.Getenv("TEST_PG_DSN"); dsn != "" {
		return dsn
	}
	// Try local default
	dsn := "postgres://localhost/postgres?sslmode=disable"
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("postgres not available: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	return dsn
}

func TestRunChecks_AutoMode_BeforeAndAfter(t *testing.T) {
	dsn := testPGDSN(t)
	ctx := context.Background()

	// Create throwaway schema
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	schema := fmt.Sprintf("sta564_test_%d", os.Getpid())
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	defer db.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE") //nolint:errcheck

	migSQL := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s.corporate_values (
    id   serial PRIMARY KEY,
    key  text NOT NULL UNIQUE,
    label text NOT NULL,
    sort_order int NOT NULL DEFAULT 0
);
ALTER TABLE %s.corporate_values ENABLE ROW LEVEL SECURITY;
CREATE POLICY cv_select ON %s.corporate_values FOR SELECT USING (true);
CREATE INDEX idx_%s_cv_sort ON %s.corporate_values (sort_order);
INSERT INTO %s.corporate_values (key, label, sort_order) VALUES
    ('integrity', 'Integrity', 1),
    ('collaboration', 'Collaboration', 2);
`, schema, schema, schema, schema, schema, schema)

	checks := migration.ParseChecks(migSQL)
	if len(checks) == 0 {
		t.Fatal("no checks generated")
	}

	// --- Before applying: all checks should fail ---
	resultsBefore, err := migration.RunChecks(ctx, dsn, checks)
	if err != nil {
		t.Fatalf("RunChecks before: %v", err)
	}
	if migration.AllPassed(resultsBefore) {
		t.Error("expected at least one failure before migration is applied")
	}

	// --- Apply the migration ---
	if _, err := db.ExecContext(ctx, migSQL); err != nil {
		t.Fatalf("apply migration: %v", err)
	}

	// --- After applying: all checks should pass ---
	resultsAfter, err := migration.RunChecks(ctx, dsn, checks)
	if err != nil {
		t.Fatalf("RunChecks after: %v", err)
	}
	if !migration.AllPassed(resultsAfter) {
		failed := migration.FailedDescriptions(resultsAfter)
		t.Errorf("expected all checks to pass after migration; failed: %v", failed)
	}
}

// ------------- Tests using the exact 20261003120000_corporate_values.sql ----

// exactCorporateValuesMigration is the verbatim content of the migration added
// in rhizome commit 3da7576 (STA-604). The parser must produce pg_policies
// checks for the two quoted-identifier policy names and a rows check for the
// four seed titles inserted via SELECT … FROM (VALUES …).
const exactCorporateValuesMigration = `-- About page: "Our corporate values" block
-- Additive only. Lives in its own table rather than in company_values so the
-- live "Our commitments" block cannot pick these rows up, whichever of the
-- migration and the frontend deploy lands first.

CREATE TABLE IF NOT EXISTS public.corporate_values (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    title TEXT NOT NULL,
    description TEXT,
    icon TEXT NOT NULL DEFAULT 'leaf',
    sort_order INTEGER NOT NULL DEFAULT 0,
    is_active BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE public.corporate_values ENABLE ROW LEVEL SECURITY;

-- Same access rules company_values ends up with after 20260127100000
DROP POLICY IF EXISTS "Public can view active corporate values" ON public.corporate_values;
CREATE POLICY "Public can view active corporate values"
    ON public.corporate_values
    FOR SELECT
    USING (is_active = true);

DROP POLICY IF EXISTS "Admins can manage corporate values" ON public.corporate_values;
CREATE POLICY "Admins can manage corporate values"
    ON public.corporate_values
    FOR ALL
    USING (
        EXISTS (
            SELECT 1 FROM public.profiles
            WHERE id = auth.uid()
            AND role IN ('admin', 'editor')
        )
    );

DROP TRIGGER IF EXISTS update_corporate_values_updated_at ON public.corporate_values;
CREATE TRIGGER update_corporate_values_updated_at
    BEFORE UPDATE ON public.corporate_values
    FOR EACH ROW
    EXECUTE FUNCTION update_updated_at_column();

-- Seed only into an empty table so a re-run never duplicates rows
INSERT INTO public.corporate_values (title, icon, sort_order)
SELECT v.title, v.icon, v.sort_order
FROM (VALUES
    ('Integrity', 'shield', 1),
    ('Creativity', 'lightbulb', 2),
    ('Teamwork', 'users', 3),
    ('Responsible citizenship', 'civic', 4)
) AS v(title, icon, sort_order)
WHERE NOT EXISTS (SELECT 1 FROM public.corporate_values);

-- Section heading. DO NOTHING so an edited value is never overwritten.
INSERT INTO public.site_settings (key, value, category, description) VALUES
    ('about_corporate_values_title', '"Our corporate values"', 'about', 'About page corporate values section title'),
    ('about_corporate_values_subtitle', '"Collective Achievement"', 'about', 'About page corporate values section subtitle (motto)')
ON CONFLICT (key) DO NOTHING;
`

// TestParseChecks_ExactCorporateValuesMigration verifies that the parser
// produces pg_policies checks for both quoted-identifier policies and a
// KindRows check covering the 4 INSERT…SELECT…FROM (VALUES…) seed titles.
func TestParseChecks_ExactCorporateValuesMigration(t *testing.T) {
	checks := migration.ParseChecks(exactCorporateValuesMigration)

	var policyDescs []string
	var rowsDescs []string
	for _, c := range checks {
		switch c.Kind {
		case migration.KindPolicy:
			policyDescs = append(policyDescs, c.Description)
			if c.SQL == "" {
				t.Errorf("policy check %q has empty SQL", c.Description)
			}
		case migration.KindRows:
			rowsDescs = append(rowsDescs, c.Description)
		}
	}

	// Both quoted-identifier policies must produce pg_policies checks.
	wantPolicies := []string{
		"Public can view active corporate values",
		"Admins can manage corporate values",
	}
	for _, want := range wantPolicies {
		found := false
		for _, c := range checks {
			if c.Kind == migration.KindPolicy && strings.Contains(c.SQL, want) {
				found = true
				if !strings.Contains(c.SQL, "pg_policies") {
					t.Errorf("policy check for %q does not query pg_policies: %s", want, c.SQL)
				}
			}
		}
		if !found {
			t.Errorf("no KindPolicy check found for policy %q; got policy checks: %v", want, policyDescs)
		}
	}

	// The INSERT…SELECT…FROM (VALUES…) seed must produce a KindRows check
	// covering all 4 titles, not be silently omitted.
	wantTitles := []string{"Integrity", "Creativity", "Teamwork", "Responsible citizenship"}
	foundRows := false
	for _, c := range checks {
		if c.Kind != migration.KindRows {
			continue
		}
		if !strings.Contains(c.SQL, "corporate_values") {
			continue
		}
		allPresent := true
		for _, title := range wantTitles {
			if !strings.Contains(c.SQL, title) {
				allPresent = false
				break
			}
		}
		if allPresent {
			foundRows = true
			break
		}
	}
	if !foundRows {
		t.Errorf("no KindRows check covering all 4 seed titles; got rows checks: %v", rowsDescs)
	}
}

// TestParseChecks_QuotedPolicyName verifies the policy name case is preserved
// (pg_policies stores quoted-identifier names with original case).
func TestParseChecks_QuotedPolicyName(t *testing.T) {
	sql := `CREATE POLICY "Mixed Case Policy" ON public.things FOR SELECT USING (true);`
	checks := migration.ParseChecks(sql)
	found := false
	for _, c := range checks {
		if c.Kind == migration.KindPolicy && strings.Contains(c.SQL, "Mixed Case Policy") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected policy check preserving case 'Mixed Case Policy', got %v", checks)
	}
}

// TestParseChecks_InsertSelectValues verifies that INSERT…SELECT…FROM (VALUES…)
// seed rows are checked, not silently omitted.
func TestParseChecks_InsertSelectValues(t *testing.T) {
	sql := `INSERT INTO public.settings (key, value)
SELECT v.key, v.value
FROM (VALUES
    ('alpha', 'a'),
    ('beta', 'b'),
    ('gamma', 'c')
) AS v(key, value)
WHERE NOT EXISTS (SELECT 1 FROM public.settings);`
	checks := migration.ParseChecks(sql)
	found := false
	for _, c := range checks {
		if c.Kind == migration.KindRows && strings.Contains(c.SQL, "settings") {
			found = true
			if !strings.Contains(c.SQL, "'alpha'") || !strings.Contains(c.SQL, "'beta'") || !strings.Contains(c.SQL, "'gamma'") {
				t.Errorf("rows check missing expected keys: %s", c.SQL)
			}
		}
	}
	if !found {
		t.Errorf("INSERT…SELECT…FROM (VALUES…) produced no KindRows check; got %v", checks)
	}
}

func TestRunChecks_ReadOnly(t *testing.T) {
	dsn := testPGDSN(t)
	ctx := context.Background()

	// Attempt a write inside the verify transaction via a crafted "check".
	writeCheck := migration.Check{
		Kind:        migration.KindRows,
		Description: "write attempt (should fail)",
		SQL:         "SELECT (INSERT INTO pg_catalog.pg_class DEFAULT VALUES RETURNING 1) AS ok",
	}
	results, err := migration.RunChecks(ctx, dsn, []migration.Check{writeCheck})
	// Either err != nil or the check failed — a write must not succeed
	if err == nil && len(results) > 0 && results[0].Passed {
		t.Error("write inside read-only transaction should not pass")
	}
}
