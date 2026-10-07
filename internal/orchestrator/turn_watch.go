package orchestrator

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// DefaultStallTimeout is how long a turn may go without any agent output or
// tool activity before the harness stops it, when config.toml does not set
// stall_timeout. There is no default limit on how long an active turn runs.
const DefaultStallTimeout = 20 * time.Minute

// defaultWatchPoll is how often the turn watcher checks the clock.
const defaultWatchPoll = 15 * time.Second

// Turn stop reasons recorded by turnWatch.
const (
	turnStopStall   = "stall"
	turnStopTimeout = "turn_timeout"
)

// stallTimeout resolves cfg.StallTimeout: 0 means the default, negative off.
func (cfg RunConfig) stallTimeout() time.Duration {
	switch {
	case cfg.StallTimeout < 0:
		return 0
	case cfg.StallTimeout == 0:
		return DefaultStallTimeout
	default:
		return cfg.StallTimeout
	}
}

func (cfg RunConfig) now() time.Time {
	if cfg.Clock != nil {
		return cfg.Clock()
	}
	return time.Now()
}

// fmtWatchDuration renders a timeout for timeline rows: "20m", "1h30m", "45s".
func fmtWatchDuration(d time.Duration) string {
	d = d.Round(time.Second)
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// turnWatch stops one adapter turn when it goes quiet for longer than the
// stall timeout, or (if set) runs past the turn timeout. Any byte the agent
// writes to stdout or stderr counts as activity; so does the Board holding
// the run (paused, or a pending gate approval), since the agent is waiting
// on a person then, not stuck.
type turnWatch struct {
	now     func() time.Time
	stall   time.Duration
	limit   time.Duration
	waiting func() bool

	mu      sync.Mutex
	start   time.Time
	last    time.Time
	reason  string
	stopped chan struct{}
	done    chan struct{}
}

// startTurnWatch starts watching a turn; cancel is called when it trips.
// Returns nil when neither timeout is set.
func startTurnWatch(cfg RunConfig, waiting func() bool, cancel context.CancelFunc) *turnWatch {
	stall := cfg.stallTimeout()
	limit := cfg.TurnTimeout
	if limit < 0 {
		limit = 0
	}
	if stall == 0 && limit == 0 {
		return nil
	}
	poll := cfg.watchPoll
	if poll <= 0 {
		poll = defaultWatchPoll
	}
	start := cfg.now()
	w := &turnWatch{
		now:     cfg.now,
		stall:   stall,
		limit:   limit,
		waiting: waiting,
		start:   start,
		last:    start,
		stopped: make(chan struct{}),
		done:    make(chan struct{}),
	}
	go func() {
		defer close(w.done)
		t := time.NewTicker(poll)
		defer t.Stop()
		for {
			select {
			case <-w.stopped:
				return
			case <-t.C:
				if w.check() {
					cancel()
					return
				}
			}
		}
	}()
	return w
}

// check reports whether the turn should be stopped, recording why.
func (w *turnWatch) check() bool {
	if w.waiting != nil && w.waiting() {
		w.Touch()
		return false
	}
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case w.limit > 0 && now.Sub(w.start) >= w.limit:
		w.reason = turnStopTimeout
	case w.stall > 0 && now.Sub(w.last) >= w.stall:
		w.reason = turnStopStall
	default:
		return false
	}
	return true
}

// Touch records agent activity.
func (w *turnWatch) Touch() {
	if w == nil {
		return
	}
	now := w.now()
	w.mu.Lock()
	if now.After(w.last) {
		w.last = now
	}
	w.mu.Unlock()
}

// Stop ends the watch and returns why it stopped the turn ("" if it did not).
func (w *turnWatch) Stop() string {
	if w == nil {
		return ""
	}
	select {
	case <-w.stopped:
	default:
		close(w.stopped)
	}
	<-w.done
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reason
}

// LastActivity returns the time of the last recorded activity.
func (w *turnWatch) LastActivity() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last
}

// stopRow is the timeline title and body for a turn the watch stopped.
func (w *turnWatch) stopRow(reason string) (title, body string) {
	switch reason {
	case turnStopTimeout:
		d := fmtWatchDuration(w.limit)
		return "Stopped: turn ran longer than " + d,
			fmt.Sprintf("The turn hit the turn_timeout of %s set in config.toml and was stopped. Raise or clear turn_timeout to let long turns finish.", d)
	default:
		d := fmtWatchDuration(w.stall)
		return "Stopped: no activity for " + d,
			fmt.Sprintf("The agent printed nothing and ran no tools for %s, so the turn was stopped as stuck. Change stall_timeout in config.toml to wait longer (0 turns this check off).", d)
	}
}

