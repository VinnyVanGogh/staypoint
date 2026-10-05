//go:build !windows

package server_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

// STA-727: start-dev on a Supabase project with no dev_command auto-proposes a
// config and upserts the whole row. That upsert must keep the Board's
// live_credentials flag, or one confirmed start would silently turn the
// project back into a non-live one. The same goes for the Board's merge mode
// and gh_config_dir (STA-717).
func TestShipReviewLive_SupabaseAutoProposeKeepsFlag(t *testing.T) {
	e := newLiveShipEnv(t)
	e.stopDevOnCleanup(t)
	// Live flag but no dev_command yet: start-dev will auto-propose.
	ghDir := filepath.Join(t.TempDir(), "gh")
	e.putDevConfig(t, map[string]any{"live_credentials": true, "merge_mode": "open_pr", "gh_config_dir": ghDir})
	if err := os.MkdirAll(filepath.Join(e.repoDir, "supabase"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.repoDir, "supabase", "config.toml"), []byte("project_id = \"live\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Without the Board gate the proposal must not even be written.
	resp, rb := shipDoReq(t, e.client, e.token, "POST", e.cardURL()+"/start-dev", []byte(`{"confirm_live":true}`))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("agent start-dev on live Supabase project: want 403, got %d %s", resp.StatusCode, rb)
	}

	// Confirmed Board start: no package.json, so the proposal has an empty
	// dev_command and start-dev ends in 409, but the row is upserted.
	resp, rb = shipDoReq(t, e.client, e.token, "POST", e.cardURL()+"/start-dev", []byte(`{"confirm_live":true}`), e.board, "", "mock-assertion")
	if resp.StatusCode == http.StatusForbidden {
		t.Fatalf("confirmed Board start-dev refused: %d %s", resp.StatusCode, rb)
	}
	if v, _ := e.listedLiveFlag(t); v != true {
		t.Errorf("after Supabase auto-propose: live_credentials = %v, want true (kept)", v)
	}
	cfg, err := shipreview.GetProjectDevConfig(e.db, e.repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MergeMode != "open_pr" || cfg.GHConfigDir != ghDir {
		t.Errorf("after Supabase auto-propose: merge_mode=%q gh_config_dir=%q, want open_pr and %q (kept)", cfg.MergeMode, cfg.GHConfigDir, ghDir)
	}
}
