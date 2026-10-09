package archive

import (
	"context"
	"log/slog"
	"os"
	"time"
)

// Disabled is the kill switch: STAYPOINT_ARCHIVE=off stops the daemon loop.
func Disabled() bool { return os.Getenv("STAYPOINT_ARCHIVE") == "off" }

// nightlyHour is the local hour the daily pass runs.
const nightlyHour = 3

// NextNightly is the next nightlyHour:00 local time strictly after now.
func NextNightly(now time.Time) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), nightlyHour, 0, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// RunLoop archives shortly after daemon start, then every night, until ctx ends.
func RunLoop(ctx context.Context, opts Options, startDelay time.Duration) {
	if Disabled() {
		slog.Info("Transcript archive disabled (STAYPOINT_ARCHIVE=off)")
		return
	}
	wait := startDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		runOnce(ctx, opts)
		wait = time.Until(NextNightly(time.Now()))
	}
}

func runOnce(ctx context.Context, opts Options) {
	a, err := Open(opts)
	if err != nil {
		slog.Warn("Transcript archive: open failed", slog.Any("error", err))
		return
	}
	defer a.Close()
	rep, err := a.Run(ctx)
	if err != nil {
		slog.Warn("Transcript archive: run failed", slog.Any("error", err))
		return
	}
	slog.Info("Transcript archive pass",
		slog.Int("scanned", rep.Scanned), slog.Int("archived", rep.Archived),
		slog.Int("unchanged", rep.Unchanged), slog.Int("failed", rep.Failed),
		slog.Int64("src_bytes", rep.SrcBytes), slog.Int64("stored_bytes", rep.StoredBytes))
	for _, e := range firstN(rep.Errors, 5) {
		slog.Warn("Transcript archive error", slog.String("detail", e))
	}
}
