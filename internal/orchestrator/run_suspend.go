package orchestrator

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/checkpoint"
	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
	"github.com/VinnyVanGogh/staypoint/internal/procwatch"
	"github.com/VinnyVanGogh/staypoint/internal/security"
)

// Zero-kill deploys (task-db71fba9). Each harness turn is a fresh agent CLI
// process, so the gap between two turns is a lossless place to stop a run:
// the worktree holds everything the agent did, and the next turn is built
// from the brief and comments. A drain suspends a run there; the next daemon
// resumes it from that turn in the same worktree.

// SuspendedDisposition is RunResult.Disposition for a run suspended for a
// deploy. The task goes back to todo and the run waits in the run queue.
const SuspendedDisposition = "suspended"

// agentExitGrace is how long a turn may stay open after its agent CLI exited
// before the harness ends it. The CLI's output pipe can be held open by a
// grandchild outside its process group, which left a run "running" with no
// agent and blocked Run Now until a daemon restart (task-0e2d556c).
var agentExitGrace = 60 * time.Second

// agentExitPoll is how often a turn checks its agent process.
var agentExitPoll = 5 * time.Second

// turnGuard watches one adapter turn: it cancels the turn when the drain
// reaches DrainNow, and when the agent CLI has exited but the turn has not
// ended within agentExitGrace. The spawn hook it installs records the agent's
// process group in live_runs.
type turnGuard struct {
	pid     atomic.Int64
	drained atomic.Bool
	reaped  atomic.Bool
}

