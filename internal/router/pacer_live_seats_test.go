package router

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/telemetry/quota"
)

// Work-seat rows land on the work pool and lock it; personal rows stay on the
// personal pool. Neither depends on which account ~/.claude.json names.
func TestLoadPacerState_PerSeatRowsMapWithoutEmailGuessing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A work email in the default profile used to send the single fetch to the
	// work pool. It must no longer matter.
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"me@corp.example"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	seedQuota(t, &quota.Snapshot{
		Provider:  quota.ProviderClaudeWork,
		FetchedAt: now.Add(-time.Minute),
		FiveHour:  &quota.Window{Utilization: 100, ResetsAt: now.Add(90 * time.Minute)},
		Weekly:    &quota.Window{Utilization: 64, ResetsAt: now.Add(72 * time.Hour)},
	})
	seedQuota(t, &quota.Snapshot{
		Provider:  quota.ProviderClaudePersonal,
		FetchedAt: now.Add(-time.Minute),
		FiveHour:  &quota.Window{Utilization: 12, ResetsAt: now.Add(2 * time.Hour)},
		Weekly:    &quota.Window{Utilization: 30, ResetsAt: now.Add(96 * time.Hour)},
	})
	// Legacy key carries a different value; it must not be applied anywhere.
	seedQuota(t, &quota.Snapshot{
		Provider:  quota.ProviderClaudeLegacy,
		FetchedAt: now.Add(-time.Minute),
		FiveHour:  &quota.Window{Utilization: 77, ResetsAt: now.Add(2 * time.Hour)},
	})

	st, err := LoadPacerState()
	if err != nil {
		t.Fatal(err)
	}
	w := st.Pools[PoolWorkClaude]
	if !w.FiveHour.Known || w.FiveHour.UsedPct != 100 || !w.IsLocked || w.LockoutUntil.IsZero() {
		t.Errorf("work pool should be locked at 100%%: %+v locked=%v until=%v", w.FiveHour, w.IsLocked, w.LockoutUntil)
	}
	if w.Weekly.UsedPct != 64 {
		t.Errorf("work weekly = %v, want 64", w.Weekly.UsedPct)
	}
	p := st.Pools[PoolPersonalClaude]
	if !p.FiveHour.Known || p.FiveHour.UsedPct != 12 || p.Weekly.UsedPct != 30 || p.IsLocked {
		t.Errorf("personal pool wrong: %+v %+v locked=%v", p.FiveHour, p.Weekly, p.IsLocked)
	}
}

// With no work-seat rows (missing creds), the work pool stays unmeasured and
// the personal pool is unaffected.
func TestLoadPacerState_MissingWorkRowsLeaveWorkUnknown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	now := time.Now()
	seedQuota(t, &quota.Snapshot{
		Provider:  quota.ProviderClaudePersonal,
		FetchedAt: now.Add(-time.Minute),
		FiveHour:  &quota.Window{Utilization: 40, ResetsAt: now.Add(time.Hour)},
	})
	st, err := LoadPacerState()
	if err != nil {
		t.Fatal(err)
	}
	if w := st.Pools[PoolWorkClaude]; w.FiveHour.Known || w.Weekly.Known || w.IsLocked {
		t.Errorf("work pool must be unknown without rows: %+v %+v", w.FiveHour, w.Weekly)
	}
	if p := st.Pools[PoolPersonalClaude]; !p.FiveHour.Known || p.FiveHour.UsedPct != 40 {
		t.Errorf("personal pool = %+v, want 40%% known", p.FiveHour)
	}
}

// A statusline sample older than the weekly window must not mark a seat as
// measured (it used to auto-reset to a fake 0% "rolling" reading).
func TestReadSamplesTail_IgnoresSamplesOlderThanAWeek(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.ndjson")
	old := time.Now().Add(-8 * 24 * time.Hour)
	body := `{"ts":"` + old.Format(time.RFC3339) + `","account_email":"me@corp.example","five_hour_pct":49,"five_hour_resets_at":` +
		itoa(old.Add(2*time.Hour).Unix()) + `,"seven_day_pct":38}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	st := &PacerState{Pools: map[PoolID]*QuotaPool{
		PoolWorkClaude:     {ID: PoolWorkClaude},
		PoolPersonalClaude: {ID: PoolPersonalClaude},
	}}
	readSamplesTail(path, st)
	if w := st.Pools[PoolWorkClaude]; w.FiveHour.Known || w.Weekly.Known {
		t.Errorf("8-day-old sample must be ignored, got %+v %+v", w.FiveHour, w.Weekly)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// The statusline attributes a Claude session to a seat by CLAUDE_CONFIG_DIR,
// never by $HOME/.claude.json or the repo alone.
func TestClaudeSessionIsWorkSeat(t *testing.T) {
	home := "/Users/someone"
	cases := []struct {
		name      string
		configDir string
		workRepo  bool
		want      bool
	}{
		{"unset in personal repo", "", false, false},
		{"unset in work repo is still the personal seat", "", true, false},
		{"work config dir in personal repo", home + "/.claude-work", false, true},
		{"work config dir with trailing slash", home + "/.claude-work/", false, true},
		{"unmanaged config dir falls back to repo", "/elsewhere/.claude-x", true, true},
		{"unmanaged config dir personal repo", "/elsewhere/.claude-x", false, false},
	}
	for _, c := range cases {
		if got := claudeSessionIsWorkSeat(c.configDir, home, c.workRepo); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
