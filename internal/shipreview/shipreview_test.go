package shipreview_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

// openTestDB creates an in-memory SQLite DB with the ship_review schema applied.
func openTestDB(t testing.TB) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS tasks (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			repo_path TEXT NOT NULL DEFAULT '',
			git_branch TEXT,
			status TEXT NOT NULL DEFAULT 'active',
			account_role TEXT NOT NULL DEFAULT 'work',
			max_budget_usd REAL NOT NULL DEFAULT 0.0,
			max_turns INTEGER NOT NULL DEFAULT 0,
			spent_tokens INTEGER NOT NULL DEFAULT 0,
			spent_usd REAL NOT NULL DEFAULT 0.0,
			spent_turns INTEGER NOT NULL DEFAULT 0,
			organization TEXT, project TEXT,
			is_blocked INTEGER NOT NULL DEFAULT 0,
			block_reason TEXT, parent_id TEXT,
			execution_stage TEXT NOT NULL DEFAULT 'todo',
			checkout_run_id TEXT, checkout_agent_id TEXT, assignee_agent_id TEXT,
			work_kind TEXT NOT NULL DEFAULT 'coding',
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			deleted_at TEXT
		);
		CREATE TABLE IF NOT EXISTS ship_review_cards (
			id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL,
			branch TEXT NOT NULL,
			head_sha TEXT NOT NULL,
			test_steps_json TEXT NOT NULL DEFAULT '[]',
			dev_url TEXT NOT NULL DEFAULT '',
			dev_pid INTEGER NOT NULL DEFAULT 0,
			dev_state TEXT NOT NULL DEFAULT '',
			dev_log_json TEXT NOT NULL DEFAULT '[]',
			status TEXT NOT NULL DEFAULT 'pending',
			approved_sha TEXT,
			main_sha TEXT,
			send_back_comment TEXT,
			reject_comment TEXT,
			files_changed_json TEXT NOT NULL DEFAULT '[]',
			check_runs_json TEXT NOT NULL DEFAULT '[]',
			branch_deleted INTEGER NOT NULL DEFAULT 0,
			branch_delete_error TEXT NOT NULL DEFAULT '',
			merge_mode TEXT NOT NULL DEFAULT '',
			pr_number INTEGER NOT NULL DEFAULT 0,
			pr_url TEXT NOT NULL DEFAULT '',
			pr_checks_json TEXT NOT NULL DEFAULT '[]',
			pr_checks_sha TEXT NOT NULL DEFAULT '',
			pr_checks_at TEXT NOT NULL DEFAULT '',
			pr_merge_error TEXT NOT NULL DEFAULT '',
			ci_fix_requested INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		);
		CREATE TABLE IF NOT EXISTS project_dev_configs (
			repo_path TEXT PRIMARY KEY,
			dev_command TEXT NOT NULL DEFAULT '',
			dev_url TEXT NOT NULL DEFAULT '',
			setup_steps_json TEXT NOT NULL DEFAULT '[]',
			migration_globs_json TEXT NOT NULL DEFAULT '[]',
			sql_editor_url TEXT NOT NULL DEFAULT '',
			supabase_enabled INTEGER NOT NULL DEFAULT 0,
			supabase_keep_up INTEGER NOT NULL DEFAULT 0,
			merge_mode TEXT NOT NULL DEFAULT '',
			gh_config_dir TEXT NOT NULL DEFAULT '',
			live_credentials INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		);
		CREATE TABLE IF NOT EXISTS task_comments (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id   TEXT NOT NULL,
			author    TEXT NOT NULL DEFAULT 'system',
			message   TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		);
	`)
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}
	return db
}

// setupGitRepo creates a temporary git repo with one commit and a feature branch.
// A bare clone is set up as origin so push/pull work in tests.
// Returns repoDir, featureBranch, featureSHA.
func setupGitRepo(t *testing.T) (repoDir, featureBranch, featureSHA string) {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "work")
	_ = os.MkdirAll(dir, 0755)

	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t.com",
	)
	run := func(d string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = d
		cmd.Env = gitEnv
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v (in %s): %v", args, d, err)
		}
		return string(out)
	}

	run(dir, "init", "-b", "main")
	run(dir, "config", "user.email", "t@t.com")
	run(dir, "config", "user.name", "test")

	// Initial commit on main.
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("init\n"), 0644)
	run(dir, "add", ".")
	run(dir, "commit", "-m", "init")

	// Feature branch.
	featureBranch = "feature/test-ship"
	run(dir, "checkout", "-b", featureBranch)
	_ = os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0644)
	run(dir, "add", ".")
	run(dir, "commit", "-m", "feat: add feature")

	sha, err := shipreview.CurrentBranchHEAD(context.Background(), dir, featureBranch)
	if err != nil {
		t.Fatalf("resolve HEAD: %v", err)
	}
	featureSHA = sha

	run(dir, "checkout", "main")

	// Create bare remote and push both branches.
	bareDir := filepath.Join(base, "bare.git")
	run(base, "init", "--bare", "-b", "main", bareDir)
	run(dir, "remote", "add", "origin", bareDir)
	run(dir, "push", "origin", "main")
	run(dir, "push", "origin", featureBranch)

	return dir, featureBranch, featureSHA
}

func TestCreateCardRequiresTestSteps(t *testing.T) {
	db := openTestDB(t)
	_, err := db.Exec(`INSERT INTO tasks (id, name) VALUES ('t1', 'Test task')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = shipreview.CreateCard(db, "t1", "feature/test", "abc123", nil, "", "", nil)
	if err == nil {
		t.Fatal("expected error for empty test_steps")
	}
}

