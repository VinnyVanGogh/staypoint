package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/governance"
	"github.com/VinnyVanGogh/staypoint/internal/procwatch"
)

// tsLayout is a fixed-width UTC timestamp, so stored times sort as text.
const tsLayout = "2006-01-02T15:04:05.000000000Z"

func fmtTS(t time.Time) string { return t.UTC().Format(tsLayout) }

func parseTS(s string) time.Time {
	t, err := time.Parse(tsLayout, s)
	if err != nil {
		t, _ = time.Parse(time.RFC3339Nano, s)
	}
	return t
}

// QueueStore persists the run queue (STA-846).
type QueueStore interface {
	// SaveQueued inserts q, or updates its wait and slot key while keeping
	// its place (queued_at) when it is already stored.
	SaveQueued(q QueuedRun) error
	DeleteQueued(taskID string) error
}

// SQLQueueStore stores the run queue in the run_queue table.
type SQLQueueStore struct{ DB *sql.DB }

func (s SQLQueueStore) SaveQueued(q QueuedRun) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plain := 0
	if q.Plain {
		plain = 1
	}
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO run_queue (task_id, repo_key, plain, org, reason, wait, queued_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(task_id) DO UPDATE SET
		   repo_key=excluded.repo_key, plain=excluded.plain, org=excluded.org, wait=excluded.wait`,
		q.TaskID, q.RepoKey, plain, q.Org, q.Reason, q.Wait, fmtTS(q.QueuedAt))
	return err
}

func (s SQLQueueStore) DeleteQueued(taskID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.DB.ExecContext(ctx, `DELETE FROM run_queue WHERE task_id=?`, taskID)
	return err
}

// LoadQueue returns the persisted run queue, oldest first.
func LoadQueue(ctx context.Context, db *sql.DB) ([]QueuedRun, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT task_id, repo_key, plain, org, reason, wait, queued_at
		   FROM run_queue ORDER BY queued_at, rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueuedRun
	for rows.Next() {
		var q QueuedRun
		var plain int
		var at string
		if err := rows.Scan(&q.TaskID, &q.RepoKey, &plain, &q.Org, &q.Reason, &q.Wait, &at); err != nil {
			return nil, err
		}
		q.Plain = plain != 0
		q.QueuedAt = parseTS(at)
		out = append(out, q)
	}
	return out, rows.Err()
}

// Live run states (live_runs.state).
const (
	liveRunning     = "running"
	liveSuspended   = "suspended"   // stopped cleanly at a turn boundary by a drain
	liveInterrupted = "interrupted" // its daemon died mid-run (crash, kill -9, launchd timeout)
)

// maxAutoResumes caps how often one run is resumed after a daemon restart
// without a Board action, so a run that keeps taking the daemon down does
// not loop.
const maxAutoResumes = 3

// orphanStopGrace is how long an orphaned agent group gets after SIGTERM
// before SIGKILL.
var orphanStopGrace = 10 * time.Second

// LiveRun is a live_runs row: a run the daemon is driving, or one its
// previous daemon left behind.
type LiveRun struct {
	TaskID            string
	RunID             string
	DaemonPID         int
	AgentPID          int
	AgentStartedAt    time.Time
	Worktree          string
	NextTurn          int
	LastSeenCommentID int64
	WakeReason        string
	PreCheckpointID   string
	PreCheckpointSHA  string
	CheckpointSHA     string
	State             string
	BoardPaused       bool
	Resumes           int
	StartedAt         time.Time
}

const liveRunCols = `task_id, run_id, daemon_pid, agent_pid, agent_started_at, worktree, next_turn,
	last_seen_comment_id, wake_reason, pre_checkpoint_id, pre_checkpoint_sha, checkpoint_sha,
	state, board_paused, resumes, started_at`

func scanLiveRun(sc interface{ Scan(...any) error }) (LiveRun, error) {
	var r LiveRun
	var agentAt, startedAt string
	var paused int
	err := sc.Scan(&r.TaskID, &r.RunID, &r.DaemonPID, &r.AgentPID, &agentAt, &r.Worktree, &r.NextTurn,
		&r.LastSeenCommentID, &r.WakeReason, &r.PreCheckpointID, &r.PreCheckpointSHA, &r.CheckpointSHA,
		&r.State, &paused, &r.Resumes, &startedAt)
	if agentAt != "" {
		r.AgentStartedAt = parseTS(agentAt)
	}
	r.StartedAt = parseTS(startedAt)
	r.BoardPaused = paused != 0
	return r, err
}

// liveRun returns taskID's live_runs row, or nil.
func liveRun(ctx context.Context, db *sql.DB, taskID string) *LiveRun {
	if db == nil {
		return nil
	}
	r, err := scanLiveRun(db.QueryRowContext(ctx, `SELECT `+liveRunCols+` FROM live_runs WHERE task_id=?`, taskID))
	if err != nil {
		return nil
	}
	return &r
}

// liveBegin records that runID is driving taskID in this daemon. resumes is
// carried over from the run it resumes.
func liveBegin(ctx context.Context, db *sql.DB, r LiveRun) {
	if db == nil {
		return
	}
	now := fmtTS(time.Now())
	paused := 0
	if r.BoardPaused {
		paused = 1
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO live_runs (`+liveRunCols+`, updated_at)
		 VALUES (?, ?, ?, 0, '', ?, ?, ?, ?, ?, ?, '', 'running', ?, ?, ?, ?)
		 ON CONFLICT(task_id) DO UPDATE SET
		   run_id=excluded.run_id, daemon_pid=excluded.daemon_pid, agent_pid=0, agent_started_at='',
		   worktree=excluded.worktree, next_turn=excluded.next_turn,
		   last_seen_comment_id=excluded.last_seen_comment_id, wake_reason=excluded.wake_reason,
		   pre_checkpoint_id=excluded.pre_checkpoint_id, pre_checkpoint_sha=excluded.pre_checkpoint_sha,
		   checkpoint_sha='', state='running', board_paused=excluded.board_paused,
		   resumes=excluded.resumes, started_at=excluded.started_at, updated_at=excluded.updated_at`,
		r.TaskID, r.RunID, os.Getpid(), r.Worktree, r.NextTurn, r.LastSeenCommentID, r.WakeReason,
		r.PreCheckpointID, r.PreCheckpointSHA, paused, r.Resumes, now, now,
	); err != nil {
		slog.Warn("live run: record failed; a restart could not find this run's agent",
			slog.String("task", r.TaskID), slog.Any("error", err))
	}
}

