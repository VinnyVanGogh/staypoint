package server_test

// STA-535: regression tests for ship-review handler fixes.
//
// Bug 1: dev_url was set in-memory by UpsertCard but not persisted; GET
//        returned "" even after StartDevServer set it on the card struct.
// Bug 4: MarkTaskDone requires a task_work_products row; Approve silently
//        discarded the error.  Fix: AddWorkProduct(commit,mainSHA) before
//        MarkTaskDone.

import (
	"bytes"
	gocontext "context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
	"github.com/VinnyVanGogh/staypoint/internal/workspace"
)

// ── helpers ────────────────────────────────────────────────────────────────

// shipDoReq sends an authenticated request. Variadic opts:
//   opts[0] = boardToken (staypoint_board cookie value)
//   opts[1] = X-WebAuthn-Session header value
//   opts[2] = X-WebAuthn-Assertion header value
func shipDoReq(t *testing.T, client *http.Client, token, method, urlStr string, body []byte, opts ...string) (*http.Response, []byte) {
	t.Helper()
	var br io.Reader
	if body != nil {
		br = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, urlStr, br)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if len(opts) > 0 && opts[0] != "" {
		req.AddCookie(&http.Cookie{Name: "staypoint_board", Value: opts[0]})
	}
	if len(opts) > 1 && opts[1] != "" {
		req.Header.Set("X-WebAuthn-Session", opts[1])
	}
	if len(opts) > 2 && opts[2] != "" {
		req.Header.Set("X-WebAuthn-Assertion", opts[2])
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// createShipTask inserts a task with a real git repo so UpsertCard can resolve HEAD.
// UpsertCard (post-STA-533) always resolves branch "staypoint/<taskID>", so after
// creating the task we create that branch in the repo and push it.
// Returns taskID and repoDir.
func createShipTask(t *testing.T, database *sql.DB, baseURL, token string, client *http.Client) (taskID, repoDir string) {
	t.Helper()

	// Set up a minimal bare + work git repo.
	base := t.TempDir()
	work := filepath.Join(base, "work")
	_ = os.MkdirAll(work, 0755)
	bare := filepath.Join(base, "bare.git")

	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t.com",
	)
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = gitEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(work, "init", "-b", "main")
	run(work, "config", "user.email", "t@t.com")
	run(work, "config", "user.name", "test")
	_ = os.WriteFile(filepath.Join(work, "README.md"), []byte("init\n"), 0644)
	run(work, "add", ".")
	run(work, "commit", "-m", "init")

	// Feature commit on main (we'll rename the branch after we know the task ID).
	_ = os.WriteFile(filepath.Join(work, "feat.txt"), []byte("feature\n"), 0644)
	run(work, "add", ".")
	run(work, "commit", "-m", "feat: ship test")

	// Bare remote
	cmd := exec.Command("git", "init", "--bare", "-b", "main", bare)
	cmd.Env = gitEnv
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bare init: %v\n%s", err, out)
	}
	run(work, "remote", "add", "origin", bare)
	run(work, "push", "origin", "main")

	// Create task via API pointing at work repo.
	taskBody, _ := json.Marshal(map[string]any{
		"name":         fmt.Sprintf("ship-test-%d", len(token)),
		"repo_path":    work,
		"git_branch":   "main",
		"organization": "STA",
		"project":      "ship-review-test",
	})
	resp, rb := shipDoReq(t, client, token, "POST", baseURL+"/api/tasks", taskBody)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("create task: %d %s", resp.StatusCode, rb)
	}
	var taskResp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rb, &taskResp)
	if taskResp.ID == "" {
		t.Fatalf("no task id in response: %s", rb)
	}

	// UpsertCard always resolves "staypoint/<taskID>" — create that branch now.
	branch := "staypoint/" + taskResp.ID
	// The daemon records the task's base when it creates the worktree
	// (STA-774); cards and Approve are measured from it.
	mainTip := gitOut(t, work, "rev-parse", "main")
	if err := workspace.RecordTaskBase(gocontext.Background(), database, work, taskResp.ID, mainTip); err != nil {
		t.Fatalf("RecordTaskBase: %v", err)
	}
	run(work, "checkout", "-b", branch)
	_ = os.WriteFile(filepath.Join(work, "task.txt"), []byte("task work\n"), 0644)
	run(work, "add", ".")
	run(work, "commit", "-m", "task: add work file")
	run(work, "push", "origin", branch)
	// Return to main so the repo root is on main (ApproveAndMerge merges into main).
	run(work, "checkout", "main")

	return taskResp.ID, work
}

