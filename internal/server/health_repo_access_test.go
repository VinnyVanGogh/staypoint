package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/repoaccess"
	"github.com/VinnyVanGogh/staypoint/internal/server"
)

type fakeRepoAccess struct{ snap repoaccess.Snapshot }

func (f fakeRepoAccess) Snapshot() repoaccess.Snapshot { return f.snap }

const healthToken = "test-secret-token-1234567890abcdef"

func startHealthServer(t *testing.T, ra server.RepoAccessReporter) *server.Server {
	t.Helper()
	srv, err := server.New(server.Options{
		BindHost:        "127.0.0.1",
		AuthToken:       healthToken,
		TelemetryDBPath: filepath.Join(t.TempDir(), "t.db"),
		GitCommit:       "abc1234",
		RepoAccess:      ra,
	})
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
	return srv
}

func getJSON(t *testing.T, srv *server.Server, path string, auth bool) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL()+path, nil)
	if auth {
		req.Header.Set("Authorization", "Bearer "+healthToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func sampleSnapshot() repoaccess.Snapshot {
	at := time.Date(2026, 10, 5, 14, 50, 0, 0, time.UTC)
	ok := repoaccess.Result{Path: "/repos/R&D <main>", Label: "R&D", OK: true, Cause: repoaccess.CauseOK, Message: "R&D: /repos/R&D <main> is reachable", CheckedAt: at}
	blocked := repoaccess.Result{
		Path: "/Users/x/Documents/dev/rhizome_site", Label: "rhizome-site",
		Cause: repoaccess.CauseBlocked, Step: repoaccess.StepOpen,
		Message:   "rhizome-site: can't read /Users/x/Documents/dev/rhizome_site: open(/Users/x/Documents/dev/rhizome_site) blocked for 3s (possible causes: macOS privacy prompt pending, file provider, network mount) [step open; staypointd uid 501 /bin/staypointd; 2026-10-05T14:50:00Z]",
		DaemonUID: 501, Executable: "/bin/staypointd", CheckedAt: at,
	}
	return repoaccess.Snapshot{Checked: true, CheckedAt: &at, DaemonUID: 501, Executable: "/bin/staypointd",
		Repos: []repoaccess.Result{ok, blocked}}
}

func TestHealth_ReportsFailingReposWithTheirMessage(t *testing.T) {
	snap := sampleSnapshot()
	srv := startHealthServer(t, fakeRepoAccess{snap})
	code, raw := getJSON(t, srv, "/api/health", true)
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	var body struct {
		Status     string `json:"status"`
		GitCommit  string `json:"git_commit"`
		RepoAccess struct {
			Checked      bool                `json:"checked"`
			Inaccessible []repoaccess.Result `json:"inaccessible"`
		} `json:"repo_access"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("health is not JSON: %v: %s", err, raw)
	}
	if body.Status != "ok" || body.GitCommit != "abc1234" || !body.RepoAccess.Checked {
		t.Fatalf("health lost existing fields: %s", raw)
	}
	if len(body.RepoAccess.Inaccessible) != 1 || body.RepoAccess.Inaccessible[0].Message != snap.Repos[1].Message {
		t.Fatalf("inaccessible = %+v, want only the blocked repo with its message", body.RepoAccess.Inaccessible)
	}
	// Messages carry paths and parentheses; keep them readable for the shell.
	if strings.Contains(string(raw), `\u003c`) || strings.Contains(string(raw), `\u0026`) {
		t.Errorf("health HTML-escapes its JSON: %s", raw)
	}
}

func TestHealthRepos_ListsEveryRepoWithDaemonEvidence(t *testing.T) {
	snap := sampleSnapshot()
	srv := startHealthServer(t, fakeRepoAccess{snap})
	code, raw := getJSON(t, srv, "/api/health/repos", true)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	var body struct {
		Checked    bool                `json:"checked"`
		CheckedAt  *time.Time          `json:"checked_at"`
		DaemonUID  int                 `json:"daemon_uid"`
		Executable string              `json:"executable"`
		Repos      []repoaccess.Result `json:"repos"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("not JSON: %v: %s", err, raw)
	}
	if !body.Checked || body.CheckedAt == nil || body.DaemonUID != 501 || body.Executable != "/bin/staypointd" {
		t.Fatalf("repos header = %s", raw)
	}
	if len(body.Repos) != 2 || !body.Repos[0].OK || body.Repos[1].Cause != repoaccess.CauseBlocked ||
		body.Repos[1].Step != repoaccess.StepOpen || body.Repos[1].Message != snap.Repos[1].Message {
		t.Fatalf("repos = %+v", body.Repos)
	}
}

func TestHealthRepos_RequiresAuth(t *testing.T) {
	srv := startHealthServer(t, fakeRepoAccess{sampleSnapshot()})
	if code, _ := getJSON(t, srv, "/api/health/repos", false); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api/health/repos = %d, want 401", code)
	}
}

func TestHealth_NoCheckedAtBeforeFirstCheck(t *testing.T) {
	srv := startHealthServer(t, fakeRepoAccess{repoaccess.Snapshot{}})
	for _, p := range []string{"/api/health", "/api/health/repos"} {
		_, raw := getJSON(t, srv, p, true)
		if strings.Contains(string(raw), "checked_at") {
			t.Fatalf("%s reports a check time before any check: %s", p, raw)
		}
	}
}

func TestHealth_RepoAccessUncheckedWithoutChecker(t *testing.T) {
	srv := startHealthServer(t, nil)
	_, raw := getJSON(t, srv, "/api/health", true)
	if !strings.Contains(string(raw), `"repo_access":{"checked":false,"inaccessible":[]}`) {
		t.Fatalf("health without a checker = %s", raw)
	}
	code, raw := getJSON(t, srv, "/api/health/repos", true)
	if code != http.StatusOK || !strings.Contains(string(raw), `"checked":false`) {
		t.Fatalf("/api/health/repos without a checker = %d %s", code, raw)
	}
}