// liveTurn records the turn a run is about to start and its comment cursor:
// a restart resumes from here.
func liveTurn(db *sql.DB, taskID, runID string, turn int, lastSeen int64) {
	if db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = db.ExecContext(ctx,
		`UPDATE live_runs SET next_turn=?, last_seen_comment_id=?, agent_pid=0, agent_started_at='', updated_at=?
		  WHERE task_id=? AND run_id=?`,
		turn, lastSeen, fmtTS(time.Now()), taskID, runID)
}

// liveAgent records the agent CLI process group a turn spawned.
func liveAgent(db *sql.DB, taskID, runID string, pid int, startedAt time.Time) {
	if db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = db.ExecContext(ctx,
		`UPDATE live_runs SET agent_pid=?, agent_started_at=?, updated_at=? WHERE task_id=? AND run_id=?`,
		pid, fmtTS(startedAt), fmtTS(time.Now()), taskID, runID)
}

// liveSuspend marks a run suspended at a turn boundary: the next daemon
// resumes it at turn next.
func liveSuspend(db *sql.DB, taskID, runID string, next int, lastSeen int64, cpSHA string, boardPaused bool) error {
	if db == nil {
		return errors.New("no database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	paused := 0
	if boardPaused {
		paused = 1
	}
	res, err := db.ExecContext(ctx,
		`UPDATE live_runs SET state='suspended', next_turn=?, last_seen_comment_id=?, checkpoint_sha=?,
		        board_paused=?, agent_pid=0, agent_started_at='', updated_at=?
		  WHERE task_id=? AND run_id=?`,
		next, lastSeen, cpSHA, paused, fmtTS(time.Now()), taskID, runID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no live run row for %s/%s", taskID, runID)
	}
	return nil
}

// liveEnd removes runID's row: the run ended in this daemon.
func liveEnd(db *sql.DB, taskID, runID string) {
	if db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = db.ExecContext(ctx, `DELETE FROM live_runs WHERE task_id=? AND run_id=?`, taskID, runID)
}

// ErrAgentStillRunning is returned by Run when an agent CLI from an earlier
// run is still running in the task's worktree. Starting a second agent there
// would have two agents editing one checkout (task-ae1414b0).
var ErrAgentStillRunning = errors.New("an agent from an earlier run is still running in this task's worktree")

// previousAgentAlive reports whether taskID's live_runs row, left by an
// earlier daemon, names an agent process group that is still running (or
// alive and unverifiable, which counts as running).
func previousAgentAlive(ctx context.Context, db *sql.DB, taskID string) (int, bool) {
	r := liveRun(ctx, db, taskID)
	if r == nil || r.AgentPID <= 0 || r.DaemonPID == os.Getpid() {
		return 0, false
	}
	ours, err := procwatch.Verify(r.AgentPID, r.AgentStartedAt)
	return r.AgentPID, ours || err != nil
}

// RecoveryReport says what RecoverLiveRuns did.
type RecoveryReport struct {
	Stopped   []string // tasks whose orphaned agent group was stopped
	Unstopped []string // tasks with an agent group still alive (not verified or would not die)
	Resumed   []string // tasks queued to resume
	Held      []string // tasks left for the Board (paused, or resumed too often)
}

// RecoverLiveRuns runs once at daemon start, before RecoveryScan and before
// any run is claimed. For every run its previous daemon left in live_runs it
// stops the run's agent CLI process group if it is still running (it was
// orphaned when the daemon died), then queues the run to resume from the
// turn it stopped at, in the persisted run queue. keyFor gives the slot key
// for a task's queue entry (nil leaves it empty; Acquire computes the real
// one).
func RecoverLiveRuns(ctx context.Context, db *sql.DB, keyFor func(taskID string) SlotKey) RecoveryReport {
	var rep RecoveryReport
	rows, err := db.QueryContext(ctx, `SELECT `+liveRunCols+` FROM live_runs WHERE daemon_pid<>? ORDER BY started_at`, os.Getpid())
	if err != nil {
		slog.Error("live run recovery: read failed", slog.Any("error", err))
		return rep
	}
	var runs []LiveRun
	for rows.Next() {
		r, err := scanLiveRun(rows)
		if err != nil {
			slog.Warn("live run recovery: bad row", slog.Any("error", err))
			continue
		}
		runs = append(runs, r)
	}
	rows.Close()

	for _, r := range runs {
		if r.DaemonPID > 1 && procwatch.PidAlive(r.DaemonPID) {
			// Still driven by a live process (a `staypoint run` CLI, say):
			// not left behind, not ours to stop or resume.
			continue
		}
		note := ""
		if r.AgentPID > 0 {
			ours, verr := procwatch.Verify(r.AgentPID, r.AgentStartedAt)
			switch {
			case verr != nil:
				rep.Unstopped = append(rep.Unstopped, r.TaskID)
				note = fmt.Sprintf("An agent process group (pgid %d) from the previous daemon is still alive but could not be verified, so it was left running; this task will not start again until it exits.", r.AgentPID)
			case ours:
				if procwatch.StopGroup(r.AgentPID, orphanStopGrace) {
					rep.Stopped = append(rep.Stopped, r.TaskID)
					note = fmt.Sprintf("Stopped the agent (process group %d) the previous daemon left running in this task's worktree.", r.AgentPID)
				} else {
					rep.Unstopped = append(rep.Unstopped, r.TaskID)
					note = fmt.Sprintf("The agent (process group %d) the previous daemon left running would not stop; this task will not start again until it exits.", r.AgentPID)
				}
			}
			slog.Warn("live run recovery: orphaned agent", slog.String("task", r.TaskID),
				slog.Int("pgid", r.AgentPID), slog.String("result", note))
		}

		var stage string
		_ = db.QueryRowContext(ctx, `SELECT execution_stage FROM tasks WHERE id=?`, r.TaskID).Scan(&stage)
		if stage == "" || !governance.IsRunnableStage(stage) {
			// Closed or parked while it ran: nothing to resume.
			_, _ = db.ExecContext(ctx, `DELETE FROM live_runs WHERE task_id=?`, r.TaskID)
			if note != "" {
				postHarnessComment(ctx, db, r.TaskID, note)
			}
			continue
		}

		how := "was suspended for a deploy"
		if r.State != liveSuspended {
			how = "was cut off when the daemon stopped (its current turn is redone)"
			_, _ = db.ExecContext(ctx, `UPDATE live_runs SET state='interrupted', updated_at=? WHERE task_id=?`,
				fmtTS(time.Now()), r.TaskID)
		}
		msg := fmt.Sprintf("StayPoint restarted. Run %s %s at turn %d.", r.RunID, how, r.NextTurn+1)
		if note != "" {
			msg = note + "\n" + msg
		}
		switch {
		case r.BoardPaused:
			rep.Held = append(rep.Held, r.TaskID)
			msg += " The Board had paused it, so it was not restarted: press Run Now to resume it from that turn."
		case r.Resumes >= maxAutoResumes:
			rep.Held = append(rep.Held, r.TaskID)
			msg += fmt.Sprintf(" It has already been resumed after %d restarts, so it was not restarted again: press Run Now to resume it.", r.Resumes)
		default:
			var key SlotKey
			if keyFor != nil {
				key = keyFor(r.TaskID)
			}
			if err := enqueueResume(ctx, db, r, key); err != nil {
				slog.Warn("live run recovery: queue failed", slog.String("task", r.TaskID), slog.Any("error", err))
				msg += " Queuing it to resume failed; press Run Now to resume it."
			} else {
				rep.Resumed = append(rep.Resumed, r.TaskID)
				msg += " It is queued to resume from there."
			}
		}
		postHarnessComment(ctx, db, r.TaskID, msg)
	}
	if n := len(runs); n > 0 {
		slog.Info("live run recovery", slog.Int("runs", n),
			slog.Any("stopped", rep.Stopped), slog.Any("unstopped", rep.Unstopped),
			slog.Any("resumed", rep.Resumed), slog.Any("held", rep.Held))
	}
	return rep
}

// enqueueResume puts r first in line among later arrivals: it keeps the
// earlier of its original start and any queue entry the task already has.
func enqueueResume(ctx context.Context, db *sql.DB, r LiveRun, key SlotKey) error {
	plain := 0
	if key.Plain {
		plain = 1
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO run_queue (task_id, repo_key, plain, org, reason, wait, queued_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(task_id) DO UPDATE SET
		   wait=excluded.wait, reason=excluded.reason,
		   queued_at=MIN(run_queue.queued_at, excluded.queued_at)`,
		r.TaskID, key.Dir, plain, OrgBucket(key.Org), "resume_after_restart", WaitResume, fmtTS(r.StartedAt))
	return err
}

func postHarnessComment(ctx context.Context, db *sql.DB, taskID, msg string) {
	_, _ = db.ExecContext(ctx, `INSERT INTO task_comments (task_id, author, message) VALUES (?, 'harness', ?)`, taskID, msg)
}