// ── Bug 1: dev_url persistence ─────────────────────────────────────────────

// TestShipReview_DevURLPersistedAfterUpsert confirms that a dev_url supplied
// in the PUT body survives a subsequent GET (was lost before STA-535 fix).
func TestShipReview_DevURLPersistedAfterUpsert(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	baseURL := srv.URL()
	boardToken := srv.BoardToken()
	_ = boardToken
	client := &http.Client{}

	taskID, _ := createShipTask(t, database, baseURL, token, client)

	// Upsert card WITH an explicit dev_url.
	upsertBody, _ := json.Marshal(map[string]any{
		"test_steps": []string{"1. Open /"},
		"dev_url":    "http://127.0.0.1:8799",
	})
	resp, rb := shipDoReq(t, client, token, "PUT", baseURL+"/api/tasks/"+taskID+"/ship-review", upsertBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("UpsertCard: %d %s", resp.StatusCode, rb)
	}

	// GET must return the same dev_url.
	resp2, rb2 := shipDoReq(t, client, token, "GET", baseURL+"/api/tasks/"+taskID+"/ship-review", nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GetCard: %d %s", resp2.StatusCode, rb2)
	}
	var card struct {
		DevURL string `json:"dev_url"`
	}
	if err := json.Unmarshal(rb2, &card); err != nil {
		t.Fatalf("unmarshal card: %v", err)
	}
	if card.DevURL != "http://127.0.0.1:8799" {
		t.Errorf("want dev_url http://127.0.0.1:8799, got %q", card.DevURL)
	}
}

// ── Bug 2: head_moved 409 body accessible ─────────────────────────────────

// TestShipReview_ApproveReturns409WithBodyOnMovedHead confirms the handler
// returns a parseable JSON body on 409 so the frontend can detect head_moved.
func TestShipReview_ApproveReturns409WithBodyOnMovedHead(t *testing.T) {
	database := setupTestDB(t)
	seedBoardWebAuthnCredential(t, database)
	srv, token := startTestServer(t, database)
	setter, ok := any(srv).(webAuthnVerifierSetter)
	if !ok {
		t.Fatal("*server.Server must implement SetWebAuthnVerifier")
	}
	setter.SetWebAuthnVerifier(func(_ *http.Request, _ string) error { return nil })
	baseURL := srv.URL()
	boardToken := srv.BoardToken()
	client := &http.Client{}

	taskID, repoDir := createShipTask(t, database, baseURL, token, client)

	// Upsert card — get the real HEAD pinned.
	upsertBody, _ := json.Marshal(map[string]any{
		"test_steps": []string{"1. Open /"},
	})
	resp, rb := shipDoReq(t, client, token, "PUT", baseURL+"/api/tasks/"+taskID+"/ship-review", upsertBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("UpsertCard: %d %s", resp.StatusCode, rb)
	}

	// Push another commit on the branch to move HEAD past the pinned SHA.
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@t.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@t.com",
	)
	_ = os.WriteFile(filepath.Join(repoDir, "extra.txt"), []byte("extra\n"), 0644)
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = gitEnv
		_ = cmd.Run()
	}
	run("checkout", "staypoint/"+taskID)
	run("add", ".")
	run("commit", "-m", "extra commit after pin")
	run("push", "origin", "staypoint/"+taskID)
	run("checkout", "main")

	// Approve should 409 because HEAD moved past pinned SHA.
	resp3, rb3 := shipDoReq(t, client, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/approve", nil, boardToken, "", "mock-assertion")
	if resp3.StatusCode != http.StatusConflict {
		t.Fatalf("want 409 on moved head, got %d: %s", resp3.StatusCode, rb3)
	}
	var errBody struct {
		Error      string `json:"error"`
		NewHeadSHA string `json:"new_head_sha"`
	}
	if err := json.Unmarshal(rb3, &errBody); err != nil {
		t.Fatalf("want parseable JSON on 409, got: %s", rb3)
	}
	if errBody.Error != "head_moved" {
		t.Errorf("want error=head_moved, got %q", errBody.Error)
	}
	if !strings.HasPrefix(errBody.NewHeadSHA, "") || len(errBody.NewHeadSHA) < 4 {
		t.Errorf("want non-empty new_head_sha, got %q", errBody.NewHeadSHA)
	}
}

