package context

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/db"
)

func createParked(t *testing.T, store *db.Store, name, org string) *Task {
	t.Helper()
	task, err := CreateTaskWithOptions(store.DB(), TaskCreateOptions{
		Name: name, RepoPath: t.TempDir(), GitBranch: "main", AccountRole: "personal",
		Organization: org, ExecutionStage: "backlog",
	})
	if err != nil {
		t.Fatalf("create %q: %v", name, err)
	}
	return task
}

// Concurrent creates through separate connections (two processes: the CLI
// and the daemon) each get a distinct number, with no gaps.
func TestCreateTask_ConcurrentCreatesGetDistinctNumbers(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mesh.db")
	const conns, perConn = 4, 10
	stores := make([]*db.Store, conns)
	for i := range stores {
		s, err := db.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		stores[i] = s
	}
	var wg sync.WaitGroup
	errs := make(chan error, conns*perConn)
	nums := make(chan int, conns*perConn)
	for i, s := range stores {
		for j := 0; j < perConn; j++ {
			wg.Add(1)
			go func(i, j int, s *db.Store) {
				defer wg.Done()
				task, err := CreateTaskWithOptions(s.DB(), TaskCreateOptions{
					Name: fmt.Sprintf("c%d-%d", i, j), RepoPath: "/tmp/r", GitBranch: "main",
					AccountRole: "personal", Organization: "StayPoint", ExecutionStage: "backlog",
				})
				if err != nil {
					errs <- err
					return
				}
				nums <- task.Number
			}(i, j, s)
		}
	}
	wg.Wait()
	close(errs)
	close(nums)
	for err := range errs {
		t.Fatalf("concurrent create: %v", err)
	}
	seen := map[int]bool{}
	for n := range nums {
		if seen[n] {
			t.Fatalf("number %d handed out twice", n)
		}
		seen[n] = true
	}
	for n := 1; n <= conns*perConn; n++ {
		if !seen[n] {
			t.Fatalf("numbers not dense: %d missing from %v", n, seen)
		}
	}
}

func TestCreateTask_OrgCountersIndependent(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s1 := createParked(t, store, "Playwright UI specs", "StayPoint")
	m1 := createParked(t, store, "Work thing", "Managed Solution")
	s2 := createParked(t, store, "Second", "")
	m2 := createParked(t, store, "Work two", "Managed Solution")
	for _, c := range []struct {
		task *Task
		want string
	}{{s1, "STA-1"}, {m1, "MAN-1"}, {s2, "STA-2"}, {m2, "MAN-2"}} {
		if c.task.Identifier != c.want {
			t.Errorf("%s identifier = %q, want %q", c.task.Name, c.task.Identifier, c.want)
		}
	}
	if s1.Slug != "playwright-ui-specs" || s1.URL != "http://localhost:41421/STA-1/playwright-ui-specs" {
		t.Fatalf("slug/url = %q %q", s1.Slug, s1.URL)
	}
	// GetTask accepts the reference anywhere a task id is accepted.
	got, err := GetTask(store.DB(), "man-2")
	if err != nil || got.ID != m2.ID {
		t.Fatalf("GetTask(man-2) = %v, %v; want %s", got, err, m2.ID)
	}
}

func TestResolveTaskRef_UnknownAndLegacy(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conn := store.DB()

	// Failure cases first: unknown number, not a reference.
	if _, _, err := ResolveTaskRef(conn, "STA-99"); !errors.Is(err, ErrTaskRefNotFound) {
		t.Fatalf("unknown number err = %v, want ErrTaskRefNotFound", err)
	}
	if _, _, err := ResolveTaskRef(conn, "task-1234"); !errors.Is(err, ErrTaskRefNotFound) {
		t.Fatalf("non-reference err = %v", err)
	}
	if _, err := GetTask(conn, "STA-99"); err == nil {
		t.Fatal("GetTask(STA-99) found a task; want not found")
	}

	// An imported task keeps its Paperclip label as source_ref and gets a new
	// number; the label collides with nothing yet, so it resolves as alias.
	imported, err := CreateTaskWithOptions(conn, TaskCreateOptions{
		Name: "[STA-2] Imported", RepoPath: "/tmp/r", GitBranch: "main", AccountRole: "personal",
		Organization: "StayPoint", ExecutionStage: "backlog", SourceRef: "STA-2", SourceID: "uuid-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if imported.Identifier != "STA-1" {
		t.Fatalf("imported identifier = %q, want STA-1 (new numbering, not the label)", imported.Identifier)
	}
	id, legacy, err := ResolveTaskRef(conn, "STA-2")
	if err != nil || id != imported.ID || !legacy {
		t.Fatalf("legacy STA-2 = %q legacy=%v err=%v; want %s via alias", id, legacy, err, imported.ID)
	}

	// Once a new task is numbered STA-2, the new numbering wins.
	native := createParked(t, store, "Native", "StayPoint")
	if native.Identifier != "STA-2" {
		t.Fatalf("native identifier = %q, want STA-2", native.Identifier)
	}
	id, legacy, err = ResolveTaskRef(conn, "STA-2")
	if err != nil || id != native.ID || legacy {
		t.Fatalf("STA-2 after collision = %q legacy=%v err=%v; want new %s", id, legacy, err, native.ID)
	}

	// Never reused: deleting the newest task does not free its number.
	if _, err := conn.Exec(`DELETE FROM tasks WHERE id = ?`, native.ID); err != nil {
		t.Fatal(err)
	}
	next := createParked(t, store, "After delete", "StayPoint")
	if next.Identifier != "STA-3" {
		t.Fatalf("after delete identifier = %q, want STA-3", next.Identifier)
	}
}
