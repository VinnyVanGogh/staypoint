package router

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/telemetry/quota"
)

// The notifier and quota-poller write a generic "Claude" key alongside the
// seat-specific ones. It holds whichever seat's statusline ran last, so it must
// never override seat-specific data; and since Go map iteration is random, the
// old code let it win on some loads and lose on others (STA-283).
func TestApplyStateJSON_GenericClaudeKeyNeverOverridesSeats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	body := `{"quotas":{
		"Claude (Work)":     {"five_hour_used":80,"five_hour_remaining":20,"weekly_used":60,"weekly_remaining":40},
		"Claude (Personal)": {"five_hour_used":10,"five_hour_remaining":90,"weekly_used":43,"weekly_remaining":57},
		"Claude":            {"five_hour_used":80,"five_hour_remaining":20,"weekly_used":60,"weekly_remaining":40}
	}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		st := &PacerState{Pools: map[PoolID]*QuotaPool{
			PoolWorkClaude:     {ID: PoolWorkClaude},
			PoolPersonalClaude: {ID: PoolPersonalClaude},
		}}
		applyStateJSON(path, st)
		if got := st.Pools[PoolPersonalClaude].FiveHour.UsedPct; got != 10 {
			t.Fatalf("iteration %d: personal 5h = %v, want 10 (generic key leaked in)", i, got)
		}
		if got := st.Pools[PoolWorkClaude].FiveHour.UsedPct; got != 80 {
			t.Fatalf("iteration %d: work 5h = %v, want 80", i, got)
		}
	}
}

// With no seat-specific personal entry, the generic key still backfills the
// personal pool so single-seat installs keep working.
func TestApplyStateJSON_GenericClaudeKeyBackfillsPersonal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	body := `{"quotas":{"Claude":{"five_hour_used":25,"five_hour_remaining":75,"weekly_used":5,"weekly_remaining":95}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	st := &PacerState{Pools: map[PoolID]*QuotaPool{
		PoolWorkClaude:     {ID: PoolWorkClaude},
		PoolPersonalClaude: {ID: PoolPersonalClaude},
	}}
	applyStateJSON(path, st)
	if p := st.Pools[PoolPersonalClaude]; !p.FiveHour.Known || p.FiveHour.UsedPct != 25 {
		t.Errorf("generic key should backfill personal, got %+v", p.FiveHour)
	}
	if st.Pools[PoolWorkClaude].FiveHour.Known {
		t.Error("generic key must not populate the work pool")
	}
}

// The live claude_personal rows come from the personal seat's own Keychain
// item, which does not change when a statusline sample from the other seat
// lands. Interleaved samples used
// to move those rows between pools on alternate loads (STA-283).
func TestLoadPacerState_LiveClaudeRowsStableAcrossInterleavedSamples(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	now := time.Now()
	seedQuota(t, &quota.Snapshot{
		Provider:  "claude_personal",
		FetchedAt: now.Add(-time.Minute),
		FiveHour:  &quota.Window{Utilization: 61, ResetsAt: now.Add(2 * time.Hour)},
		Weekly:    &quota.Window{Utilization: 41, ResetsAt: now.Add(72 * time.Hour)},
	})
	samples := filepath.Join(home, ".config", "token-telemetry", "statusline-samples.ndjson")
	if err := os.MkdirAll(filepath.Dir(samples), 0o755); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-48 * time.Hour).Format(time.RFC3339)
	write := func(lastEmail string) {
		other := "me@gmail.com"
		if lastEmail == other {
			other = "me@corp.example"
		}
		line := func(email string) string {
			return `{"ts":"` + old + `","account_email":"` + email + `","five_hour_pct":1,"seven_day_pct":1}` + "\n"
		}
		if err := os.WriteFile(samples, []byte(line(other)+line(lastEmail)), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var first [2]float64
	for i, email := range []string{"me@corp.example", "me@gmail.com", "me@corp.example", "me@gmail.com"} {
		write(email)
		st, err := LoadPacerState()
		if err != nil {
			t.Fatal(err)
		}
		got := [2]float64{st.Pools[PoolWorkClaude].FiveHour.UsedPct, st.Pools[PoolPersonalClaude].FiveHour.UsedPct}
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("load %d (latest sample %s): work/personal 5h = %v, first load was %v", i, email, got, first)
		}
	}
}