// ── Bug 4: MarkTaskDone execution_stage ───────────────────────────────────

// TestShipReview_ApproveMovesTaskToDone verifies that after a successful
// Approve the task's execution_stage becomes "done" (was "stopped" before
// STA-535 because MarkTaskDone silently failed without a work product).
func TestShipReview_ApproveMovesTaskToDone(t *testing.T) {
	database := setupTestDB(t)
	seedBoardWebAuthnCredential(t, database)
	srv, token := startTestServer(t, database)
	setter, ok := any(srv).(webAuthnVerifierSetter)
	if !ok {
		t.Fatal("*server.Server must implement SetWebAuthnVerifier")
	}
	setter.SetWebAuthnVerifier(func(_ *http.Request, _ string) error { return nil })
	baseURL := srv.URL()
	boardToken := srv.BoardToken()
	client := &http.Client{}

	taskID, _ := createShipTask(t, database, baseURL, token, client)

	// Upsert card.
	upsertBody, _ := json.Marshal(map[string]any{
		"test_steps": []string{"1. Open /", "2. Verify page loads"},
	})
	resp, rb := shipDoReq(t, client, token, "PUT", baseURL+"/api/tasks/"+taskID+"/ship-review", upsertBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("UpsertCard: %d %s", resp.StatusCode, rb)
	}

	// Approve — must succeed (200). Board session + WebAuthn assertion required (STA-583).
	resp2, rb2 := shipDoReq(t, client, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/approve", nil, boardToken, "", "mock-assertion")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("Approve: %d %s", resp2.StatusCode, rb2)
	}

	// Verify main_sha is present in response.
	var approveResp struct {
		MainSHA string `json:"main_sha"`
	}
	if err := json.Unmarshal(rb2, &approveResp); err != nil {
		t.Fatalf("unmarshal approve response: %v", err)
	}
	if approveResp.MainSHA == "" {
		t.Error("want main_sha in approve response, got empty")
	}

	// GET task and verify execution_stage = done.
	resp3, rb3 := shipDoReq(t, client, token, "GET", baseURL+"/api/tasks/"+taskID, nil)
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("GetTask: %d %s", resp3.StatusCode, rb3)
	}
	var taskResp struct {
		Task struct {
			ExecutionStage string `json:"execution_stage"`
			Status         string `json:"status"`
		} `json:"task"`
	}
	if err := json.Unmarshal(rb3, &taskResp); err != nil {
		t.Fatalf("unmarshal task: %v", err)
	}
	if taskResp.Task.ExecutionStage != "done" {
		t.Errorf("want execution_stage=done, got %q", taskResp.Task.ExecutionStage)
	}
}

// ── STA-637: delete the task branch after Approve & merge ──────────────────

