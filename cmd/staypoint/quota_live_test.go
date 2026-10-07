package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/telemetry/quota"
)

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
