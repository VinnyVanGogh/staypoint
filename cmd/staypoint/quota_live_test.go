package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/VinnyVanGogh/staypoint/internal/telemetry/quota"
)

// Pacer reset times are stored in UTC; the report must show them in the
// viewer's zone (07:30Z is 12:30 AM PDT), never the raw UTC clock.
func TestRenderPacerPoolResetsInLocalTime(t *testing.T) {
	pdt := time.FixedZone("PDT", -7*3600)
	now := time.Date(2026, 10, 9, 22, 0, 0, 0, pdt)
	pool := &router.QuotaPool{Name: "Claude (Work)"}
	pool.FiveHour.ResetsAt = time.Date(2026, 10, 10, 7, 30, 0, 0, time.UTC)
	pool.Weekly.ResetsAt = time.Date(2026, 10, 13, 16, 0, 0, 0, time.UTC)

	var b bytes.Buffer
	renderPacerPool(&b, pool, now)
	out := b.String()
	for _, want := range []string{"(at tomorrow 12:30 AM)", "(at Tue 9:00 AM)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"07:30", "16:00"} {
		if strings.Contains(out, bad) {
			t.Errorf("output shows UTC clock %q:\n%s", bad, out)
		}
	}
}

func TestRenderLiveQuota(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	st := quota.Store{DB: d.DB()}
	now := time.Now()
	_ = st.Save(&quota.Snapshot{Provider: "claude_personal", FetchedAt: now, FiveHour: &quota.Window{Utilization: 12.5, ResetsAt: now.Add(time.Hour)}, Weekly: &quota.Window{Utilization: 40}})
	_ = st.Save(&quota.Snapshot{Provider: "codex", FetchedAt: now.Add(-3 * quota.StaleAfter), Weekly: &quota.Window{Utilization: 5}})

	_ = st.Save(&quota.Snapshot{Provider: "claude_work", FetchedAt: now, FiveHour: &quota.Window{Utilization: 100, ResetsAt: now.Add(time.Hour)}})

	var b bytes.Buffer
	renderLiveQuota(&b, st, now)
	out := b.String()
	for _, want := range []string{"Claude (Personal)", "Claude (Work)", "100.0%", "Codex", "Cursor", "Gemini", "12.5%", "stale", "no live data"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
