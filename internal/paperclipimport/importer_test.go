package paperclipimport

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/paperclip"
	"github.com/VinnyVanGogh/staypoint/internal/paperclip/paperclipfake"
)

const (
	staID = "co-sta"
	manID = "co-man"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "import.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store.DB()
}

// seed builds two companies:
//
//	STA: 230 open top-level issues (more than one 200-row page and far more
//	     than tasks.max_children), a 4-level chain STA-1 > STA-2 > STA-3 > STA-4,
//	     a child of a done issue, a long description, and done/cancelled
//	     issues that must not import;
//	MAN: two issues in a project without a workspace.
func seed(t *testing.T) *paperclipfake.Server {
	t.Helper()
	fake := paperclipfake.New(t)
	fake.AddCompany(staID, "StayPoint", "STA")
	fake.AddCompany(manID, "Managed Solution", "MAN")
	fake.AddProject(staID, "p-ws", "Orchestrator", "/repos/agent-mesh", "")
	fake.AddProject(staID, "p-none", "Core", "", "")
	fake.AddProject(manID, "p-man", "Platform", "", "/repos/mansol")

	add := func(company, prefix string, n int, status, parent, project, desc string) {
		fake.AddIssue(paperclip.ImportIssue{
			ID: fmt.Sprintf("%s-%d", prefix, n), Identifier: fmt.Sprintf("%s-%d", prefix, n), IssueNumber: n,
			Title: fmt.Sprintf("Issue %d", n), Description: desc, Status: status, Priority: "high",
			CompanyID: company, ProjectID: project, ParentID: parent,
		})
	}
	add(staID, "STA", 1, "todo", "", "p-ws", "root")
	add(staID, "STA", 2, "in_progress", "STA-1", "p-ws", "child")
	add(staID, "STA", 3, "blocked", "STA-2", "p-ws", "grandchild")
	add(staID, "STA", 4, "in_review", "STA-3", "p-ws", "great-grandchild")
	add(staID, "STA", 5, "done", "", "p-ws", "closed parent")
	add(staID, "STA", 6, "backlog", "STA-5", "p-none", strings.Repeat("x", 3000))
	add(staID, "STA", 7, "cancelled", "", "p-none", "gone")
	for n := 100; n < 330; n++ {
		add(staID, "STA", n, "backlog", "", "p-none", "bulk")
	}
	add(manID, "MAN", 1, "todo", "", "p-man", "man one")
	add(manID, "MAN", 2, "blocked", "MAN-1", "p-man", "man two")
	return fake
}

