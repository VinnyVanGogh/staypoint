package router

import (
	"database/sql"
	"math"
	"os"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/config"
	"github.com/VinnyVanGogh/staypoint/internal/telemetry/quota"

	_ "modernc.org/sqlite"
)

// applyLiveQuotas overlays fresh rows from quota_windows onto the pools. Any
// failure (no DB, unreadable, stale rows) leaves the pools untouched so routing
// keeps working on local estimates.
//
// Each Claude seat has its own fetcher and pool key (claude_personal from the
// default profile, claude_work from CLAUDE_CONFIG_DIR=~/.claude-work), so rows
// map straight to their pool with no account guessing. The legacy "claude"
// key is written by the personal seat only, for older readers, and is not
// read here. Order is fixed so the result never depends on map iteration
// (STA-283).
func applyLiveQuotas(state *PacerState, now time.Time) {
	conn := openQuotaDB()
	if conn == nil {
		return
	}
	defer conn.Close()
	applyLiveQuotaStore(state, quota.Store{DB: conn}, now)
}

// liveQuotaPools is the fixed mapping from quota_windows pool key to pool.
var liveQuotaPools = []struct {
	provider string
	id       PoolID
}{
	{quota.ProviderClaudePersonal, PoolPersonalClaude},
	{quota.ProviderClaudeWork, PoolWorkClaude},
	{"gemini", PoolGeminiNative},
}

func applyLiveQuotaStore(state *PacerState, store quota.Store, now time.Time) {
	for _, e := range liveQuotaPools {
		rows, err := store.Load(e.provider)
		if err != nil || len(rows) == 0 {
			continue
		}
		if p, ok := state.Pools[e.id]; ok && p != nil {
			applyQuotaRows(p, rows, now)
		}
	}
}

func applyQuotaRows(p *QuotaPool, rows []quota.Row, now time.Time) {
	for _, r := range rows {
		if r.UpdatedAt.IsZero() || now.Sub(r.UpdatedAt) > quota.StaleAfter {
			continue
		}
		var w *QuotaWindow
		switch r.WindowType {
		case quota.WindowFiveHour:
			w = &p.FiveHour
		case quota.WindowWeekly:
			w = &p.Weekly
		default:
			continue
		}
		w.UsedPct, w.RemainingPct, w.Known = r.UsedPct, math.Max(0, 100-r.UsedPct), true
		w.ResetsAt = r.ResetsAt
		if r.UpdatedAt.After(p.LastUpdated) {
			p.LastUpdated = r.UpdatedAt
		}
	}
}

// openQuotaDB opens the StayPoint database read-only, or returns nil when it
// does not exist yet. It never creates the file as a side effect.
func openQuotaDB() *sql.DB {
	cfg, err := config.LoadConfig()
	if err != nil || cfg.DBPath == "" {
		return nil
	}
	if _, err := os.Stat(cfg.DBPath); err != nil {
		return nil
	}
	conn, err := sql.Open("sqlite", "file:"+cfg.DBPath+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return nil
	}
	conn.SetMaxOpenConns(1)
	return conn
}
