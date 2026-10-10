//go:build !windows

package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/procwatch"
)

// startGroup starts `sh -c script` as the leader of its own process group,
// as the adapter starts agent CLIs, and reaps it in the background so a
// killed group does not linger as a zombie.
func startGroup(t *testing.T, dir, script string) (int, time.Time) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	return pid, started
}

// TestZeroKillSleepHelper is a fake agent whose environment ps can read
// (macOS hides it for its own platform binaries such as /bin/sleep).
func TestZeroKillSleepHelper(t *testing.T) {
	if os.Getenv("ZK_SLEEP") == "" {
		t.Skip("helper process")
	}
	time.Sleep(5 * time.Minute)
}

// startAgent starts a fake agent in dir with no terminal and env.
func startAgent(t *testing.T, dir string, env ...string) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestZeroKillSleepHelper$")
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "ZK_SLEEP=1"), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	return pid
}

func useOrphanGrace(t *testing.T, d time.Duration) {
	prev := orphanStopGrace
	orphanStopGrace = d
	t.Cleanup(func() { orphanStopGrace = prev })
}

// task-ae1414b0: a daemon restart orphaned a running agent CLI, and the next
// Run Now put a second agent in the same worktree. The agent's process group
// is recorded; while it lives the task does not start, and the next daemon's
// recovery stops it (SIGKILL when it ignores SIGTERM) and queues the run to
// resume.
func TestZeroKill_OrphanedAgentBlocksDuplicateThenIsStopped(t *testing.T) {
	useOrphanGrace(t, 300*time.Millisecond)
	repo := t.TempDir()
	initGitRepo(t, repo)
	d := zkDB(t)
	zkTask(t, d, "zk-o", repo)
	_, _ = d.Exec(`UPDATE tasks SET execution_stage='in_progress', checkout_run_id='old-run' WHERE id='zk-o'`)
	ctx := context.Background()

	pid, started := startGroup(t, repo, `trap "" TERM; sleep 300`)
	liveBegin(ctx, d, LiveRun{TaskID: "zk-o", RunID: "old-run", NextTurn: 3})
	liveAgent(d, "zk-o", "old-run", pid, started)
	_, _ = d.Exec(`UPDATE live_runs SET daemon_pid=1, daemon_id='previous-daemon'`) // the previous daemon's row

	s := useSlots(t, 9)
	h := zkHarness(t, d, repo)
	_, _ = d.Exec(`UPDATE tasks SET checkout_run_id=NULL WHERE id='zk-o'`) // RecoveryScan would clear it
	ran := false
	_, err := h.Run(ctx, "zk-o", zkRunCfg(1, func(context.Context, string, string, []string, []string, io.Writer, io.Writer) error {
		ran = true
		return nil
	}))
	if !errors.Is(err, ErrAgentStillRunning) || ran {
		t.Fatalf("Run with the old agent alive: err=%v ran=%v, want ErrAgentStillRunning and no second agent", err, ran)
	}
	if s.Active() != 0 {
		t.Fatalf("refused run kept its slot")
	}
	var stage string
	var checkout sql.NullString
	_ = d.QueryRow(`SELECT execution_stage, checkout_run_id FROM tasks WHERE id='zk-o'`).Scan(&stage, &checkout)
	if stage != "todo" || checkout.Valid {
		t.Fatalf("refused run left stage=%q checkout=%v; want todo with no checkout", stage, checkout)
	}

	rep := RecoverLiveRuns(ctx, d, nil)
	if len(rep.Stopped) != 1 || rep.Stopped[0] != "zk-o" {
		t.Fatalf("recovery = %+v, want the orphan stopped", rep)
	}
	waitFor(t, "orphan group gone", func() bool { return !procwatch.GroupAlive(pid) })
	if got := queueIDs(t, d); len(got) != 1 || got[0] != "zk-o:resume" {
		t.Fatalf("queue = %v, want the cut-off run queued to resume", got)
	}
}

