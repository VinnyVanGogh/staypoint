package orchestrator

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/gitgate"
	"github.com/VinnyVanGogh/staypoint/internal/workspace"
)

func gitOutT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", name)
	gitT(t, dir, "commit", "-q", "-m", name)
}

func TestPreflightBranch_UsesRecordedTaskTarget(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec(`INSERT INTO task_worktree_bases (task_id, repo_path, base_sha, target_branch) VALUES ('task-dev', '/r', 'abc', 'dev-server')`); err != nil {
		t.Fatal(err)
	}
	if got := preflightBranch(context.Background(), db, "/r", "task-dev"); got != "dev-server" {
		t.Fatalf("preflightBranch = %q, want dev-server", got)
	}
}

func TestPreflightBranch_FallsBackToMain(t *testing.T) {
	db := openTestDB(t)
	repo := t.TempDir()
	initGitRepo(t, repo)
	if got := preflightBranch(context.Background(), db, repo, "task-none"); got != "main" {
		t.Fatalf("preflightBranch = %q, want main", got)
	}
}

// A task cut from dev-server, in a repo where dev-server and main have
// diverged, must fast-forward against dev-server. Against main (the old
// hardcoded branch) pre-flight must fail, never merge main into the task.
func TestPreflightBranch_DevServerTaskFastForwards(t *testing.T) {
	ctx := context.Background()
	origin := t.TempDir()
	initGitRepo(t, origin)
	gitT(t, origin, "checkout", "-q", "-b", "dev-server")
	commitFile(t, origin, "dev.txt", "dev 1\n")
	gitT(t, origin, "checkout", "-q", "main")
	commitFile(t, origin, "main.txt", "main only\n")

	clone := filepath.Join(t.TempDir(), "clone")
	gitT(t, origin, "clone", "-q", origin, clone)
	gitT(t, clone, "config", "user.email", "test@test.com")
	gitT(t, clone, "config", "user.name", "Test")
	wt := filepath.Join(t.TempDir(), "wt")
	gitT(t, clone, "worktree", "add", "-q", "--no-track", "-b", "staypoint/task-dev", wt, "origin/dev-server")
	base := gitOutT(t, wt, "rev-parse", "HEAD")

	// dev-server moves on after the task was cut.
	gitT(t, origin, "checkout", "-q", "dev-server")
	commitFile(t, origin, "dev2.txt", "dev 2\n")

	db := openTestDB(t)
	if err := workspace.RecordTaskBase(ctx, db, clone, "task-dev", base); err != nil {
		t.Fatal(err)
	}
	if err := workspace.RecordTaskTarget(ctx, db, "task-dev", "dev-server"); err != nil {
		t.Fatal(err)
	}

	if got := preflightMergeBase(ctx, db, clone, "task-dev"); got != base {
		t.Fatalf("preflightMergeBase = %q, want the recorded base %q", got, base)
	}
	// The old wrong-target bug: pre-flight against main, ff-only or with the
	// task's base, must fail and leave the dev-server task branch alone.
	for _, mergeBase := range []string{"", base} {
		if r, err := gitgate.PreFlightMerge(ctx, wt, "main", mergeBase); err != nil || r.OK {
			t.Fatalf("pre-flight against main should fail for a dev-server task (base %q); got ok=%v err=%v", mergeBase, r != nil && r.OK, err)
		}
		if head := gitOutT(t, wt, "rev-parse", "HEAD"); head != base {
			t.Fatalf("pre-flight against main moved the dev-server task branch: %s -> %s", base, head)
		}
	}

	branch := preflightBranch(ctx, db, clone, "task-dev")
	if branch != "dev-server" {
		t.Fatalf("preflightBranch = %q, want dev-server", branch)
	}
	r, err := gitgate.PreFlight(ctx, wt, branch)
	if err != nil || !r.OK {
		t.Fatalf("pre-flight against %s failed: err=%v result=%+v", branch, err, r)
	}
	if r.Behind != 1 {
		t.Fatalf("behind = %d, want 1 (fast-forwarded the new dev-server commit)", r.Behind)
	}
	if _, err := os.Stat(filepath.Join(wt, "dev2.txt")); err != nil {
		t.Fatalf("worktree was not fast-forwarded to origin/dev-server: %v", err)
	}
}