// shipApproveServer starts a server whose Board passkey check always passes,
// creates a ship task with a real bare remote, and upserts its card.
func shipApproveServer(t *testing.T) (database *sql.DB, baseURL, token, boardToken, taskID, repoDir string, client *http.Client) {
	t.Helper()
	database = setupTestDB(t)
	seedBoardWebAuthnCredential(t, database)
	srv, token := startTestServer(t, database)
	setter, ok := any(srv).(webAuthnVerifierSetter)
	if !ok {
		t.Fatal("*server.Server must implement SetWebAuthnVerifier")
	}
	setter.SetWebAuthnVerifier(func(_ *http.Request, _ string) error { return nil })
	baseURL = srv.URL()
	boardToken = srv.BoardToken()
	client = &http.Client{}

	taskID, repoDir = createShipTask(t, database, baseURL, token, client)
	upsertBody, _ := json.Marshal(map[string]any{"test_steps": []string{"1. Open /"}})
	resp, rb := shipDoReq(t, client, token, "PUT", baseURL+"/api/tasks/"+taskID+"/ship-review", upsertBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("UpsertCard: %d %s", resp.StatusCode, rb)
	}
	return database, baseURL, token, boardToken, taskID, repoDir, client
}

type approveResult struct {
	MainSHA           string `json:"main_sha"`
	BranchDeleted     bool   `json:"branch_deleted"`
	BranchDeleteError string `json:"branch_delete_error"`
	Warning           string `json:"warning"`
	Card              struct {
		Status            string `json:"status"`
		BranchDeleted     bool   `json:"branch_deleted"`
		BranchDeleteError string `json:"branch_delete_error"`
	} `json:"card"`
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func lastBoardAudit(t *testing.T, database *sql.DB, action string) map[string]any {
	t.Helper()
	rows, err := database.Query(`SELECT payload FROM board_audit_log ORDER BY id DESC`)
	if err != nil {
		t.Fatalf("query board_audit_log: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if json.Unmarshal([]byte(payload), &m) == nil && m["action"] == action {
			return m
		}
	}
	t.Fatalf("no board_audit_log row with action %q", action)
	return nil
}

// TestShipReview_ApproveDeletesTaskBranch: a successful Approve & merge deletes
// the task branch from the remote and locally, and records branch_deleted in
// the response, on the card, and in board_audit_log.
func TestShipReview_ApproveDeletesTaskBranch(t *testing.T) {
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	branch := "staypoint/" + taskID
	bare := gitOut(t, repoDir, "remote", "get-url", "origin")

	resp, rb := shipDoReq(t, client, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/approve", nil, boardToken, "", "mock-assertion")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Approve: %d %s", resp.StatusCode, rb)
	}
	var res approveResult
	if err := json.Unmarshal(rb, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !res.BranchDeleted || !res.Card.BranchDeleted || res.Warning != "" {
		t.Fatalf("want branch_deleted=true and no warning, got %s", rb)
	}

	if out := gitOut(t, bare, "branch", "--list", branch); out != "" {
		t.Errorf("branch %q still on remote: %q", branch, out)
	}
	if out := gitOut(t, repoDir, "branch", "--list", branch); out != "" {
		t.Errorf("branch %q still local: %q", branch, out)
	}
	// The merge itself landed on the remote main.
	if got := gitOut(t, bare, "rev-parse", "main"); got != res.MainSHA {
		t.Errorf("remote main = %s, want %s", got, res.MainSHA)
	}

	audit := lastBoardAudit(t, database, "approve")
	if audit["branch_deleted"] != true || audit["branch"] != branch {
		t.Errorf("board_audit_log approve row = %v, want branch_deleted=true branch=%s", audit, branch)
	}
}

// TestShipReview_ApproveBranchDeleteFailureStillMerges: when the remote refuses
// the delete, Approve still returns 200 with the merge in place and a warning;
// the Board's retry then deletes the branch.
func TestShipReview_ApproveBranchDeleteFailureStillMerges(t *testing.T) {
	database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	branch := "staypoint/" + taskID
	bare := gitOut(t, repoDir, "remote", "get-url", "origin")

	// The remote accepts pushes but refuses branch deletions.
	hook := filepath.Join(bare, "hooks", "pre-receive")
	script := "#!/bin/sh\nwhile read old new ref; do\n  case $new in 0000000000000000000000000000000000000000) echo \"deletes disabled\" >&2; exit 1;; esac\ndone\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	resp, rb := shipDoReq(t, client, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/approve", nil, boardToken, "", "mock-assertion")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Approve must succeed when only the branch delete fails: %d %s", resp.StatusCode, rb)
	}
	var res approveResult
	if err := json.Unmarshal(rb, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res.BranchDeleted || res.Card.BranchDeleted {
		t.Fatalf("want branch_deleted=false, got %s", rb)
	}
	if !strings.HasPrefix(res.Warning, "merged; branch delete failed: ") || res.Card.BranchDeleteError == "" {
		t.Errorf("want merged-with-warning response, got %s", rb)
	}
	if res.Card.Status != "approved" {
		t.Errorf("card status = %q, want approved (merge must not roll back)", res.Card.Status)
	}
	if got := gitOut(t, bare, "rev-parse", "main"); got != res.MainSHA {
		t.Errorf("remote main = %s, want merged %s", got, res.MainSHA)
	}
	if out := gitOut(t, bare, "branch", "--list", branch); out == "" {
		t.Error("remote branch vanished although the delete was refused")
	}
	if audit := lastBoardAudit(t, database, "approve"); audit["branch_deleted"] != false || audit["branch_delete_error"] == "" {
		t.Errorf("board_audit_log approve row = %v, want branch_deleted=false with error", audit)
	}

	// Retry while the remote still refuses: 409, merge untouched.
	retryURL := baseURL + "/api/tasks/" + taskID + "/ship-review/delete-branch"
	resp2, rb2 := shipDoReq(t, client, token, "POST", retryURL, nil, boardToken, "", "mock-assertion")
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("retry with failing remote: want 409, got %d %s", resp2.StatusCode, rb2)
	}

	// Remote fixed: retry succeeds and the branch is gone.
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	resp3, rb3 := shipDoReq(t, client, token, "POST", retryURL, nil, boardToken, "", "mock-assertion")
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("retry: %d %s", resp3.StatusCode, rb3)
	}
	var res3 approveResult
	_ = json.Unmarshal(rb3, &res3)
	if !res3.BranchDeleted || !res3.Card.BranchDeleted || res3.Card.BranchDeleteError != "" {
		t.Errorf("retry: want branch_deleted=true with error cleared, got %s", rb3)
	}
	if out := gitOut(t, bare, "branch", "--list", branch); out != "" {
		t.Errorf("branch %q still on remote after retry", branch)
	}
	if audit := lastBoardAudit(t, database, "delete_branch_retry"); audit["branch_deleted"] != true {
		t.Errorf("board_audit_log retry row = %v, want branch_deleted=true", audit)
	}
}