// A recorded pgid whose leader is a different process now (pid reused after
// the old agent died) is never killed.
func TestZeroKill_ReusedPidIsNotKilled(t *testing.T) {
	useOrphanGrace(t, 300*time.Millisecond)
	d := zkDB(t)
	zkTask(t, d, "zk-u", "")
	ctx := context.Background()
	pid, _ := startGroup(t, t.TempDir(), "sleep 300")
	liveBegin(ctx, d, LiveRun{TaskID: "zk-u", RunID: "old-run"})
	liveAgent(d, "zk-u", "old-run", pid, time.Now().Add(-time.Hour)) // recorded start: an hour ago
	_, _ = d.Exec(`UPDATE live_runs SET daemon_pid=1, daemon_id='previous-daemon'`)

	rep := RecoverLiveRuns(ctx, d, nil)
	if len(rep.Stopped) != 0 || len(rep.Unstopped) != 0 {
		t.Fatalf("recovery = %+v, want the unrelated process left alone", rep)
	}
	time.Sleep(200 * time.Millisecond)
	if !procwatch.GroupAlive(pid) {
		t.Fatal("recovery killed a process that is not the recorded agent")
	}
}

// An agent no live_runs row knows about (left by a daemon from before the
// record existed) still blocks the run, and its worktree is not removed from
// under it. A headless process that is not the task's agent (git's
// fsmonitor daemon, say) does not block it.
func TestZeroKill_UnrecordedAgentInWorktreeBlocksRun(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	d := zkDB(t)
	zkTask(t, d, "zk-h", repo)
	// The database began recording agents after these agents started.
	if _, err := d.Exec(`UPDATE schema_migrations SET applied_at=? WHERE version=47`,
		time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(repo, ".worktrees", "zk-h")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	// Not an agent: headless, in the worktree, but no task marker.
	startAgent(t, wt, "STAYPOINT_TASK_ID=zk-other")
	time.Sleep(300 * time.Millisecond)
	if got := agentsIn(context.Background(), d, wt, "zk-h"); len(got) != 0 {
		t.Fatalf("a process without the task's marker counted as its agent: %v", got)
	}
	startAgent(t, wt, "STAYPOINT_TASK_ID=zk-h") // the orphaned agent
	useSlots(t, 9)
	h := zkHarness(t, d, repo)
	waitFor(t, "lsof to see the agent", func() bool { return len(agentsIn(context.Background(), d, wt, "zk-h")) > 0 })

	ran := false
	_, err := h.Run(context.Background(), "zk-h", zkRunCfg(1, func(context.Context, string, string, []string, []string, io.Writer, io.Writer) error {
		ran = true
		return nil
	}))
	if !errors.Is(err, ErrAgentStillRunning) || ran {
		t.Fatalf("err=%v ran=%v, want ErrAgentStillRunning and no run", err, ran)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree removed from under the live agent: %v", err)
	}
}

// task-0e2d556c: the agent CLI exited but its turn never ended (its output
// stayed open), which left the task "running" and Run Now refused. The turn
// is ended after agentExitGrace and the slot is freed.
//
// This is the real shape (local-e478f754, Board review of 4136a01): a
// grandchild holds the stdout pipe, so the adapter never reaches Wait and the
// exited CLI stays an unreaped zombie, which kill(pid, 0) still reports alive.
func TestZeroKill_DeadAgentTurnIsReaped(t *testing.T) {
	prevGrace, prevPoll := agentExitGrace, agentExitPoll
	agentExitGrace, agentExitPoll = 200*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { agentExitGrace, agentExitPoll = prevGrace, prevPoll })

	repo := t.TempDir()
	initGitRepo(t, repo)
	d := zkDB(t)
	zkTask(t, d, "zk-d", repo)
	s := useSlots(t, 9)
	h := zkHarness(t, d, repo)

	var reaped bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = h.Run(context.Background(), "zk-d", zkRunCfg(1, func(ctx context.Context, _, _ string, _, _ []string, _, _ io.Writer) error {
			// The CLI exits at once; its background child keeps stdout open.
			cli := exec.Command("sh", "-c", "sleep 20 & exit 0")
			cli.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			out, err := cli.StdoutPipe()
			if err != nil {
				return err
			}
			if err := cli.Start(); err != nil {
				return err
			}
			pid := cli.Process.Pid
			defer func() {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
				_ = cli.Wait()
			}()
			procwatch.Spawned(ctx, pid)
			read := make(chan struct{})
			go func() { _, _ = io.Copy(io.Discard, out); close(read) }()
			// No Wait until stdout closes, as the adapter does.
			select {
			case <-ctx.Done():
				reaped = true
				return ctx.Err()
			case <-read:
				return nil
			}
		}))
	}()
	select {
	case <-done:
	case <-time.After(25 * time.Second):
		t.Fatal("run with a dead agent never ended")
	}
	if !reaped {
		t.Fatal("the hung turn was not ended after its agent exited")
	}
	if s.Active() != 0 {
		t.Fatal("reaped run kept its slot: Run Now would be refused")
	}
}