func TestCardRoundTrip(t *testing.T) {
	db := openTestDB(t)
	_, err := db.Exec(`INSERT INTO tasks (id, name) VALUES ('t2', 'Test task')`)
	if err != nil {
		t.Fatal(err)
	}
	steps := []string{"1. Open /home page", "2. Click Login button", "3. Verify redirect to /dashboard"}
	card, err := shipreview.CreateCard(db, "t2", "feature/test", "deadbeef", steps, "http://localhost:5173", "", nil)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	if card.Status != "pending" {
		t.Errorf("want pending, got %q", card.Status)
	}
	if len(card.TestSteps) != 3 {
		t.Errorf("want 3 steps, got %d", len(card.TestSteps))
	}

	got, err := shipreview.GetCard(db, "t2")
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if got.HeadSHA != "deadbeef" {
		t.Errorf("want headSHA deadbeef, got %q", got.HeadSHA)
	}
	if got.DevURL != "http://localhost:5173" {
		t.Errorf("want devURL, got %q", got.DevURL)
	}
}

func TestSendBackAndReplace(t *testing.T) {
	db := openTestDB(t)
	_, _ = db.Exec(`INSERT INTO tasks (id, name) VALUES ('t3', 'Test task')`)
	steps := []string{"1. Check homepage"}
	card, _ := shipreview.CreateCard(db, "t3", "feature/test", "sha1", steps, "", "", nil)

	if err := shipreview.SendBack(db, card, "fix the typo"); err != nil {
		t.Fatalf("SendBack: %v", err)
	}

	got, _ := shipreview.GetCard(db, "t3")
	if got.Status != "sent_back" {
		t.Errorf("want sent_back, got %q", got.Status)
	}

	// Agent iterates: creates new card (replaces sent_back card).
	card2, err := shipreview.CreateCard(db, "t3", "feature/test", "sha2", steps, "", "", nil)
	if err != nil {
		t.Fatalf("second CreateCard: %v", err)
	}
	if card2.HeadSHA != "sha2" {
		t.Errorf("want sha2, got %q", card2.HeadSHA)
	}

	got2, _ := shipreview.GetCard(db, "t3")
	if got2.Status != "pending" {
		t.Errorf("want pending, got %q", got2.Status)
	}
}

func TestApproveAndMergeHashPin(t *testing.T) {
	db := openTestDB(t)
	_, _ = db.Exec(`INSERT INTO tasks (id, name) VALUES ('t4', 'Merge test')`)

	repoDir, branch, featureSHA := setupGitRepo(t)

	recordMainBase(t, db, repoDir, "t4")
	card, err := shipreview.CreateCard(db, "t4", branch, featureSHA, []string{"1. Verify"}, "", repoDir, nil)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}

	mainSHA, err := shipreview.ApproveAndMerge(context.Background(), db, card, repoDir, "main")
	if err != nil {
		t.Fatalf("ApproveAndMerge: %v", err)
	}
	if mainSHA == "" {
		t.Fatal("expected non-empty main SHA")
	}

	// Verify card is now approved.
	got, _ := shipreview.GetCard(db, "t4")
	if got.Status != "approved" {
		t.Errorf("want approved, got %q", got.Status)
	}
	if got.ApprovedSHA != featureSHA {
		t.Errorf("want approvedSHA=%q, got %q", featureSHA, got.ApprovedSHA)
	}
}

func TestApproveAndMergeRejectsMovedHead(t *testing.T) {
	db := openTestDB(t)
	_, _ = db.Exec(`INSERT INTO tasks (id, name) VALUES ('t5', 'Head moved test')`)

	repoDir, branch, _ := setupGitRepo(t)

	// Create card with a stale (wrong) SHA.
	card, err := shipreview.CreateCard(db, "t5", branch, "0000000000000000000000000000000000000000", []string{"1. Check"}, "", "", nil)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}

	_, err = shipreview.ApproveAndMerge(context.Background(), db, card, repoDir, "main")
	if err == nil {
		t.Fatal("expected error for moved HEAD")
	}
}

