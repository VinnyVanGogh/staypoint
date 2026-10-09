package fleet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/paperclip"
)

// task-3387cad2: the sidebar's per-org number was task_counts.running. It
// showed RuneLite (0) and General (0) while each had a run going. These pin
// what "running" means and the faults found in how it was counted.

func liveTestDB(t *testing.T, rows string) *Aggregator {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err := store.DB().Exec(`INSERT INTO tasks (id, name, repo_path, organization, project, status, execution_stage, checkout_run_id) VALUES ` + rows); err != nil {
		t.Fatal(err)
	}
	return &Aggregator{DB: store.DB(), Now: func() time.Time { return time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC) }}
}

func orgByName(o *FleetOverview, name string) *OrgFleetSummary {
	for i := range o.Organizations {
		if o.Organizations[i].Name == name {
			return &o.Organizations[i]
		}
	}
	return nil
}

func taskByID(o *FleetOverview, id string) *TaskItem {
	for i := range o.Tasks {
		if o.Tasks[i].ID == id {
			return &o.Tasks[i]
		}
	}
	return nil
}

// Running is a run in flight: checkout_run_id set AND the daemon holds its
// slot. Stage in_progress alone (a stopped or crashed run) is not running.
func TestGather_RunningIsALiveRun(t *testing.T) {
	agg := liveTestDB(t, `
		('t-live', 'Live run', '/r', 'General', '', 'active', 'in_progress', 'run-a'),
		('t-stale', 'Stale checkout', '/r', 'General', '', 'active', 'in_progress', 'run-b'),
		('t-stage', 'Stage only', '/r', 'General', '', 'active', 'in_progress', NULL),
		('t-todo', 'Todo', '/r', 'General', '', 'active', 'todo', NULL)`)
	started := time.Date(2026, 10, 9, 11, 30, 0, 0, time.UTC)
	agg.LiveRuns = func() map[string]time.Time {
		// t-todo holds a slot with no checkout: a run between Acquire and
		// the claim UPDATE. Not running until the task is checked out.
		return map[string]time.Time{"t-live": started, "t-todo": started}
	}
	o, err := agg.Gather(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	g := orgByName(o, "General")
	if g == nil {
		t.Fatalf("no General org: %+v", o.Organizations)
	}
	if g.TaskCounts.Running != 1 || o.GlobalTasks.Running != 1 {
		t.Fatalf("running: org %d global %d, want 1 and 1", g.TaskCounts.Running, o.GlobalTasks.Running)
	}
	live := taskByID(o, "t-live")
	if !live.Running || live.Status != "running" || live.RunStartedAt != started.Format(time.RFC3339Nano) {
		t.Fatalf("t-live: running=%v status=%q started=%q", live.Running, live.Status, live.RunStartedAt)
	}
	for _, id := range []string{"t-stale", "t-stage", "t-todo"} {
		if ti := taskByID(o, id); ti.Running || ti.Status == "running" {
			t.Errorf("%s counted as running: %+v", id, ti)
		}
	}
	if stage := taskByID(o, "t-stale").ExecutionStage; stage != "in_progress" {
		t.Errorf("stage must still say in_progress, got %q", stage)
	}
}

// Without a LiveRuns source (the CLI) a checkout is the best evidence.
func TestGather_NoLiveSourceFallsBackToCheckout(t *testing.T) {
	agg := liveTestDB(t, `
		('t-a', 'A', '/r', 'General', '', 'active', 'in_progress', 'run-a'),
		('t-b', 'B', '/r', 'General', '', 'active', 'in_progress', NULL)`)
	o, err := agg.Gather(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n := orgByName(o, "General").TaskCounts.Running; n != 1 {
		t.Fatalf("running = %d, want 1", n)
	}
}

// paperclipStub serves one company with the given name, prefix and issues.
func paperclipStub(t *testing.T, name, prefix, issues string) *paperclip.Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/companies":
			_ = json.NewEncoder(w).Encode([]paperclip.CompanyResponse{{ID: "c1", Name: name, IssuePrefix: prefix, Status: "active"}})
		case strings.HasPrefix(r.URL.Path, "/api/companies/c1/issues"):
			_, _ = w.Write([]byte(issues))
		case strings.HasPrefix(r.URL.Path, "/api/companies/c1/"):
			_, _ = w.Write([]byte(`[]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)
	return paperclip.NewClient(ts.URL, "k")
}

// Two faults found in the aggregator: a Paperclip company ("RuneLite Plugins")
// was listed as its own org next to the org the import mapped it to
// ("RuneLite"), and a local task that shared a title with a Paperclip issue was
// dropped in favour of the frozen issue.
func TestGather_LocalTaskWinsAndPaperclipCompanyMergesIntoImportedOrg(t *testing.T) {
	agg := liveTestDB(t, `('t-rl', 'Ship the XP tracker', '/r', 'RuneLite', '', 'active', 'in_progress', 'run-rl')`)
	agg.LiveRuns = func() map[string]time.Time { return map[string]time.Time{"t-rl": {}} }
	agg.PaperclipClient = paperclipStub(t, "RuneLite Plugins", "RUN", `[
		{"id": "iss-1", "identifier": "RUN-1", "title": "Ship the XP tracker", "status": "todo"},
		{"id": "iss-2", "identifier": "RUN-2", "title": "Old Paperclip work", "status": "in_progress"}
	]`)
	o, err := agg.Gather(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if orgByName(o, "RuneLite Plugins") != nil {
		t.Fatalf("Paperclip company listed under its own name next to RuneLite: %+v", o.Organizations)
	}
	rl := orgByName(o, "RuneLite")
	if rl == nil {
		t.Fatal("no RuneLite org")
	}
	if rl.TaskCounts.Running != 1 {
		t.Fatalf("RuneLite running = %d, want 1 (the local run)", rl.TaskCounts.Running)
	}
	if ti := taskByID(o, "t-rl"); ti == nil || !ti.Running {
		t.Fatalf("local task missing or not running: %+v", ti)
	}
	if taskByID(o, "iss-1") != nil {
		t.Error("frozen Paperclip copy of a local task is still listed")
	}
	old := taskByID(o, "iss-2")
	if old == nil || old.Running || old.Status == "running" || old.ExecutionStage != "in_progress" || old.Origin != "legacy" {
		t.Fatalf("frozen in_progress issue: %+v", old)
	}
	// Legacy is left out of counts by default: only the local task counts.
	if rl.TaskCounts.Total != 1 {
		t.Errorf("RuneLite total = %d, want 1 (legacy Paperclip issues hidden)", rl.TaskCounts.Total)
	}
	all, err := agg.GatherWith(context.Background(), GatherOptions{IncludeHidden: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := orgByName(all, "RuneLite").TaskCounts; got.Total != 2 || got.Running != 1 {
		t.Errorf("include hidden: %+v, want total 2 running 1", got)
	}
}
