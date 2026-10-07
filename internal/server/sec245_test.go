package server_test

// #245: registered branches over HTTP. A case variant of a protected branch
// is refused at registration, and Approve deletes only a branch this task
// owns (its staypoint/<task> branch, or one it registered before the branch
// existed); any other branch is merged but left in place with a warning.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func registerBranchHTTP(t *testing.T, client *http.Client, baseURL, token, taskID, ref string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"type": "branch", "ref": ref})
	resp, rb := shipDoReq(t, client, token, "POST", baseURL+"/api/tasks/"+taskID+"/work-products", body)
	return resp.StatusCode, string(rb)
}

func commitFile(t *testing.T, repo, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repo, "add", name)
	gitOut(t, repo, "-c", "user.email=t@t.com", "-c", "user.name=test", "commit", "-m", "add "+name)
}

func TestSec245_RegistrationRefusesCaseVariantOfProtectedBranch(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	client := &http.Client{}
	taskID, repoDir := createShipTask(t, database, srv.URL(), token, client)
	gitOut(t, repoDir, "branch", "dev-server", "main")
	gitOut(t, repoDir, "push", "origin", "dev-server")

	for _, ref := range []string{"Main", "MAIN", "origin/Main", "Master", "Dev-Server", "DEV-SERVER", "HEAD", "a^b"} {
		if st, body := registerBranchHTTP(t, client, srv.URL(), token, taskID, ref); st != http.StatusBadRequest {
			t.Errorf("register branch %q: %d %s, want 400", ref, st, body)
		}
	}
	var n int
	_ = database.QueryRow(`SELECT COUNT(*) FROM task_work_products WHERE task_id = ? AND product_type = 'branch'`, taskID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d refused branch registrations were stored", n)
	}
}

// approveRegisteredBranch makes the task's own branch empty so the card ships
// the registered branch, then builds the card and approves it.
func approveRegisteredBranch(t *testing.T, register func(client *http.Client, baseURL, token, taskID, repoDir string) string) (res approveResult, repoDir, branch string) {
	t.Helper()
	database := setupTestDB(t)
	seedBoardWebAuthnCredential(t, database)
	srv, token := startTestServer(t, database)
	any(srv).(webAuthnVerifierSetter).SetWebAuthnVerifier(func(_ *http.Request, _ string) error { return nil })
	baseURL, boardToken, client := srv.URL(), srv.BoardToken(), &http.Client{}

	taskID, repoDir := createShipTask(t, database, baseURL, token, client)
	// The task's own branch has no changes: the card ships the registered one.
	gitOut(t, repoDir, "branch", "-f", "staypoint/"+taskID, "main")
	gitOut(t, repoDir, "push", "-f", "origin", "staypoint/"+taskID)

	branch = register(client, baseURL, token, taskID, repoDir)

	upsertBody, _ := json.Marshal(map[string]any{"test_steps": []string{"1. Open /"}})
	resp, rb := shipDoReq(t, client, token, "PUT", baseURL+"/api/tasks/"+taskID+"/ship-review", upsertBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("UpsertCard: %d %s", resp.StatusCode, rb)
	}
	var card struct {
		Branch string `json:"branch"`
	}
	if err := json.Unmarshal(rb, &card); err != nil || card.Branch != branch {
		t.Fatalf("card branch = %q (err %v), want %q: %s", card.Branch, err, branch, rb)
	}
	resp, rb = shipDoReq(t, client, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/approve", nil, boardToken, "", "mock-assertion")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Approve: %d %s", resp.StatusCode, rb)
	}
	if err := json.Unmarshal(rb, &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return res, repoDir, branch
}

// A branch that already existed (someone else's) is merged but not deleted.
func TestSec245_ApproveKeepsBranchTheTaskDoesNotOwn(t *testing.T) {
	res, repoDir, branch := approveRegisteredBranch(t, func(client *http.Client, baseURL, token, taskID, repoDir string) string {
		gitOut(t, repoDir, "checkout", "-b", "fix/someone-elses", "main")
		commitFile(t, repoDir, "theirs.txt")
		gitOut(t, repoDir, "push", "origin", "fix/someone-elses")
		gitOut(t, repoDir, "checkout", "main")
		if st, body := registerBranchHTTP(t, client, baseURL, token, taskID, "fix/someone-elses"); st != http.StatusCreated {
			t.Fatalf("register: %d %s", st, body)
		}
		return "fix/someone-elses"
	})
	if res.BranchDeleted || res.Card.BranchDeleted {
		t.Fatalf("a branch the task does not own was deleted: %+v", res)
	}
	want := "branch " + branch + " is not owned by this task; not deleted"
	if !strings.Contains(res.BranchDeleteError, want) || !strings.Contains(res.Card.BranchDeleteError, want) {
		t.Fatalf("branch_delete_error = %q / card %q, want %q", res.BranchDeleteError, res.Card.BranchDeleteError, want)
	}
	if gitOut(t, repoDir, "ls-remote", "--heads", "origin", "refs/heads/"+branch) == "" {
		t.Fatalf("remote %s deleted", branch)
	}
	if gitOut(t, repoDir, "branch", "--list", branch) == "" {
		t.Fatalf("local %s deleted", branch)
	}
}

// A branch the task registered before it existed, then created and pushed,
// is the task's own and is deleted after the merge.
func TestSec245_ApproveDeletesBranchTheTaskCreated(t *testing.T) {
	res, repoDir, branch := approveRegisteredBranch(t, func(client *http.Client, baseURL, token, taskID, repoDir string) string {
		if st, body := registerBranchHTTP(t, client, baseURL, token, taskID, "fix/mine"); st != http.StatusCreated {
			t.Fatalf("register: %d %s", st, body)
		}
		gitOut(t, repoDir, "checkout", "-b", "fix/mine", "main")
		commitFile(t, repoDir, "mine.txt")
		gitOut(t, repoDir, "push", "origin", "fix/mine")
		gitOut(t, repoDir, "checkout", "main")
		return "fix/mine"
	})
	if !res.BranchDeleted || res.BranchDeleteError != "" {
		t.Fatalf("own branch not deleted: %+v", res)
	}
	if gitOut(t, repoDir, "ls-remote", "--heads", "origin", "refs/heads/"+branch) != "" {
		t.Fatalf("remote %s still present", branch)
	}
}
