package fleet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/db"
)

func seatQuotaDB(t *testing.T, insert string, args ...any) *Aggregator {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // keep the pacer (LoadPacerState) empty
	store, err := db.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if insert != "" {
		if _, err := store.DB().Exec(insert, args...); err != nil {
			t.Fatal(err)
		}
	}
	return &Aggregator{DB: store.DB(), Now: time.Now}
}

func gatherQuotas(a *Aggregator) map[string]*ProviderQuotaGauge {
	ov := &FleetOverview{ProviderQuotas: map[string]*ProviderQuotaGauge{}}
	a.gatherProviderQuotas(ov, a.Now())
	return ov.ProviderQuotas
}

const seatInsert = `INSERT INTO quota_windows (pool_key, window_type, used_percent, remaining_pct, is_locked, resets_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// Both seats render from their own rows; a locked work seat shows its real
// 100%, reset time and lock instead of 0% / "rolling".
func TestGatherProviderQuotas_BothClaudeSeatsFromOwnRows(t *testing.T) {
	now := time.Now()
	reset := now.Add(90 * time.Minute).Truncate(time.Second)
	a := seatQuotaDB(t, "")
	for _, r := range [][]any{
		{"claude_work", "rolling_5h", 100.0, 0.0, 1, ts(reset), ts(now)},
		{"claude_work", "weekly_7d", 64.0, 36.0, 0, ts(now.Add(72 * time.Hour)), ts(now)},
		{"claude_personal", "rolling_5h", 12.0, 88.0, 0, ts(now.Add(2 * time.Hour)), ts(now)},
		{"claude_personal", "weekly_7d", 30.0, 70.0, 0, ts(now.Add(96 * time.Hour)), ts(now)},
		// Legacy key with a different value must not override claude_personal.
		{"claude", "rolling_5h", 77.0, 23.0, 0, ts(now.Add(2 * time.Hour)), ts(now)},
	} {
		if _, err := a.DB.Exec(seatInsert, r...); err != nil {
			t.Fatal(err)
		}
	}
	q := gatherQuotas(a)

	w := q["claude_work"]
	if !w.Measured || !w.FiveHourMeasured || w.FiveHourUsedPct != 100 || !w.IsLocked || w.ProjectionStatus != "locked_out" {
		t.Errorf("work card = %+v", w)
	}
	if w.FiveHourResetsAt == nil || !w.FiveHourResetsAt.Equal(reset) || w.LockoutUntil == nil {
		t.Errorf("work card missing reset/lockout time: resets=%v until=%v", w.FiveHourResetsAt, w.LockoutUntil)
	}
	if !w.WeeklyMeasured || w.WeeklyUsedPct != 64 {
		t.Errorf("work weekly = %v measured=%v", w.WeeklyUsedPct, w.WeeklyMeasured)
	}
	p := q["claude_personal"]
	if !p.Measured || p.FiveHourUsedPct != 12 || p.WeeklyUsedPct != 30 || p.IsLocked {
		t.Errorf("personal card = %+v", p)
	}
}

// Missing work credentials leave no claude_work rows: the work card must say
// "no data" (not 0% used) and the personal card is unaffected.
func TestGatherProviderQuotas_MissingWorkSeatIsNoData(t *testing.T) {
	now := time.Now()
	a := seatQuotaDB(t, "")
	for _, r := range [][]any{
		{"claude_personal", "rolling_5h", 40.0, 60.0, 0, ts(now.Add(time.Hour)), ts(now)},
		// Stale work row (fetch has been failing): ignored, not shown as data.
		{"claude_work", "rolling_5h", 3.0, 97.0, 0, ts(now.Add(time.Hour)), ts(now.Add(-3 * time.Hour))},
	} {
		if _, err := a.DB.Exec(seatInsert, r...); err != nil {
			t.Fatal(err)
		}
	}
	q := gatherQuotas(a)
	w := q["claude_work"]
	if w.Measured || w.FiveHourMeasured || w.WeeklyMeasured || w.ProjectionStatus != "unknown" {
		t.Errorf("work card should be unmeasured, got %+v", w)
	}
	raw, _ := json.Marshal(w)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m["measured"] != false {
		t.Errorf("API must expose measured=false, got %s", raw)
	}
	p := q["claude_personal"]
	if !p.Measured || p.FiveHourUsedPct != 40 {
		t.Errorf("personal card = %+v", p)
	}
}

// state.json: omitted fields stay unmeasured, expired lockouts are ignored,
// and an entry older than the live row does not override it.
func TestApplyRateLimitsState_RespectsMeasurementAndAge(t *testing.T) {
	now := time.Now()
	a := seatQuotaDB(t, "")
	if _, err := a.DB.Exec(seatInsert, "claude_work", "rolling_5h", 55.0, 45.0, 0, ts(now.Add(time.Hour)), ts(now)); err != nil {
		t.Fatal(err)
	}
	state := map[string]any{
		"quotas": map[string]any{
			"Claude (Work)":     map[string]any{"five_hour_used": 100, "last_updated": now.Add(-time.Hour).Format(time.RFC3339)},
			"Claude (Personal)": map[string]any{"five_hour_used": 14, "last_updated": now.Format(time.RFC3339)},
		},
		"lockouts": map[string]any{
			"Claude (Personal)": map[string]any{"locked": true, "resets_at": now.Add(-time.Minute).Unix()},
		},
	}
	path := filepath.Join(t.TempDir(), "state.json")
	b, _ := json.Marshal(state)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	a.RateLimitsPath = path
	q := gatherQuotas(a)

	if w := q["claude_work"]; w.FiveHourUsedPct != 55 || w.IsLocked {
		t.Errorf("older state.json entry overrode live work row: %+v", w)
	}
	p := q["claude_personal"]
	if !p.FiveHourMeasured || p.FiveHourUsedPct != 14 || p.FiveHourRemainingPct != 86 {
		t.Errorf("personal 5h from state.json = %+v", p)
	}
	if p.WeeklyMeasured {
		t.Error("weekly omitted from state.json must stay unmeasured")
	}
	if p.IsLocked {
		t.Error("expired lockout must not lock the card")
	}
}
