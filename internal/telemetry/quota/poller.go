package quota

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"
)

const (
	// MinInterval is the floor between live fetches of one provider. These are
	// undocumented endpoints; never hit them more often than this.
	MinInterval = 15 * time.Minute
	// RejectedBackoff is the wait after a 401/403 or missing credentials, when
	// retrying sooner cannot help.
	RejectedBackoff = time.Hour
)

// Poller runs each fetcher at most once per MinInterval, persisting the result
// and the throttle in SQLite so the daemon and CLI share one budget. Failures
// never propagate: the cached rows age out (StaleAfter) and consumers fall back
// to local estimation.
type Poller struct {
	Store    Store
	Fetchers []Fetcher
	Now      func() time.Time
}

// legacyProvider is implemented by fetchers whose snapshot is also saved under
// an older pool key (the personal Claude seat also writes "claude").
type legacyProvider interface {
	LegacyProvider() string
}

// NewPoller returns a Poller over the production fetchers: one per Claude
// seat on this machine, then Codex, Cursor and Gemini.
func NewPoller(s Store) *Poller {
	home, _ := os.UserHomeDir()
	fetchers := NewClaudeSeatFetchers(home)
	fetchers = append(fetchers, NewCodexFetcher(), NewCursorFetcher(), NewGeminiFetcher())
	return &Poller{Store: s, Now: time.Now, Fetchers: fetchers}
}

// PollOnce fetches every provider whose throttle has elapsed. It returns the
// providers actually fetched.
func (p *Poller) PollOnce(ctx context.Context) []string {
	var fetched []string
	for _, f := range p.Fetchers {
		if ctx.Err() != nil {
			return fetched
		}
		name := f.Provider()
		now := p.Now()
		st, ok, err := p.Store.State(name)
		if err != nil {
			slog.Debug("quota state read failed", slog.String("provider", name), slog.Any("error", err))
			continue
		}
		if ok && now.Before(st.NextAttempt) {
			continue
		}
		fetched = append(fetched, name)

		snap, ferr := f.Fetch(ctx)
		st.LastAttempt = now
		switch {
		case ferr == nil:
			if snap.FetchedAt.IsZero() {
				snap.FetchedAt = now
			}
			if serr := p.Store.Save(snap); serr != nil {
				slog.Debug("quota save failed", slog.String("provider", name), slog.Any("error", serr))
				st.Status = "error: save"
				st.NextAttempt = now.Add(MinInterval)
				break
			}
			st.LastSuccess, st.Status, st.NextAttempt = now, "ok", now.Add(MinInterval)
			if lp, ok := f.(legacyProvider); ok {
				if legacy := lp.LegacyProvider(); legacy != "" && legacy != name {
					alias := *snap
					alias.Provider = legacy
					if serr := p.Store.Save(&alias); serr != nil {
						slog.Debug("quota legacy save failed", slog.String("provider", legacy), slog.Any("error", serr))
					}
				}
			}
		case errors.Is(ferr, ErrUnauthorized), errors.Is(ferr, ErrNoCredentials):
			st.Status, st.NextAttempt = "rejected", now.Add(RejectedBackoff)
		default:
			st.Status, st.NextAttempt = "error", now.Add(MinInterval)
		}
		if ferr != nil {
			slog.Debug("quota fetch failed, failing open", slog.String("provider", name), slog.Any("error", ferr))
		}
		if serr := p.Store.SetState(name, st); serr != nil {
			slog.Debug("quota state write failed", slog.String("provider", name), slog.Any("error", serr))
		}
	}
	return fetched
}
