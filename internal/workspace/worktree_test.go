package workspace

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func setupTestGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s failed: %v\nOutput: %s", strings.Join(args, " "), err, string(out))
		}
	}

	run("init")
	run("config", "user.email", "test@staypoint.dev")
	run("config", "user.name", "StayPoint Test")

	fileA := filepath.Join(dir, "README.md")
	if err := os.WriteFile(fileA, []byte("# StayPoint Test Repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "initial commit")

	return dir
}

func TestNewWorktreeManager(t *testing.T) {
	repoRoot := "/tmp/test-repo"
	wm := NewWorktreeManager(repoRoot, nil)
	if wm.RepoRoot != repoRoot {
		t.Errorf("RepoRoot = %q, want %q", wm.RepoRoot, repoRoot)
	}
	if wm.DB != nil {
		t.Errorf("DB = %v, want nil", wm.DB)
	}
}

func TestWorktreeManager_CreateAndPrune(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	wm := NewWorktreeManager(repoDir, nil)

	taskID := "task-test-01"
	wtPath, err := wm.Create(taskID, "sess-01")
	if err != nil {
		t.Fatalf("wm.Create failed: %v", err)
	}

	expectedPath := filepath.Join(repoDir, ".worktrees", taskID)
	if wtPath != expectedPath {
		t.Errorf("wtPath = %q, want %q", wtPath, expectedPath)
	}

	if info, err := os.Stat(wtPath); err != nil || !info.IsDir() {
		t.Fatalf("worktree path %q does not exist as dir: %v", wtPath, err)
	}

	// Verify git branch exists
	cmd := exec.Command("git", "branch", "--list", "staypoint/"+taskID)
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "staypoint/"+taskID) {
		t.Fatalf("branch staypoint/%s was not created: %v, out: %s", taskID, err, string(out))
	}

	// Prune
	if err := wm.Prune(taskID); err != nil {
		t.Fatalf("wm.Prune failed: %v", err)
	}

	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("worktree path %q still exists after Prune", wtPath)
	}

	// Verify git branch was deleted
	cmd = exec.Command("git", "branch", "--list", "staypoint/"+taskID)
	cmd.Dir = repoDir
	out, err = cmd.CombinedOutput()
	if strings.Contains(string(out), "staypoint/"+taskID) {
		t.Errorf("branch staypoint/%s still exists after Prune: %s", taskID, string(out))
	}
}

func TestWorktreeManager_Create_BranchAlreadyExists(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	wm := NewWorktreeManager(repoDir, nil)

	taskID := "task-existing-branch"
	branch := "staypoint/" + taskID

	// Pre-create branch
	cmd := exec.Command("git", "branch", branch)
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to pre-create branch: %v, out: %s", err, string(out))
	}

	// Create should succeed by falling back to existing branch
	wtPath, err := wm.Create(taskID, "sess-02")
	if err != nil {
		t.Fatalf("wm.Create failed when branch already exists: %v", err)
	}

	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("worktree dir not found: %v", err)
	}

	// Clean up
	if err := wm.Prune(taskID); err != nil {
		t.Fatalf("wm.Prune failed: %v", err)
	}
}

func TestWorktreeManager_Create_StaleWorktreeCleanup(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	wm := NewWorktreeManager(repoDir, nil)

	taskID := "task-stale"
	// Create once
	wtPath, err := wm.Create(taskID, "sess-stale-1")
	if err != nil {
		t.Fatalf("first wm.Create failed: %v", err)
	}

	// Create again without explicit prune (simulates crash / stale worktree)
	wtPath2, err := wm.Create(taskID, "sess-stale-2")
	if err != nil {
		t.Fatalf("second wm.Create (with stale worktree) failed: %v", err)
	}
	if wtPath2 != wtPath {
		t.Errorf("wtPath2 = %q, want %q", wtPath2, wtPath)
	}

	// Clean up
	if err := wm.Prune(taskID); err != nil {
		t.Fatalf("wm.Prune failed: %v", err)
	}
}

func TestWorktreeManager_Prune_NonExistent(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	wm := NewWorktreeManager(repoDir, nil)

	if err := wm.Prune("task-ghost-none"); err != nil {
		t.Errorf("wm.Prune on non-existent worktree returned error: %v", err)
	}
}

