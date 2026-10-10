package main

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/config"
	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/VinnyVanGogh/staypoint/internal/server"
)

// quotaRetryInterval is how often runs queued on a quota-locked pool are
// retried. Pool locks clear on the pacer's own schedule, not on run release.
const quotaRetryInterval = time.Minute

// runLimitsFrom reads the parallel-run caps from config.toml (STA-773,
// STA-867): max_concurrent_runs, max_runs_per_repo, max_runs_per_org and the
// [run_limits.orgs] overrides.
func runLimitsFrom(cfg *config.Config) orchestrator.RunLimits {
	return orchestrator.RunLimits{
		Global:  cfg.MaxConcurrentRunsOrDefault(),
		PerRepo: cfg.MaxRunsPerRepoOrDefault(),
		PerOrg:  cfg.MaxRunsPerOrgOrDefault(),
		Orgs:    cfg.OrgRunLimits(),
	}
}

// wireRunQueue applies the parallel-run caps to the run limiter (STA-773),
// publishes queue changes to the Board as "run.queue" SSE events, and retries
// quota-queued runs every quotaRetryInterval until ctx ends.
func wireRunQueue(ctx context.Context, limits orchestrator.RunLimits, srv *server.Server) {
	slots := orchestrator.GlobalRunSlots
	if srv != nil {
		hub := srv.Hub()
		slots.OnChange = func(q []orchestrator.QueuedRun) {
			hub.Publish("run.queue", map[string]any{"queue": q, "drain": slots.DrainStatus()})
		}
	}
	slots.SetLimits(limits)
	l := slots.Limits()
	slog.Info("run limits", slog.Int("max_concurrent_runs", l.Global), slog.Int("max_runs_per_repo", l.PerRepo), slog.Int("max_runs_per_org", l.PerOrg), slog.Any("org_overrides", l.Orgs))
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

// taskQuotaLocked reports whether every slot of the task's routed chain
// (work_kind + repo seat, STA-772) is locked by the pacer, so a run would only
// fail. A work-repo coding run with both Claude seats locked waits here: Gemini
// is never in its chain (router.GeminiCodeForbidden, STA-856). Swappable in tests.
var taskQuotaLocked = func(dbConn *sql.DB, taskID, repoRoot string) bool {
	pacer, err := loadPacer()
	if err != nil || pacer == nil {
		return false // unknown quota state: let the run try
	}
	// A seat whose CLI answered with its limit stays out until its reset.
	router.ApplySeatLimits(pacer, time.Now())
	return resolveTaskRoute(dbConn, taskID, repoRoot, pacer, time.Now()).AllLocked()
}

// queueRun records a run refused for capacity (or quota) so it starts on its
// own when a slot, its repo, its organization, or its pool frees up, instead
// of being dropped.
func queueRun(h *orchestrator.Harness, taskID, reason, wait string) {
	key := h.SlotKeyForTask(context.Background(), taskID)
	pos := orchestrator.GlobalRunSlots.Enqueue(taskID, key, reason, wait)
	slog.Info("run queued",
		slog.String("task", taskID),
		slog.String("wait", wait),
		slog.Int("ahead", pos.Ahead))
}
