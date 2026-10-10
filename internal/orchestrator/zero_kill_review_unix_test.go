//go:build !windows

package orchestrator

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/procwatch"
)

// Board review of 4136a01 (task-db71fba9): failure cases for items 2, 4 and 6.

// Item 2: a queued run's run_queue row is kept until the run's live_runs row
// exists, so a kill -9 between the claim and liveBegin loses neither.
func TestZeroKill_QueueRowKeptUntilRunIsLive(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	d := zkDB(t)
	zkTask(t, d, "zk-q", repo)
	s := useSlots(t, 9)
	s.Store = SQLQueueStore{DB: d}
	h := zkHarness(t, d, repo)
	s.Enqueue("zk-q", h.SlotKeyForTask(context.Background(), "zk-q"), "run_now", WaitDrain)

	if err := s.Acquire("zk-q", h.SlotKeyForTask(context.Background(), "zk-q")); err != nil {
		t.Fatal(err)
	}
	if got := queueIDs(t, d); len(got) != 1 {
		t.Fatalf("persisted queue after Acquire = %v: a kill -9 before the run is live would lose it", got)
	}
	s.Release("zk-q")

	var queuedDuringTurn []string
	var liveDuringTurn bool
	_, err := h.Run(context.Background(), "zk-q", zkRunCfg(1, func(context.Context, string, string, []string, []string, io.Writer, io.Writer) error {
		queuedDuringTurn = queueIDs(t, d)
		liveDuringTurn = liveRun(context.Background(), d, "zk-q") != nil
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !liveDuringTurn || len(queuedDuringTurn) != 0 {
		t.Fatalf("during the turn: live=%v queue=%v, want live and dequeued", liveDuringTurn, queuedDuringTurn)
	}
}

// Item 2: a run refused before it is live does not leave a stale queue row
// behind for the next daemon.
func TestZeroKill_RefusedRunDropsQueueRow(t *testing.T) {
	d := zkDB(t)
	zkTask(t, d, "zk-r", "")
	_, _ = d.Exec(`UPDATE tasks SET execution_stage='done' WHERE id='zk-r'`)
	s := useSlots(t, 9)
	s.Store = SQLQueueStore{DB: d}
	h := &Harness{DB: d, RepoRoot: t.TempDir()}
	s.Enqueue("zk-r", SlotKey{}, "run_now", WaitDrain)
	if _, err := h.Run(context.Background(), "zk-r", zkRunCfg(1, nil)); !errors.Is(err, ErrNotRunnable) {
		t.Fatalf("Run = %v, want ErrNotRunnable", err)
	}
	s.Dequeue("zk-r") // what the wake path does on ErrNotRunnable
	if got := queueIDs(t, d); len(got) != 0 {
		t.Fatalf("persisted queue = %v, want the refused run gone", got)
	}
}

// Item 4: the new daemon got the old daemon's pid. Its suspended run is still
// the previous daemon's: recovery resumes it and Run keeps its worktree.
func TestZeroKill_NewDaemonWithOldPidResumesRun(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	d := zkDB(t)
	zkTask(t, d, "zk-p", repo)
	s := useSlots(t, 9)
	s.Store = SQLQueueStore{DB: d}
	h := zkHarness(t, d, repo)

	res, err := h.Run(context.Background(), "zk-p", zkRunCfg(5, func(_ context.Context, cwd, _ string, _, _ []string, _, _ io.Writer) error {
		s.StartDrain(DrainBoundary)
		return os.WriteFile(filepath.Join(cwd, "wip.txt"), []byte("wip\n"), 0o644)
	}))
	if err != nil || res.Disposition != SuspendedDisposition {
		t.Fatalf("first run: %v %+v", err, res)
	}
	lr := liveRun(context.Background(), d, "zk-p")
	// Same pid (os.Getpid()), different daemon instance.
	if _, err := d.Exec(`UPDATE live_runs SET daemon_id='previous-daemon'`); err != nil {
		t.Fatal(err)
	}
	_, _ = d.Exec(`DELETE FROM run_queue`)
	useSlots(t, 9).Store = SQLQueueStore{DB: d}
	rep := RecoverLiveRuns(context.Background(), d, nil)
	if len(rep.Resumed) != 1 {
		t.Fatalf("recovery = %+v, want the run resumed", rep)
	}
	var cwd2, wip string
	if _, err := h.Run(context.Background(), "zk-p", zkRunCfg(5, func(_ context.Context, cwd, _ string, _, _ []string, stdout, _ io.Writer) error {
		cwd2 = cwd
		b, _ := os.ReadFile(filepath.Join(cwd, "wip.txt"))
		wip = string(b)
		_, err := io.WriteString(stdout, taskCompleteMarker+"\n")
		return err
	})); err != nil {
		t.Fatal(err)
	}
	if cwd2 != lr.Worktree || wip != "wip\n" {
		t.Fatalf("resumed in %q with wip %q: the worktree was recreated and its work destroyed", cwd2, wip)
	}
}

// Item 4: the old daemon's pid now belongs to an unrelated live process.
// Its run is still left behind: the orphan is stopped and the run resumed.
func TestZeroKill_OldDaemonPidReusedStillRecovers(t *testing.T) {
	useOrphanGrace(t, 300*time.Millisecond)
	d := zkDB(t)
	zkTask(t, d, "zk-x", "")
	ctx := context.Background()
	other, _ := startGroup(t, t.TempDir(), "sleep 300") // took the old daemon's pid
	agent, agentAt := startGroup(t, t.TempDir(), "sleep 300")
	liveBegin(ctx, d, LiveRun{TaskID: "zk-x", RunID: "old-run", NextTurn: 2})
	liveAgent(d, "zk-x", "old-run", agent, agentAt)
	if _, err := d.Exec(`UPDATE live_runs SET daemon_id='previous-daemon', daemon_pid=?, daemon_started_at=?`,
		other, fmtTS(time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	rep := RecoverLiveRuns(ctx, d, nil)
	if len(rep.Stopped) != 1 || len(rep.Resumed) != 1 {
		t.Fatalf("recovery = %+v, want the orphan stopped and the run resumed", rep)
	}
	waitFor(t, "orphan gone", func() bool { return !procwatch.GroupAlive(agent) })
}

// Item 6: a process a finished run left in the worktree (an MCP or dev
// server keeps STAYPOINT_TASK_ID and its cwd) does not block later runs.
// Only agents that predate the live_runs record count.
func TestZeroKill_LeftoverProcessDoesNotBlockNextRun(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	d := zkDB(t)
	zkTask(t, d, "zk-l", repo)
	wt := filepath.Join(repo, ".worktrees", "zk-l")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // ps start times are to the second
	startAgent(t, wt, "STAYPOINT_TASK_ID=zk-l")
	waitFor(t, "lsof to see the leftover", func() bool {
		pids, _ := procwatch.AgentsInDir(wt, "STAYPOINT_TASK_ID=zk-l")
		return len(pids) > 0
	})
	useSlots(t, 9)
	h := zkHarness(t, d, repo)
	ran := false
	_, err := h.Run(context.Background(), "zk-l", zkRunCfg(1, func(context.Context, string, string, []string, []string, io.Writer, io.Writer) error {
		ran = true
		return nil
	}))
	if err != nil || !ran {
		t.Fatalf("err=%v ran=%v: a leftover process from a recorded run blocked the task", err, ran)
	}
}
