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
