package orchestrator

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/workspace"
)

// Zero-kill deploys (task-db71fba9): drain, persisted queue, suspend at a
// turn boundary, resume on the next daemon.

func zkDB(t *testing.T) *sql.DB {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "zk.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store.DB()
}

func zkTask(t *testing.T, d *sql.DB, id, repo string) {
	t.Helper()
	if _, err := d.Exec(`INSERT INTO tasks (id, name, repo_path) VALUES (?, ?, ?)`, id, "zk "+id, repo); err != nil {
		t.Fatalf("insert task: %v", err)
	}
}

func zkHarness(t *testing.T, d *sql.DB, repo string) *Harness {
	t.Helper()
	h := &Harness{DB: d, RepoRoot: repo, WM: workspace.NewWorktreeManager(repo, d), Interceptor: NewInterceptor(d)}
	h.Interceptor.Guards = nil
	return h
}

func stageOf(t *testing.T, d *sql.DB, id string) string {
	t.Helper()
	var s string
	_ = d.QueryRow(`SELECT execution_stage FROM tasks WHERE id=?`, id).Scan(&s)
	return s
}

func queueIDs(t *testing.T, d *sql.DB) []string {
	t.Helper()
	q, err := LoadQueue(context.Background(), d)
	if err != nil {
		t.Fatalf("LoadQueue: %v", err)
	}
	var ids []string
	for _, r := range q {
		ids = append(ids, r.TaskID+":"+r.Wait)
	}
	return ids
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A run started while the daemon drains is queued (and persisted), not
// refused, and nothing is dispatched until the drain ends.
func TestZeroKill_ClaimDuringDrainIsQueuedNotRefused(t *testing.T) {
	d := zkDB(t)
	zkTask(t, d, "zk-a", "")
	s := useSlots(t, 3)
	s.Store = SQLQueueStore{DB: d}
	var mu sync.Mutex
	var woke []string
	s.Wake = func(id, _ string) { mu.Lock(); woke = append(woke, id); mu.Unlock() }

	s.StartDrain(DrainFinish)
	h := &Harness{DB: d, RepoRoot: t.TempDir()}
	err := h.Claim(context.Background(), "zk-a", "run-1", "a")
	if !errors.Is(err, ErrDraining) || !errors.Is(err, ErrConcurrencyCap) {
		t.Fatalf("Claim during drain = %v, want ErrDraining (a capacity refusal callers queue)", err)
	}
	if WaitFor(err) != WaitDrain {
		t.Fatalf("WaitFor = %q, want %q", WaitFor(err), WaitDrain)
	}
	if got := stageOf(t, d, "zk-a"); got != "todo" {
		t.Fatalf("refused claim changed the stage to %q", got)
	}
	s.Enqueue("zk-a", h.SlotKeyForTask(context.Background(), "zk-a"), "run_now", WaitFor(err))
	s.Pump()
	if len(woke) != 0 {
		t.Fatalf("queue dispatched during a drain: %v", woke)
	}
	if got := queueIDs(t, d); len(got) != 1 || got[0] != "zk-a:drain" {
		t.Fatalf("persisted queue = %v, want [zk-a:drain]", got)
	}
	if st := s.DrainStatus(); st.Label() != "Draining for deploy: 0 runs left, 1 queued" {
		t.Fatalf("label = %q", st.Label())
	}

	s.CancelDrain()
	mu.Lock()
	defer mu.Unlock()
	if len(woke) != 1 || woke[0] != "zk-a" {
		t.Fatalf("cancelled drain did not start the queued run: %v", woke)
	}
}

// The queue is written through to SQLite, so a daemon killed with SIGKILL
// (no shutdown code at all) loses nothing: a child test process queues three
// runs and is killed; the next "daemon" loads them in order.
func TestZeroKill_QueueSurvivesKill9(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zk.db")
	store, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"zk-1", "zk-2", "zk-3"} {
		zkTask(t, store.DB(), id, "")
	}
	store.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestZeroKillQueueChild$", "-test.v")
	cmd.Env = append(os.Environ(), "ZK_CHILD_DB="+path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "ZK_READY") {
				ready <- true
				break
			}
		}
		_, _ = io.Copy(io.Discard, out)
	}()
	select {
	case <-ready:
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("child never queued its runs")
	}
	_ = cmd.Process.Kill() // SIGKILL: no deferred code, no shutdown drain
	_ = cmd.Wait()

	store, err = db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if got := queueIDs(t, store.DB()); strings.Join(got, ",") != "zk-1:drain,zk-2:drain,zk-3:drain" {
		t.Fatalf("queue after kill -9 = %v, want zk-1, zk-2, zk-3 in order", got)
	}
	q, _ := LoadQueue(context.Background(), store.DB())
	s := NewRunSlots(9)
	var woke []string
	s.Wake = func(id, _ string) { woke = append(woke, id) }
	s.Restore(q)
	s.Pump()
	if strings.Join(woke, ",") != "zk-1,zk-2,zk-3" {
		t.Fatalf("restored queue dispatched %v, want zk-1, zk-2, zk-3", woke)
	}
}