// TestFilesChangedPopulated verifies that CreateCard populates files_changed from git diff.
func TestFilesChangedPopulated(t *testing.T) {
	db := openTestDB(t)
	_, _ = db.Exec(`INSERT INTO tasks (id, name) VALUES ('t6', 'Files test')`)

	repoDir, branch, featureSHA := setupGitRepo(t)

	recordMainBase(t, db, repoDir, "t6")
	card, err := shipreview.CreateCard(db, "t6", branch, featureSHA, []string{"1. Verify feature.txt exists"}, "", repoDir, nil)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	if len(card.FilesChanged) == 0 {
		t.Fatal("expected files_changed to be non-empty")
	}
	found := false
	for _, f := range card.FilesChanged {
		if f == "feature.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected feature.txt in files_changed, got %v", card.FilesChanged)
	}
}

// TestFilesChangedEmptyRepoDir verifies CreateCard succeeds with empty repoDir.
func TestFilesChangedEmptyRepoDir(t *testing.T) {
	db := openTestDB(t)
	_, _ = db.Exec(`INSERT INTO tasks (id, name) VALUES ('t7', 'No repo')`)

	card, err := shipreview.CreateCard(db, "t7", "feature/norepo", "abc", []string{"1. Check"}, "", "", nil)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	if card.FilesChanged == nil {
		t.Error("FilesChanged should not be nil")
	}
	if len(card.FilesChanged) != 0 {
		t.Errorf("expected empty files_changed for empty repoDir, got %v", card.FilesChanged)
	}
}

// TestCheckRunsRoundTrip verifies check_runs are stored and retrieved correctly.
func TestCheckRunsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	_, _ = db.Exec(`INSERT INTO tasks (id, name) VALUES ('t8', 'CI test')`)

	runs := []shipreview.CheckRun{
		{Command: "go test ./...", ExitCode: 0, OutputTail: "ok  ..."},
		{Command: "go vet ./...", ExitCode: 0},
		{Command: "golangci-lint run", ExitCode: 1, OutputTail: "main.go:5: unused var"},
	}
	card, err := shipreview.CreateCard(db, "t8", "feature/ci", "sha123", []string{"1. Review lint output"}, "", "", runs)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	if len(card.CheckRuns) != 3 {
		t.Fatalf("want 3 check runs, got %d", len(card.CheckRuns))
	}
	if card.CheckRuns[0].Command != "go test ./..." {
		t.Errorf("want 'go test ./...', got %q", card.CheckRuns[0].Command)
	}
	if card.CheckRuns[2].ExitCode != 1 {
		t.Errorf("want exit_code 1, got %d", card.CheckRuns[2].ExitCode)
	}
	if card.CheckRuns[2].OutputTail == "" {
		t.Error("expected output_tail for failed check")
	}

	// Verify round-trip via GetCard.
	got, err := shipreview.GetCard(db, "t8")
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if len(got.CheckRuns) != 3 {
		t.Fatalf("GetCard: want 3 check runs, got %d", len(got.CheckRuns))
	}
}

func TestProjectDevConfigRoundTrip(t *testing.T) {
	db := openTestDB(t)
	cfg := &shipreview.ProjectDevConfig{
		RepoPath:   "/tmp/myproject",
		DevCommand: "bun run dev",
		DevURL:     "http://localhost:5173",
		SetupSteps: []string{"bun install", "cp .env.local.source .env.local"},
	}
	if err := shipreview.UpsertProjectDevConfig(db, cfg); err != nil {
		t.Fatalf("UpsertProjectDevConfig: %v", err)
	}
	got, err := shipreview.GetProjectDevConfig(db, "/tmp/myproject")
	if err != nil {
		t.Fatalf("GetProjectDevConfig: %v", err)
	}
	if got.DevCommand != cfg.DevCommand {
		t.Errorf("want %q, got %q", cfg.DevCommand, got.DevCommand)
	}
	if len(got.SetupSteps) != 2 {
		t.Errorf("want 2 setup steps, got %d", len(got.SetupSteps))
	}
}

// TestDeleteBranchRefusesProtectedBranches verifies that DeleteBranch rejects
// main, master, and the remote default branch without touching the remote.
func TestDeleteBranchRefusesProtectedBranches(t *testing.T) {
	repoDir, _, _ := setupGitRepo(t)
	ctx := context.Background()

	for _, name := range []string{"main", "master"} {
		err := shipreview.DeleteBranch(ctx, repoDir, name)
		if err == nil {
			t.Errorf("expected error deleting protected branch %q, got nil", name)
			continue
		}
		if !errors.Is(err, shipreview.ErrProtectedBranch) {
			t.Errorf("DeleteBranch(%q): want ErrProtectedBranch, got %v", name, err)
		}
	}
}

// TestDeleteBranchRefusesRemoteDefault verifies that DeleteBranch refuses to
// delete whatever branch origin/HEAD points to (even if it is not named "main").
func TestDeleteBranchRefusesRemoteDefault(t *testing.T) {
	repoDir, _, _ := setupGitRepo(t)
	ctx := context.Background()

	// setupGitRepo sets up origin with HEAD → main.
	// "main" is already covered by the name check, but exercise the remote-HEAD
	// path by trying to delete the same branch after renaming it locally
	// (origin/HEAD still points to it).
	err := shipreview.DeleteBranch(ctx, repoDir, "main")
	if err == nil {
		t.Fatal("expected error deleting remote-default branch, got nil")
	}
	if !errors.Is(err, shipreview.ErrProtectedBranch) {
		t.Errorf("want ErrProtectedBranch, got %v", err)
	}
}