// TestShipReview_DeleteBranchRequiresApprovedCard: the retry endpoint never
// deletes a branch whose review is still open.
func TestShipReview_DeleteBranchRequiresApprovedCard(t *testing.T) {
	_, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
	bare := gitOut(t, repoDir, "remote", "get-url", "origin")

	resp, rb := shipDoReq(t, client, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/delete-branch", nil, boardToken, "", "mock-assertion")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("pending card: want 409, got %d %s", resp.StatusCode, rb)
	}
	if out := gitOut(t, bare, "branch", "--list", "staypoint/"+taskID); out == "" {
		t.Error("branch deleted while review still open")
	}
}

// ── STA-654: start-dev refuses closed cards ────────────────────────────────

// TestShipReview_StartDevRefusesNonPendingCard: once a review is closed
// (approved, rejected, or sent back), start-dev returns 409 and never
// recreates .worktrees/devserver-<id> or touches the card's dev state. A dev
// command is configured so the only thing standing between the request and
// StartDevServerAsync is the status check.
func TestShipReview_StartDevRefusesNonPendingCard(t *testing.T) {
	cases := []struct {
		status string
		action string
		body   []byte
	}{
		{"approved", "approve", nil},
		{"rejected", "reject", []byte(`{"comment":"no"}`)},
		{"sent_back", "send-back", []byte(`{"comment":"rework"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			database, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)
			if err := shipreview.UpsertProjectDevConfig(database, &shipreview.ProjectDevConfig{
				RepoPath:   repoDir,
				DevCommand: "true",
				DevURL:     "http://localhost:3999",
			}); err != nil {
				t.Fatalf("UpsertProjectDevConfig: %v", err)
			}

			cardURL := baseURL + "/api/tasks/" + taskID + "/ship-review"
			resp, rb := shipDoReq(t, client, token, "POST", cardURL+"/"+tc.action, tc.body, boardToken, "", "mock-assertion")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s: %d %s", tc.action, resp.StatusCode, rb)
			}

			resp, rb = shipDoReq(t, client, token, "POST", cardURL+"/start-dev", nil)
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("start-dev on %s card: want 409, got %d %s", tc.status, resp.StatusCode, rb)
			}
			if !strings.Contains(string(rb), "card is not pending") {
				t.Errorf("start-dev body = %s, want \"card is not pending\"", rb)
			}

			devDir := filepath.Join(repoDir, ".worktrees", "devserver-"+taskID)
			if _, err := os.Stat(devDir); !os.IsNotExist(err) {
				t.Errorf("devserver worktree %s exists after refused start-dev (stat err=%v)", devDir, err)
			}

			card, err := shipreview.GetCard(database, taskID)
			if err != nil {
				t.Fatalf("GetCard: %v", err)
			}
			if card.Status != tc.status {
				t.Errorf("card status = %q, want %q", card.Status, tc.status)
			}
			if card.DevState == shipreview.DevStateStarting {
				t.Errorf("card dev_state = %q after refused start-dev", card.DevState)
			}
		})
	}
}

// A task cut from dev-server makes a card targeting dev-server, and Approve
// merges into dev-server, never into main.
func TestShipReview_ApproveMergesIntoTaskTarget(t *testing.T) {
	database := setupTestDB(t)
	seedBoardWebAuthnCredential(t, database)
	srv, token := startTestServer(t, database)
	any(srv).(webAuthnVerifierSetter).SetWebAuthnVerifier(func(_ *http.Request, _ string) error { return nil })
	baseURL, boardToken, client := srv.URL(), srv.BoardToken(), &http.Client{}

	taskID, repoDir := createShipTask(t, database, baseURL, token, client)
	bare := gitOut(t, repoDir, "remote", "get-url", "origin")
	gitOut(t, repoDir, "branch", "dev-server", "main")
	gitOut(t, repoDir, "push", "origin", "dev-server")
	if err := workspace.RecordTaskTarget(gocontext.Background(), database, taskID, "dev-server"); err != nil {
		t.Fatalf("RecordTaskTarget: %v", err)
	}
	mainBefore := gitOut(t, bare, "rev-parse", "main")

	upsertBody, _ := json.Marshal(map[string]any{"test_steps": []string{"1. Open /"}})
	resp, rb := shipDoReq(t, client, token, "PUT", baseURL+"/api/tasks/"+taskID+"/ship-review", upsertBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("UpsertCard: %d %s", resp.StatusCode, rb)
	}
	var card struct {
		TargetBranch string `json:"target_branch"`
	}
	if err := json.Unmarshal(rb, &card); err != nil || card.TargetBranch != "dev-server" {
		t.Fatalf("card target = %q (err %v), want dev-server: %s", card.TargetBranch, err, rb)
	}

	resp, rb = shipDoReq(t, client, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/approve", nil, boardToken, "", "mock-assertion")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Approve: %d %s", resp.StatusCode, rb)
	}
	var res approveResult
	if err := json.Unmarshal(rb, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := gitOut(t, bare, "rev-parse", "dev-server"); got != res.MainSHA {
		t.Errorf("remote dev-server = %s, want merge %s", got, res.MainSHA)
	}
	if got := gitOut(t, bare, "rev-parse", "main"); got != mainBefore {
		t.Errorf("remote main moved to %s; Approve must merge into dev-server only", got)
	}
}