func TestWorktreeManager_SweepOrphans_NilDB(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	wm := NewWorktreeManager(repoDir, nil)

	wtA, err := wm.Create("task-sweep-a", "sess-a")
	if err != nil {
		t.Fatalf("Create task-sweep-a: %v", err)
	}
	wtB, err := wm.Create("task-sweep-b", "sess-b")
	if err != nil {
		t.Fatalf("Create task-sweep-b: %v", err)
	}

	if err := wm.SweepOrphans(); err != nil {
		t.Fatalf("SweepOrphans failed: %v", err)
	}

	if _, err := os.Stat(wtA); !os.IsNotExist(err) {
		t.Errorf("expected %q to be pruned", wtA)
	}
	if _, err := os.Stat(wtB); !os.IsNotExist(err) {
		t.Errorf("expected %q to be pruned", wtB)
	}
}

func TestWorktreeManager_SweepOrphans_WithDB(t *testing.T) {
	repoDir := setupTestGitRepo(t)

	dbFile := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	schema := `
	CREATE TABLE agent_sessions (
		id TEXT PRIMARY KEY,
		status TEXT NOT NULL
	);
	CREATE TABLE agent_working_files (
		session_id TEXT NOT NULL,
		file_path TEXT NOT NULL
	);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create tables: %v", err)
	}

	wm := NewWorktreeManager(repoDir, db)

	wtActive, err := wm.Create("task-active", "sess-act")
	if err != nil {
		t.Fatalf("Create task-active: %v", err)
	}
	wtInactive, err := wm.Create("task-inactive", "sess-inact")
	if err != nil {
		t.Fatalf("Create task-inactive: %v", err)
	}

	// Insert active session for task-active
	relPathActive := filepath.Join(".worktrees", "task-active")
	if _, err := db.Exec("INSERT INTO agent_sessions (id, status) VALUES ('s1', 'active')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO agent_working_files (session_id, file_path) VALUES ('s1', ?)", relPathActive); err != nil {
		t.Fatal(err)
	}

	// Insert inactive session for task-inactive
	relPathInactive := filepath.Join(".worktrees", "task-inactive")
	if _, err := db.Exec("INSERT INTO agent_sessions (id, status) VALUES ('s2', 'completed')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO agent_working_files (session_id, file_path) VALUES ('s2', ?)", relPathInactive); err != nil {
		t.Fatal(err)
	}

	if err := wm.SweepOrphans(); err != nil {
		t.Fatalf("SweepOrphans failed: %v", err)
	}

	// task-active should still exist
	if _, err := os.Stat(wtActive); err != nil {
		t.Errorf("expected active worktree %q to still exist, but got: %v", wtActive, err)
	}
	// task-inactive should have been pruned
	if _, err := os.Stat(wtInactive); !os.IsNotExist(err) {
		t.Errorf("expected inactive worktree %q to be pruned", wtInactive)
	}

	// Clean up active one
	_ = wm.Prune("task-active")
}

func TestWorktreeManager_SweepOrphans_NoWorktreesDir(t *testing.T) {
	emptyDir := t.TempDir()
	wm := NewWorktreeManager(emptyDir, nil)

	if err := wm.SweepOrphans(); err != nil {
		t.Errorf("SweepOrphans returned error on missing .worktrees: %v", err)
	}
}

// TestWorktreeManager_Create_StaleWorktreeKeepsBranch: recovering a stale
// worktree must not delete or reset the task branch, which holds committed
// work and backs open ship-review previews (STA-637).
func TestWorktreeManager_Create_StaleWorktreeKeepsBranch(t *testing.T) {
	repoDir := setupTestGitRepo(t)
	wm := NewWorktreeManager(repoDir, nil)
	taskID := "task-stale-keep"

	wtPath, err := wm.Create(taskID, "sess-1")
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.WriteFile(filepath.Join(wtPath, "work.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(wtPath, "add", ".")
	git(wtPath, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "task work")
	tip := git(repoDir, "rev-parse", "staypoint/"+taskID)

	// Worktree left behind (crash) → Create again.
	if _, err := wm.Create(taskID, "sess-2"); err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if got := git(repoDir, "rev-parse", "staypoint/"+taskID); got != tip {
		t.Errorf("branch tip = %s after stale recovery, want %s (committed work lost)", got, tip)
	}
	_ = wm.Prune(taskID)
}
