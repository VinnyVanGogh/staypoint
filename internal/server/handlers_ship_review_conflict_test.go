package server_test

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

// task-2bdcdc74: an Approve whose branch conflicts with the target answers
// 409 merge_conflict with the conflicting files, never 500, and leaves the
// repo root checkout clean so the next Approve is not wedged.
func TestShipReview_ApproveConflictReturns409WithFiles(t *testing.T) {
	_, baseURL, token, boardToken, taskID, repoDir, client := shipApproveServer(t)

	// main gains its own task.txt after the task branch was cut.
	scratch := filepath.Join(t.TempDir(), "scratch")
	gitOut(t, repoDir, "worktree", "add", "--detach", scratch, "main")
	if err := os.WriteFile(filepath.Join(scratch, "task.txt"), []byte("main's version\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, scratch, "add", "task.txt")
	commit := exec.Command("git", "-C", scratch, "-c", "user.name=test", "-c", "user.email=t@t.com", "commit", "-m", "main: task.txt")
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	mainTip := gitOut(t, scratch, "rev-parse", "HEAD")
	gitOut(t, repoDir, "push", "origin", mainTip+":refs/heads/main")
	gitOut(t, repoDir, "worktree", "remove", "--force", scratch)
	rootHead := gitOut(t, repoDir, "rev-parse", "HEAD")

	resp, rb := shipDoReq(t, client, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/approve", nil, boardToken, "", "mock-assertion")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("Approve: %d %s, want 409", resp.StatusCode, rb)
	}
	var body struct {
		Error  string   `json:"error"`
		Target string   `json:"target"`
		Files  []string `json:"files"`
	}
	if err := json.Unmarshal(rb, &body); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rb)
	}
	if body.Error != "merge_conflict" || body.Target != "main" || !reflect.DeepEqual(body.Files, []string{"task.txt"}) {
		t.Fatalf("body = %s, want merge_conflict on main in [task.txt]", rb)
	}

	if got := gitOut(t, repoDir, "rev-parse", "HEAD"); got != rootHead {
		t.Errorf("root HEAD moved %s -> %s", rootHead, got)
	}
	if st := gitOut(t, repoDir, "status", "--porcelain"); st != "" {
		t.Errorf("root checkout dirty after conflict:\n%s", st)
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".git", "MERGE_HEAD")); err == nil {
		t.Error("MERGE_HEAD left in root")
	}
	if got := gitOut(t, repoDir, "ls-remote", "origin", "refs/heads/main"); got[:40] != mainTip {
		t.Errorf("origin/main moved on conflict: %s", got)
	}

	// The card is still pending: the Board can send it back.
	resp, rb = shipDoReq(t, client, token, "GET", baseURL+"/api/tasks/"+taskID+"/ship-review", nil)
	var card struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(rb, &card)
	if resp.StatusCode != http.StatusOK || card.Status != "pending" {
		t.Fatalf("card after conflict: %d %s, want pending", resp.StatusCode, rb)
	}
}