// activityWriter forwards writes and records each one as turn activity.
type activityWriter struct {
	dst io.Writer
	w   *turnWatch
}

func (a *activityWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		a.w.Touch()
	}
	return a.dst.Write(p)
}

// boardWaiting reports whether the Board is holding this task's run: paused
// at a step boundary, or any Red-tier gate request still pending (requests
// carry the provider session id, not the task, so any pending one counts).
func boardWaiting(db *sql.DB, rc *RunControl, taskID string) func() bool {
	return func() bool {
		if rc != nil && rc.IsPaused(taskID) {
			return true
		}
		if db == nil {
			return false
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM security_gate_requests WHERE status='pending'`).Scan(&n); err != nil {
			return false
		}
		return n > 0
	}
}

// ActiveTurn is the live turn of a running task, shown on the task page.
type ActiveTurn struct {
	Turn            int       `json:"turn"`
	StartedAt       time.Time `json:"started_at"`
	StallTimeoutSec float64   `json:"stall_timeout_sec,omitempty"`
	TurnTimeoutSec  float64   `json:"turn_timeout_sec,omitempty"`
}

// ActiveTurns tracks the turn in flight per task.
type ActiveTurns struct {
	mu    sync.Mutex
	turns map[string]ActiveTurn
}

// GlobalActiveTurns is read by GET /api/tasks/{id}.
var GlobalActiveTurns = &ActiveTurns{turns: map[string]ActiveTurn{}}

// Get returns the task's turn in flight, or nil.
func (a *ActiveTurns) Get(taskID string) *ActiveTurn {
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.turns[taskID]
	if !ok {
		return nil
	}
	return &t
}

func (a *ActiveTurns) set(taskID string, t ActiveTurn) {
	a.mu.Lock()
	a.turns[taskID] = t
	a.mu.Unlock()
}

func (a *ActiveTurns) clear(taskID string) {
	a.mu.Lock()
	delete(a.turns, taskID)
	a.mu.Unlock()
}

// beginTurn registers the turn and publishes run.turn so the open task page
// shows how long it has been running.
func beginTurn(cfg RunConfig, taskID string, turn int, sr *StepRecorder) ActiveTurn {
	at := ActiveTurn{
		Turn:            turn,
		StartedAt:       cfg.now().UTC(),
		StallTimeoutSec: cfg.stallTimeout().Seconds(),
	}
	if cfg.TurnTimeout > 0 {
		at.TurnTimeoutSec = cfg.TurnTimeout.Seconds()
	}
	GlobalActiveTurns.set(taskID, at)
	if sr != nil {
		sr.publish("run.turn", map[string]any{"task_id": taskID, "run_id": sr.runID, "turn": at})
	}
	return at
}

func endTurn(taskID string, sr *StepRecorder) {
	GlobalActiveTurns.clear(taskID)
	if sr != nil {
		sr.publish("run.turn", map[string]any{"task_id": taskID, "run_id": sr.runID, "turn": nil})
	}
}

// stopTurnForWatch ends the run after the turn watch stopped a turn: it
// records the reason as a timeline row, an activity entry and the run's
// diagnostic. A stall is a failed run ("error"); a turn_timeout is a cap.
// Returns true: the caller breaks out of the turn loop.
func (h *Harness) stopTurnForWatch(result *RunResult, w *turnWatch, reason, taskID string, stdout io.Writer, sr *StepRecorder) bool {
	if stw, ok := stdout.(*stepTeeWriter); ok {
		_ = stw.Close()
	}
	title, body := w.stopRow(reason)
	result.Turns++
	result.DiagnosticMsg = title + ". " + body
	if reason == turnStopTimeout {
		result.Disposition = "capped"
	} else {
		result.Disposition = "error"
	}
	if sr != nil {
		sr.EmitMessage(title, body, "error")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = h.DB.ExecContext(ctx,
		`INSERT INTO activity_log (task_id, event_type, details) VALUES (?, 'turn_stopped', ?)`,
		taskID, fmt.Sprintf("reason=%s %s", reason, title),
	)
	return true
}
