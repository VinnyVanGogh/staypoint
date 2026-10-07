package orchestrator

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/workspace"
)

// addDoneGateColumns adds the production columns the minimal test schema
// lacks (work_kind, is_blocked, block_reason).
func addDoneGateColumns(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, stmt := range []string{
		`ALTER TABLE tasks ADD COLUMN work_kind TEXT NOT NULL DEFAULT 'coding'`,
		`ALTER TABLE tasks ADD COLUMN is_blocked INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE tasks ADD COLUMN block_reason TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			t.Fatal(stmt, err)
		}
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// 2026-10-07 deadlock: five parallel tasks on one repo, each in its own
// worktree, each refused done because a sibling was in_progress.
func TestDoneGate_ParallelWorktreeTasksDoNotDeadlock(t *testing.T) {
	db := openTestDB(t)
	repoRoot := "/home/agent/agent-mesh"
	for _, id := range []string{"a", "b", "c"} {
		insertTask(t, db, id, repoRoot)
		_, _ = db.Exec(`UPDATE tasks SET execution_stage='in_progress', status='active' WHERE id=?`, id)
	}
	ic := NewInterceptor(db)
	ic.Guards = []GuardFunc{ic.checkMutexLease}
	for _, id := range []string{"a", "b", "c"} {
		ok, diag, err := ic.InterceptCompletion(context.Background(), id, repoRoot+"/.worktrees/task-"+id, repoRoot)
		if err != nil || !ok {
			t.Fatalf("%s in its own worktree must not be blocked by siblings; diag=%v err=%v", id, diag, err)
		}
	}
	// A task working directly in the shared checkout is still guarded.
	if ok, _, _ := ic.InterceptCompletion(context.Background(), "a", repoRoot, repoRoot); ok {
		t.Fatal("a task running in the repo root itself must still be blocked by an in_progress sibling")
	}
}

func TestDoneGate_NonCodeTaskAcceptsStoredDocument(t *testing.T) {
	db := openTestDB(t)
	addDoneGateColumns(t, db)
	insertTask(t, db, "plan", "/tmp")
	_, _ = db.Exec(`UPDATE tasks SET work_kind='planning' WHERE id='plan'`)
	_, _ = db.Exec(`INSERT INTO task_documents (task_id, doc_key, version, content) VALUES ('plan', 'description', 1, 'brief')`)
	ic := NewInterceptor(db)
	ic.Guards = []GuardFunc{ic.checkWorkProducts}

	ok, diag, _ := ic.InterceptCompletion(context.Background(), "plan", "", "/tmp")
	if ok {
		t.Fatal("the description alone is not a deliverable")
	}
	if diag == nil || !strings.Contains(diag.Message, "staypoint task doc add plan") {
		t.Fatalf("non-code rejection must say how to store the document; got %v", diag)
	}

	_, _ = db.Exec(`INSERT INTO task_documents (task_id, doc_key, version, content) VALUES ('plan', 'plan', 1, '# Plan')`)
	if ok, diag, _ := ic.InterceptCompletion(context.Background(), "plan", "", "/tmp"); !ok {
		t.Fatalf("a stored plan document must satisfy a planning task; diag=%v", diag)
	}
}

func TestDoneGate_CodingTaskStillNeedsRealWorkProduct(t *testing.T) {
	db := openTestDB(t)
	addDoneGateColumns(t, db)
	insertTask(t, db, "code", "/tmp")
	_, _ = db.Exec(`INSERT INTO task_documents (task_id, doc_key, version, content) VALUES ('code', 'plan', 1, '# Plan')`)
	ic := NewInterceptor(db)
	ic.Guards = []GuardFunc{ic.checkWorkProducts}
	if ok, _, _ := ic.InterceptCompletion(context.Background(), "code", "", "/tmp"); ok {
		t.Fatal("a document must not satisfy a coding task")
	}
}

func TestDoneGate_NonCodeTaskNeedsNoShipCard(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	gitIn(t, repo, "checkout", "-b", "feature")
	_ = os.WriteFile(filepath.Join(repo, "notes.md"), []byte("x"), 0o644)
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-m", "notes")

	db := openTestDB(t)
	addDoneGateColumns(t, db)
	insertTask(t, db, "rev", repo)
	_, _ = db.Exec(`UPDATE tasks SET work_kind='review' WHERE id='rev'`)
	ic := NewInterceptor(db)
	ic.Guards = []GuardFunc{ic.checkShipReviewCard}
	if ok, diag, _ := ic.InterceptCompletion(context.Background(), "rev", repo, repo); !ok {
		t.Fatalf("non-code task must not need a Ship Review card; diag=%v", diag)
	}
}

// A worktree based on origin/main with a stale local main used to look
// "ahead" and demand a Ship Review card for a task that changed nothing.
func TestDoneGate_ShipCardComparesAgainstOriginMain(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	gitIn(t, repo, "checkout", "-b", "upstream")
	_ = os.WriteFile(filepath.Join(repo, "new.txt"), []byte("x"), 0o644)
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-m", "landed upstream")
	gitIn(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	gitIn(t, repo, "checkout", "-b", "staypoint/task-x") // == origin/main, ahead of stale main

	db := openTestDB(t)
	addDoneGateColumns(t, db)
	insertTask(t, db, "task-x", repo)
	ic := NewInterceptor(db)
	ic.Guards = []GuardFunc{ic.checkShipReviewCard}
	if ok, diag, _ := ic.InterceptCompletion(context.Background(), "task-x", repo, repo); !ok {
		t.Fatalf("no commits beyond origin/main: no card needed; diag=%v", diag)
	}

	// One real commit on top of origin/main still needs a card.
	_ = os.WriteFile(filepath.Join(repo, "work.go"), []byte("package x"), 0o644)
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-m", "work")
	if ok, _, _ := ic.InterceptCompletion(context.Background(), "task-x", repo, repo); ok {
		t.Fatal("a commit beyond origin/main must still require a Ship Review card")
	}
}

// The agent keeps emitting [[TASK_COMPLETE]] and the gate keeps refusing:
// the run must stop after maxCompletionRejections and park the task.
func TestDoneGate_RepeatedRejectionParksTask(t *testing.T) {
	useSlots(t, 1)
	repo := t.TempDir()
	initGitRepo(t, repo)
	db := openTestDB(t)
	addDoneGateColumns(t, db)
	insertTask(t, db, "loop", repo)

	h := &Harness{DB: db, RepoRoot: repo, WM: workspace.NewWorktreeManager(repo, db), Interceptor: NewInterceptor(db)}
	h.Interceptor.Guards = []GuardFunc{func(context.Context, string, string, string) (string, error) {
		return "never satisfiable", nil
	}}
	calls := 0
	result, err := h.Run(context.Background(), "loop", RunConfig{
		MaxTurns: 20, AgentID: "tester", MaxWallclock: 60 * time.Second,
		RunAdapter: func(_ context.Context, _, _ string, _, _ []string, stdout, _ io.Writer) error {
			calls++
			_, _ = fmt.Fprintf(stdout, "done\n%s\n", taskCompleteMarker)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != maxCompletionRejections {
		t.Fatalf("adapter ran %d times, want exactly %d", calls, maxCompletionRejections)
	}
	if result.Disposition != "backlog" {
		t.Fatalf("disposition = %q, want backlog", result.Disposition)
	}
	var stage string
	var blocked int
	_ = db.QueryRow(`SELECT execution_stage, is_blocked FROM tasks WHERE id='loop'`).Scan(&stage, &blocked)
	if stage != "backlog" || blocked != 1 {
		t.Fatalf("task must be parked: stage=%q is_blocked=%d", stage, blocked)
	}
	if !strings.Contains(result.DiagnosticMsg, "never satisfiable") {
		t.Fatalf("diagnostic must carry the last rejection; got %q", result.DiagnosticMsg)
	}
}
