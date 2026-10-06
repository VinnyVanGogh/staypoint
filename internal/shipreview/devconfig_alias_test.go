package shipreview_test

import (
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
