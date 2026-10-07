package server_test

import (
	gocontext "context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
	"github.com/VinnyVanGogh/staypoint/internal/workspace"
)

// STA-774: the card, Approve and the Diff tab all measure from the task's
// recorded base, and every one of them fails closed when the agent moved it.

type wholeRunDiff struct {
	Files        []string `json:"files"`
	CheckpointID string   `json:"checkpoint_id"`
	BaseVerified bool     `json:"base_verified"`
	AnswerOnly   bool     `json:"answer_only"`
	FilesRead    []string `json:"files_read"`
}

func getWholeRunDiff(t *testing.T, baseURL, token, taskID string) (int, wholeRunDiff, string) {
	t.Helper()
	resp, body, _ := timedReq(t, token, "GET", baseURL+"/api/tasks/"+taskID+"/diff")
	var d wholeRunDiff
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &d); err != nil {
			t.Fatalf("unmarshal diff: %v (%s)", err, body)
		}
	}
	return resp.StatusCode, d, string(body)
}

// The Diff tab's whole run lists exactly the card's files, from the same base.
func TestTaskBase_DiffTabMatchesCard(t *testing.T) {
	database, baseURL, token, _, taskID, repoDir, _ := shipApproveServer(t)
	card, err := shipreview.GetCard(database, taskID)
	if err != nil {
		t.Fatal(err)
	}
	code, d, body := getWholeRunDiff(t, baseURL, token, taskID)
	if code != http.StatusOK {
		t.Fatalf("GET diff: %d %s", code, body)
	}
	base, err := workspace.VerifiedBase(gocontext.Background(), database, repoDir, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if !d.BaseVerified || d.CheckpointID != base || d.AnswerOnly {
		t.Fatalf("diff = %+v, want verified base %s and not answer-only", d, base)
	}
	if !reflect.DeepEqual(d.Files, card.FilesChanged) {
		t.Fatalf("Diff tab files %v != card files %v", d.Files, card.FilesChanged)
	}
}

// A run that committed nothing: no card, 409 no_changes, and the Diff tab
// reports an answer-only run with the files it read.
func TestTaskBase_AnswerOnlyRun(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	baseURL := srv.URL()
	client := &http.Client{}
	taskID, repoDir := createShipTask(t, database, baseURL, token, client)
	gitOut(t, repoDir, "branch", "-f", "staypoint/"+taskID, "main")
	if _, err := database.Exec(
		`INSERT INTO run_steps (id, run_id, task_id, seq, kind, title) VALUES
		 ('s1', 'r1', ?, 1, 'read', 'Read README.md'),
		 ('s2', 'r1', ?, 2, 'read', 'Read README.md'),
		 ('s3', 'r1', ?, 3, 'read', 'Search codebase'),
		 ('s4', 'r1', ?, 4, 'read', 'Read docs/a.md')`,
		taskID, taskID, taskID, taskID); err != nil {
		t.Fatal(err)
	}

	upsert, _ := json.Marshal(map[string]any{"test_steps": []string{"1. Open /"}})
	resp, rb := shipDoReq(t, client, token, "PUT", baseURL+"/api/tasks/"+taskID+"/ship-review", upsert)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(rb), `"no_changes"`) {
		t.Fatalf("UpsertCard on answer-only run: %d %s, want 409 no_changes", resp.StatusCode, rb)
	}
	if _, err := shipreview.GetCard(database, taskID); err == nil {
		t.Fatal("answer-only run got a card")
	}

	code, d, body := getWholeRunDiff(t, baseURL, token, taskID)
	if code != http.StatusOK {
		t.Fatalf("GET diff: %d %s", code, body)
	}
	if !d.AnswerOnly || len(d.Files) != 0 {
		t.Fatalf("diff = %+v, want answer_only with no files", d)
	}
	if want := []string{"README.md", "docs/a.md"}; !reflect.DeepEqual(d.FilesRead, want) {
		t.Fatalf("files_read = %v, want %v", d.FilesRead, want)
	}
}

// The agent moves the base pin onto its own head after the card was made:
// Approve refuses, the card stays pending, and the Diff tab refuses to show
// a diff from the moved base.
func TestTaskBase_TamperedPinBlocksApproveAndDiff(t *testing.T) {
	database, baseURL, token, boardToken, taskID, repoDir, _ := shipApproveServer(t)
	mainBefore := gitOut(t, repoDir, "rev-parse", "main")
	gitOut(t, repoDir, "update-ref", workspace.BaseRef(taskID), "staypoint/"+taskID)

	resp, body, _ := timedReq(t, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/approve", boardToken, "", "mock-assertion")
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), `"base_tampered"`) {
		t.Fatalf("Approve over a moved pin: %d %s, want 409 base_tampered", resp.StatusCode, body)
	}
	card, err := shipreview.GetCard(database, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if card.Status != shipreview.StatusPending {
		t.Fatalf("card status = %s, want pending", card.Status)
	}
	if got := gitOut(t, repoDir, "rev-parse", "main"); got != mainBefore {
		t.Fatalf("main moved %s -> %s", mainBefore, got)
	}

	code, _, dbody := getWholeRunDiff(t, baseURL, token, taskID)
	if code != http.StatusConflict || !strings.Contains(dbody, "base_tampered") {
		t.Fatalf("GET diff over a moved pin: %d %s, want 409 base_tampered", code, dbody)
	}

	// Re-submitting the card does not launder it either.
	upsert, _ := json.Marshal(map[string]any{"test_steps": []string{"1. Open /"}})
	r2, rb := shipDoReq(t, &http.Client{}, token, "PUT", baseURL+"/api/tasks/"+taskID+"/ship-review", upsert)
	if r2.StatusCode != http.StatusConflict || !strings.Contains(string(rb), `"base_tampered"`) {
		t.Fatalf("UpsertCard over a moved pin: %d %s, want 409 base_tampered", r2.StatusCode, rb)
	}
}

// A task with no recorded base cannot be approved even if a card exists.
func TestTaskBase_UnrecordedBaseBlocksApprove(t *testing.T) {
	database, baseURL, token, boardToken, taskID, _, _ := shipApproveServer(t)
	if _, err := database.Exec(`DELETE FROM task_worktree_bases WHERE task_id = ?`, taskID); err != nil {
		t.Fatal(err)
	}
	resp, body, _ := timedReq(t, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/approve", boardToken, "", "mock-assertion")
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), `"base_unverified"`) {
		t.Fatalf("Approve without a recorded base: %d %s, want 409 base_unverified", resp.StatusCode, body)
	}
	// The Diff tab may still show a legacy view, but says it is unverified.
	code, d, dbody := getWholeRunDiff(t, baseURL, token, taskID)
	if code != http.StatusOK || d.BaseVerified || d.AnswerOnly {
		t.Fatalf("GET diff without a recorded base: %d %s", code, dbody)
	}
}
