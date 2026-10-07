package main

import (
	"fmt"
	"io"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/VinnyVanGogh/staypoint/internal/telemetry/quota"
)

var liveProviders = []struct{ key, name string }{
	{"claude_personal", "Claude (Personal)"},
	{"claude_work", "Claude (Work)"},
	{"codex", "Codex"},
	{"cursor", "Cursor"},
	{"gemini", "Gemini"},
}

// renderLiveQuota prints the cached 5h / weekly (and Cursor monthly) reading for
// each provider. Missing or stale data is labelled, never shown as 0%.
func renderLiveQuota(w io.Writer, store quota.Store, now time.Time) {
	for _, p := range liveProviders {
		rows, err := store.Load(p.key)
		fmt.Fprintf(w, "\n\033[1mProvider:\033[0m %s\n", p.name)
		if err != nil || len(rows) == 0 {
			fmt.Fprintln(w, "  no live data (provider not signed in, or endpoint unavailable; routing uses local estimates)")
			continue
		}
		byType := map[string]quota.Row{}
		for _, r := range rows {
			byType[r.WindowType] = r
		}
		for _, wt := range []struct{ typ, label string }{
			{quota.WindowFiveHour, "5-Hour"}, {quota.WindowWeekly, "Weekly"}, {quota.WindowMonthly, "Monthly"},
		} {
			r, ok := byType[wt.typ]
			if !ok {
				if wt.typ != quota.WindowMonthly {
					fmt.Fprintf(w, "  [%-7s] n/a\n", wt.label)
				}
				continue
			}
			line := fmt.Sprintf("  [%-7s] Used: %5.1f%% | Remaining: %5.1f%%", wt.label, r.UsedPct, 100-r.UsedPct)
			if !r.ResetsAt.IsZero() && r.ResetsAt.After(now) {
				line += fmt.Sprintf(" | Resets in %s", router.FormatDuration(r.ResetsAt.Sub(now)))
			}
			if now.Sub(r.UpdatedAt) > quota.StaleAfter {
				line += " \033[0;33m(stale, ignored for routing)\033[0m"
			}
			fmt.Fprintln(w, line)
		}
	}
}
