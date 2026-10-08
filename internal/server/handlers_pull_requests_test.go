package server_test

// task-9fb380ef / task-f6777004: Pull Requests page and Run all children.
// GitHub is a Go fake behind server.PRClient; nothing reaches GitHub.

import (
	gocontext "context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/server"
	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

type fakePRClient struct {
	mu sync.Mutex
	// heads is each PR's current head on "GitHub".
	heads map[int]string
	// conflicting PRs report mergeable=CONFLICTING.
	conflicting map[int]bool
	prs         []shipreview.OpenPR
	mergeCalls  []int
	merged      []int
	combineErr  error
	combineReqs []shipreview.CombineRequest
}

func (f *fakePRClient) ListOpenPRs(gocontext.Context) ([]shipreview.OpenPR, error) {
	return f.prs, nil
}

func (f *fakePRClient) MergePRAt(_ gocontext.Context, n int, sha string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mergeCalls = append(f.mergeCalls, n)
	if f.heads[n] != sha {
		return shipreview.ErrHeadMoved
	}
	if f.conflicting[n] {
		return fmt.Errorf("%w: PR #%d", shipreview.ErrPRNotMergeable, n)
	}
	f.merged = append(f.merged, n)
	return nil
}

func (f *fakePRClient) CombinePRs(_ gocontext.Context, req shipreview.CombineRequest) (*shipreview.CombineResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.combineReqs = append(f.combineReqs, req)
	if f.combineErr != nil {
		return nil, f.combineErr
	}
	return &shipreview.CombineResult{Branch: req.Branch, Number: 99, URL: "https://github.com/o/r/pull/99"}, nil
}

// startPRServer starts a Board-gated server whose PR client is fake, with one
// task in a repo named agent-mesh so the page lists that repo.
func startPRServer(t *testing.T) (*sql.DB, *server.Server, string, string, *fakePRClient) {
	t.Helper()
	database, srv, token := startBoardGateServer(t)
	repo := filepath.Join(t.TempDir(), "agent-mesh")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	resp, task := postTask(t, srv.URL(), token, map[string]any{"name": "pr task", "repo_path": repo})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create task: %d %v", resp.StatusCode, task)
	}
	fake := &fakePRClient{heads: map[int]string{1: "aaa1", 2: "bbb2", 3: "ccc3"}, conflicting: map[int]bool{}}
	fake.prs = []shipreview.OpenPR{
		{Number: 1, Title: "one", URL: "https://github.com/o/r/pull/1", HeadRefName: "feature/one", HeadRefOid: "aaa1", BaseRefName: "main", CreatedAt: "2026-10-07T00:00:00Z"},
		{Number: 2, Title: "two", URL: "https://github.com/o/r/pull/2", HeadRefName: "staypoint/" + task["id"].(string), HeadRefOid: "bbb2", BaseRefName: "main"},
	}
	if err := meshContext.AddWorkProduct(database, task["id"].(string), "pull_request", "https://github.com/o/r/pull/1"); err != nil {
		t.Fatal(err)
	}
	srv.SetPRClientFactory(func(*sql.DB, string) (server.PRClient, error) { return fake, nil })
	return database, srv, token, repo, fake
}

func prBody(repo string, pins ...any) map[string]any {
	var prs []map[string]any
	for i := 0; i+1 < len(pins); i += 2 {
		prs = append(prs, map[string]any{"number": pins[i], "head_sha": pins[i+1]})
	}
	return map[string]any{"repo_path": repo, "prs": prs}
}

func TestPullRequests_ActionsRefuseWithoutBoardGate(t *testing.T) {
	_, srv, token, repo, fake := startPRServer(t)
	for _, path := range []string{"/api/pull-requests/merge", "/api/pull-requests/combine"} {
		body := prBody(repo, 1, "aaa1", 2, "bbb2")
		// Agent token alone.
		if code, errCode, raw := boardPost(t, srv, token, path, body, false); code != http.StatusForbidden {
			t.Errorf("%s token-only: %d %s %s", path, code, errCode, raw)
		}
		// Board cookie but a bad passkey assertion.
		raw, _ := json.Marshal(body)
		code, _, resp := doBoard(t, srv, token, boardReq{method: http.MethodPost, path: path, body: string(raw), cookie: true, assertion: "bad"})
		if code == http.StatusOK || code == http.StatusCreated {
			t.Errorf("%s bad assertion passed: %d %s", path, code, resp)
		}
	}
	if len(fake.mergeCalls) != 0 || len(fake.combineReqs) != 0 {
		t.Fatalf("GitHub reached without the Board gate: merges=%v combines=%d", fake.mergeCalls, len(fake.combineReqs))
	}
}

