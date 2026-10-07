package server_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
)

type docSummary struct {
	TaskID        string `json:"task_id"`
	TaskName      string `json:"task_name"`
	Organization  string `json:"organization"`
	DocKey        string `json:"doc_key"`
	LatestVersion int    `json:"latest_version"`
	VersionCount  int    `json:"version_count"`
	Size          int    `json:"size"`
	Versions      []struct {
		Version int `json:"version"`
		Size    int `json:"size"`
	} `json:"versions"`
}

func docsGet(t *testing.T, base, token, path string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw json.RawMessage
	_ = json.NewDecoder(resp.Body).Decode(&raw)
	return resp.StatusCode, raw
}

func docsList(t *testing.T, base, token, path string) []docSummary {
	t.Helper()
	code, raw := docsGet(t, base, token, path)
	if code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, code, raw)
	}
	var out struct {
		Documents []docSummary `json:"documents"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("GET %s: decode: %v (%s)", path, err, raw)
	}
	if out.Documents == nil {
		t.Fatalf("GET %s: documents must be a JSON array, got %s", path, raw)
	}
	return out.Documents
}

func seedDocTask(t *testing.T, database *sql.DB, name, org, kind string) *meshContext.Task {
	t.Helper()
	task, err := meshContext.CreateTaskWithOptions(database, meshContext.TaskCreateOptions{
		Name: name, RepoPath: "/repo/x", GitBranch: "main", AccountRole: "personal",
		Organization: org, Project: "core", WorkKind: kind, Description: "brief for " + name,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func addDoc(t *testing.T, database *sql.DB, taskID, key, content string) {
	t.Helper()
	if err := meshContext.AddTaskDocument(database, taskID, key, content); err != nil {
		t.Fatal(err)
	}
}

func TestDocumentsAPI_ErrorsAndEmpty(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())
	task := seedDocTask(t, database, "empty plan", "acme", "planning")

	// Unauthenticated callers get nothing.
	req, _ := http.NewRequest(http.MethodGet, base+"/api/documents", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", resp.StatusCode)
	}

	// A task with only a description lists no documents, as an empty array.
	if docs := docsList(t, base, token, "/api/tasks/"+task.ID+"/documents"); len(docs) != 0 {
		t.Errorf("description must not be listed as an artifact: %+v", docs)
	}
	if docs := docsList(t, base, token, "/api/documents"); len(docs) != 0 {
		t.Errorf("cross-task list must skip descriptions: %+v", docs)
	}

	cases := []struct {
		path string
		want int
	}{
		{"/api/tasks/no-such-task/documents", http.StatusNotFound},
		{"/api/tasks/" + task.ID + "/documents/plan", http.StatusNotFound},
		{"/api/tasks/" + task.ID + "/documents/plan?version=0", http.StatusBadRequest},
		{"/api/tasks/" + task.ID + "/documents/plan?version=abc", http.StatusBadRequest},
		{"/api/documents?task=no-such-task", http.StatusNotFound},
	}
	for _, c := range cases {
		if code, raw := docsGet(t, base, token, c.path); code != c.want {
			t.Errorf("GET %s: got %d (%s), want %d", c.path, code, raw, c.want)
		}
	}
}

func TestDocumentsAPI_TaskListingVersionsAndContent(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())
	task := seedDocTask(t, database, "plan auth", "acme", "planning")
	addDoc(t, database, task.ID, "plan", "# Plan v1")
	addDoc(t, database, task.ID, "plan", "# Plan v2 é")
	addDoc(t, database, task.ID, "architecture", "## Arch")

	docs := docsList(t, base, token, "/api/tasks/"+task.ID+"/documents")
	if len(docs) != 2 || docs[0].DocKey != "architecture" || docs[1].DocKey != "plan" {
		t.Fatalf("want architecture, plan sorted by key; got %+v", docs)
	}
	plan := docs[1]
	if plan.LatestVersion != 2 || plan.VersionCount != 2 || len(plan.Versions) != 2 {
		t.Fatalf("plan versions: %+v", plan)
	}
	if plan.Versions[0].Version != 2 || plan.Versions[1].Version != 1 {
		t.Errorf("versions must be newest first: %+v", plan.Versions)
	}
	// Size is bytes of the latest version ("é" is two bytes).
	if want := len("# Plan v2 é"); plan.Size != want || plan.Versions[0].Size != want {
		t.Errorf("size: got %d / %d, want %d", plan.Size, plan.Versions[0].Size, want)
	}

	var doc meshContext.TaskDocument
	code, raw := docsGet(t, base, token, "/api/tasks/"+task.ID+"/documents/plan")
	if code != http.StatusOK || json.Unmarshal(raw, &doc) != nil || doc.Version != 2 || doc.Content != "# Plan v2 é" {
		t.Errorf("latest plan: %d %s", code, raw)
	}
	code, raw = docsGet(t, base, token, "/api/tasks/"+task.ID+"/documents/plan?version=1")
	if code != http.StatusOK || json.Unmarshal(raw, &doc) != nil || doc.Version != 1 || doc.Content != "# Plan v1" {
		t.Errorf("plan v1: %d %s", code, raw)
	}
	if code, raw := docsGet(t, base, token, "/api/tasks/"+task.ID+"/documents/plan?version=9"); code != http.StatusNotFound {
		t.Errorf("missing version: %d %s", code, raw)
	}
}

func TestDocumentsAPI_CrossTaskFilters(t *testing.T) {
	database := setupTestDB(t)
	srv, token := startTestServer(t, database)
	base := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())
	a := seedDocTask(t, database, "acme plan", "Acme", "planning")
	b := seedDocTask(t, database, "other plan", "Other", "architecture")
	gone := seedDocTask(t, database, "deleted plan", "Acme", "planning")
	addDoc(t, database, a.ID, "plan", "a plan")
	addDoc(t, database, a.ID, "bundle", "a bundle")
	addDoc(t, database, b.ID, "plan", "b plan")
	addDoc(t, database, gone.ID, "plan", "gone")
	if _, err := database.Exec(`UPDATE tasks SET deleted_at = '2026-10-07T00:00:00Z', status = 'soft_deleted' WHERE id = ?`, gone.ID); err != nil {
		t.Fatal(err)
	}

	keys := func(docs []docSummary) map[string]bool {
		m := map[string]bool{}
		for _, d := range docs {
			m[d.TaskID+"/"+d.DocKey] = true
		}
		return m
	}

	all := keys(docsList(t, base, token, "/api/documents"))
	if len(all) != 3 || !all[a.ID+"/plan"] || !all[a.ID+"/bundle"] || !all[b.ID+"/plan"] {
		t.Errorf("all: %v (soft-deleted task must be hidden)", all)
	}
	// org matches case-insensitively.
	acme := docsList(t, base, token, "/api/documents?org=acme")
	if got := keys(acme); len(got) != 2 || !got[a.ID+"/plan"] || !got[a.ID+"/bundle"] {
		t.Errorf("org=acme: %v", got)
	}
	if acme[0].TaskName != "acme plan" || acme[0].Organization != "Acme" {
		t.Errorf("summary carries task name and org: %+v", acme[0])
	}
	if got := keys(docsList(t, base, token, "/api/documents?kind=plan")); len(got) != 2 || !got[a.ID+"/plan"] || !got[b.ID+"/plan"] {
		t.Errorf("kind=plan: %v", got)
	}
	if got := keys(docsList(t, base, token, "/api/documents?task="+b.ID)); len(got) != 1 || !got[b.ID+"/plan"] {
		t.Errorf("task=b: %v", got)
	}
	if got := keys(docsList(t, base, token, "/api/documents?org=Other&kind=bundle")); len(got) != 0 {
		t.Errorf("org=Other&kind=bundle: %v", got)
	}
	// Briefs are listed only when asked for by kind.
	if got := keys(docsList(t, base, token, "/api/documents?kind=description")); len(got) != 2 {
		t.Errorf("kind=description: %v", got)
	}
}
