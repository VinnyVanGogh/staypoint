package server_test

// PR #249 re-review: symbolic refs, Reject's branch delete, and a
// pre-existing branch checked out in the task worktree. Adapted from the
// reviewer's repros; each must fail closed.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
)

type sec249Env struct {
	db                         *sql.DB
	baseURL, token, boardToken string
	client                     *http.Client
	taskID, repo               string
}

// newSec249Env is a task whose own branch has no changes, so the card ships
// whatever branch the test registers.
func newSec249Env(t *testing.T) *sec249Env {
	t.Helper()
	database := setupTestDB(t)
	seedBoardWebAuthnCredential(t, database)
	srv, token := startTestServer(t, database)
	any(srv).(webAuthnVerifierSetter).SetWebAuthnVerifier(func(_ *http.Request, _ string) error { return nil })
	e := &sec249Env{db: database, baseURL: srv.URL(), token: token, boardToken: srv.BoardToken(), client: &http.Client{}}
	e.taskID, e.repo = createShipTask(t, database, e.baseURL, token, e.client)
	gitOut(t, e.repo, "branch", "-f", "staypoint/"+e.taskID, "main")
	gitOut(t, e.repo, "push", "-f", "origin", "staypoint/"+e.taskID)
	return e
}

func (e *sec249Env) register(t *testing.T, ref string) {
	t.Helper()
	if st, body := registerBranchHTTP(t, e.client, e.baseURL, e.token, e.taskID, ref); st != http.StatusCreated {
		t.Fatalf("register %s: %d %s", ref, st, body)
	}
}

func (e *sec249Env) upsert(t *testing.T) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"test_steps": []string{"1. Open /"}})
	resp, rb := shipDoReq(t, e.client, e.token, "PUT", e.baseURL+"/api/tasks/"+e.taskID+"/ship-review", body)
	return resp.StatusCode, string(rb)
}

func (e *sec249Env) board(t *testing.T, action string, body any) (int, string) {
	t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	resp, rb := shipDoReq(t, e.client, e.token, "POST", e.baseURL+"/api/tasks/"+e.taskID+"/ship-review/"+action, b, e.boardToken, "", "mock-assertion")
	return resp.StatusCode, string(rb)
}

func hasLocalRef(t *testing.T, repo, ref string) bool {
	t.Helper()
	return gitOut(t, repo, "for-each-ref", "--format=%(refname)", ref) != ""
}

func (e *sec249Env) provenance(t *testing.T, ref string) string {
	t.Helper()
	var p string
	_ = e.db.QueryRow(`SELECT provenance FROM task_work_products WHERE task_id = ? AND reference = ? ORDER BY id DESC LIMIT 1`, e.taskID, ref).Scan(&p)
	return p
}

// Repro A: a branch registered before it existed (task provenance) is made
// a symref to dev-server/master. No card is built for it, and nothing that
// follows can delete the target.
func TestSec249_SymrefRegisteredBranchNeverDeletesTarget(t *testing.T) {
	for _, tgt := range []string{"dev-server", "master"} {
		t.Run(tgt, func(t *testing.T) {
			e := newSec249Env(t)
			e.register(t, "fix/sym")
			gitOut(t, e.repo, "checkout", "-b", tgt, "main")
			commitFile(t, e.repo, tgt+".txt")
			gitOut(t, e.repo, "push", "origin", tgt)
			gitOut(t, e.repo, "checkout", "main")
			gitOut(t, e.repo, "symbolic-ref", "refs/heads/fix/sym", "refs/heads/"+tgt)
			gitOut(t, e.repo, "push", "origin", "fix/sym")
			gitOut(t, e.repo, "fetch", "origin")

			st, body := e.upsert(t)
			if st == http.StatusCreated {
				st2, body2 := e.board(t, "approve", nil)
				t.Errorf("card built for a symref branch: %s; approve: %d %s", body, st2, body2)
			}
			if !hasLocalRef(t, e.repo, "refs/heads/"+tgt) {
				t.Fatalf("local %s deleted via symref fix/sym", tgt)
			}
			if gitOut(t, e.repo, "ls-remote", "--heads", "origin", "refs/heads/"+tgt) == "" {
				t.Fatalf("origin %s deleted", tgt)
			}
		})
	}
}