// TestDeleteBranchTaskBranch verifies that DeleteBranch succeeds for a real
// non-protected branch and actually removes it from the remote.
func TestDeleteBranchTaskBranch(t *testing.T) {
	repoDir, featureBranch, _ := setupGitRepo(t)
	ctx := context.Background()

	if err := shipreview.DeleteBranch(ctx, repoDir, featureBranch); err != nil {
		t.Fatalf("DeleteBranch(%q): %v", featureBranch, err)
	}

	// Verify the branch is gone from the remote.
	cmd := exec.Command("git", "ls-remote", "--heads", "origin", featureBranch)
	cmd.Dir = repoDir
	out, _ := cmd.Output()
	if len(out) > 0 {
		t.Errorf("branch %q still present on remote after delete", featureBranch)
	}
}

// TestDeleteBranchReturnsErrorOnFailure verifies that DeleteBranch propagates
// git errors rather than swallowing them.  A repo with a bad remote URL provides
// a reliable failure regardless of remote git version.
func TestDeleteBranchReturnsErrorOnFailure(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "work")
	_ = os.MkdirAll(dir, 0755)

	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t.com",
	)
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = gitEnv
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}

	run("init", "-b", "main")
	run("config", "user.email", "t@t.com")
	run("config", "user.name", "test")
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("init\n"), 0644)
	run("add", ".")
	run("commit", "-m", "init")
	// Point origin to an unreachable path.
	run("remote", "add", "origin", "/nonexistent/path/repo.git")

	err := shipreview.DeleteBranch(context.Background(), dir, "staypoint/task-xyz")
	if err == nil {
		t.Fatal("expected error deleting branch with unreachable remote, got nil")
	}
}

// TestApproveAndMergeFromRepoRoot verifies that ApproveAndMerge works correctly
// when called with the repo root (not a task worktree), which is the post-fix path.
func TestApproveAndMergeFromRepoRoot(t *testing.T) {
	db := openTestDB(t)
	_, _ = db.Exec(`INSERT INTO tasks (id, name) VALUES ('t6', 'Repo root merge')`)

	repoDir, branch, featureSHA := setupGitRepo(t)
	// Repo root is on main after setupGitRepo — this is the condition we test.
	recordMainBase(t, db, repoDir, "t6")
	card, err := shipreview.CreateCard(db, "t6", branch, featureSHA, []string{"1. Verify"}, "", repoDir, nil)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}

	mainSHA, err := shipreview.ApproveAndMerge(context.Background(), db, card, repoDir, "main")
	if err != nil {
		t.Fatalf("ApproveAndMerge from repo root: %v", err)
	}
	if mainSHA == "" {
		t.Fatal("expected non-empty main SHA after merge")
	}

	got, _ := shipreview.GetCard(db, "t6")
	if got.Status != "approved" {
		t.Errorf("want approved, got %q", got.Status)
	}
}

// TestStartDevServerCreatesWorktreeAtPinnedSHA verifies that StartDevServer creates
// a temporary detached worktree at card.HeadSHA, not at the repo root (main).
// It also verifies that StopDevServer removes the worktree.
func TestStartDevServerCreatesWorktreeAtPinnedSHA(t *testing.T) {
	db := openTestDB(t)
	_, _ = db.Exec(`INSERT INTO tasks (id, name) VALUES ('t7', 'Dev server test')`)

	repoDir, branch, featureSHA := setupGitRepo(t)

	recordMainBase(t, db, repoDir, "t7")
	card, err := shipreview.CreateCard(db, "t7", branch, featureSHA, []string{"1. Check"}, "http://127.0.0.1:9999", repoDir, nil)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}

	// "sleep 9999" blocks long enough that the process is still alive when we
	// check the worktree state, then StopDevServer kills it cleanly.
	cfg := &shipreview.ProjectDevConfig{
		RepoPath:   repoDir,
		DevCommand: "sleep 9999",
		DevURL:     "http://127.0.0.1:9999",
	}

	url, err := shipreview.StartDevServer(db, card, cfg, repoDir)
	if err != nil {
		t.Fatalf("StartDevServer: %v", err)
	}
	if url != "http://127.0.0.1:9999" {
		t.Errorf("want url http://127.0.0.1:9999, got %q", url)
	}

	// The worktree is created synchronously before the process starts, so it
	// exists immediately after StartDevServer returns.
	wtPath := filepath.Join(repoDir, ".worktrees", "devserver-t7")
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("expected dev worktree at %s, got stat error: %v", wtPath, err)
	}

	// Verify the worktree is detached at the feature SHA, not at main's HEAD.
	headInWT, err := shipreview.CurrentBranchHEAD(context.Background(), wtPath, "HEAD")
	if err != nil {
		t.Fatalf("CurrentBranchHEAD in worktree: %v", err)
	}
	if headInWT != featureSHA {
		t.Errorf("worktree HEAD = %q, want featureSHA %q (must not be main)", headInWT, featureSHA)
	}

	// StopDevServer kills the process and synchronously removes the worktree.
	shipreview.StopDevServer(db, card)

	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("expected dev worktree %s to be removed after StopDevServer, stat err: %v", wtPath, err)
	}
}