// TestZeroKillQueueChild is the daemon TestZeroKill_QueueSurvivesKill9 kills.
func TestZeroKillQueueChild(t *testing.T) {
	path := os.Getenv("ZK_CHILD_DB")
	if path == "" {
		t.Skip("helper process for TestZeroKill_QueueSurvivesKill9")
	}
	store, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := NewRunSlots(9)
	s.Store = SQLQueueStore{DB: store.DB()}
	s.Wake = func(string, string) {}
	s.StartDrain(DrainFinish)
	for _, id := range []string{"zk-1", "zk-2", "zk-3"} {
		s.Enqueue(id, SlotKey{}, "run_now", WaitDrain)
		time.Sleep(2 * time.Millisecond)
	}
	fmt.Println("ZK_READY")
	time.Sleep(2 * time.Minute)
}

// zkRunCfg is a run config for a fake adapter in a real git worktree.
func zkRunCfg(maxTurns int, fn AdapterRunFunc) RunConfig {
	return RunConfig{MaxTurns: maxTurns, AgentID: "zk", SkipGitPreflight: true, RunAdapter: fn}
}

// A drain with two live runs waits for both: it never cuts or suspends them
// (finish mode), and each run's second turn still runs.
func TestZeroKill_DrainWaitsForTwoLiveRuns(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	d := zkDB(t)
	for _, id := range []string{"zk-r1", "zk-r2", "zk-r3"} {
		zkTask(t, d, id, repo)
	}
	s := useSlots(t, 9)
	h := zkHarness(t, d, repo)

	release := map[string]chan struct{}{"zk-r1": make(chan struct{}), "zk-r2": make(chan struct{})}
	var mu sync.Mutex
	calls := map[string]int{}
	cut := map[string]bool{}
	adapter := func(id string) AdapterRunFunc {
		return func(ctx context.Context, _, _ string, _, _ []string, _, _ io.Writer) error {
			mu.Lock()
			calls[id]++
			n := calls[id]
			mu.Unlock()
			if n == 1 {
				<-release[id]
				if ctx.Err() != nil {
					mu.Lock()
					cut[id] = true
					mu.Unlock()
				}
			}
			return nil
		}
	}
	results := make(chan *RunResult, 2)
	for _, id := range []string{"zk-r1", "zk-r2"} {
		go func(id string) {
			r, err := h.Run(context.Background(), id, zkRunCfg(2, adapter(id)))
			if err != nil {
				t.Errorf("run %s: %v", id, err)
			}
			results <- r
		}(id)
	}
	waitFor(t, "both runs in their first turn", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls["zk-r1"] == 1 && calls["zk-r2"] == 1
	})

	s.StartDrain(DrainFinish)
	if st := s.DrainStatus(); st.Live != 2 {
		t.Fatalf("live = %d, want 2", st.Live)
	}
	if _, err := h.Run(context.Background(), "zk-r3", zkRunCfg(1, adapter("zk-r3"))); !errors.Is(err, ErrDraining) {
		t.Fatalf("third run during drain: %v, want ErrDraining", err)
	}

	close(release["zk-r1"])
	waitFor(t, "one run left", func() bool { return s.DrainStatus().Live == 1 })
	close(release["zk-r2"])
	waitFor(t, "no run left", func() bool { return s.DrainStatus().Live == 0 })

	for i := 0; i < 2; i++ {
		if r := <-results; r == nil || r.Disposition == SuspendedDisposition {
			t.Fatalf("a finish drain suspended a run: %+v", r)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["zk-r1"] != 2 || calls["zk-r2"] != 2 {
		t.Fatalf("turns = %v, want both runs to finish their 2 turns", calls)
	}
	if cut["zk-r1"] || cut["zk-r2"] {
		t.Fatalf("a finish drain cancelled a live turn: %v", cut)
	}
}

// --max-wait escalates the drain to boundary: the run stops after the turn
// in flight ends on its own (never mid-turn), keeps its worktree and its
// uncommitted work, and the next daemon resumes it from the next turn with
// the whole brief and a note saying where it stopped.
func TestZeroKill_BoundarySuspendsAndNextDaemonResumes(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	d := zkDB(t)
	zkTask(t, d, "zk-s", repo)
	s := useSlots(t, 9)
	s.Store = SQLQueueStore{DB: d}
	h := zkHarness(t, d, repo)

	var turn1Cut error
	calls := 0
	res, err := h.Run(context.Background(), "zk-s", zkRunCfg(5, func(ctx context.Context, cwd, _ string, _, _ []string, _, _ io.Writer) error {
		calls++
		if err := os.WriteFile(filepath.Join(cwd, "wip.txt"), []byte("half done\n"), 0o644); err != nil {
			return err
		}
		s.StartDrain(DrainBoundary) // --max-wait passed during turn 1
		time.Sleep(100 * time.Millisecond)
		turn1Cut = ctx.Err()
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Disposition != SuspendedDisposition {
		t.Fatalf("disposition = %q, want suspended", res.Disposition)
	}
	if calls != 1 || turn1Cut != nil {
		t.Fatalf("calls = %d, turn 1 cut = %v: want turn 1 to finish uncut and no turn 2", calls, turn1Cut)
	}
	lr := liveRun(context.Background(), d, "zk-s")
	if lr == nil || lr.State != liveSuspended || lr.NextTurn != 1 || lr.CheckpointSHA == "" {
		t.Fatalf("live run = %+v, want suspended at turn 1 with a checkpoint", lr)
	}
	if b, err := os.ReadFile(filepath.Join(lr.Worktree, "wip.txt")); err != nil || string(b) != "half done\n" {
		t.Fatalf("worktree lost its uncommitted work: %q %v", b, err)
	}
	if got := stageOf(t, d, "zk-s"); got != "todo" {
		t.Fatalf("stage = %q, want todo", got)
	}
	if got := queueIDs(t, d); len(got) != 1 || got[0] != "zk-s:resume" {
		t.Fatalf("queue = %v, want [zk-s:resume]", got)
	}

	// The next daemon.
	if _, err := d.Exec(`UPDATE live_runs SET daemon_pid=1`); err != nil {
		t.Fatal(err)
	}
	s2 := useSlots(t, 9)
	s2.Store = SQLQueueStore{DB: d}
	rep := RecoverLiveRuns(context.Background(), d, nil)
	if len(rep.Resumed) != 1 || rep.Resumed[0] != "zk-s" {
		t.Fatalf("recovery = %+v, want zk-s resumed", rep)
	}
	if got := queueIDs(t, d); len(got) != 1 || got[0] != "zk-s:resume" {
		t.Fatalf("queue after recovery = %v, want one zk-s:resume", got)
	}

	var prompt, cwd2, wip string
	res2, err := h.Run(context.Background(), "zk-s", zkRunCfg(5, func(_ context.Context, cwd, _ string, raw, _ []string, stdout, _ io.Writer) error {
		prompt, cwd2 = raw[1], cwd
		b, _ := os.ReadFile(filepath.Join(cwd, "wip.txt"))
		wip = string(b)
		fmt.Fprintln(stdout, taskCompleteMarker)
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res2.Disposition != "in_review" {
		t.Fatalf("resumed run disposition = %q, want in_review", res2.Disposition)
	}
	if cwd2 != lr.Worktree || wip != "half done\n" {
		t.Fatalf("resumed in %q with wip %q, want %q with the suspended work", cwd2, wip, lr.Worktree)
	}
	for _, want := range []string{"<<<TASK_BRIEF_BEGIN>>>", "(turn 2)", "before turn 2", "do not start over"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("resumed prompt lacks %q:\n%s", want, prompt)
		}
	}
	var n int
	_ = d.QueryRow(`SELECT COUNT(*) FROM task_comments WHERE task_id='zk-s' AND message LIKE 'git-preflight: skipped (resuming run%'`).Scan(&n)
	if n != 1 {
		t.Errorf("no 'pre-flight skipped' comment for the resumed run")
	}
	if liveRun(context.Background(), d, "zk-s") != nil {
		t.Errorf("finished run left its live_runs row")
	}
}

// --now cuts the turn in flight and suspends the run to redo that turn.
func TestZeroKill_DrainNowCutsTurnAndRedoesIt(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	d := zkDB(t)
	zkTask(t, d, "zk-n", repo)
	s := useSlots(t, 9)
	h := zkHarness(t, d, repo)

	res, err := h.Run(context.Background(), "zk-n", zkRunCfg(5, func(ctx context.Context, _, _ string, _, _ []string, _, _ io.Writer) error {
		go s.StartDrain(DrainNow)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Second):
			return errors.New("turn was not cut")
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Disposition != SuspendedDisposition {
		t.Fatalf("disposition = %q, want suspended", res.Disposition)
	}
	if lr := liveRun(context.Background(), d, "zk-n"); lr == nil || lr.NextTurn != 0 || lr.State != liveSuspended {
		t.Fatalf("live run = %+v, want suspended to redo turn 1", lr)
	}
	var n int
	_ = d.QueryRow(`SELECT COUNT(*) FROM run_errors WHERE task_id='zk-n'`).Scan(&n)
	if n != 0 {
		t.Errorf("a drain cut was recorded as an adapter error")
	}
}

// Recovery leaves a Board-paused run and a run resumed too often for the
// Board, drops the row of a task closed meanwhile, and queues nothing for them.
func TestZeroKill_RecoveryHoldsPausedAndLoopingRuns(t *testing.T) {
	d := zkDB(t)
	ctx := context.Background()
	for _, id := range []string{"zk-p", "zk-l", "zk-c"} {
		zkTask(t, d, id, "")
		liveBegin(ctx, d, LiveRun{TaskID: id, RunID: "old-" + id, NextTurn: 2})
	}
	if err := liveSuspend(d, "zk-p", "old-zk-p", 3, 0, "", true); err != nil {
		t.Fatal(err)
	}
	_, _ = d.Exec(`UPDATE live_runs SET resumes=? WHERE task_id='zk-l'`, maxAutoResumes)
	_, _ = d.Exec(`UPDATE tasks SET execution_stage='done' WHERE id='zk-c'`)
	_, _ = d.Exec(`UPDATE live_runs SET daemon_pid=1`)

	rep := RecoverLiveRuns(ctx, d, nil)
	if len(rep.Resumed) != 0 || strings.Join(rep.Held, ",") != "zk-p,zk-l" && strings.Join(rep.Held, ",") != "zk-l,zk-p" {
		t.Fatalf("recovery = %+v, want zk-p and zk-l held, none resumed", rep)
	}
	if got := queueIDs(t, d); len(got) != 0 {
		t.Fatalf("queued %v for held or closed runs", got)
	}
	if liveRun(ctx, d, "zk-c") != nil {
		t.Errorf("closed task kept its live_runs row")
	}
	if lr := liveRun(ctx, d, "zk-l"); lr == nil || lr.State != liveInterrupted {
		t.Errorf("cut-off run not marked interrupted: %+v", lr)
	}
}

func TestZeroKill_DrainOnlyEscalates(t *testing.T) {
	s := NewRunSlots(3)
	s.Wake = func(string, string) {}
	s.StartDrain(DrainBoundary)
	s.StartDrain(DrainFinish)
	if s.Drain() != DrainBoundary {
		t.Fatalf("drain relaxed to %v", s.Drain())
	}
	select {
	case <-s.DrainBoundaryChan():
	default:
		t.Fatal("boundary channel not closed at DrainBoundary")
	}
	select {
	case <-s.DrainNowChan():
		t.Fatal("now channel closed below DrainNow")
	default:
	}
	nowCh := s.DrainNowChan()
	s.CancelDrain()
	if s.Drain() != DrainOff || s.SuspendRequested() {
		t.Fatal("CancelDrain did not end the drain")
	}
	// A turn holding the channel from before the cancel still hears a later --now.
	s.StartDrain(DrainNow)
	select {
	case <-nowCh:
	default:
		t.Fatal("a later DrainNow did not reach a channel taken before the cancel")
	}
}