// The branch is swapped to a symref to main after the card is built.
func TestSec249_SymrefSwapBeforeApproveKeepsMain(t *testing.T) {
	e := newSec249Env(t)
	e.register(t, "fix/swap")
	gitOut(t, e.repo, "checkout", "-b", "fix/swap", "main")
	commitFile(t, e.repo, "swap.txt")
	gitOut(t, e.repo, "push", "origin", "fix/swap")
	gitOut(t, e.repo, "checkout", "main")
	if st, body := e.upsert(t); st != http.StatusCreated {
		t.Fatalf("UpsertCard: %d %s", st, body)
	}
	gitOut(t, e.repo, "update-ref", "-d", "refs/heads/fix/swap")
	gitOut(t, e.repo, "symbolic-ref", "refs/heads/fix/swap", "refs/heads/main")
	st, body := e.board(t, "approve", nil)
	if st == http.StatusOK {
		var res approveResult
		_ = json.Unmarshal([]byte(body), &res)
		if res.BranchDeleted {
			t.Errorf("approve reported the swapped symref deleted: %s", body)
		}
	}
	if !hasLocalRef(t, e.repo, "refs/heads/main") {
		t.Fatalf("local main deleted via symref swap (approve %d %s)", st, body)
	}
}

// Repro B: cleanup fails on Approve (branch checked out elsewhere); the
// branch becomes a symref to main; the Board retries the delete.
func TestSec249_RetryAfterSymrefSwapKeepsMain(t *testing.T) {
	e := newSec249Env(t)
	e.register(t, "fix/swap")
	gitOut(t, e.repo, "checkout", "-b", "fix/swap", "main")
	commitFile(t, e.repo, "swap.txt")
	gitOut(t, e.repo, "push", "origin", "fix/swap")
	gitOut(t, e.repo, "checkout", "main")
	other := filepath.Join(t.TempDir(), "other")
	gitOut(t, e.repo, "worktree", "add", other, "fix/swap")
	if st, body := e.upsert(t); st != http.StatusCreated {
		t.Fatalf("UpsertCard: %d %s", st, body)
	}
	if st, body := e.board(t, "approve", nil); st != http.StatusOK {
		t.Fatalf("approve: %d %s", st, body)
	}
	gitOut(t, e.repo, "worktree", "remove", "--force", other)
	gitOut(t, e.repo, "update-ref", "-d", "refs/heads/fix/swap")
	gitOut(t, e.repo, "symbolic-ref", "refs/heads/fix/swap", "refs/heads/main")

	st, body := e.board(t, "delete-branch", nil)
	if st == http.StatusOK {
		t.Errorf("retry deleted a symref branch: %d %s", st, body)
	}
	if !hasLocalRef(t, e.repo, "refs/heads/main") {
		t.Fatalf("local main deleted via retry after symref swap (%d %s)", st, body)
	}
}

