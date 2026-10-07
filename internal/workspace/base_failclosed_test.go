package workspace

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/checkpoint"

	_ "modernc.org/sqlite"
)

// Every path that cannot verify a base fails closed (security review of
// STA-774): no base guessed from a checkpoint, merge-base or HEAD, and no
// worktree made from a base the daemon did not record.

// A task worktree made before bases were recorded (from the user's feature
// branch, with a pre-run checkpoint the agent could move) has no trusted base.
func TestTaskBase_LegacyTaskFailsClosed(t *testing.T) {
	repo, _ := repoWithOrigin(t)
	gitIn(t, repo, "checkout", "-b", "feature")
	commitFile(t, repo, "a.txt", "a\n", "feature 1")
	wt := filepath.Join(repo, ".worktrees", "task-legacy")
	gitIn(t, repo, "worktree", "add", "-b", "staypoint/task-legacy", wt, "HEAD")
	if _, err := checkpoint.CreateCheckpoint(context.Background(), checkpoint.CreateOptions{
		WorkDir: wt, SessionID: "run1", Message: "pre-run task-legacy",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := TaskFilesChanged(context.Background(), testDB(t), repo, "task-legacy", "staypoint/task-legacy"); !errors.Is(err, ErrNoTaskBase) {
		t.Fatalf("legacy task: err = %v, want ErrNoTaskBase", err)
	}
}

// A pin with no DB record behind it (one the agent planted) is not trusted.
func TestTaskBase_UnrecordedPinFailsClosed(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-b", "main")
	commitFile(t, repo, "a", "a\n", "a")
	gitIn(t, repo, "checkout", "-b", "staypoint/task-planted")
	head := commitFile(t, repo, "b", "b\n", "b")
	gitIn(t, repo, "update-ref", BaseRef("task-planted"), head)
	if got, err := TaskBase(context.Background(), testDB(t), repo, "task-planted", head); !errors.Is(err, ErrNoTaskBase) {
		t.Fatalf("TaskBase = %q (err %v), want ErrNoTaskBase", got, err)
	}
}

func TestTaskBase_NoDBFailsClosed(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-b", "main")
	head := commitFile(t, repo, "a", "a\n", "a")
	if _, err := TaskBase(context.Background(), nil, repo, "task-nodb", head); !errors.Is(err, ErrNoTaskBase) {
		t.Fatalf("nil DB: err = %v, want ErrNoTaskBase", err)
	}
}

// A DB that cannot be read (here: never migrated) is an error, not "no base".
func TestTaskBase_UnreadableDBFailsClosed(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-b", "main")
	head := commitFile(t, repo, "a", "a\n", "a")
	raw := bareDB(t)
	got, err := TaskBase(context.Background(), raw, repo, "task-baredb", head)
	if err == nil || got != "" {
		t.Fatalf("unmigrated DB: TaskBase = %q, err = %v; want an error", got, err)
	}
	if errors.Is(err, ErrNoTaskBase) {
		t.Fatalf("unmigrated DB reported as a plain missing base: %v", err)
	}
}

func TestCreate_NoDBRefused(t *testing.T) {
	repo, _ := repoWithOrigin(t)
	_, err := NewWorktreeManager(repo, nil).Create("task-nodb", "s")
	if !errors.Is(err, ErrNoTaskBase) {
		t.Fatalf("Create without DB: err = %v, want ErrNoTaskBase", err)
	}
	assertNoTaskWorktree(t, repo, "task-nodb")
}

// No origin default and no local main/master: never branch from HEAD.
func TestCreate_NoDefaultBranchRefused(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-b", "feature")
	commitFile(t, repo, "f", "f\n", "f")
	_, err := NewWorktreeManager(repo, testDB(t)).Create("task-nodefault", "s")
	if !errors.Is(err, ErrNoDefaultBranch) {
		t.Fatalf("Create: err = %v, want ErrNoDefaultBranch", err)
	}
	assertNoTaskWorktree(t, repo, "task-nodefault")
}

// Recording the base fails (unmigrated DB): no worktree, no branch, no pin.
func TestCreate_RecordFailureRefused(t *testing.T) {
	repo, _ := repoWithOrigin(t)
	if _, err := NewWorktreeManager(repo, bareDB(t)).Create("task-norecord", "s"); err == nil {
		t.Fatal("Create succeeded without recording a base")
	}
	assertNoTaskWorktree(t, repo, "task-norecord")
}

// A re-run on a branch whose pin the agent moved is refused.
func TestCreate_RerunWithTamperedPinRefused(t *testing.T) {
	repo, _ := repoWithOrigin(t)
	ctx := context.Background()
	wm := NewWorktreeManager(repo, testDB(t))
	wt, err := wm.Create("task-retamper", "s1")
	if err != nil {
		t.Fatal(err)
	}
	head := commitFile(t, wt, "x.txt", "x\n", "work")
	gitIn(t, wt, "update-ref", BaseRef("task-retamper"), head)
	if err := wm.PruneWorktreeDirContext(ctx, "task-retamper"); err != nil {
		t.Fatal(err)
	}
	if _, err := wm.Create("task-retamper", "s2"); !errors.Is(err, ErrTaskBaseTampered) {
		t.Fatalf("re-run Create: err = %v, want ErrTaskBaseTampered", err)
	}
	if _, statErr := os.Stat(filepath.Join(repo, ".worktrees", "task-retamper")); !os.IsNotExist(statErr) {
		t.Fatalf("worktree re-created over a tampered base (stat err %v)", statErr)
	}
}

// bareDB is a SQLite DB that never ran the daemon migrations.
func bareDB(t *testing.T) *sql.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "bare.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	return raw
}

func assertNoTaskWorktree(t *testing.T, repo, taskID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := os.Stat(filepath.Join(repo, ".worktrees", taskID)); !os.IsNotExist(err) {
		t.Fatalf("worktree for %s exists (stat err %v)", taskID, err)
	}
	if commitOf(ctx, repo, "refs/heads/staypoint/"+taskID) != "" {
		t.Fatalf("branch staypoint/%s exists", taskID)
	}
	if commitOf(ctx, repo, BaseRef(taskID)) != "" {
		t.Fatalf("pin %s exists", BaseRef(taskID))
	}
}