// resumedTaskRepo sets up a clone whose task branch staypoint/<taskID> holds
// one pushed commit from an earlier run (writing taskFile) while origin/main
// moved on with a commit writing mainFile. The task is cut from main with
// main as its recorded target.
func resumedTaskRepo(t *testing.T, taskID, taskFile, mainFile string) (string, *Harness) {
	t.Helper()
	return resumedTaskRepoFrom(t, taskID, taskFile, mainFile, "main", "main")
}

// resumedTaskRepoFrom is resumedTaskRepo for a task cut from cutFrom, with
// target recorded as its target ("" records none, as for tasks cut before
// targets were recorded). A cutFrom other than main is created on origin with
// one commit of its own before the clone.
func resumedTaskRepoFrom(t *testing.T, taskID, taskFile, mainFile, cutFrom, target string) (string, *Harness) {
	t.Helper()
	ctx := context.Background()
	origin := t.TempDir()
	initGitRepo(t, origin)
	if cutFrom != "main" {
		gitT(t, origin, "checkout", "-q", "-b", cutFrom)
		commitFile(t, origin, cutFrom+".txt", "only on "+cutFrom+"\n")
		gitT(t, origin, "checkout", "-q", "main")
	}

	clone := filepath.Join(t.TempDir(), "clone")
	gitT(t, origin, "clone", "-q", origin, clone)
	gitT(t, clone, "config", "user.email", "test@test.com")
	gitT(t, clone, "config", "user.name", "Test")
	base := gitOutT(t, clone, "rev-parse", "origin/"+cutFrom)

	branch := "staypoint/" + taskID
	gitT(t, clone, "checkout", "-q", "-b", branch, base)
	commitFile(t, clone, taskFile, "from the earlier run\n")
	gitT(t, clone, "push", "-q", "origin", branch)
	gitT(t, clone, "checkout", "-q", "main")

	commitFile(t, origin, mainFile, "main moved on\n")

	db := openTestDB(t)
	insertTask(t, db, taskID, clone)
	if err := workspace.RecordTaskBase(ctx, db, clone, taskID, base); err != nil {
		t.Fatal(err)
	}
	if target != "" {
		if err := workspace.RecordTaskTarget(ctx, db, taskID, target); err != nil {
			t.Fatal(err)
		}
	}
	h := &Harness{
		DB:          db,
		RepoRoot:    clone,
		WM:          workspace.NewWorktreeManager(clone, db),
		Interceptor: NewInterceptor(db),
	}
	h.Interceptor.Guards = []GuardFunc{h.Interceptor.checkWorkProducts}
	return clone, h
}