func TestPullRequests_ListLinksTasks(t *testing.T) {
	_, srv, token, repo, _ := startPRServer(t)
	code, _, raw := doBoard(t, srv, token, boardReq{method: http.MethodGet, path: "/api/pull-requests"})
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, raw)
	}
	var out struct {
		Repos []struct {
			RepoPath string `json:"repo_path"`
			PRs      []struct {
				Number int `json:"number"`
				Task   *struct {
					ID string `json:"id"`
				} `json:"task"`
			} `json:"prs"`
		} `json:"repos"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Repos) != 1 || out.Repos[0].RepoPath != repo || len(out.Repos[0].PRs) != 2 {
		t.Fatalf("unexpected repos: %s", raw)
	}
	for _, pr := range out.Repos[0].PRs {
		if pr.Task == nil {
			t.Errorf("PR #%d not linked to its task (work product / branch): %s", pr.Number, raw)
		}
	}
}

func TestPullRequests_MergePinsHeadSHA(t *testing.T) {
	_, srv, token, repo, fake := startPRServer(t)
	code, _, raw := boardPost(t, srv, token, "/api/pull-requests/merge", prBody(repo, 1, "stale-sha"), true)
	if code != http.StatusConflict {
		t.Fatalf("stale head: want 409, got %d %s", code, raw)
	}
	var out struct {
		Merged []int `json:"merged"`
		Failed *struct {
			Number int    `json:"number"`
			Error  string `json:"error"`
		} `json:"failed"`
	}
	_ = json.Unmarshal([]byte(raw), &out)
	if out.Failed == nil || out.Failed.Error != "head_moved" || len(out.Merged) != 0 || len(fake.merged) != 0 {
		t.Fatalf("stale head merged or wrong error: %s merged=%v", raw, fake.merged)
	}
	code, _, raw = boardPost(t, srv, token, "/api/pull-requests/merge", prBody(repo, 1, "aaa1"), true)
	if code != http.StatusOK || len(fake.merged) != 1 || fake.merged[0] != 1 {
		t.Fatalf("pinned merge: %d %s merged=%v", code, raw, fake.merged)
	}
}

func TestPullRequests_MergeRejectsUnlistedRepo(t *testing.T) {
	_, srv, token, _, fake := startPRServer(t)
	code, _, raw := boardPost(t, srv, token, "/api/pull-requests/merge", prBody(t.TempDir(), 1, "aaa1"), true)
	if code != http.StatusBadRequest || len(fake.mergeCalls) != 0 {
		t.Fatalf("unlisted repo: %d %s calls=%v", code, raw, fake.mergeCalls)
	}
}

func TestPullRequests_BatchMergeStopsAtFirstFailure(t *testing.T) {
	_, srv, token, repo, fake := startPRServer(t)
	fake.conflicting[2] = true
	code, _, raw := boardPost(t, srv, token, "/api/pull-requests/merge", prBody(repo, 1, "aaa1", 2, "bbb2", 3, "ccc3"), true)
	if code != http.StatusConflict {
		t.Fatalf("want 409, got %d %s", code, raw)
	}
	var out struct {
		Merged       []int `json:"merged"`
		NotAttempted []int `json:"not_attempted"`
		Failed       *struct {
			Number int    `json:"number"`
			Error  string `json:"error"`
		} `json:"failed"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(out.Merged) != "[1]" || out.Failed == nil || out.Failed.Number != 2 || out.Failed.Error != "not_mergeable" || fmt.Sprint(out.NotAttempted) != "[3]" {
		t.Fatalf("wrong batch report: %s", raw)
	}
	if fmt.Sprint(fake.mergeCalls) != "[1 2]" {
		t.Fatalf("PR 3 must not be attempted after PR 2 failed: calls=%v", fake.mergeCalls)
	}
}

