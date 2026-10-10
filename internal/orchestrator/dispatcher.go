package orchestrator

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"time"
)

// Dispatcher wakes agents without polling.
type Dispatcher struct {
	mu       sync.Mutex
	seenKeys map[string]time.Time
	OnWake   func(taskID string, reason string)
	wg       sync.WaitGroup // tracks in-flight OnWake goroutines
	// closed is set by Close: later wakes run OnWake inline in the caller
	// (the daemon is draining, so OnWake only queues the run) instead of
	// in a goroutine nobody waits for.
	closed bool
}

var GlobalDispatcher *Dispatcher

func init() {
	GlobalDispatcher = NewDispatcher()
}

func NewDispatcher() *Dispatcher {
	return &Dispatcher{
		seenKeys: make(map[string]time.Time),
	}
}

// Wake triggers an agent to wake up for a specific task.
// idempotencyKey ensures we don't wake multiple times for the same event.
func (d *Dispatcher) Wake(taskID, reason, idempotencyKey string) {
	if idempotencyKey != "" {
		d.mu.Lock()
		// Clean up old keys periodically (lazy)
		if len(d.seenKeys) > 1000 {
			now := time.Now()
			for k, v := range d.seenKeys {
				if now.Sub(v) > 24*time.Hour {
					delete(d.seenKeys, k)
				}
			}
		}

		if _, ok := d.seenKeys[idempotencyKey]; ok {
			d.mu.Unlock()
			slog.Debug("wake dispatcher: ignored duplicate event", slog.String("key", idempotencyKey), slog.String("task", taskID))
			return
		}
		d.seenKeys[idempotencyKey] = time.Now()
		d.mu.Unlock()
	}

	slog.Info("wake dispatcher: waking agent", slog.String("task", taskID), slog.String("reason", reason))
	if d.OnWake == nil {
		return
	}
	d.mu.Lock()
	closed := d.closed
	if !closed {
		d.wg.Add(1) // under mu, so never after Drain's Wait began
	}
	d.mu.Unlock()
	if closed {
		d.OnWake(taskID, reason)
		return
	}
	go func() {
		defer d.wg.Done()
		d.OnWake(taskID, reason)
	}()
}

// Drain blocks until all in-flight OnWake goroutines have returned.
func (d *Dispatcher) Drain() {
	d.wg.Wait()
}

// Close is Drain for daemon shutdown: from now on a wake runs OnWake inline
// in its caller (an HTTP handler httpServer.Shutdown waits for) rather than
// in a goroutine started after the wait, which would write SQLite while the
// database closes.
func (d *Dispatcher) Close() {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	d.Drain()
}

// RecoveryScan runs once at daemon start to reset stale claims.
// A claim is stale if execution_stage = 'in_progress' but the daemon just started,
// since it's a single binary and all previous sessions are dead.
func RecoveryScan(ctx context.Context, dbConn *sql.DB) error {
	slog.Info("running startup recovery scan for stale task claims")

	// 1. Mark all active agent sessions as closed since the daemon restarted.
	_, err := dbConn.ExecContext(ctx, "UPDATE agent_sessions SET status = 'closed' WHERE status = 'active'")
	if err != nil {
		return err
	}

	// 2. Reset tasks that were 'in_progress' or 'paused' back to 'todo' and clear checkout.
	res, err := dbConn.ExecContext(ctx, "UPDATE tasks SET execution_stage = 'todo', checkout_run_id = NULL, checkout_agent_id = NULL, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE execution_stage IN ('in_progress', 'paused')")
	if err != nil {
		return err
	}

	affected, _ := res.RowsAffected()
	if affected > 0 {
		slog.Info("recovered stale claims", slog.Int64("count", affected))
	}

	// Clear stale run_control flags.
	_, _ = dbConn.ExecContext(ctx, "UPDATE run_control SET pause_after_step=0, stop_requested=0")

	// 3. Reset capped tasks to todo so they can be retried.
	resCapped, err := dbConn.ExecContext(ctx,
		"UPDATE tasks SET execution_stage='todo', checkout_run_id=NULL, checkout_agent_id=NULL, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE execution_stage='capped'")
	if err != nil {
		return err
	}
	affectedCapped, _ := resCapped.RowsAffected()
	if affectedCapped > 0 {
		slog.Info("recovered capped tasks", slog.Int64("count", affectedCapped))
	}
	return nil
}
