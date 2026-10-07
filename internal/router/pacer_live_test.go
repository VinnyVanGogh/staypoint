package router

import (
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/config"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/telemetry/quota"
)

func seedQuota(t *testing.T, snap *quota.Snapshot) {
	t.Helper()
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := (quota.Store{DB: store.DB()}).Save(snap); err != nil {
		t.Fatal(err)
	}
}

func TestLoadPacerState_UsesCachedLiveQuota(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Now()
	seedQuota(t, &quota.Snapshot{
		Provider:  "claude_personal",
		FetchedAt: now.Add(-time.Minute),
		FiveHour:  &quota.Window{Utilization: 15, ResetsAt: now.Add(2 * time.Hour)},
		Weekly:    &quota.Window{Utilization: 20, ResetsAt: now.Add(72 * time.Hour)},
	})
	seedQuota(t, &quota.Snapshot{Provider: "gemini", FetchedAt: now, FiveHour: &quota.Window{Utilization: 100, ResetsAt: now.Add(time.Hour)}})

	st, err := LoadPacerState()
	if err != nil {
		t.Fatal(err)
	}
	p := st.Pools[PoolPersonalClaude]
	if p.FiveHour.RemainingPct != 85 || p.Weekly.RemainingPct != 80 || !p.FiveHour.Known {
		t.Errorf("claude live quota not applied: %+v %+v", p.FiveHour, p.Weekly)
	}
	if g := st.Pools[PoolGeminiNative]; !g.IsLocked {
		t.Errorf("gemini at 100%% used should lock the pool")
	}
}

func TestLoadPacerState_StaleOrMissingCacheFailsOpen(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// No DB at all: must not error or block routing.
	st, err := LoadPacerState()
	if err != nil {
		t.Fatal(err)
	}
	if st.Pools[PoolPersonalClaude].FiveHour.Known {
		t.Error("no cache must leave window unknown")
	}

	stale := time.Now().Add(-2 * quota.StaleAfter)
	seedQuota(t, &quota.Snapshot{Provider: "claude_personal", FetchedAt: stale, FiveHour: &quota.Window{Utilization: 99, ResetsAt: time.Now().Add(time.Hour)}})
	st, err = LoadPacerState()
	if err != nil {
		t.Fatal(err)
	}
	p := st.Pools[PoolPersonalClaude]
	if p.FiveHour.Known || p.IsLocked {
		t.Errorf("stale cache must be ignored, got %+v locked=%v", p.FiveHour, p.IsLocked)
	}
}