// task-25ac4f3b: a resumed task branch with its own commit, behind main, used
// to fail --ff-only and end the run in seconds. It now merges main and runs.
func TestRun_DivergedTaskBranchMergesAndRuns(t *testing.T) {
	useSlots(t, 1)
	const taskID = "diverged-task"
	clone, h := resumedTaskRepo(t, taskID, "task.txt", "main.txt")

	var ran bool
	result, err := h.Run(context.Background(), taskID, RunConfig{
		MaxTurns:     1,
		AgentID:      "tester",
		MaxWallclock: 30 * time.Second,
		RunAdapter: func(_ context.Context, cwd, _ string, _, _ []string, stdout, _ io.Writer) error {
			ran = true
			for _, f := range []string{"task.txt", "main.txt"} {
				if _, err := os.Stat(filepath.Join(cwd, f)); err != nil {
					t.Errorf("%s missing in worktree at turn start: %v", f, err)
				}
			}
			_, _ = fmt.Fprintln(stdout, "working")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatalf("adapter never ran; disposition=%q diagnostic=%s", result.Disposition, result.DiagnosticMsg)
	}
	if result.Disposition == "blocked" {
		t.Fatalf("run blocked: %s", result.DiagnosticMsg)
	}
	if err := exec.Command("git", "-C", clone, "merge-base", "--is-ancestor", "origin/main", "staypoint/"+taskID).Run(); err != nil {
		t.Errorf("task branch does not contain origin/main after pre-flight: %v", err)
	}
}

// A resumed branch that conflicts with main parks the task as blocked, with
// the conflicting file in the block reason and on the run timeline, and never
// starts a turn.
func TestRun_ConflictingTaskBranchBlocksWithReason(t *testing.T) {
	useSlots(t, 1)
	const taskID = "conflict-task"
	_, h := resumedTaskRepo(t, taskID, "shared.txt", "shared.txt")

	var steps []RunStep
	sr := NewStepRecorder(h.DB, func(ev string, data any) {
		if s, ok := data.(RunStep); ok && ev == "run.step" {
			steps = append(steps, s)
		}
	}, "run-conflict", taskID)
	result, err := h.Run(context.Background(), taskID, RunConfig{
		MaxTurns:     1,
		AgentID:      "tester",
		MaxWallclock: 30 * time.Second,
		StepRecorder: sr,
		RunAdapter: func(context.Context, string, string, []string, []string, io.Writer, io.Writer) error {
			t.Error("adapter ran despite a conflicting pre-flight")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != "blocked" {
		t.Fatalf("disposition = %q, want blocked", result.Disposition)
	}

	var stage, reason string
	var blocked int
	if err := h.DB.QueryRow(`SELECT execution_stage, is_blocked, COALESCE(block_reason,'') FROM tasks WHERE id=?`, taskID).
		Scan(&stage, &blocked, &reason); err != nil {
		t.Fatal(err)
	}
	if stage != "blocked" || blocked != 1 {
		t.Errorf("stage=%q is_blocked=%d, want blocked/1", stage, blocked)
	}
	if !strings.Contains(reason, "shared.txt") || !strings.Contains(reason, "origin/main") {
		t.Errorf("block_reason should name the conflict and the branch, got %q", reason)
	}

	var msg *RunStep
	for i := range steps {
		if steps[i].Kind == StepMessage && steps[i].Title == "Git pre-flight failed" {
			msg = &steps[i]
		}
	}
	if msg == nil {
		t.Fatalf("no 'Git pre-flight failed' timeline message; steps=%+v", steps)
	}
	if msg.Status != "error" || !strings.Contains(msg.Body, "shared.txt") {
		t.Errorf("timeline message should be an error naming the conflicting file, got status=%q body=%q", msg.Status, msg.Body)
	}
	var comments int
	_ = h.DB.QueryRow(`SELECT COUNT(1) FROM task_comments WHERE task_id=? AND message LIKE '%shared.txt%'`, taskID).Scan(&comments)
	if comments == 0 {
		t.Error("no task comment names the conflicting file")
	}
}

// Review guard for the old wrong-target bug: a resumed dev-server task with
// no recorded target falls back to pre-flighting against main. Pre-flight
// must not merge main (prod-only commits) into it; the task is parked as
// blocked with a visible reason and its branch is left where it was.
func TestRun_DevServerTaskWithoutRecordedTargetDoesNotMergeMain(t *testing.T) {
	useSlots(t, 1)
	const taskID = "devserver-task"
	clone, h := resumedTaskRepoFrom(t, taskID, "task.txt", "main.txt", "dev-server", "")
	branch := "staypoint/" + taskID
	before := gitOutT(t, clone, "rev-parse", branch)

	if got := preflightBranch(context.Background(), h.DB, clone, taskID); got != "main" {
		t.Fatalf("preflightBranch = %q, want the main fallback this test guards", got)
	}

	result, err := h.Run(context.Background(), taskID, RunConfig{
		MaxTurns:     1,
		AgentID:      "tester",
		MaxWallclock: 30 * time.Second,
		RunAdapter: func(context.Context, string, string, []string, []string, io.Writer, io.Writer) error {
			t.Error("adapter ran despite a refused pre-flight")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != "blocked" {
		t.Fatalf("disposition = %q, want blocked", result.Disposition)
	}
	if after := gitOutT(t, clone, "rev-parse", branch); after != before {
		t.Errorf("task branch moved: %s -> %s", before, after)
	}
	if err := exec.Command("git", "-C", clone, "merge-base", "--is-ancestor", "origin/main", branch).Run(); err == nil {
		t.Error("origin/main was merged into the dev-server task branch")
	}

	var stage, reason string
	var blocked int
	if err := h.DB.QueryRow(`SELECT execution_stage, is_blocked, COALESCE(block_reason,'') FROM tasks WHERE id=?`, taskID).
		Scan(&stage, &blocked, &reason); err != nil {
		t.Fatal(err)
	}
	if stage != "blocked" || blocked != 1 {
		t.Errorf("stage=%q is_blocked=%d, want blocked/1", stage, blocked)
	}
	if !strings.Contains(reason, "fast-forward only") {
		t.Errorf("block_reason should say why pre-flight did not merge, got %q", reason)
	}
}
