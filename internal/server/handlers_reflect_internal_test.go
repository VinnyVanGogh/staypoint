package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/config"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/reflection"
)

func newReflectFixture(t *testing.T) (*ReflectHandler, chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	store, err := db.Open(filepath.Join(dir, "staypoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	conn := store.DB()
	ts := time.Now().UTC().Add(-time.Hour).Format("2006-01-02T15:04:05.000Z")
	if _, err := conn.Exec(`INSERT INTO tasks (id, name, repo_path, organization, execution_stage, updated_at, created_at)
		VALUES ('task-r1','ship it','/r/x','StayPoint','done',?,?)`, ts, ts); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO task_comments (task_id, author, message, created_at) VALUES ('task-r1','board','stop guessing',?)`, ts); err != nil {
		t.Fatal(err)
	}
	h := NewReflectHandler(conn)
	h.loadFn = func() (*config.Config, error) {
		return &config.Config{DataDir: dir, TelemetryDBPath: filepath.Join(dir, "none.db")}, nil
	}
	release := make(chan struct{})
	h.runner = func(string) reflection.Runner {
		return func(ctx context.Context, prompt string) (string, error) {
			<-release
			return `{"themes":[{"text":"Shipping","sources":["e1"]}]}`, nil
		}
	}
	return h, release
}

func getReflect(t *testing.T, h *ReflectHandler, q string) (int, reflectResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Get(rec, httptest.NewRequest(http.MethodGet, "/api/reflect"+q, nil))
	var out reflectResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestReflectHandlerFactsAndSummaryJob(t *testing.T) {
	h, release := newReflectFixture(t)
	if code, _ := getReflect(t, h, "?since=bogus"); code != http.StatusBadRequest {
		t.Fatalf("bad period: %d", code)
	}
	code, out := getReflect(t, h, "?since=30d")
	if code != 200 || out.Facts == nil || out.Facts.TasksShippedN != 1 || out.Summary != nil {
		t.Fatalf("facts: %d %+v", code, out)
	}

	post := func() int {
		rec := httptest.NewRecorder()
		h.StartSummary(rec, httptest.NewRequest(http.MethodPost, "/api/reflect/summary?since=30d", nil))
		return rec.Code
	}
	if c := post(); c != http.StatusAccepted {
		t.Fatalf("start: %d", c)
	}
	if c := post(); c != http.StatusConflict {
		t.Fatalf("second start while running: %d", c)
	}
	if _, out := getReflect(t, h, "?since=30d"); out.Job == nil || !out.Job.Running {
		t.Fatalf("job state not reported: %+v", out.Job)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, out = getReflect(t, h, "?since=30d")
		if out.Summary != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if out.Summary == nil || len(out.Summary.Themes) != 1 || out.Summary.Themes[0].Sources[0].URL != "/tasks/task-r1#comment-1" {
		t.Fatalf("summary: %+v job=%+v", out.Summary, out.Job)
	}
	if out.Job.Running || out.Job.Error != "" {
		t.Fatalf("job: %+v", out.Job)
	}
}
