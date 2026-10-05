package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/repoaccess"
	"github.com/VinnyVanGogh/staypoint/internal/server"
)

type fakeRepoAccess struct{ snap repoaccess.Snapshot }

func (f fakeRepoAccess) Snapshot() repoaccess.Snapshot { return f.snap }

type healthBody struct {
	Status     string              `json:"status"`
	GitCommit  string              `json:"git_commit"`
	RepoAccess repoaccess.Snapshot `json:"repo_access"`
}

func getHealth(t *testing.T, opts server.Options) healthBody {
	t.Helper()
	token := "test-secret-token-1234567890abcdef"
	opts.BindHost = "127.0.0.1"
	opts.AuthToken = token
	opts.TelemetryDBPath = filepath.Join(t.TempDir(), "t.db")
	srv, err := server.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	req, _ := http.NewRequest(http.MethodGet, srv.URL()+"/api/health", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status %d", resp.StatusCode)
	}
	var body healthBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("health is not JSON: %v", err)
	}
	return body
}

func TestHealth_ReportsInaccessibleRepoPaths(t *testing.T) {
	blocked := repoaccess.Result{
		Path:   "/Users/x/Documents/dev/repo",
		Status: repoaccess.StatusTimeout,
		Detail: "staypointd can't access /Users/x/Documents/dev/repo: macOS permission needed",
	}
	body := getHealth(t, server.Options{
		GitCommit:  "abc1234",
		RepoAccess: fakeRepoAccess{repoaccess.Snapshot{Checked: true, Inaccessible: []repoaccess.Result{blocked}}},
	})
	if body.Status != "ok" || body.GitCommit != "abc1234" {
		t.Fatalf("health lost its existing fields: %+v", body)
	}
	if !body.RepoAccess.Checked {
		t.Fatal("repo_access.checked = false")
	}
	if len(body.RepoAccess.Inaccessible) != 1 || body.RepoAccess.Inaccessible[0].Path != blocked.Path ||
		body.RepoAccess.Inaccessible[0].Status != repoaccess.StatusTimeout {
		t.Fatalf("repo_access.inaccessible = %+v, want the blocked path", body.RepoAccess.Inaccessible)
	}
}

func TestHealth_RepoAccessUncheckedWithoutChecker(t *testing.T) {
	body := getHealth(t, server.Options{GitCommit: "abc1234"})
	if body.RepoAccess.Checked || len(body.RepoAccess.Inaccessible) != 0 {
		t.Fatalf("repo_access = %+v, want unchecked and empty", body.RepoAccess)
	}
}
