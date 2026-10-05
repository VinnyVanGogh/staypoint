package shipreview

import (
	"errors"
	"strings"
	"testing"
)

func TestEffectiveMergeMode(t *testing.T) {
	cases := []struct {
		cfg    *ProjectDevConfig
		isWork bool
		want   string
	}{
		{nil, false, MergeModeDirect},                                            // existing projects: today's behaviour
		{&ProjectDevConfig{}, false, MergeModeDirect},                            // unset
		{&ProjectDevConfig{}, true, MergeModeOpenPR},                             // work repos default to Open PR
		{&ProjectDevConfig{MergeMode: MergeModePRMerge}, true, MergeModePRMerge}, // explicit wins
		{&ProjectDevConfig{MergeMode: MergeModeDirect}, true, MergeModeDirect},
	}
	for _, c := range cases {
		if got := EffectiveMergeMode(c.cfg, c.isWork); got != c.want {
			t.Errorf("EffectiveMergeMode(%+v, work=%v) = %q, want %q", c.cfg, c.isWork, got, c.want)
		}
	}
	for _, m := range []string{"", "direct", "open_pr", "pr_merge"} {
		if !ValidMergeMode(m) {
			t.Errorf("ValidMergeMode(%q) = false", m)
		}
	}
	if ValidMergeMode("merge") {
		t.Error(`ValidMergeMode("merge") = true`)
	}
}

func TestSummarizeChecks(t *testing.T) {
	c := func(bucket string) PRCheck { return PRCheck{Name: bucket, Bucket: bucket} }
	cases := []struct {
		checks []PRCheck
		want   string
	}{
		{nil, ChecksNone},
		{[]PRCheck{c("pass"), c("skipping")}, ChecksPassed},
		{[]PRCheck{c("pass"), c("pending")}, ChecksRunning},
		{[]PRCheck{c("pending"), c("fail")}, ChecksFailed},
		{[]PRCheck{c("pass"), c("cancel")}, ChecksFailed},
	}
	for _, tc := range cases {
		if got := SummarizeChecks(tc.checks); got != tc.want {
			t.Errorf("SummarizeChecks(%v) = %q, want %q", tc.checks, got, tc.want)
		}
	}
	if ng := NotGreen([]PRCheck{c("pass"), c("fail"), c("pending"), c("skipping")}); len(ng) != 2 {
		t.Errorf("NotGreen = %v, want fail + pending", ng)
	}
}

func TestFailingLines(t *testing.T) {
	log := strings.Join([]string{
		"build\tRun tests\t2026-10-05T10:00:00.1Z ok  \tpkg/a\t0.1s",
		"build\tRun tests\t2026-10-05T10:00:00.2Z --- FAIL: TestX (0.00s)",
		"build\tRun tests\t2026-10-05T10:00:00.3Z --- FAIL: TestX (0.00s)",
		"build\tRun tests\t2026-10-05T10:00:00.4Z ##[error]Process completed with exit code 1.",
		"e2e\tRun specs\t2026-10-05T10:00:00.5Z   ✘  3 specs/09-ship-review.spec.ts:12 › merge (10s)",
	}, "\n")
	got := failingLines(log)
	want := []string{
		"--- FAIL: TestX (0.00s)",
		"##[error]Process completed with exit code 1.",
		"✘  3 specs/09-ship-review.spec.ts:12 › merge (10s)",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("failingLines =\n%q\nwant\n%q", got, want)
	}
}

func TestResolveGHAuth_WorkRepoNeverUsesDefaultLogin(t *testing.T) {
	def := t.TempDir()
	prev := defaultGHConfigDir
	defaultGHConfigDir = func() string { return def }
	t.Cleanup(func() { defaultGHConfigDir = prev })

	if _, err := ResolveGHAuth(&ProjectDevConfig{}, "/r", true); !errors.Is(err, ErrWorkRepoGHAuth) {
		t.Errorf("work repo, no gh_config_dir: err = %v", err)
	}
	if _, err := ResolveGHAuth(&ProjectDevConfig{GHConfigDir: def}, "/r", true); !errors.Is(err, ErrWorkRepoGHAuth) {
		t.Errorf("work repo on the default gh dir: err = %v", err)
	}
	if _, err := ResolveGHAuth(&ProjectDevConfig{GHConfigDir: "rel"}, "/r", false); err == nil {
		t.Error("relative gh_config_dir accepted")
	}
	work := t.TempDir()
	a, err := ResolveGHAuth(&ProjectDevConfig{GHConfigDir: work}, "/r", true)
	if err != nil {
		t.Fatalf("work repo with its own dir: %v", err)
	}
	t.Setenv("GH_TOKEN", "personal")
	t.Setenv("GITHUB_TOKEN", "personal")
	env := strings.Join(a.env(), "\n")
	if strings.Contains(env, "GH_TOKEN=personal") || strings.Contains(env, "GITHUB_TOKEN=personal") || !strings.Contains(env, "GH_CONFIG_DIR="+work) {
		t.Errorf("work env leaks the daemon's token or misses the work dir:\n%s", env)
	}
	// Personal repos keep the daemon's own gh setup.
	p, err := ResolveGHAuth(&ProjectDevConfig{}, "/r", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.env(), "\n"), "GH_TOKEN=personal") {
		t.Error("personal repo lost GH_TOKEN")
	}
}
