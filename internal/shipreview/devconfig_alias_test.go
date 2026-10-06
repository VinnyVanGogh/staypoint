package shipreview_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

// STA-767: a repo path that reaches a live_credentials repo through an alias
// must load the live row, even when a non-live row already exists under the
// alias itself (e.g. one written by Supabase auto-propose before the fix).
func TestGetProjectDevConfig_AliasResolvesToLiveRow(t *testing.T) {
	db := openTestDB(t)
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	other := filepath.Join(base, "other")
	for _, d := range []string{repo, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}

	mustUpsert := func(cfg *shipreview.ProjectDevConfig) {
		t.Helper()
		if err := shipreview.UpsertProjectDevConfig(db, cfg); err != nil {
			t.Fatalf("UpsertProjectDevConfig(%s): %v", cfg.RepoPath, err)
		}
	}
	mustUpsert(&shipreview.ProjectDevConfig{RepoPath: repo, LiveCredentials: true, MergeMode: "open_pr"})
	mustUpsert(&shipreview.ProjectDevConfig{RepoPath: link, DevCommand: "bun run dev", SupabaseEnabled: true})
	mustUpsert(&shipreview.ProjectDevConfig{RepoPath: other, DevCommand: "exec sleep 60"})

	for _, p := range []string{repo, link, link + "/", repo + "/", base + "/./repo", base + "/other/../repo"} {
		cfg, err := shipreview.GetProjectDevConfig(db, p)
		if err != nil {
			t.Fatalf("GetProjectDevConfig(%q): %v", p, err)
		}
		if !cfg.LiveCredentials {
			t.Errorf("GetProjectDevConfig(%q).LiveCredentials = false, want true (same repo as live %s)", p, repo)
		}
		if cfg.MergeMode != "open_pr" {
			t.Errorf("GetProjectDevConfig(%q).MergeMode = %q, want the live row's open_pr", p, cfg.MergeMode)
		}
	}

	cfg, err := shipreview.GetProjectDevConfig(db, other)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LiveCredentials || cfg.DevCommand != "exec sleep 60" {
		t.Errorf("unrelated repo: got live=%v dev_command=%q, want its own non-live row", cfg.LiveCredentials, cfg.DevCommand)
	}
}

// STA-767: a repo path that cannot be stat'ed cannot be ruled out as an alias
// of a live repo, so LiveGateConfig gates it. With no live project at all
// there is nothing to alias and it is not gated.
func TestLiveGateConfig_UnstattablePathFailsClosed(t *testing.T) {
	db := openTestDB(t)
	base := t.TempDir()
	missing := filepath.Join(base, "missing")

	if _, gated, err := shipreview.LiveGateConfig(db, missing); err != nil || gated {
		t.Fatalf("no live projects: gated=%v err=%v, want false, nil", gated, err)
	}

	live := filepath.Join(base, "live")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := shipreview.UpsertProjectDevConfig(db, &shipreview.ProjectDevConfig{RepoPath: live, LiveCredentials: true}); err != nil {
		t.Fatal(err)
	}
	cfg, gated, err := shipreview.LiveGateConfig(db, missing)
	if err != nil || !gated {
		t.Fatalf("unstattable path with a live project: gated=%v err=%v, want true, nil", gated, err)
	}
	if cfg.LiveCredentials {
		t.Errorf("unstattable path loaded the live row; want defaults (gated only)")
	}

	other := filepath.Join(base, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, gated, err := shipreview.LiveGateConfig(db, other); err != nil || gated {
		t.Errorf("unrelated existing repo: gated=%v err=%v, want false, nil", gated, err)
	}
}

// STA-798: a live row whose path fails to stat for a reason other than not
// existing (here EACCES from a chmod 000 parent) could be the repo being
// started, so LiveGateConfig gates every other repo too. A non-live row that
// cannot be stat'ed, and a live row whose path no longer exists, cannot hide a
// live repo and leave the gate open.
func TestLiveGateConfig_UnstattableLiveRowFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so stat never returns EACCES")
	}
	db := openTestDB(t)
	base := t.TempDir()
	other := filepath.Join(base, "other")
	locked := filepath.Join(base, "locked")
	for _, d := range []string{other, filepath.Join(locked, "nonlive"), filepath.Join(locked, "live")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustUpsert := func(cfg *shipreview.ProjectDevConfig) {
		t.Helper()
		if err := shipreview.UpsertProjectDevConfig(db, cfg); err != nil {
			t.Fatalf("UpsertProjectDevConfig(%s): %v", cfg.RepoPath, err)
		}
	}
	mustUpsert(&shipreview.ProjectDevConfig{RepoPath: filepath.Join(locked, "nonlive"), DevCommand: "exec sleep 60"})
	mustUpsert(&shipreview.ProjectDevConfig{RepoPath: filepath.Join(base, "gone"), LiveCredentials: true})

	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if _, err := os.Stat(filepath.Join(locked, "live")); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("stat under chmod 000 dir: err = %v, want EACCES", err)
	}

	if _, gated, err := shipreview.LiveGateConfig(db, other); err != nil || gated {
		t.Fatalf("non-live EACCES row + ENOENT live row: gated=%v err=%v, want false, nil", gated, err)
	}

	mustUpsert(&shipreview.ProjectDevConfig{RepoPath: filepath.Join(locked, "live"), LiveCredentials: true})
	cfg, gated, err := shipreview.LiveGateConfig(db, other)
	if err != nil || !gated {
		t.Fatalf("live EACCES row: gated=%v err=%v, want true, nil", gated, err)
	}
	if cfg.LiveCredentials {
		t.Errorf("unrelated repo loaded the unstattable live row; want defaults (gated only)")
	}
}
