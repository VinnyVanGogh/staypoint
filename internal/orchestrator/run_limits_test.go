package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/checkpoint"
	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
	"github.com/VinnyVanGogh/staypoint/internal/gitgate"
	"github.com/VinnyVanGogh/staypoint/internal/workorgs"
	"github.com/VinnyVanGogh/staypoint/internal/workspace"
)

// STA-867: per-repo cap (default 3), per-organization cap (default 3), global
// cap (default 9), queue fairness across organizations, and per-repo git
// locks for runs that share a repo.

func insertOrgTask(t *testing.T, db *sql.DB, id, repoPath, org string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO tasks (id, name, repo_path, organization) VALUES (?, ?, ?, ?)`,
		id, "task "+id, repoPath, org); err != nil {
		t.Fatal("insert task:", err)
	}
}

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// initRepoWithOrigin makes a repo with a commit on main pushed to a local
// bare origin, so fetch, worktree add and preflight all do real work.
func initRepoWithOrigin(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, root, "init", "--bare", "-b", "main", origin)
	initGitRepo(t, repo)
	gitT(t, repo, "remote", "add", "origin", origin)
	gitT(t, repo, "push", "-u", "origin", "main")
	return repo
}

// Three tasks in the same repo and organization all run at once, each in its
// own worktree, while the repo-shared git steps of a run start (fetch,
// worktree add, base pin, checkpoint refs, preflight fetch) go through the
// per-repo git locks without failing each other. Run with -race.
func TestRunLimits_SameRepoSameOrgThreeRunConcurrently(t *testing.T) {
	slots := useSlots(t, 0) // defaults: 9 global, 3 per repo, 3 per org
	repo := initRepoWithOrigin(t)
	db := openTestDB(t)
	db.SetMaxOpenConns(1)
	ids := []string{"same-a", "same-b", "same-c"}
	for _, id := range ids {
		insertOrgTask(t, db, id, repo, "StayPoint")
	}
	h := &Harness{DB: db, RepoRoot: repo}

	before := map[string]int{}
	for _, lane := range []string{"fetch", "worktree", "refs"} {
		before[lane] = gitexec.RepoLockCount(repo, lane)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 3*4)
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			ctx := context.Background()
			if err := h.Claim(ctx, id, "run-"+id, "agent"); err != nil {
				errs <- fmt.Errorf("claim %s: %w", id, err)
				return
			}
			wm := workspace.NewWorktreeManager(repo, db)
			wt, err := wm.CreateContext(ctx, id, "run-"+id)
			if err != nil {
				errs <- fmt.Errorf("worktree %s: %w", id, err)
				return
			}
			if err := os.WriteFile(filepath.Join(wt, id+".txt"), []byte(id), 0o644); err != nil {
				errs <- err
				return
			}
			if _, err := checkpoint.CreateCheckpoint(ctx, checkpoint.CreateOptions{WorkDir: wt, SessionID: id}); err != nil {
				errs <- fmt.Errorf("checkpoint %s: %w", id, err)
			}
			if r, err := gitgate.PreFlight(ctx, wt, "main"); err != nil || len(r.Errors) > 0 {
				errs <- fmt.Errorf("preflight %s: %v %v", id, err, r.Errors)
			}
		}(id)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := slots.Active(); n != 3 {
		t.Fatalf("active runs = %d, want all 3 same-repo runs at once", n)
	}
	for _, id := range ids {
		if _, err := os.Stat(filepath.Join(repo, ".worktrees", id, id+".txt")); err != nil {
			t.Errorf("worktree for %s: %v", id, err)
		}
	}
	// Each run fetched twice (base + preflight), added a worktree and wrote
	// refs (base pin, checkpoint refs): all of it under the repo's locks.
	for lane, min := range map[string]int{"fetch": 6, "worktree": 3, "refs": 3} {
		if got := gitexec.RepoLockCount(repo, lane) - before[lane]; got < min {
			t.Errorf("%s lock taken %d times, want >= %d", lane, got, min)
		}
	}

	// A fourth task in the same organization (another repo) queues on the
	// org cap.
	insertOrgTask(t, db, "same-d", t.TempDir(), "StayPoint")
	err := h.Claim(context.Background(), "same-d", "run-same-d", "agent")
	if !errors.Is(err, ErrOrgBusy) || WaitFor(err) != WaitOrg {
		t.Fatalf("4th same-org run: got %v (wait %q), want ErrOrgBusy / %q", err, WaitFor(err), WaitOrg)
	}
	if pos := slots.Enqueue("same-d", h.SlotKeyForTask(context.Background(), "same-d"), "test", WaitFor(err)); pos.Wait != WaitOrg {
		t.Fatalf("queued wait = %q, want %q", pos.Wait, WaitOrg)
	}
	for _, id := range ids {
		h.Release(id, "run-"+id)
	}
}

// A fourth run in the same repo queues on the repo cap.
func TestRunLimits_FourthSameRepoQueuesOnRepo(t *testing.T) {
	s := NewRunSlotsWithLimits(RunLimits{PerOrg: 10})
	s.Wake = func(string, string) {}
	for _, id := range []string{"a", "b", "c"} {
		if err := s.Acquire(id, SlotKey{Dir: "/repo", Org: "A"}); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	err := s.Acquire("d", SlotKey{Dir: "/repo", Org: "A"})
	if !errors.Is(err, ErrRepoBusy) || WaitFor(err) != WaitRepo {
		t.Fatalf("4th same-repo run: %v", err)
	}
}

// Three organizations x three runs fill the default global cap of 9; a
// tenth run (a fourth organization) queues on the global cap.
func TestRunLimits_NineAcrossThreeOrgsTenthQueuesGlobal(t *testing.T) {
	useSlots(t, 0)
	db := openTestDB(t)
	db.SetMaxOpenConns(1)
	h := &Harness{DB: db, RepoRoot: "/nonexistent-sta867"}
	n := 0
	for _, org := range []string{"Managed Solution", "StayPoint", "Personal"} {
		for i := 0; i < 3; i++ {
			id := fmt.Sprintf("g-%d", n)
			n++
			// Distinct missing repo paths: git keys, no repo cap in play.
			insertOrgTask(t, db, id, fmt.Sprintf("/nonexistent-sta867/%s", id), org)
			if err := h.Claim(context.Background(), id, "run-"+id, "agent"); err != nil {
				t.Fatalf("%s (%s): %v", id, org, err)
			}
		}
	}
	insertOrgTask(t, db, "g-10", "/nonexistent-sta867/g-10", "Research")
	err := h.Claim(context.Background(), "g-10", "run-g-10", "agent")
	if !errors.Is(err, ErrConcurrencyCap) || errors.Is(err, ErrOrgBusy) || WaitFor(err) != WaitSlots {
		t.Fatalf("10th run: got %v (wait %q), want global cap", err, WaitFor(err))
	}
}

// Runs in the same plain (non-git) folder run one at a time; scratch tasks
// (no repo_path) each have their own folder and run together.
func TestRunLimits_SamePlainDirOneAtATime(t *testing.T) {
	slots := useSlots(t, 0)
	db := openTestDB(t)
	db.SetMaxOpenConns(1)
	plain := t.TempDir()
	insertOrgTask(t, db, "plain-1", plain, "A")
	insertOrgTask(t, db, "plain-2", plain, "B")
	insertOrgTask(t, db, "scratch-1", "", "A")
	insertOrgTask(t, db, "scratch-2", "", "A")
	h := &Harness{DB: db, RepoRoot: "/nonexistent-sta867"}
	ctx := context.Background()

	if k := h.SlotKeyForTask(ctx, "plain-1"); !k.Plain || k.Dir != RepoKey(plain) {
		t.Fatalf("plain key = %+v", k)
	}
	if err := h.Claim(ctx, "plain-1", "r1", "agent"); err != nil {
		t.Fatal(err)
	}
	err := h.Claim(ctx, "plain-2", "r2", "agent")
	if !errors.Is(err, ErrPlainDirBusy) || WaitFor(err) != WaitDir {
		t.Fatalf("second run in the same plain folder: got %v, want ErrPlainDirBusy", err)
	}
	slots.Enqueue("plain-2", h.SlotKeyForTask(ctx, "plain-2"), "test", WaitFor(err))
	for _, id := range []string{"scratch-1", "scratch-2"} {
		if err := h.Claim(ctx, id, "r-"+id, "agent"); err != nil {
			t.Fatalf("scratch task %s: %v", id, err)
		}
	}
	h.Release("plain-1", "r1")
	if err := h.Claim(ctx, "plain-2", "r2", "agent"); err != nil {
		t.Fatalf("plain-2 after plain-1 released: %v", err)
	}
}

// An organization at its cap does not starve the others: queued runs behind
// it from other organizations start, newcomers from other organizations start,
// and the freed slot is not taken by a newcomer ahead of a queued run that
// fits.
func TestRunLimits_OrgAtCapDoesNotStarveOthers(t *testing.T) {
	s := NewRunSlotsWithLimits(RunLimits{Global: 4, PerOrg: 2})
	var mu sync.Mutex
	var woke []string
	s.Wake = func(id, _ string) { mu.Lock(); woke = append(woke, id); mu.Unlock() }
	key := func(org string, i int) SlotKey { return SlotKey{Dir: fmt.Sprintf("/r/%s%d", org, i), Org: org} }

	for i, k := range []SlotKey{key("A", 1), key("A", 2), key("B", 1), key("C", 1)} {
		if err := s.Acquire(fmt.Sprint("run", i), k); err != nil {
			t.Fatal(err)
		}
	}
	// Queue, oldest first: two more from A (A is at its cap), then one from B.
	for _, q := range []struct {
		id string
		k  SlotKey
	}{{"A3", key("A", 3)}, {"A4", key("A", 4)}, {"B2", key("B", 2)}} {
		err := s.Acquire(q.id, q.k)
		if !errors.Is(err, ErrConcurrencyCap) {
			t.Fatalf("%s: %v", q.id, err)
		}
		s.Enqueue(q.id, q.k, "test", WaitFor(err))
	}
	if p := s.Position("A3"); p.Wait != WaitOrg {
		t.Fatalf("A3 wait = %q, want org", p.Wait)
	}
	if p := s.Position("B2"); p.Wait != WaitSlots || p.Ahead != 2 {
		t.Fatalf("B2 position = %+v, want 2 ahead, global", p)
	}

	s.Release("run3") // C1 frees a global slot
	mu.Lock()
	got := fmt.Sprint(woke)
	mu.Unlock()
	if got != "[B2]" {
		t.Fatalf("pump woke %s, want [B2]: A3 and A4 are held by A's cap, B2 behind them must start", got)
	}
	// The freed slot is B2's: a newcomer from another org may not take it.
	if err := s.Acquire("C2", key("C", 2)); !errors.Is(err, ErrConcurrencyCap) {
		t.Fatalf("newcomer took the slot planned for queued B2: %v", err)
	}
	if err := s.Acquire("B2", key("B", 2)); err != nil {
		t.Fatalf("queued B2: %v", err)
	}

	// A newcomer from a free org is not held back by A's queued runs.
	s2 := NewRunSlotsWithLimits(RunLimits{Global: 9, PerOrg: 2})
	s2.Wake = func(string, string) {}
	_ = s2.Acquire("a1", key("A", 1))
	_ = s2.Acquire("a2", key("A", 2))
	s2.Enqueue("a3", key("A", 3), "test", WaitOrg)
	if err := s2.Acquire("b1", key("B", 1)); err != nil {
		t.Fatalf("B newcomer blocked by A's queued run: %v", err)
	}
	// ...but a newcomer from A may not jump A's queued run once A has room.
	s2.Release("a1")
	if err := s2.Acquire("a9", key("A", 9)); !errors.Is(err, ErrOrgBusy) {
		t.Fatalf("A newcomer jumped queued a3: %v", err)
	}
	if err := s2.Acquire("a3", key("A", 3)); err != nil {
		t.Fatalf("queued a3: %v", err)
	}
}

// [run_limits.orgs] overrides apply case-insensitively; blank org is the
// Unassigned bucket.
func TestRunLimits_OrgOverridesAndUnassigned(t *testing.T) {
	s := NewRunSlotsWithLimits(RunLimits{PerOrg: 1, Orgs: map[string]int{"managed solution": 2}})
	s.Wake = func(string, string) {}
	if err := s.Acquire("m1", SlotKey{Dir: "/m1", Org: "Managed Solution"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Acquire("m2", SlotKey{Dir: "/m2", Org: "Managed Solution"}); err != nil {
		t.Fatalf("override of 2 not applied: %v", err)
	}
	if err := s.Acquire("m3", SlotKey{Dir: "/m3", Org: "Managed Solution"}); !errors.Is(err, ErrOrgBusy) {
		t.Fatalf("m3: %v", err)
	}
	if err := s.Acquire("u1", SlotKey{Dir: "/u1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Acquire("u2", SlotKey{Dir: "/u2", Org: "  "}); !errors.Is(err, ErrOrgBusy) {
		t.Fatalf("blank orgs must share the Unassigned bucket: %v", err)
	}
	s.Enqueue("u2", SlotKey{Dir: "/u2"}, "test", WaitOrg)
	if q := s.Queue(); q[0].Org != UnassignedOrg {
		t.Fatalf("queued org = %q, want %q", q[0].Org, UnassignedOrg)
	}
}

// Board 2026-10-09: every work org shares the one Managed Solution cap (the
// work seat), whatever [run_limits.orgs] says for the other work org. Other
// orgs keep their own bucket.
func TestRunLimits_WorkOrgsShareOneCap(t *testing.T) {
	t.Cleanup(func() { workorgs.Set(nil) })
	workorgs.Set([]string{"Power Platform"})
	s := NewRunSlotsWithLimits(RunLimits{PerOrg: 3, Orgs: map[string]int{"Managed Solution": 3, "Power Platform": 3}})
	s.Wake = func(string, string) {}
	for i, org := range []string{"Managed Solution", "Power Platform", "power platform"} {
		if err := s.Acquire(fmt.Sprintf("w%d", i), SlotKey{Dir: fmt.Sprintf("/w%d", i), Org: org}); err != nil {
			t.Fatalf("work run %d (%s): %v", i, org, err)
		}
	}
	for _, org := range []string{"Power Platform", "Managed Solution", "MAN"} {
		if err := s.Acquire("w-extra", SlotKey{Dir: "/wx", Org: org}); !errors.Is(err, ErrOrgBusy) {
			t.Fatalf("4th work run (%s) must hit the shared work cap: %v", org, err)
		}
	}
	if err := s.Acquire("sp1", SlotKey{Dir: "/sp1", Org: "StayPoint"}); err != nil {
		t.Fatalf("StayPoint must not count against the work cap: %v", err)
	}
	s.Enqueue("w-extra", SlotKey{Dir: "/wx", Org: "Power Platform"}, "test", WaitOrg)
	if q := s.Queue(); q[0].Org != "Power Platform" {
		t.Fatalf("queued org = %q, want the task's own org", q[0].Org)
	}
	s.Release("w1")
	if err := s.Acquire("w-extra", SlotKey{Dir: "/wx", Org: "Power Platform"}); err != nil {
		t.Fatalf("freed work slot not reusable: %v", err)
	}

	// Unconfigured, Power Platform is its own org with its own cap.
	workorgs.Set(nil)
	s2 := NewRunSlotsWithLimits(RunLimits{PerOrg: 1})
	s2.Wake = func(string, string) {}
	if err := s2.Acquire("m", SlotKey{Dir: "/m", Org: "Managed Solution"}); err != nil {
		t.Fatal(err)
	}
	if err := s2.Acquire("p", SlotKey{Dir: "/p", Org: "Power Platform"}); err != nil {
		t.Fatalf("unconfigured Power Platform shares the work cap: %v", err)
	}
}

// A task in a subfolder or worktree of a repo counts against the repo's main
// checkout.
func TestSlotKeyForTask_GitRepoKeyedByMainCheckout(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	sub := filepath.Join(repo, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	gitT(t, repo, "worktree", "add", "--detach", wt)
	db := openTestDB(t)
	insertOrgTask(t, db, "k-root", repo, "")
	insertOrgTask(t, db, "k-sub", sub, "")
	insertOrgTask(t, db, "k-wt", wt, "X")
	h := &Harness{DB: db}
	want := RepoKey(repo)
	for _, id := range []string{"k-root", "k-sub", "k-wt"} {
		k := h.SlotKeyForTask(context.Background(), id)
		if k.Plain || k.Dir != want {
			t.Errorf("%s key = %+v, want git key %s", id, k, want)
		}
	}
	if k := h.SlotKeyForTask(context.Background(), "k-root"); k.Org != UnassignedOrg {
		t.Errorf("blank org = %q, want %q", k.Org, UnassignedOrg)
	}
}
