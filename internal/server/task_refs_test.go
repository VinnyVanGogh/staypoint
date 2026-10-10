package server

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
)

func taskRefFixture(t *testing.T) (*sql.DB, *context.Task, *context.Task, *context.Task) {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	conn := store.DB()
	mk := func(name, org, sourceRef string) *context.Task {
		task, err := context.CreateTaskWithOptions(conn, context.TaskCreateOptions{
			Name: name, RepoPath: "/tmp/r", GitBranch: "main", AccountRole: "personal",
			Organization: org, Project: "CI & Testing", ExecutionStage: "backlog", SourceRef: sourceRef,
		})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	// STA-1 carries the Paperclip label STA-775; MAN-1 is in its own sequence.
	sta := mk("Playwright UI specs", "StayPoint", "STA-775")
	man := mk("Work task", "Managed Solution", "MAN-602")
	sta2 := mk("Second task", "StayPoint", "")
	return conn, sta, man, sta2
}

func serveUI(t *testing.T, conn *sql.DB, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	RegisterUIRoutes(mux, "tok", conn)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func TestTaskPage_UnknownReference404(t *testing.T) {
	conn, _, _, _ := taskRefFixture(t)
	for _, p := range []string{"/STA-99", "/STA-99/whatever", "/MAN-2", "/RES-1"} {
		if w := serveUI(t, conn, p); w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, w.Code)
		}
	}
}

func TestTaskPage_RedirectsToCanonical(t *testing.T) {
	conn, sta, man, _ := taskRefFixture(t)
	canonical := "/STA-1/playwright-ui-specs"
	for path, want := range map[string]string{
		"/STA-1/wrong-slug":          canonical,
		"/sta-1":                     canonical,
		"/sta-1/playwright-ui-specs": canonical,
		// Legacy label with no task numbered 775: the alias.
		"/STA-775": canonical,
		"/MAN-602": "/MAN-1/work-task",
		// Old URL forms, project with spaces and & in the path.
		"/tasks/STA/CI%20%26%20Testing/" + sta.ID: canonical,
		"/tasks/" + sta.ID:                        canonical,
		"/tasks/STA/default/STA-775":              canonical,
		"/tasks/MAN/default/" + man.ID:            "/MAN-1/work-task",
		"/issues/STA/default/STA-1":               canonical,
		"/STA-1/wrong?token=abc":                  canonical + "?token=abc",
	} {
		w := serveUI(t, conn, path)
		if w.Code != http.StatusMovedPermanently {
			t.Errorf("GET %s = %d, want 301", path, w.Code)
			continue
		}
		if got := w.Header().Get("Location"); got != want {
			t.Errorf("GET %s -> %q, want %q", path, got, want)
		}
	}
}

func TestTaskPage_ServesCanonicalAndFleetPaths(t *testing.T) {
	conn, _, _, _ := taskRefFixture(t)
	for _, p := range []string{
		"/STA-1", "/STA-1/playwright-ui-specs", "/STA-2/second-task", "/MAN-1/work-task",
		// Not daemon tasks: served for the client-side fleet lookup, as before.
		"/tasks/RHI/default/RHI-task-e", "/tasks/STA/default/task-ffffffff", "/tasks",
	} {
		if w := serveUI(t, conn, p); w.Code != http.StatusOK {
			t.Errorf("GET %s = %d (Location %q), want 200", p, w.Code, w.Header().Get("Location"))
		}
	}
	if w := serveUI(t, conn, "/STA-1/a/b"); w.Code != http.StatusNotFound {
		t.Errorf("GET /STA-1/a/b = %d, want 404 (not a task page)", w.Code)
	}
}

func TestAPI_TaskReferenceInPath(t *testing.T) {
	conn, sta, man, _ := taskRefFixture(t)
	mux := http.NewServeMux()
	var gotID, gotTail string
	mux.HandleFunc("GET /api/tasks/{id}", func(w http.ResponseWriter, r *http.Request) { gotID, gotTail = r.PathValue("id"), "" })
	mux.HandleFunc("GET /api/tasks/{id}/comments", func(w http.ResponseWriter, r *http.Request) { gotID, gotTail = r.PathValue("id"), "comments" })
	h := resolveTaskRefPaths(conn, mux)
	for path, want := range map[string][2]string{
		"/api/tasks/STA-1":          {sta.ID, ""},
		"/api/tasks/man-1/comments": {man.ID, "comments"},
		"/api/tasks/STA-775":        {sta.ID, ""},
		"/api/tasks/" + sta.ID:      {sta.ID, ""},
		// Unknown: left alone for the handler to 404.
		"/api/tasks/STA-99": {"STA-99", ""},
	} {
		gotID, gotTail = "", "-"
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
		if gotID != want[0] || gotTail != want[1] {
			t.Errorf("%s -> id %q tail %q, want %q %q", path, gotID, gotTail, want[0], want[1])
		}
	}
}
