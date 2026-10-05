package shipreview

import (
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

func TestResolveGHAuth(t *testing.T) {
	t.Setenv("GH_TOKEN", "daemon-token")
	t.Setenv("GITHUB_TOKEN", "daemon-token")

	// No override: the repo's normal gh auth, environment untouched (Board,
	// 2026-10-05: no separate work credential).
	a, err := ResolveGHAuth(&ProjectDevConfig{}, "/r")
	if err != nil {
		t.Fatal(err)
	}
	if env := strings.Join(a.env(), "\n"); !strings.Contains(env, "GH_TOKEN=daemon-token") || !strings.Contains(env, "GITHUB_TOKEN=daemon-token") {
		t.Errorf("default auth changed the environment:\n%s", env)
	}
	if _, err := ResolveGHAuth(nil, "/r"); err != nil {
		t.Errorf("nil config: %v", err)
	}

	// An override wins over tokens in the daemon's environment.
	dir := t.TempDir()
	o, err := ResolveGHAuth(&ProjectDevConfig{GHConfigDir: dir}, "/r")
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(o.env(), "\n")
	if strings.Contains(env, "GH_TOKEN=daemon-token") || strings.Contains(env, "GITHUB_TOKEN=daemon-token") || !strings.Contains(env, "GH_CONFIG_DIR="+dir) {
		t.Errorf("override env:\n%s", env)
	}

	if _, err := ResolveGHAuth(&ProjectDevConfig{GHConfigDir: "rel"}, "/r"); err == nil {
		t.Error("relative gh_config_dir accepted")
	}
	if _, err := ResolveGHAuth(&ProjectDevConfig{GHConfigDir: dir + "/missing"}, "/r"); err == nil {
		t.Error("missing gh_config_dir accepted")
	}
}
