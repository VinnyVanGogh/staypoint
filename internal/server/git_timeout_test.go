package server_test

// STA-685: after a redeploy, every git child the daemon spawned in a client
// repo blocked forever in open() (macOS privacy prompt). Requests that shell
// out to git hung until the browser gave up. These tests swap in a `git` that
// never returns and check that each git-backed endpoint answers promptly with
// an error, and that Approve fails closed instead of skipping the migration
// gate.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

// hangGit puts a `git` that never returns first on PATH and shortens the
// daemon's git timeout so the test runs in well under a second per call.
func hangGit(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	// No `exec`: the orphaned sleep keeps stdout open after sh is killed, which
	// is the worst case for a caller that waits on the pipe.
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(gitexec.TimeoutEnv, "300ms")
	t.Setenv(gitexec.SlowTimeoutEnv, "300ms")
}

// timedReq runs shipDoReq on a client with a hard deadline, so a hang fails
// the test instead of stalling it.
func timedReq(t *testing.T, token, method, url string, opts ...string) (*http.Response, []byte, time.Duration) {
	t.Helper()
	client := &http.Client{Timeout: 8 * time.Second}
	start := time.Now()
	resp, body := shipDoReq(t, client, token, method, url, nil, opts...)
	return resp, body, time.Since(start)
}

func TestGitHang_ShipReviewGetCardAnswers(t *testing.T) {
	_, baseURL, token, _, taskID, _, _ := shipApproveServer(t)
	hangGit(t)

	resp, body, took := timedReq(t, token, "GET", baseURL+"/api/tasks/"+taskID+"/ship-review")
	if took > 5*time.Second {
		t.Fatalf("GET ship-review took %s", took)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET ship-review: %d %s", resp.StatusCode, body)
	}
	var card map[string]any
	if err := json.Unmarshal(body, &card); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if card["status"] != shipreview.StatusPending {
		t.Errorf("card status = %v, want pending", card["status"])
	}
	if msg, _ := card["repo_error"].(string); !strings.Contains(msg, "timed out") {
		t.Errorf("repo_error = %q, want a git timeout message", msg)
	}
}

func TestGitHang_MigrationsReturns504(t *testing.T) {
	_, baseURL, token, _, taskID, _, _ := shipApproveServer(t)
	hangGit(t)

	resp, body, took := timedReq(t, token, "GET", baseURL+"/api/tasks/"+taskID+"/migrations")
	if took > 5*time.Second {
		t.Fatalf("GET migrations took %s", took)
	}
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("GET migrations: %d %s, want 504", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "timed out") {
		t.Errorf("body %s does not mention the timeout", body)
	}
}

func TestGitHang_DiffReturns504(t *testing.T) {
	_, baseURL, token, _, taskID, _, _ := shipApproveServer(t)
	hangGit(t)

	resp, body, took := timedReq(t, token, "GET", baseURL+"/api/tasks/"+taskID+"/diff")
	if took > 5*time.Second {
		t.Fatalf("GET diff took %s", took)
	}
	if resp.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("GET diff: %d %s, want 504", resp.StatusCode, body)
	}
}

func TestGitHang_ApproveFailsClosed(t *testing.T) {
	database, baseURL, token, boardToken, taskID, _, _ := shipApproveServer(t)
	hangGit(t)

	resp, body, took := timedReq(t, token, "POST", baseURL+"/api/tasks/"+taskID+"/ship-review/approve", boardToken, "", "mock-assertion")
	if took > 5*time.Second {
		t.Fatalf("Approve took %s", took)
	}
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("Approve succeeded while git was unreachable: %s", body)
	}
	if !strings.Contains(string(body), "timed out") {
		t.Errorf("Approve body %s does not mention the timeout", body)
	}
	card, err := shipreview.GetCard(database, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if card.Status != shipreview.StatusPending {
		t.Errorf("card status = %s, want pending", card.Status)
	}
}
