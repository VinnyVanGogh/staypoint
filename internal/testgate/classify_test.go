package testgate

import (
	"reflect"
	"testing"
)

func TestIsTestFile(t *testing.T) {
	cases := map[string]bool{
		// Go
		"internal/foo/bar_test.go": true,
		"internal/foo/bar.go":      false,
		"internal/foo/test.go":     false,
		// Python
		"pkg/test_api.py":   true,
		"pkg/api_test.py":   true,
		"pkg/conftest.py":   true,
		"pkg/api.py":        false,
		"pkg/testing.py":    false,
		"pkg/contest_ab.py": false,
		// TS / JS
		"web/src/Card.test.tsx":   true,
		"web/src/Card.spec.tsx":   true,
		"web/src/util.test.ts":    true,
		"web/src/util.spec.ts":    true,
		"web/src/util.test.js":    true,
		"web/src/util.spec.jsx":   true,
		"web/src/util.ts":         false,
		"web/src/spec.ts":         false,
		"web/src/latest.ts":       false,
		"web/src/protest.spec.md": false,
		// test directories
		"tests/ui/specs/20-pr.spec.ts":    true,
		"tests/helpers/fixtures.ts":       true,
		"web/src/__tests__/Card.tsx":      true,
		"internal/foo/testdata/in.golden": true,
		"contests/main.go":                false,
	}
	for path, want := range cases {
		if got := IsTestFile(path); got != want {
			t.Errorf("IsTestFile(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestIsExemptDefaults(t *testing.T) {
	cases := map[string]bool{
		"README.md":                   true,
		"docs/guide/setup.md":         true,
		"docs/diagram.png":            true,
		"internal/server/webui/a.css": true,
		"web/theme.scss":              true,
		".github/workflows/ci.yml":    true,
		"config.toml":                 true,
		".golangci.yml":               true,
		"package.json":                true,
		"go.mod":                      true,
		"go.sum":                      true,
		"pnpm-lock.yaml":              true,
		"LICENSE":                     true,
		".gitignore":                  true,
		"main.go":                     false,
		"internal/server/webui/app.js": false,
		"scripts/deploy.sh":           false,
		"db/migrations/001_init.sql":  false,
		"Makefile":                    false,
	}
	for path, want := range cases {
		if got := IsExempt(path, nil); got != want {
			t.Errorf("IsExempt(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestIsExemptProjectGlobs(t *testing.T) {
	extra := []string{"scripts/**", "*.sql", "Makefile", "  ", "internal/gen/*.go"}
	cases := map[string]bool{
		"scripts/deploy.sh":           true,
		"scripts/sub/dir/x.py":        true,
		"db/migrations/001_init.sql":  true,
		"Makefile":                    true,
		"internal/gen/models.go":      true,
		"internal/gen/sub/models.go":  false,
		"internal/server/server.go":   false,
		"scriptsx/deploy.sh":          false,
	}
	for path, want := range cases {
		if got := IsExempt(path, extra); got != want {
			t.Errorf("IsExempt(%q, extra) = %v, want %v", path, got, want)
		}
	}
}

func TestClassify(t *testing.T) {
	files := []string{
		"README.md",
		"internal/foo/foo.go",
		"internal/foo/foo_test.go",
		"web/app.js",
		"tests/ui/specs/1.spec.ts",
		"web/style.css",
	}
	c := Classify(files, nil)
	if want := []string{"internal/foo/foo.go", "web/app.js"}; !reflect.DeepEqual(c.Sources, want) {
		t.Errorf("Sources = %v, want %v", c.Sources, want)
	}
	if want := []string{"internal/foo/foo_test.go", "tests/ui/specs/1.spec.ts"}; !reflect.DeepEqual(c.Tests, want) {
		t.Errorf("Tests = %v, want %v", c.Tests, want)
	}
	if want := []string{"README.md", "web/style.css"}; !reflect.DeepEqual(c.Exempt, want) {
		t.Errorf("Exempt = %v, want %v", c.Exempt, want)
	}
}

// A test file under an exempt path (e.g. a docs test) is still a test, and a
// test file always wins over the exempt list so a test change is never hidden.
func TestClassifyTestBeatsExempt(t *testing.T) {
	c := Classify([]string{"docs/tests/check_links.py"}, nil)
	if len(c.Tests) != 1 || len(c.Exempt) != 0 {
		t.Fatalf("got %+v, want the file classed as a test", c)
	}
}

func TestUnmatchedSources(t *testing.T) {
	cases := []struct {
		name    string
		sources []string
		tests   []string
		want    []string
	}{
		{
			name:    "go: any test in the same package dir matches",
			sources: []string{"internal/a/a.go", "internal/b/b.go"},
			tests:   []string{"internal/a/other_test.go"},
			want:    []string{"internal/b/b.go"},
		},
		{
			name:    "go: test in another package does not match",
			sources: []string{"internal/a/a.go"},
			tests:   []string{"internal/a/sub/a_test.go"},
			want:    []string{"internal/a/a.go"},
		},
		{
			name:    "python: test_<stem> anywhere matches",
			sources: []string{"app/billing.py", "app/users.py"},
			tests:   []string{"tests/test_billing.py"},
			want:    []string{"app/users.py"},
		},
		{
			name:    "python: <stem>_test anywhere matches",
			sources: []string{"app/billing.py"},
			tests:   []string{"tests/unit/billing_test.py"},
			want:    nil,
		},
		{
			name:    "ts: <stem>.test / <stem>.spec anywhere matches",
			sources: []string{"src/Card.tsx", "src/util.ts", "src/api.ts"},
			tests:   []string{"src/Card.test.tsx", "test/util.spec.ts"},
			want:    []string{"src/api.ts"},
		},
		{
			name:    "ts: __tests__/<stem>.* matches",
			sources: []string{"src/Card.tsx"},
			tests:   []string{"src/__tests__/Card.tsx"},
			want:    nil,
		},
		{
			name:    "no tests at all: every source is unmatched",
			sources: []string{"a.go", "b.py"},
			tests:   nil,
			want:    []string{"a.go", "b.py"},
		},
		{
			name:    "short stems do not match by substring",
			sources: []string{"web/ui.js"},
			tests:   []string{"tests/build.spec.ts"},
			want:    []string{"web/ui.js"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := UnmatchedSources(tc.sources, tc.tests)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("UnmatchedSources = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseExemptGlobs(t *testing.T) {
	got := ParseGlobList("docs/**\n*.sql, scripts/** \n\n  Makefile ,")
	want := []string{"docs/**", "*.sql", "scripts/**", "Makefile"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseGlobList = %v, want %v", got, want)
	}
}