func (h *Harness) guardTurn(ctx context.Context, taskID, runID string, cancel context.CancelFunc) (context.Context, *turnGuard) {
	g := &turnGuard{}
	ctx = procwatch.WithSpawnHook(ctx, func(pid int) {
		g.pid.Store(int64(pid))
		// The start time ps reports, so a later Verify compares ps with ps.
		at, ok := procwatch.StartTime(pid)
		if !ok {
			at = time.Now()
		}
		liveAgent(h.DB, taskID, runID, pid, at)
	})
	nowCh := h.slots().DrainNowChan()
	go func() {
		t := time.NewTicker(agentExitPoll)
		defer t.Stop()
		var goneSince time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-nowCh:
				if ctx.Err() != nil {
					return // the turn had already ended
				}
				g.drained.Store(true)
				cancel()
				return
			case <-t.C:
				if !procwatch.Supported {
					// No process checks here (Windows): the stubs would
					// read every agent as exited and cut its turn.
					continue
				}
				pid := int(g.pid.Load())
				// A zombie (exited, not yet reaped because a grandchild
				// holds its output open) counts as exited.
				if pid <= 0 || procwatch.PidRunning(pid) {
					goneSince = time.Time{}
					continue
				}
				if goneSince.IsZero() {
					goneSince = time.Now()
					continue
				}
				if time.Since(goneSince) >= agentExitGrace {
					g.reaped.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	return ctx, g
}

// worktreeOnBranch reports whether dir is a git worktree checked out on
// branch, so a resumed run can keep it (and its uncommitted work).
func worktreeOnBranch(ctx context.Context, dir, branch string) bool {
	if dir == "" {
		return false
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return false
	}
	cmd := gitexec.Command(ctx, "symbolic-ref", "--short", "HEAD")
	cmd.Dir = dir
	cmd.Env = security.ChildEnv()
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == branch
}

// agentsIn lists the agents for taskID still working in dir (see
// procwatch.AgentsInDir) that no live_runs row could have recorded: those
// started before this database began recording agents (recordingSince).
// Anything newer is either recorded (previousAgentAlive covers it) or a
// leftover of a finished run, such as an MCP or dev server, which must not
// block the task. A failed check is logged and treated as none: the recorded
// process groups in live_runs are the primary guard.
func agentsIn(ctx context.Context, db *sql.DB, dir, taskID string) []int {
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	pids, err := procwatch.AgentsInDir(dir, "STAYPOINT_TASK_ID="+taskID)
	if err != nil {
		slog.Warn("worktree agent check failed; relying on recorded agent processes",
			slog.String("dir", dir), slog.Any("error", err))
		return nil
	}
	since, ok := recordingSince(ctx, db)
	if !ok {
		return pids
	}
	var old []int
	for _, p := range pids {
		// ps rounds start times down to the second.
		if at, ok := procwatch.StartTime(p); !ok || at.Before(since.Truncate(time.Second)) {
			old = append(old, p)
		}
	}
	return old
}

// refuseSecondAgent records why a run did not start: an agent is still
// running in the task's worktree. The wake path starts the run again once
// it exits (AgentStillRunningError).
func refuseSecondAgent(ctx context.Context, db *sql.DB, taskID, who string) {
	msg := fmt.Sprintf("Run not started: %s is still running in this task's worktree, and two agents in one worktree overwrite each other's work. "+
		"It is a leftover from an earlier run. The run starts on its own once it exits; stop it to start sooner.", who)
	slog.Warn("run refused: agent still running in worktree", slog.String("task", taskID), slog.String("agent", who))
	// Claim moved the task to in_progress; with no run it must not stay there.
	_, _ = db.ExecContext(ctx, `UPDATE tasks SET execution_stage='todo', updated_at=? WHERE id=? AND execution_stage='in_progress'`,
		time.Now().UTC().Format(time.RFC3339Nano), taskID)
	postHarnessComment(ctx, db, taskID, msg)
	_, _ = db.ExecContext(ctx, `INSERT INTO activity_log (task_id, event_type, details) VALUES (?, 'run_refused_live_agent', ?)`, taskID, who)
}

// resumeNote is added to the first prompt of a resumed run.
func resumeNote(r *LiveRun) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[StayPoint] This run was stopped by a StayPoint daemon restart before turn %d", r.NextTurn+1)
	if r.State == liveInterrupted {
		b.WriteString(" (it was cut off mid-turn, so the last turn's work may be partly done)")
	}
	b.WriteString(". Your worktree is as the run left it, uncommitted changes included")
	if r.CheckpointSHA != "" {
		fmt.Fprintf(&b, " (daemon checkpoint %.12s)", r.CheckpointSHA)
	}
	b.WriteString(". Check `git status` and `git log`, then continue the task from there; do not start over.")
	return b.String()
}

// suspendRun stops a run at a turn boundary for a deploy: it checkpoints the
// worktree, marks the live_runs row suspended at turn next, puts the task
// back to todo and queues the run (persisted) to resume. It returns false
// when the suspension could not be recorded; the run then carries on, since
// a suspended run nobody can find again would be lost.
func (h *Harness) suspendRun(taskID, runID, wtPath string, nonGit bool, next int, lastSeen int64, boardPaused bool, sr *StepRecorder, runLog *slog.Logger) bool {
	cpSHA := ""
	if !nonGit {
		cpCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cp, err := checkpoint.CreateCheckpoint(cpCtx, checkpoint.CreateOptions{
			WorkDir:   wtPath,
			SessionID: runID,
			Message:   fmt.Sprintf("suspended for deploy before turn %d %s", next+1, taskID),
			Timeout:   30 * time.Second,
		})
		cancel()
		if err != nil {
			runLog.Warn("suspend: checkpoint failed; the worktree still holds the work", slog.Any("error", err))
		} else if cp != nil {
			cpSHA = cp.CommitSHA
			if sr != nil {
				sr.EmitCheckpoint(cp.ID, fmt.Sprintf("suspended before turn %d", next+1))
			}
		}
	}
	if err := liveSuspend(h.DB, taskID, runID, next, lastSeen, cpSHA, boardPaused); err != nil {
		runLog.Error("suspend: could not record the suspended run; carrying on instead", slog.Any("error", err))
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = h.DB.ExecContext(ctx,
		`UPDATE tasks SET execution_stage='todo', updated_at=? WHERE id=?`+closedStageGuard, now, taskID)
	msg := fmt.Sprintf("Paused for a StayPoint deploy before turn %d. The run resumes from there, in the same worktree, when the daemon restarts.", next+1)
	if boardPaused {
		msg = fmt.Sprintf("Suspended for a StayPoint deploy while the Board had it paused (before turn %d). It is not restarted on its own: press Run Now to resume it from that turn.", next+1)
	}
	postHarnessComment(ctx, h.DB, taskID, msg)
	_, _ = h.DB.ExecContext(ctx, `INSERT INTO activity_log (task_id, event_type, details) VALUES (?, 'run_suspended', ?)`,
		taskID, fmt.Sprintf("next_turn=%d checkpoint=%s board_paused=%t", next, cpSHA, boardPaused))
	if sr != nil {
		sr.EmitMessage("Paused for daemon restart", msg, "done")
	}
	if !boardPaused {
		h.slots().Enqueue(taskID, h.SlotKeyForTask(ctx, taskID), "resume_after_restart", WaitResume)
	}
	runLog.Info("run suspended for deploy", slog.Int("next_turn", next), slog.String("checkpoint", cpSHA))
	return true
}
