package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/governance"
)

// tasks.max_running_children: at most N children of one parent hold a run at
// once. The next child is refused with ErrParentBusy (a capacity refusal, so
// it queues), tasks without that parent are unaffected, and the child starts
// once a sibling's run ends.
func TestClaim_CapsRunningChildrenPerParent(t *testing.T) {
	db := openTestDB(t)
	insertTask(t, db, "task-parent", "/repo/p")
	for _, id := range []string{"task-c1", "task-c2", "task-c3"} {
		insertTask(t, db, id, "/repo/"+id)
		if _, err := db.Exec(`UPDATE tasks SET parent_id = 'task-parent' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
	insertTask(t, db, "task-other", "/repo/o")
	if _, err := db.Exec(`INSERT INTO settings_kv (key, value) VALUES (?, '2')`, governance.SettingMaxRunningChildren); err != nil {
		t.Fatal(err)
	}
	h := &Harness{DB: db}
	ctx := context.Background()

	for i, id := range []string{"task-c1", "task-c2"} {
		if err := h.Claim(ctx, id, "run-"+id, "agent"); err != nil {
			t.Fatalf("child %d: %v", i+1, err)
		}
	}
	err := h.Claim(ctx, "task-c3", "run-c3", "agent")
	if !errors.Is(err, ErrParentBusy) {
		t.Fatalf("third child: err = %v, want ErrParentBusy", err)
	}
	if !errors.Is(err, ErrConcurrencyCap) || WaitFor(err) != WaitParent {
		t.Errorf("ErrParentBusy must queue as a capacity refusal with wait %q; got wait %q", WaitParent, WaitFor(err))
	}
	var stage string
	_ = db.QueryRow(`SELECT execution_stage FROM tasks WHERE id = 'task-c3'`).Scan(&stage)
	if stage == "in_progress" {
		t.Fatal("refused claim still moved the child to in_progress")
	}

	if err := h.Claim(ctx, "task-other", "run-other", "agent"); err != nil {
		t.Fatalf("task with no parent: %v", err)
	}
	h.Release("task-other", "run-other")

	h.Release("task-c1", "run-task-c1")
	if err := h.Claim(ctx, "task-c3", "run-c3", "agent"); err != nil {
		t.Fatalf("third child after a sibling finished: %v", err)
	}
	h.Release("task-c2", "run-task-c2")
	h.Release("task-c3", "run-c3")
}

// 0 turns the running-children cap off.
func TestClaim_RunningChildrenCapZeroDisables(t *testing.T) {
	db := openTestDB(t)
	insertTask(t, db, "task-parent", "/repo/p")
	ids := []string{"task-c1", "task-c2"}
	for _, id := range ids {
		insertTask(t, db, id, "/repo/"+id)
		if _, err := db.Exec(`UPDATE tasks SET parent_id = 'task-parent' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO settings_kv (key, value) VALUES (?, '0')`, governance.SettingMaxRunningChildren); err != nil {
		t.Fatal(err)
	}
	h := &Harness{DB: db}
	ctx := context.Background()
	for _, id := range ids {
		if err := h.Claim(ctx, id, "run-"+id, "agent"); err != nil {
			t.Fatalf("%s with cap off: %v", id, err)
		}
	}
	for _, id := range ids {
		h.Release(id, "run-"+id)
	}
}

// A sibling whose checkout survived after its task left in_progress (a run
// that crashed after moving its task to in_review, which RecoveryScan does not
// clear) must not hold the parent busy forever.
func TestClaim_StaleSiblingCheckoutDoesNotCount(t *testing.T) {
	db := openTestDB(t)
	insertTask(t, db, "task-parent", "/repo/p")
	for _, id := range []string{"task-stale", "task-next"} {
		insertTask(t, db, id, "/repo/"+id)
		if _, err := db.Exec(`UPDATE tasks SET parent_id = 'task-parent' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE tasks SET checkout_run_id = 'run-dead', execution_stage = 'in_review' WHERE id = 'task-stale'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO settings_kv (key, value) VALUES (?, '1')`, governance.SettingMaxRunningChildren); err != nil {
		t.Fatal(err)
	}
	h := &Harness{DB: db}
	if err := h.Claim(context.Background(), "task-next", "run-next", "agent"); err != nil {
		t.Fatalf("stale sibling checkout blocked the parent: %v", err)
	}
	h.Release("task-next", "run-next")
}
