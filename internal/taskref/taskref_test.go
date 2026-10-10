package taskref

import "testing"

func TestParse_RejectsNonReferences(t *testing.T) {
	for _, s := range []string{
		"", "task-662143ce", "STA", "STA-", "-12", "STA-0", "STA-012", "S-1",
		"TOOLONG-1", "STA-12/slug", "STA-1234567890", "STA 12", "STA-12a",
	} {
		if _, _, ok := Parse(s); ok {
			t.Errorf("Parse(%q) ok, want not a reference", s)
		}
	}
}

func TestParse_UpperCasesKey(t *testing.T) {
	key, n, ok := Parse(" sta-123 ")
	if !ok || key != "STA" || n != 123 {
		t.Fatalf("Parse = %q %d %v, want STA 123 true", key, n, ok)
	}
	if got := Format(key, n); got != "STA-123" {
		t.Fatalf("Format = %q", got)
	}
}

func TestOrgKey(t *testing.T) {
	for org, want := range map[string]string{
		"":                 "STA",
		"StayPoint":        "STA",
		"managed solution": "MAN",
		"Managed Solution": "MAN",
		"Maintenance":      "PER",
		"Research":         "RES",
		"RuneLite":         "RUN",
		"STA":              "STA",
		"Rhizome":          "RHI",
		"Ünïcode Co":       "NCO",
		"42":               "TSK",
	} {
		if got := OrgKey(org); got != want {
			t.Errorf("OrgKey(%q) = %q, want %q", org, got, want)
		}
	}
}

func TestSlugify(t *testing.T) {
	for name, want := range map[string]string{
		"Playwright UI specs":                        "playwright-ui-specs",
		"Readable task URLs: /STA-123/playwright-ui": "readable-task-urls-sta-123",
		"[STA-775] Task page URL missing org":        "task-page-url-missing-org",
		"CI & Testing":                               "ci-testing",
		"Don't break the café":                       "dont-break-the-cafe",
		"  --  ":                                     "",
		"日本語":                                        "",
		"one two three four five six seven":          "one-two-three-four-five",
	} {
		if got := Slugify(name); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestURL_UsesLocalhostAndOverride(t *testing.T) {
	t.Setenv("STAYPOINT_BASE_URL", "")
	if got := URL("STA-1", "x"); got != "http://localhost:41421/STA-1/x" {
		t.Fatalf("URL = %q", got)
	}
	if got := Path("STA-1", ""); got != "/STA-1" {
		t.Fatalf("Path = %q", got)
	}
	t.Setenv("STAYPOINT_BASE_URL", "http://staypoint.local/")
	if got := URL("MAN-2", "y"); got != "http://staypoint.local/MAN-2/y" {
		t.Fatalf("URL override = %q", got)
	}
}