func taskBySource(t *testing.T, conn *sql.DB, sourceID string) *meshContext.Task {
	t.Helper()
	var id string
	if err := conn.QueryRow(`SELECT id FROM tasks WHERE source_id = ?`, sourceID).Scan(&id); err != nil {
		t.Fatalf("task for %s: %v", sourceID, err)
	}
	task, err := meshContext.GetTask(conn, id)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestImport_EndToEnd(t *testing.T) {
	fake := seed(t)
	conn := openDB(t)
	client := paperclip.NewClient(fake.URL, "")
	ctx := context.Background()

	plan, err := BuildPlan(ctx, client, conn, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// 230 bulk + STA-1..4 + STA-6 = 235; MAN 2.
	if got := plan.ToImport(); got != 237 {
		t.Fatalf("to import = %d, want 237", got)
	}
	// 236 open STA issues need two 200-row pages; MAN one.
	if fake.ListCalls != 3 {
		t.Errorf("issue list calls = %d, want 3 (paged)", fake.ListCalls)
	}
	var sta CompanyPlan
	for _, c := range plan.Companies {
		if c.Company.IssuePrefix == "STA" {
			sta = c
		}
	}
	if sta.Organization != "StayPoint" || sta.Open != 235 || sta.Flattened != 2 || sta.ByStatus["done"] != 0 {
		t.Fatalf("STA plan: org=%s open=%d flattened=%d statuses=%v", sta.Organization, sta.Open, sta.Flattened, sta.ByStatus)
	}

	res, err := Apply(ctx, client, conn, plan, time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if res.ParentsCreated != 2 || res.TasksCreated["StayPoint"] != 235 || res.TasksCreated["Managed Solution"] != 2 {
		t.Fatalf("result: %+v", res)
	}
	if fake.DetailCalls != 1 {
		t.Errorf("detail calls = %d, want 1 (only the truncated description)", fake.DetailCalls)
	}

	parent := taskBySource(t, conn, staID)
	if parent.Name != "Paperclip backlog — StayPoint" || parent.Organization != "StayPoint" || parent.ExecutionStage != "backlog" ||
		parent.Origin != meshContext.OriginPaperclipImport || parent.RepoPath != "" || parent.ParentID != "" {
		t.Fatalf("company parent: %+v", parent)
	}
	var children int
	_ = conn.QueryRow(`SELECT COUNT(*) FROM tasks WHERE parent_id = ?`, parent.ID).Scan(&children)
	// STA-1, STA-6 (its parent is closed) and the 230 bulk issues.
	if children != 232 {
		t.Errorf("children of STA parent = %d, want 232 (max_children not applied to the import)", children)
	}
	if maxChildren, _ := meshContext.ChildTaskLimits(conn); maxChildren != meshContext.DefaultMaxChildren {
		t.Errorf("global max_children changed to %d", maxChildren)
	}

	s1, s2, s3, s4 := taskBySource(t, conn, "STA-1"), taskBySource(t, conn, "STA-2"), taskBySource(t, conn, "STA-3"), taskBySource(t, conn, "STA-4")
	if s1.ParentID != parent.ID || s2.ParentID != s1.ID {
		t.Errorf("nesting kept within the cap: STA-1 parent %s, STA-2 parent %s", s1.ParentID, s2.ParentID)
	}
	// Depth cap 2: STA-3 and STA-4 are flattened under STA-1 (depth 1).
	if s3.ParentID != s1.ID || s4.ParentID != s1.ID {
		t.Errorf("flattened: STA-3 parent %s, STA-4 parent %s, want %s", s3.ParentID, s4.ParentID, s1.ID)
	}
	if !strings.Contains(s3.Description, "child of STA-2") || !strings.Contains(s4.Description, "child of STA-3") {
		t.Errorf("flatten note missing: %q / %q", s3.Description, s4.Description)
	}
	if s1.Name != "[STA-1] Issue 1" || s1.SourceRef != "STA-1" || s1.Priority != "high" || s1.ExecutionStage != "backlog" ||
		s1.AssigneeAgentID != "" || s1.Origin != meshContext.OriginPaperclipImport || s1.Organization != "StayPoint" {
		t.Errorf("STA-1 fields: %+v", s1)
	}
	if s1.RepoPath != "/repos/agent-mesh" {
		t.Errorf("repo from project workspace: %q", s1.RepoPath)
	}
	s6 := taskBySource(t, conn, "STA-6")
	if s6.RepoPath != "" || len(s6.Description) != 3000 {
		t.Errorf("STA-6: repo %q (want empty), description %d chars (want full 3000)", s6.RepoPath, len(s6.Description))
	}
	m1 := taskBySource(t, conn, "MAN-1")
	if m1.RepoPath != "/repos/mansol" || m1.AccountRole != "work" || m1.Organization != "Managed Solution" {
		t.Errorf("MAN-1: %+v", m1)
	}
	for _, closed := range []string{"STA-5", "STA-7"} {
		var n int
		_ = conn.QueryRow(`SELECT COUNT(*) FROM tasks WHERE source_id = ?`, closed).Scan(&n)
		if n != 0 {
			t.Errorf("%s (done/cancelled) was imported", closed)
		}
	}

	// A repo-less import cannot leave backlog until the Board sets a repo.
	if err := meshContext.SetTaskExecutionStage(conn, s6.ID, "todo"); err == nil || !strings.Contains(err.Error(), "no repo") {
		t.Errorf("todo without repo: err = %v", err)
	}
	if _, err := meshContext.SetTaskRepo(conn, s6.ID, "/repos/agent-mesh", "main"); err != nil {
		t.Fatal(err)
	}
	if err := meshContext.SetTaskExecutionStage(conn, s6.ID, "todo"); err != nil {
		t.Errorf("todo after set-repo: %v", err)
	}

	// Idempotent: a second run plans and creates nothing.
	var before int
	_ = conn.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&before)
	plan2, err := BuildPlan(ctx, client, conn, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan2.ToImport() != 0 {
		t.Fatalf("re-run plans %d imports, want 0", plan2.ToImport())
	}
	res2, err := Apply(ctx, client, conn, plan2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var after int
	_ = conn.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&after)
	if after != before || res2.ParentsCreated != 0 {
		t.Fatalf("re-run created tasks: %d -> %d (%+v)", before, after, res2)
	}

	// A new Paperclip issue later is added under the existing parents.
	fake.AddIssue(paperclip.ImportIssue{ID: "STA-900", Identifier: "STA-900", IssueNumber: 900, Title: "Late", Status: "todo", CompanyID: staID, ParentID: "STA-2"})
	plan3, err := BuildPlan(ctx, client, conn, Options{Companies: []string{"sta"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan3.Companies) != 1 || plan3.ToImport() != 1 || plan3.Companies[0].ParentTaskID != parent.ID {
		t.Fatalf("plan3: %+v", plan3.Companies)
	}
	if _, err := Apply(ctx, client, conn, plan3, time.Now()); err != nil {
		t.Fatal(err)
	}
	// STA-2 is already at the depth cap, so the late child is flattened
	// under the already-imported STA-1.
	if late := taskBySource(t, conn, "STA-900"); late.ParentID != s1.ID || !strings.Contains(late.Description, "child of STA-2") {
		t.Errorf("late child parent = %s (want STA-1's task %s), description %q", late.ParentID, s1.ID, late.Description)
	}
}

func TestBuildPlan_ReadOnlyUnmigratedDB(t *testing.T) {
	fake := seed(t)
	dbPath := filepath.Join(t.TempDir(), "old.db")
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the deployed daemon's schema: no source columns yet.
	for _, stmt := range []string{`DROP INDEX idx_tasks_source_id`, `ALTER TABLE tasks DROP COLUMN source_ref`, `ALTER TABLE tasks DROP COLUMN source_id`} {
		if _, err := store.DB().Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	store.Close()

	ro, err := db.OpenReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	plan, err := BuildPlan(context.Background(), paperclip.NewClient(fake.URL, ""), ro, Options{Companies: []string{"MAN"}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ToImport() != 2 || plan.Companies[0].Organization != "Managed Solution" {
		t.Fatalf("plan: %+v", plan.Companies)
	}
	if _, err := Apply(context.Background(), paperclip.NewClient(fake.URL, ""), ro, plan, time.Now()); err == nil {
		t.Fatal("Apply must refuse an unmigrated database")
	}
	if _, err := ro.Exec(`INSERT INTO settings_kv (key, value) VALUES ('x', 'y')`); err == nil {
		t.Fatal("read-only connection accepted a write")
	}
}

func TestDepthCapOfOneFlattensEverything(t *testing.T) {
	fake := seed(t)
	conn := openDB(t)
	if err := meshContext.SetChildTaskLimits(conn, 10, 1); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), paperclip.NewClient(fake.URL, ""), conn, Options{Companies: []string{"MAN"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, pi := range plan.Companies[0].ToImport {
		if pi.ParentIssueID != "" {
			t.Errorf("%s nested under %s with max depth 1", pi.Issue.Identifier, pi.ParentIssueID)
		}
	}
	if plan.Companies[0].Flattened != 1 {
		t.Errorf("flattened = %d, want 1", plan.Companies[0].Flattened)
	}
}

func TestListIssues_FailsWhenServerIgnoresOffset(t *testing.T) {
	fake := seed(t)
	fake.IgnoreOffset = true
	_, err := paperclip.NewClient(fake.URL, "").ListIssues(context.Background(), staID, paperclip.OpenIssueStatuses)
	if err == nil || !strings.Contains(err.Error(), "not paging") {
		t.Fatalf("err = %v, want a paging error instead of an endless loop", err)
	}
}
