package main

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/adapter"
	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/VinnyVanGogh/staypoint/internal/server"
)

// quotaRetryInterval is how often runs queued on a quota-locked pool are
// retried. Pool locks clear on the pacer's own schedule, not on run release.
const quotaRetryInterval = time.Minute

// wireRunQueue applies max_concurrent_runs to the run limiter (STA-773),
// publishes queue changes to the Board as "run.queue" SSE events, and retries
// quota-queued runs every quotaRetryInterval until ctx ends.
func wireRunQueue(ctx context.Context, maxRuns int, srv *server.Server) {
	slots := orchestrator.GlobalRunSlots
	if srv != nil {
		hub := srv.Hub()
		slots.OnChange = func(q []orchestrator.QueuedRun) {
			hub.Publish("run.queue", map[string]any{"queue": q})
		}
	}
	slots.SetMax(maxRuns)
	go func() {
		t := time.NewTicker(quotaRetryInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				slots.Pump()
			}
		}
	}()
}

// taskQuotaLocked reports whether every provider in the task's quota pool
// chain is locked by the pacer, so a run would only fail. Swappable in tests.
var taskQuotaLocked = func(dbConn *sql.DB, taskID string) bool {
	var repoPath string
	_ = dbConn.QueryRowContext(context.Background(),
		"SELECT COALESCE(repo_path,'') FROM tasks WHERE id=?", taskID).Scan(&repoPath)
	isWork, _, _ := router.IsWorkRepo(repoPath)
	pacer, err := router.LoadPacerState()
	if err != nil {
		return false // unknown quota state: let the run try
	}
	return adapter.ResolveProviderChain(isWork, "", pacer).AllLocked
}

// queueRun records a run refused for capacity (or quota) so it starts on its
// own when a slot, its repo, or its pool frees up, instead of being dropped.
func queueRun(h *orchestrator.Harness, taskID, reason, wait string) {
	repo := h.RepoKeyForTask(context.Background(), taskID)
	pos := orchestrator.GlobalRunSlots.Enqueue(taskID, repo, reason, wait)
	slog.Info("run queued",
		slog.String("task", taskID),
		slog.String("wait", wait),
		slog.Int("ahead", pos.Ahead))
}
