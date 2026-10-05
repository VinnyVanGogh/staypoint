package server_test

// STA-689: after STA-685 the Approve migration gate failed closed only when git
// timed out. A repo the daemon is refused (the user denied the macOS privacy
// prompt) makes git fail fast with EPERM, and the gate read that as "no
// migrations". These tests swap in a `git` that fails that way and check the
// gate blocks, while the cases that really mean "nothing to check" (no
// checkpoint yet, no task branch, not a git repo) still pass through.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

const deniedMsg = "fatal: cannot open '.git/HEAD': Operation not permitted"

// deniedGit puts a `git` first on PATH that fails the way git does when macOS
// refuses it the repo folder.
func deniedGit(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"" + deniedMsg + "\" >&2\nexit 128\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func getCardMap(t *testing.T, baseURL, token, taskID string) map[string]any {
	t.Helper()
	resp, body, _ := timedReq(t, token, "GET", baseURL+"/api/tasks/"+taskID+"/ship-review")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET ship-review: %d %s", resp.StatusCode, body)
	}
	var card map[string]any
	if err := json.Unmarshal(body, &card); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	return card
}

func TestGitDenied_ApproveFailsClosedAtGate(t *testing.T) {
	database, baseURL, token, boardToken, taskID, _, _ := shipApproveServer(t)
	deniedGit(t)

	resp, body, _ := timedReq(t, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/approve", boardToken, "", "mock-assertion")
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("Approve succeeded while git was refused the repo: %s", body)
	}
	// The migration gate must be what stops it, not the merge step failing later.
	if !strings.Contains(string(body), "could not check migration verification status") {
		t.Errorf("Approve body %s: want the migration gate to refuse", body)
	}
	if !strings.Contains(string(body), "Operation not permitted") {
		t.Errorf("Approve body %s does not carry git's error", body)
	}
	card, err := shipreview.GetCard(database, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if card.Status != shipreview.StatusPending {
		t.Errorf("card status = %s, want pending", card.Status)
	}
}

func TestGitDenied_GetCardReportsRepoError(t *testing.T) {
	_, baseURL, token, _, taskID, _, _ := shipApproveServer(t)
	deniedGit(t)

	card := getCardMap(t, baseURL, token, taskID)
	if msg, _ := card["repo_error"].(string); !strings.Contains(msg, "Operation not permitted") {
		t.Errorf("repo_error = %q, want git's EPERM error", msg)
	}
}

// The shipApproveServer repo has a task branch but no checkpoint ref: there is
// no baseline to diff against, so there is nothing to check.
func TestMigrationGate_NoCheckpointIsNotAnError(t *testing.T) {
	_, baseURL, token, _, taskID, _, _ := shipApproveServer(t)

	card := getCardMap(t, baseURL, token, taskID)
	if v, ok := card["repo_error"]; ok {
		t.Errorf("repo_error = %v, want none when the repo has no checkpoint", v)
	}
}

func TestMigrationGate_MissingTaskBranchIsNotAnError(t *testing.T) {
	_, baseURL, token, _, taskID, repoDir, _ := shipApproveServer(t)
	gitOut(t, repoDir, "update-ref", "refs/staypoint/checkpoints/latest", "main")
	gitOut(t, repoDir, "branch", "-D", "staypoint/"+taskID)

	card := getCardMap(t, baseURL, token, taskID)
	if v, ok := card["repo_error"]; ok {
		t.Errorf("repo_error = %v, want none when the task branch does not exist", v)
	}
}

func TestMigrationGate_NotARepoIsNotAnError(t *testing.T) {
	database, baseURL, token, _, taskID, _, _ := shipApproveServer(t)
	plain := t.TempDir()
	if _, err := database.Exec(`UPDATE tasks SET repo_path = ? WHERE id = ?`, plain, taskID); err != nil {
		t.Fatal(err)
	}

	card := getCardMap(t, baseURL, token, taskID)
	if v, ok := card["repo_error"]; ok {
		t.Errorf("repo_error = %v, want none when repo_path is not a git repo", v)
	}
}

// A migration in the diff with the checkpoint baseline present is still
// detected, so the benign-case handling did not turn the gate off.
func TestMigrationGate_DetectsMigrationAgainstCheckpoint(t *testing.T) {
	_, baseURL, token, boardToken, taskID, _ := addMigrationOnTaskBranch(t)

	card := getCardMap(t, baseURL, token, taskID)
	if v, ok := card["repo_error"]; ok {
		t.Fatalf("repo_error = %v, want none", v)
	}
	got, _ := card["unverified_migrations"].([]any)
	if len(got) != 1 || got[0] != "migrations/001_add.sql" {
		t.Errorf("unverified_migrations = %v, want [migrations/001_add.sql]", card["unverified_migrations"])
	}

	resp, body, _ := timedReq(t, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/approve", boardToken, "", "mock-assertion")
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "unverified_migrations") {
		t.Errorf("Approve: %d %s, want 409 unverified_migrations", resp.StatusCode, body)
	}
}

// repo_error is for an unreadable repo. A failure reading the activity log is
// a database problem and must not be reported as one.
func TestMigrationGate_DBErrorIsNotRepoError(t *testing.T) {
	database, baseURL, token, _, taskID, _ := addMigrationOnTaskBranch(t)
	if _, err := database.Exec(`ALTER TABLE activity_log RENAME TO activity_log_gone`); err != nil {
		t.Fatal(err)
	}

	card := getCardMap(t, baseURL, token, taskID)
	if v, ok := card["repo_error"]; ok {
		t.Errorf("repo_error = %v, want none for a database error", v)
	}
	if msg, _ := card["migration_check_error"].(string); msg == "" {
		t.Errorf("migration_check_error missing; card = %v", card)
	}
}

// addMigrationOnTaskBranch sets a checkpoint at main and commits a migration
// on the task branch, so the gate has something to find.
func addMigrationOnTaskBranch(t *testing.T) (database *sql.DB, baseURL, token, boardToken, taskID, repoDir string) {
	t.Helper()
	database, baseURL, token, boardToken, taskID, repoDir, _ = shipApproveServer(t)
	gitOut(t, repoDir, "update-ref", "refs/staypoint/checkpoints/latest", "main")
	gitOut(t, repoDir, "checkout", "staypoint/"+taskID)
	if err := os.MkdirAll(filepath.Join(repoDir, "migrations"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "migrations", "001_add.sql"), []byte("ALTER TABLE t ADD COLUMN c TEXT;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repoDir, "add", ".")
	gitOut(t, repoDir, "-c", "user.name=test", "-c", "user.email=t@t.com", "commit", "-m", "task: add migration")
	gitOut(t, repoDir, "checkout", "main")
	return database, baseURL, token, boardToken, taskID, repoDir
}