// F2: Reject's delete_branch never deletes a branch the task does not own,
// and the review is not rejected when the delete is refused.
func TestSec249_RejectNeverDeletesForeignBranch(t *testing.T) {
	e := newSec249Env(t)
	gitOut(t, e.repo, "checkout", "-b", "release/staging", "main")
	commitFile(t, e.repo, "theirs.txt")
	gitOut(t, e.repo, "push", "origin", "release/staging")
	gitOut(t, e.repo, "checkout", "main")
	e.register(t, "release/staging")
	if p := e.provenance(t, "release/staging"); p != "foreign" {
		t.Fatalf("provenance %q, want foreign", p)
	}
	if st, body := e.upsert(t); st != http.StatusCreated {
		t.Fatalf("UpsertCard: %d %s", st, body)
	}
	for _, body := range []any{
		map[string]any{"comment": "no", "delete_branch": true},
		// Even naming the branch does not delete a branch the task does not own.
		map[string]any{"comment": "no", "delete_branch": true, "confirm_delete_unmerged": "release/staging"},
	} {
		st, rb := e.board(t, "reject", body)
		if st != http.StatusConflict {
			t.Errorf("reject %v: %d %s, want 409", body, st, rb)
		}
	}
	if gitOut(t, e.repo, "ls-remote", "--heads", "origin", "refs/heads/release/staging") == "" {
		t.Fatal("foreign origin release/staging deleted by Reject delete_branch")
	}
	var status string
	_ = e.db.QueryRow(`SELECT status FROM ship_review_cards WHERE task_id = ?`, e.taskID).Scan(&status)
	if status != "pending" {
		t.Fatalf("card status %q after a refused delete, want pending", status)
	}
	// Reject without deleting still works.
	if st, rb := e.board(t, "reject", map[string]any{"comment": "no"}); st != http.StatusOK {
		t.Fatalf("plain reject: %d %s", st, rb)
	}
}

// F2: the task's own branch holds work not in the target; deleting it on
// Reject needs a separate override naming the branch exactly.
func TestSec249_RejectUnmergedOwnBranchNeedsNamedOverride(t *testing.T) {
	e := newSec249Env(t)
	e.register(t, "fix/mine")
	gitOut(t, e.repo, "checkout", "-b", "fix/mine", "main")
	commitFile(t, e.repo, "mine.txt")
	gitOut(t, e.repo, "push", "origin", "fix/mine")
	gitOut(t, e.repo, "checkout", "main")
	if p := e.provenance(t, "fix/mine"); p != "task" {
		t.Fatalf("provenance %q, want task", p)
	}
	if st, body := e.upsert(t); st != http.StatusCreated {
		t.Fatalf("UpsertCard: %d %s", st, body)
	}
	for _, body := range []any{
		map[string]any{"comment": "no", "delete_branch": true},
		map[string]any{"comment": "no", "delete_branch": true, "confirm_delete_unmerged": "Fix/Mine"},
		map[string]any{"comment": "no", "delete_branch": true, "confirm_delete_unmerged": "yes"},
	} {
		if st, rb := e.board(t, "reject", body); st != http.StatusConflict {
			t.Errorf("reject %v: %d %s, want 409", body, st, rb)
		}
		if gitOut(t, e.repo, "ls-remote", "--heads", "origin", "refs/heads/fix/mine") == "" {
			t.Fatalf("unmerged fix/mine deleted without a named override (%v)", body)
		}
	}
	st, rb := e.board(t, "reject", map[string]any{"comment": "no", "delete_branch": true, "confirm_delete_unmerged": "fix/mine"})
	if st != http.StatusOK {
		t.Fatalf("reject with named override: %d %s", st, rb)
	}
	if gitOut(t, e.repo, "ls-remote", "--heads", "origin", "refs/heads/fix/mine") != "" {
		t.Fatal("fix/mine still on origin after the named override")
	}
}

// F3: a branch that already existed locally is foreign even when the agent
// checks it out inside the task's own worktree.
func TestSec249_PreexistingLocalBranchInTaskWorktreeIsForeign(t *testing.T) {
	e := newSec249Env(t)
	gitOut(t, e.repo, "checkout", "-b", "fix/human-local", "main")
	commitFile(t, e.repo, "human.txt")
	gitOut(t, e.repo, "checkout", "main")
	wt := filepath.Join(e.repo, ".worktrees", e.taskID)
	gitOut(t, e.repo, "worktree", "add", wt, "fix/human-local")
	e.register(t, "fix/human-local")
	if p := e.provenance(t, "fix/human-local"); p != "foreign" {
		t.Fatalf("provenance %q, want foreign", p)
	}
}