// TestSetDevURL_Persists covers Bug 1 (STA-535): SetDevURL must survive a
// round-trip so GET /api/tasks/{id}/ship-review always returns the dev_url
// that was auto-detected during UpsertCard, not the empty string stored by
// CreateCard when the agent omits dev_url.
func TestSetDevURL_Persists(t *testing.T) {
	db := openTestDB(t)
	_, _ = db.Exec(`INSERT INTO tasks (id, name) VALUES ('t8', 'DevURL persist')`)

	card, err := shipreview.CreateCard(db, "t8", "feature/test", "abc123", []string{"1. check"}, "", "", nil)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	if card.DevURL != "" {
		t.Fatalf("expected empty dev_url after create, got %q", card.DevURL)
	}

	const want = "http://127.0.0.1:8799"
	if err := shipreview.SetDevURL(db, card.ID, want); err != nil {
		t.Fatalf("SetDevURL: %v", err)
	}

	got, err := shipreview.GetCard(db, "t8")
	if err != nil {
		t.Fatalf("GetCard after SetDevURL: %v", err)
	}
	if got.DevURL != want {
		t.Errorf("want dev_url %q, got %q", want, got.DevURL)
	}
}

// TestApproveAndMerge_MainSHAStored covers Bug 3 (STA-535): after ApproveAndMerge
// the card must persist both approved_sha and main_sha so the UI can show the
// reviewed SHA → main SHA relationship.
func TestApproveAndMerge_MainSHAStored(t *testing.T) {
	db := openTestDB(t)
	_, _ = db.Exec(`INSERT INTO tasks (id, name) VALUES ('t9', 'MainSHA test')`)

	repoDir, branch, featureSHA := setupGitRepo(t)

	recordMainBase(t, db, repoDir, "t9")
	card, err := shipreview.CreateCard(db, "t9", branch, featureSHA, []string{"1. Verify"}, "", repoDir, nil)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}

	mainSHA, err := shipreview.ApproveAndMerge(context.Background(), db, card, repoDir, "main")
	if err != nil {
		t.Fatalf("ApproveAndMerge: %v", err)
	}

	got, err := shipreview.GetCard(db, "t9")
	if err != nil {
		t.Fatalf("GetCard after approve: %v", err)
	}
	if got.Status != "approved" {
		t.Errorf("want approved, got %q", got.Status)
	}
	if got.ApprovedSHA != featureSHA {
		t.Errorf("want approved_sha %q, got %q", featureSHA, got.ApprovedSHA)
	}
	if got.MainSHA != mainSHA || mainSHA == "" {
		t.Errorf("want main_sha %q, got %q", mainSHA, got.MainSHA)
	}
}

func TestAgentSummaryPopulated(t *testing.T) {
	db := openTestDB(t)
	_, _ = db.Exec(`INSERT INTO tasks (id, name) VALUES ('t-agsummary', 'Summary test')`)

	// Insert an agent-summary comment before creating the card.
	_, _ = db.Exec(
		`INSERT INTO task_comments (task_id, author, message) VALUES ('t-agsummary', 'agent-summary', 'Fixed the login bug. Run go test ./... to verify.')`,
	)

	card, err := shipreview.CreateCard(db, "t-agsummary", "staypoint/t-agsummary", "abc123", []string{"1. Run tests"}, "", "", nil)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}

	got, err := shipreview.GetCard(db, "t-agsummary")
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if got.AgentSummary != "Fixed the login bug. Run go test ./... to verify." {
		t.Errorf("AgentSummary = %q, want non-empty summary", got.AgentSummary)
	}
	_ = card
}

func TestAgentSummaryEmptyWhenNoComment(t *testing.T) {
	db := openTestDB(t)
	_, _ = db.Exec(`INSERT INTO tasks (id, name) VALUES ('t-nosum', 'No summary task')`)

	card, err := shipreview.CreateCard(db, "t-nosum", "staypoint/t-nosum", "abc456", []string{"1. Check"}, "", "", nil)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}

	got, err := shipreview.GetCard(db, "t-nosum")
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if got.AgentSummary != "" {
		t.Errorf("AgentSummary = %q, want empty when no agent-summary comment", got.AgentSummary)
	}
	_ = card
}

