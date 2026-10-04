package shipreview_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

func TestHasSupabaseConfig(t *testing.T) {
	dir := t.TempDir()

	if shipreview.HasSupabaseConfig(dir) {
		t.Error("want false for empty dir, got true")
	}

	subDir := filepath.Join(dir, "supabase")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(subDir, "config.toml")
	if err := os.WriteFile(configPath, []byte("[api]\nport = 54321\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !shipreview.HasSupabaseConfig(dir) {
		t.Error("want true when supabase/config.toml exists, got false")
	}
}

func TestHasEdgeRuntime(t *testing.T) {
	dir := t.TempDir()

	if shipreview.HasEdgeRuntime(dir) {
		t.Error("want false when no supabase/functions, got true")
	}

	fnsDir := filepath.Join(dir, "supabase", "functions")
	if err := os.MkdirAll(fnsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if shipreview.HasEdgeRuntime(dir) {
		t.Error("want false for empty functions dir, got true")
	}

	fnFile := filepath.Join(fnsDir, "hello.ts")
	if err := os.WriteFile(fnFile, []byte(`export default () => "hello"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !shipreview.HasEdgeRuntime(dir) {
		t.Error("want true when functions dir has files, got false")
	}
}

func TestProposeSupabaseDevConfig(t *testing.T) {
	dir := t.TempDir()

	// Without package.json — DevCommand should be empty.
	cfg := shipreview.ProposeSupabaseDevConfig(dir)
	if !cfg.SupabaseEnabled {
		t.Error("want SupabaseEnabled=true")
	}
	if cfg.DevURL != "http://127.0.0.1:5173" {
		t.Errorf("want DevURL http://127.0.0.1:5173, got %q", cfg.DevURL)
	}
	if cfg.DevCommand != "" {
		t.Errorf("want empty DevCommand without package.json, got %q", cfg.DevCommand)
	}

	// With package.json — DevCommand should be "bun run dev".
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg2 := shipreview.ProposeSupabaseDevConfig(dir)
	if cfg2.DevCommand != "bun run dev" {
		t.Errorf("want DevCommand 'bun run dev', got %q", cfg2.DevCommand)
	}
}

// --- Bug 1: no .env.local must refuse ---

func TestCopyAndValidateEnv_NoEnvFile_Refuses(t *testing.T) {
	mainRepo := t.TempDir()
	wt := t.TempDir()

	// No .env.local at all.
	var prog []string
	report := func(step, msg string, ok bool) {
		prog = append(prog, msg)
	}

	err := shipreview.CopyAndValidateEnv(mainRepo, wt, report)
	if err == nil {
		t.Fatal("want error when .env.local absent, got nil")
	}
	// Check progress reported failure.
	found := false
	for _, m := range prog {
		if len(m) > 0 {
			found = true
			break
		}
	}
	if !found {
		t.Error("want at least one progress message")
	}
	// Ensure .env.local was NOT silently created.
	if _, statErr := os.Stat(filepath.Join(wt, ".env.local")); statErr == nil {
		t.Error("should not have created .env.local in worktree when source absent")
	}
}

// --- Bug 2: prod URL in .env.local must refuse even if comment has "localhost" ---

func TestValidateSupabaseEnvURLs(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr bool
	}{
		{
			name:    "local 127.0.0.1 URL — ok",
			content: "VITE_SUPABASE_URL=http://127.0.0.1:54321\n",
			wantErr: false,
		},
		{
			name:    "local localhost URL — ok",
			content: "SUPABASE_URL=http://localhost:54321\n",
			wantErr: false,
		},
		{
			name:    "prod URL — refuse",
			content: "VITE_SUPABASE_URL=https://xyzabc.supabase.co\n",
			wantErr: true,
		},
		{
			name: "prod URL with localhost comment — still refuse",
			content: "# localhost would be nice\n" +
				"VITE_SUPABASE_URL=https://prod.supabase.co\n",
			wantErr: true,
		},
		{
			name: "prod URL in one key, localhost in another var — refuse",
			content: "SOME_OTHER_URL=http://localhost:8080\n" +
				"VITE_SUPABASE_URL=https://prod.supabase.co\n",
			wantErr: true,
		},
		{
			name:    "no supabase URL keys at all — ok (not a URL-bearing env)",
			content: "FOO=bar\n",
			wantErr: false,
		},
		{
			name:    "quoted local URL — ok",
			content: `NEXT_PUBLIC_SUPABASE_URL="http://127.0.0.1:54321"` + "\n",
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := shipreview.ValidateSupabaseEnvURLs(tc.content)
			if tc.wantErr && err == nil {
				t.Error("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("want nil, got %v", err)
			}
		})
	}
}

// --- Bug 3 + 4: ExtractSupabaseProject with multi-word service names ---

func TestExtractSupabaseProject(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
	}{
		{"single-word service", "supabase_db_rhizome_site", "rhizome_site"},
		{"pg_meta multi-word service", "supabase_pg_meta_rhizome_site", "rhizome_site"},
		{"edge_runtime multi-word service", "supabase_edge_runtime_foo", "foo"},
		{"storage service", "supabase_storage_myproject", "myproject"},
		{"kong service", "supabase_kong_my_cool_app", "my_cool_app"},
		{"not a supabase container", "nginx_proxy", ""},
		{"only prefix, no rest", "supabase_", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shipreview.ExtractSupabaseProject(tc.input)
			if got != tc.want {
				t.Errorf("ExtractSupabaseProject(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestAutoDetectSupabaseInGetProjectDevConfig(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()

	// No config stored, no supabase/config.toml → empty config.
	cfg, err := shipreview.GetProjectDevConfig(db, dir)
	if err != nil {
		t.Fatalf("GetProjectDevConfig: %v", err)
	}
	if cfg.SupabaseEnabled {
		t.Error("want SupabaseEnabled=false when no config.toml, got true")
	}

	// Create supabase/config.toml.
	subDir := filepath.Join(dir, "supabase")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "config.toml"), []byte("[api]\nport=54321\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Upsert the proposed config — simulates what StartDev does.
	proposed := shipreview.ProposeSupabaseDevConfig(dir)
	if err := shipreview.UpsertProjectDevConfig(db, proposed); err != nil {
		t.Fatalf("UpsertProjectDevConfig: %v", err)
	}

	cfg2, err := shipreview.GetProjectDevConfig(db, dir)
	if err != nil {
		t.Fatalf("GetProjectDevConfig after upsert: %v", err)
	}
	if !cfg2.SupabaseEnabled {
		t.Error("want SupabaseEnabled=true after upsert, got false")
	}
}

func TestSetDevState_AppendsLog(t *testing.T) {
	db := openTestDB(t)
	_, _ = db.Exec(`INSERT INTO tasks (id, name) VALUES ('t-state', 'state test')`)

	taskID := "t-state"
	repoDir, branch, sha := setupGitRepo(t)
	card, err := shipreview.CreateCard(db, taskID, branch, sha, []string{"check"}, "", repoDir, nil)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}

	if err := shipreview.SetDevState(db, card.ID, shipreview.DevStateStarting, "Step 1"); err != nil {
		t.Fatalf("SetDevState: %v", err)
	}
	if err := shipreview.SetDevState(db, card.ID, shipreview.DevStateStarting, "Step 2"); err != nil {
		t.Fatalf("SetDevState: %v", err)
	}
	if err := shipreview.SetDevState(db, card.ID, shipreview.DevStateReady, ""); err != nil {
		t.Fatalf("SetDevState ready: %v", err)
	}

	got, err := shipreview.GetCard(db, taskID)
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if got.DevState != shipreview.DevStateReady {
		t.Errorf("want DevState=%q, got %q", shipreview.DevStateReady, got.DevState)
	}
	if len(got.DevLog) != 2 {
		t.Errorf("want 2 log lines, got %d: %v", len(got.DevLog), got.DevLog)
	}
	if got.DevLog[0] != "Step 1" || got.DevLog[1] != "Step 2" {
		t.Errorf("unexpected log lines: %v", got.DevLog)
	}
}

func TestRunShellStep_ShellExpansion(t *testing.T) {
	db := openTestDB(t)
	_, _ = db.Exec(`INSERT INTO tasks (id, name) VALUES ('t-shell', 'shell test')`)

	repoDir, branch, sha := setupGitRepo(t)
	card, err := shipreview.CreateCard(db, "t-shell", branch, sha, []string{"check"}, "", repoDir, nil)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}

	touchFile := filepath.Join(repoDir, ".worktrees", "devserver-t-shell", "shell_ok")
	cfg := &shipreview.ProjectDevConfig{
		RepoPath:   repoDir,
		DevCommand: "sleep 9999",
		DevURL:     "http://127.0.0.1:9990",
		SetupSteps: []string{`echo hello | cat > ` + touchFile},
	}

	if _, err := shipreview.StartDevServer(db, card, cfg, repoDir); err != nil {
		t.Fatalf("StartDevServer with shell step: %v", err)
	}
	defer shipreview.StopDevServer(db, card)

	if _, statErr := os.Stat(touchFile); os.IsNotExist(statErr) {
		t.Error("shell step using pipe/redirect should have created the file, but it does not exist")
	}
}