func TestPullRequests_CombineReportsConflict(t *testing.T) {
	_, srv, token, repo, fake := startPRServer(t)
	fake.combineErr = &shipreview.CombineError{Number: 2, Err: fmt.Errorf("%w: boom", shipreview.ErrCombineConflict)}
	code, _, raw := boardPost(t, srv, token, "/api/pull-requests/combine", prBody(repo, 1, "aaa1", 2, "bbb2"), true)
	var out map[string]any
	_ = json.Unmarshal([]byte(raw), &out)
	if code != http.StatusConflict || out["error"] != "conflict" || out["failed_pr"] != float64(2) {
		t.Fatalf("conflict: %d %s", code, raw)
	}
	fake.combineErr = nil
	code, _, raw = boardPost(t, srv, token, "/api/pull-requests/combine", prBody(repo, 1, "aaa1", 2, "bbb2"), true)
	if code != http.StatusCreated {
		t.Fatalf("combine: %d %s", code, raw)
	}
	req := fake.combineReqs[len(fake.combineReqs)-1]
	if req.Base != "main" || len(req.Sources) != 2 || req.Sources[0].Number != 1 || req.Sources[1].HeadSHA != "bbb2" {
		t.Fatalf("combine request not in order / not pinned: %+v", req)
	}
	// One PR is not a combine.
	if code, _, raw := boardPost(t, srv, token, "/api/pull-requests/combine", prBody(repo, 1, "aaa1"), true); code != http.StatusBadRequest {
		t.Fatalf("single-PR combine: %d %s", code, raw)
	}
}

// ── Run all children ────────────────────────────────────────────────────────

func setStageSQL(t *testing.T, database *sql.DB, id, stage string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE tasks SET execution_stage = ? WHERE id = ?`, stage, id); err != nil {
		t.Fatal(err)
	}
}

func TestRunChildren(t *testing.T) {
	database, srv, token := startBoardGateServer(t)
	base := srv.URL()
	resp, parent := postTask(t, base, token, map[string]any{"name": "p", "repo_path": t.TempDir(), "work_kind": "planning"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("parent: %d %v", resp.StatusCode, parent)
	}
	pid := parent["id"].(string)
	child := func(name, stage, priority string) string {
		t.Helper()
		resp, c := postTask(t, base, token, map[string]any{"name": name, "parent_id": pid, "priority": priority})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("child %s: %d %v", name, resp.StatusCode, c)
		}
		id := c["id"].(string)
		setStageSQL(t, database, id, stage)
		if _, err := database.Exec(`UPDATE tasks SET priority = ? WHERE id = ?`, priority, id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	low := child("low backlog", "backlog", "low")
	high := child("high backlog", "backlog", "high")
	todo := child("todo", "todo", "medium")
	running := child("running", "in_progress", "critical")
	blocked := child("blocked", "blocked", "high")
	cancelled := child("cancelled", "cancelled", "high")
	held := child("held", "backlog", "critical")
	if _, err := database.Exec(`UPDATE tasks SET organization = 'HeldOrg' WHERE id = ?`, held); err != nil {
		t.Fatal(err)
	}
	if err := governance.SetOrgHold(database, "HeldOrg", true); err != nil {
		t.Fatal(err)
	}

	path := "/api/tasks/" + pid + "/run-children"
	if code, _, raw := boardPost(t, srv, token, path, map[string]any{}, false); code != http.StatusForbidden {
		t.Fatalf("token-only run-children: %d %s", code, raw)
	}
	if got := taskStageOf(t, database, low); got != "backlog" {
		t.Fatalf("refused request still moved a child: %s", got)
	}

	code, _, raw := boardPost(t, srv, token, path, map[string]any{}, true)
	if code != http.StatusOK {
		t.Fatalf("run-children: %d %s", code, raw)
	}
	var out struct {
		Queued   int `json:"queued"`
		Children []struct {
			TaskID string `json:"task_id"`
			Result string `json:"result"`
			Reason string `json:"reason"`
		} `json:"children"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	if out.Queued != 3 {
		t.Fatalf("want 3 queued (high, todo, low), got %d: %s", out.Queued, raw)
	}
	// Priority order: critical first, then high, medium, low.
	var queuedOrder []string
	results := map[string]string{}
	for _, c := range out.Children {
		results[c.TaskID] = c.Result
		if c.Result == "queued" {
			queuedOrder = append(queuedOrder, c.TaskID)
		}
	}
	if fmt.Sprint(queuedOrder) != fmt.Sprint([]string{high, todo, low}) {
		t.Fatalf("queued out of priority order: %v want %v", queuedOrder, []string{high, todo, low})
	}
	for _, id := range []string{running, blocked, cancelled, held} {
		if results[id] != "skipped" {
			t.Errorf("child %s: want skipped, got %q", id, results[id])
		}
	}
	for id, want := range map[string]string{low: "todo", high: "todo", todo: "todo", running: "in_progress", blocked: "blocked", cancelled: "cancelled", held: "backlog"} {
		if got := taskStageOf(t, database, id); got != want {
			t.Errorf("child %s stage: got %s want %s", id, got, want)
		}
	}
}

func taskStageOf(t *testing.T, database *sql.DB, id string) string {
	t.Helper()
	task, err := meshContext.GetTask(database, id)
	if err != nil {
		t.Fatal(err)
	}
	return task.ExecutionStage
}