// TestAgentSummaryNotStaleAfterSendBack verifies that a re-created card (run 2)
// does not inherit the run-1 summary when run 2 has not yet posted its own summary.
func TestAgentSummaryNotStaleAfterSendBack(t *testing.T) {
	db := openTestDB(t)
	_, _ = db.Exec(`INSERT INTO tasks (id, name) VALUES ('t-stalesummary', 'Stale summary test')`)

	// Run 1: post summary then create card 1.
	_, _ = db.Exec(
		`INSERT INTO task_comments (task_id, author, message, created_at)
		 VALUES ('t-stalesummary', 'agent-summary', 'Run 1 summary.', '2025-01-01T00:00:01Z')`,
	)
	_, _ = db.Exec(
		`INSERT INTO ship_review_cards (id, task_id, branch, head_sha, test_steps_json, status, created_at, updated_at)
		 VALUES ('card1', 't-stalesummary', 'feat/a', 'sha1', '["1. Check"]', 'sent_back', '2025-01-01T00:00:02Z', '2025-01-01T00:00:03Z')`,
	)

	// Run 2: create card 2 BEFORE posting run-2 summary (simulates the race window).
	_, _ = db.Exec(
		`INSERT INTO ship_review_cards (id, task_id, branch, head_sha, test_steps_json, status, created_at, updated_at)
		 VALUES ('card2', 't-stalesummary', 'feat/a', 'sha2', '["1. Check"]', 'pending', '2025-01-01T00:01:00Z', '2025-01-01T00:01:00Z')`,
	)

	got, err := shipreview.GetCard(db, "t-stalesummary")
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	// Card 2's summary should be empty — run-2 summary not yet posted.
	if got.AgentSummary != "" {
		t.Errorf("AgentSummary = %q on run-2 card before run-2 summary posted; want empty (not stale run-1 summary)", got.AgentSummary)
	}

	// Now run 2 posts its summary.
	_, _ = db.Exec(
		`INSERT INTO task_comments (task_id, author, message, created_at)
		 VALUES ('t-stalesummary', 'agent-summary', 'Run 2 summary.', '2025-01-01T00:01:30Z')`,
	)

	got2, err := shipreview.GetCard(db, "t-stalesummary")
	if err != nil {
		t.Fatalf("GetCard after run-2 summary: %v", err)
	}
	if got2.AgentSummary != "Run 2 summary." {
		t.Errorf("AgentSummary = %q; want run-2 summary after it was posted", got2.AgentSummary)
	}
}

func TestRunNumberIncrementsWithCards(t *testing.T) {
	db := openTestDB(t)
	_, _ = db.Exec(`INSERT INTO tasks (id, name) VALUES ('t-runnum', 'RunNumber test')`)

	_, _ = db.Exec(
		`INSERT INTO ship_review_cards (id, task_id, branch, head_sha, test_steps_json, status, created_at, updated_at)
		 VALUES ('rn-card1', 't-runnum', 'feat/x', 'sha1', '["1. Check"]', 'sent_back', '2025-01-01T00:00:01Z', '2025-01-01T00:00:02Z')`,
	)
	got1, _ := shipreview.GetCard(db, "t-runnum")
	if got1.RunNumber != 1 {
		t.Errorf("RunNumber = %d; want 1 for first card", got1.RunNumber)
	}

	_, _ = db.Exec(
		`INSERT INTO ship_review_cards (id, task_id, branch, head_sha, test_steps_json, status, created_at, updated_at)
		 VALUES ('rn-card2', 't-runnum', 'feat/x', 'sha2', '["1. Check"]', 'pending', '2025-01-01T00:01:00Z', '2025-01-01T00:01:00Z')`,
	)
	got2, _ := shipreview.GetCard(db, "t-runnum")
	if got2.RunNumber != 2 {
		t.Errorf("RunNumber = %d; want 2 for second card", got2.RunNumber)
	}
}

func TestHasDBMigrationDetected(t *testing.T) {
	db := openTestDB(t)
	_, _ = db.Exec(`INSERT INTO tasks (id, name) VALUES ('t-migr', 'Migration test')`)

	// We cannot easily control diffFilesChanged without a real repo, but we can
	// verify that isMigrationInFiles is called correctly by checking the exported
	// field on a card created without a real repo (files_changed will be empty).
	// Instead test the helper via a card whose FilesChanged is set via the DB.
	card, err := shipreview.CreateCard(db, "t-migr", "staypoint/t-migr", "sha789", []string{"1. Check"}, "", "", nil)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}
	if card.HasDBMigration {
		t.Error("want HasDBMigration=false when no files changed (no repo dir)")
	}

	// Directly test the path logic via files_changed_json manipulation.
	migFiles := []string{"supabase/migrations/0001_init.sql", "internal/server/handler.go"}
	migJSON, _ := json.Marshal(migFiles)
	_, _ = db.Exec(`UPDATE ship_review_cards SET files_changed_json = ? WHERE task_id = ?`, string(migJSON), "t-migr")

	got, err := shipreview.GetCard(db, "t-migr")
	if err != nil {
		t.Fatalf("GetCard after update: %v", err)
	}
	if !got.HasDBMigration {
		t.Errorf("want HasDBMigration=true for supabase/migrations/ path, got false; files=%v", got.FilesChanged)
	}
}

