package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
)

// Zero-kill deploys (task-db71fba9): startup recovery, the persisted run
// queue, the shutdown drain, and the drain status file the statusline reads.

// shutdownPoll is how often the shutdown drain checks for live runs.
var shutdownPoll = 250 * time.Millisecond

// recoverLiveRuns stops the agents the previous daemon left running and
// queues its unfinished runs to resume (orchestrator.RecoverLiveRuns).
func recoverLiveRuns(ctx context.Context, dbConn *sql.DB, repoRoot string) orchestrator.RecoveryReport {
	h := orchestrator.NewHarness(dbConn, repoRoot)
	return orchestrator.RecoverLiveRuns(ctx, dbConn, func(taskID string) orchestrator.SlotKey {
		return h.SlotKeyForTask(ctx, taskID)
	})
}

// restoreRunQueue loads the persisted run queue into slots and starts what
// fits.
func restoreRunQueue(ctx context.Context, dbConn *sql.DB, slots *orchestrator.RunSlots) {
	q, err := orchestrator.LoadQueue(ctx, dbConn)
	if err != nil {
		slog.Error("run queue: could not load the persisted queue", slog.Any("error", err))
		return
	}
	if len(q) == 0 {
		return
	}
	ids := make([]string, len(q))
	for i, r := range q {
		ids[i] = r.TaskID
	}
	slog.Info("run queue: restored from the previous daemon", slog.Int("runs", len(q)), slog.Any("tasks", ids))
	slots.Restore(q)
	slots.Pump()
}

// drainForShutdown stops new claims, asks live runs to suspend at their next
// turn boundary, and waits until none are left and every wake goroutine
// has returned.
func drainForShutdown(slots *orchestrator.RunSlots, d *orchestrator.Dispatcher, poll time.Duration) {
	st := slots.StartDrain(orchestrator.DrainBoundary)
	if st.Live > 0 {
		slog.Warn("shutdown: waiting for live runs to reach a turn boundary",
			slog.Int("live", st.Live), slog.Any("tasks", st.LiveTasks))
	}
	start := time.Now()
	lastLog := start
	for slots.Active() > 0 {
		if time.Since(lastLog) >= 30*time.Second {
			st = slots.DrainStatus()
			slog.Warn("shutdown: still waiting for live runs",
				slog.Int("live", st.Live), slog.Any("tasks", st.LiveTasks),
				slog.Duration("waited", time.Since(start).Round(time.Second)))
			lastLog = time.Now()
		}
		time.Sleep(poll)
	}
	d.Drain()
	slog.Info("shutdown: no live runs", slog.Duration("waited", time.Since(start).Round(time.Millisecond)))
}

// drainStatusFileName is read by `staypoint statusline` (router.drainLine).
const drainStatusFileName = "drain.json"

type drainStatusFile struct {
	orchestrator.DrainStatus
	Label     string    `json:"label"`
	PID       int       `json:"pid"`
	UpdatedAt time.Time `json:"updated_at"`
}

func clearDrainStatusFile(dataDir string) {
	if dataDir == "" {
		return
	}
	_ = os.Remove(filepath.Join(dataDir, drainStatusFileName))
}

// writeDrainStatusFile keeps <data_dir>/drain.json current while the daemon
// drains, and removes it otherwise. The statusline ignores a file older than
// a few seconds, so one left by a killed daemon is never shown.
func writeDrainStatusFile(ctx context.Context, dataDir string, slots *orchestrator.RunSlots) {
	if dataDir == "" {
		return
	}
	path := filepath.Join(dataDir, drainStatusFileName)
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	wrote := false
	for {
		st := slots.DrainStatus()
		switch {
		case st.Draining:
			b, _ := json.Marshal(drainStatusFile{DrainStatus: st, Label: st.Label(), PID: os.Getpid(), UpdatedAt: time.Now().UTC()})
			tmp := path + ".tmp"
			if err := os.WriteFile(tmp, b, 0o644); err == nil {
				_ = os.Rename(tmp, path)
				wrote = true
			}
		case wrote:
			_ = os.Remove(path)
			wrote = false
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
