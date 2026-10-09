package server_test

import (
	gocontext "context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/server"
	"github.com/VinnyVanGogh/staypoint/internal/workspace"
)

// PUT /api/tasks/{id}/test/base lets the Playwright suite record a task base
// for a branch it built by hand. It must exist only on TestMode servers.

func startTestModeServer(t *testing.T, testMode bool) (database *sql.DB, baseURL, token string) {
	t.Helper()
	database = setupTestDB(t)
	token = "test-secret-token-1234567890abcdef"
	srv, err := server.New(server.Options{
		BindHost:        "127.0.0.1",
		AuthToken:       token,
		DB:              database,
		TelemetryDBPath: filepath.Join(t.TempDir(), "telemetry.db"),
		TestMode:        testMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := gocontext.WithTimeout(gocontext.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return database, srv.URL(), token
}

func TestSeedTaskBase_NotRoutedOutsideTestMode(t *testing.T) {
	_, baseURL, token := startTestModeServer(t, false)
	body, _ := json.Marshal(map[string]string{"sha": "0000000000000000000000000000000000000000"})
	resp, rb := shipDoReq(t, &http.Client{}, token, "PUT", baseURL+"/api/tasks/task-00000000/test/base", body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		t.Fatalf("PUT test/base without TestMode = %d %s, want refused", resp.StatusCode, rb)
	}
}

func TestSeedTaskBase_RecordsBaseInTestMode(t *testing.T) {
	database, baseURL, token := startTestModeServer(t, true)
	client := &http.Client{}
	taskID, repoDir := createShipTask(t, database, baseURL, token, client)

	// Forget the base createShipTask recorded, as a hand-built spec branch has none.
	if _, err := database.Exec(`DELETE FROM task_worktree_bases WHERE task_id = ?`, taskID); err != nil {
		t.Fatal(err)
	}
	gitOut(t, repoDir, "update-ref", "-d", workspace.BaseRef(taskID))
	if _, err := workspace.RecordedTaskBase(gocontext.Background(), database, taskID); !errors.Is(err, workspace.ErrNoTaskBase) {
		t.Fatalf("RecordedTaskBase after delete = %v, want ErrNoTaskBase", err)
	}

	url := baseURL + "/api/tasks/" + taskID + "/test/base"
	bad, _ := json.Marshal(map[string]string{"sha": "not-a-sha"})
	if resp, rb := shipDoReq(t, client, token, "PUT", url, bad); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad sha = %d %s, want 400", resp.StatusCode, rb)
	}

	mainTip := gitOut(t, repoDir, "rev-parse", "main")
	good, _ := json.Marshal(map[string]string{"sha": mainTip, "target_branch": "main"})
	if resp, rb := shipDoReq(t, client, token, "PUT", url, good); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT test/base = %d %s", resp.StatusCode, rb)
	}
	got, err := workspace.VerifiedBase(gocontext.Background(), database, repoDir, taskID)
	if err != nil || got != mainTip {
		t.Fatalf("VerifiedBase = %q, %v; want %s", got, err, mainTip)
	}
}