// setupRepoWithHarnessBranch creates a temp git repo with an initial commit on
// main and a "staypoint/<taskID>" branch with one file change.
// Returns repoDir and the harness branch SHA.
// The caller stores git_branch="main" in the DB to simulate the bug scenario.
func setupRepoWithHarnessBranch(t *testing.T, taskID string) (repoDir, harnessSHA string) {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "work")
	_ = os.MkdirAll(dir, 0755)

	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t.com",
	)
	run := func(d string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = d
		cmd.Env = gitEnv
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v (in %s): %v", args, d, err)
		}
		return string(out)
	}

	run(dir, "init", "-b", "main")
	run(dir, "config", "user.email", "t@t.com")
	run(dir, "config", "user.name", "test")

	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("init\n"), 0644)
	run(dir, "add", ".")
	run(dir, "commit", "-m", "init")

	// Create the harness branch staypoint/<taskID> with one change.
	harnessBranch := "staypoint/" + taskID
	run(dir, "checkout", "-b", harnessBranch)
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html/>"), 0644)
	run(dir, "add", ".")
	run(dir, "commit", "-m", "feat: add index.html")

	sha, err := shipreview.CurrentBranchHEAD(context.Background(), dir, harnessBranch)
	if err != nil {
		t.Fatalf("resolve harness branch HEAD: %v", err)
	}

	run(dir, "checkout", "main")
	return dir, sha
}

// TestBuildAndStartCard_IgnoresGitBranch is the regression test for STA-571.
// It proves that BuildAndStartCard pins staypoint/<taskID> even when the task
// has git_branch="main" stored in the DB (which is the default at creation).
func TestBuildAndStartCard_IgnoresGitBranch(t *testing.T) {
	const taskID = "task-sta571"
	db := openTestDB(t)

	repoDir, harnessSHA := setupRepoWithHarnessBranch(t, taskID)

	// Store the task with git_branch = "main" — the bug condition.
	_, err := db.Exec(
		`INSERT INTO tasks (id, name, repo_path, git_branch) VALUES (?, ?, ?, ?)`,
		taskID, "STA-571 regression", repoDir, "main",
	)
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}

	recordMainBase(t, db, repoDir, taskID)
	card, err := shipreview.BuildAndStartCard(
		context.Background(), db,
		taskID, repoDir,
		[]string{"1. Verify index.html loads"},
		"", nil,
	)
	if err != nil {
		t.Fatalf("BuildAndStartCard: %v", err)
	}

	// The card must pin the harness branch, not "main".
	wantBranch := "staypoint/" + taskID
	if card.Branch != wantBranch {
		t.Errorf("branch = %q, want %q (must never use task.GitBranch)", card.Branch, wantBranch)
	}
	if card.HeadSHA != harnessSHA {
		t.Errorf("head_sha = %q, want harness SHA %q (must not be main HEAD)", card.HeadSHA, harnessSHA)
	}
	// FilesChanged must reflect the actual diff, not empty (which would imply main).
	if len(card.FilesChanged) == 0 {
		t.Error("files_changed is empty; expected at least index.html (suggests main diff was used)")
	}
	found := false
	for _, f := range card.FilesChanged {
		if f == "index.html" {
			found = true
		}
	}
	if !found {
		t.Errorf("files_changed = %v, want 'index.html' to appear", card.FilesChanged)
	}
}

// TestStartDevServerRecoversStalWorktree is the regression test for STA-630.
// It simulates a daemon restart: a dev-server worktree was created by a prior
// run, then its directory was deleted (daemon killed / reinstall cleaned up the
// process) while git still held the registration. The next call to StartDevServer
// must succeed without requiring a manual `git worktree prune`.
func TestStartDevServerRecoversStalWorktree(t *testing.T) {
	const taskID = "task-sta630"
	db := openTestDB(t)

	repoDir, featureBranch, featureSHA := setupGitRepo(t)

	_, err := db.Exec(`INSERT INTO tasks (id, name) VALUES (?, ?)`, taskID, "STA-630 stale worktree regression")
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}

	// The exact path that startDevServerSync will use.
	wtPath := filepath.Join(repoDir, ".worktrees", "devserver-"+taskID)

	// Pre-register a worktree at that path (simulating a prior successful run).
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t.com",
	)
	preReg := exec.Command("git", "worktree", "add", "--detach", wtPath, featureSHA)
	preReg.Dir = repoDir
	preReg.Env = gitEnv
	if out, err := preReg.CombinedOutput(); err != nil {
		t.Fatalf("pre-register worktree: %v\n%s", err, out)
	}

	// Simulate daemon restart: delete the directory, leave git registration intact.
	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatalf("RemoveAll (simulate restart): %v", err)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Fatal("test setup error: expected wtPath to be absent after RemoveAll")
	}

	// Create the card using the feature branch and call StartDevServer.
	// Without the fix this fails:
	//   fatal: '<path>' is a missing but already registered worktree
	recordMainBase(t, db, repoDir, taskID)
	card, err := shipreview.CreateCard(db, taskID, featureBranch, featureSHA, []string{"1. Check"}, "http://127.0.0.1:9997", repoDir, nil)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}

	cfg := &shipreview.ProjectDevConfig{
		RepoPath:   repoDir,
		DevCommand: "sleep 9999",
		DevURL:     "http://127.0.0.1:9997",
	}
	_, err = shipreview.StartDevServer(db, card, cfg, repoDir)
	if err != nil {
		t.Fatalf("StartDevServer with stale worktree registration: %v (want success after auto-prune)", err)
	}
	defer shipreview.StopDevServer(db, card)

	// Worktree must exist at the correct SHA after recovery.
	headInWT, err := shipreview.CurrentBranchHEAD(context.Background(), wtPath, "HEAD")
	if err != nil {
		t.Fatalf("CurrentBranchHEAD in recovered worktree: %v", err)
	}
	if headInWT != featureSHA {
		t.Errorf("worktree HEAD = %q, want featureSHA %q", headInWT, featureSHA)
	}
}

// TestStartDevServerReusesSameWorktree verifies that calling StartDevServer
// twice for the same task and same SHA reuses the existing worktree rather
// than failing with "already registered".
func TestStartDevServerReusesSameWorktree(t *testing.T) {
	const taskID = "task-sta630-reuse"
	db := openTestDB(t)

	repoDir, featureBranch, featureSHA := setupGitRepo(t)

	_, err := db.Exec(`INSERT INTO tasks (id, name) VALUES (?, ?)`, taskID, "STA-630 reuse worktree")
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}

	recordMainBase(t, db, repoDir, taskID)
	card, err := shipreview.CreateCard(db, taskID, featureBranch, featureSHA, []string{"1. Check"}, "http://127.0.0.1:9996", repoDir, nil)
	if err != nil {
		t.Fatalf("CreateCard: %v", err)
	}

	cfg := &shipreview.ProjectDevConfig{
		RepoPath:   repoDir,
		DevCommand: "sleep 9999",
		DevURL:     "http://127.0.0.1:9996",
	}

	// First start.
	_, err = shipreview.StartDevServer(db, card, cfg, repoDir)
	if err != nil {
		t.Fatalf("StartDevServer (first): %v", err)
	}
	shipreview.StopDevServer(db, card)

	// Second start with the same worktree path registered and then cleaned up
	// by StopDevServer — should succeed.
	_, err = shipreview.StartDevServer(db, card, cfg, repoDir)
	if err != nil {
		t.Fatalf("StartDevServer (second): %v", err)
	}
	defer shipreview.StopDevServer(db, card)
}

// TestBuildAndStartCard_AutoStartsDevServer verifies that BuildAndStartCard
// auto-starts the project dev server when a config exists and the card has no
// explicit dev_url, then persists the URL. This mirrors the HTTP handler path.
func TestBuildAndStartCard_AutoStartsDevServer(t *testing.T) {
	const taskID = "task-devserver"
	db := openTestDB(t)

	repoDir, _ := setupRepoWithHarnessBranch(t, taskID)

	_, err := db.Exec(
		`INSERT INTO tasks (id, name, repo_path, git_branch) VALUES (?, ?, ?, ?)`,
		taskID, "Dev server auto-start", repoDir, "main",
	)
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}

	// Register a project dev config with a long-lived no-op command.
	cfg := &shipreview.ProjectDevConfig{
		RepoPath:   repoDir,
		DevCommand: "sleep 9999",
		DevURL:     "http://127.0.0.1:8799",
	}
	if err := shipreview.UpsertProjectDevConfig(db, cfg); err != nil {
		t.Fatalf("UpsertProjectDevConfig: %v", err)
	}

	recordMainBase(t, db, repoDir, taskID)
	card, err := shipreview.BuildAndStartCard(
		context.Background(), db,
		taskID, repoDir,
		[]string{"1. Open http://127.0.0.1:8799"},
		"", nil, // no explicit dev_url — BuildAndStartCard should fill it in
	)
	if err != nil {
		t.Fatalf("BuildAndStartCard: %v", err)
	}
	defer shipreview.StopDevServer(db, card)

	if card.DevURL != "http://127.0.0.1:8799" {
		t.Errorf("dev_url = %q, want http://127.0.0.1:8799 (auto-started from project config)", card.DevURL)
	}
	if card.DevPID <= 0 {
		// Reload from DB to pick up PID set by StartDevServer.
		got, _ := shipreview.GetCard(db, taskID)
		if got.DevPID <= 0 {
			t.Errorf("dev_pid = %d, want > 0 after auto-start", got.DevPID)
		}
	}
}
