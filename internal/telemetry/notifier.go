package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/alerts"
	"github.com/VinnyVanGogh/staypoint/internal/osascript"
	"github.com/VinnyVanGogh/staypoint/internal/router"
)

type RateLimitNotifier struct {
	last3PLocked      bool
	lastGeminiLocked  bool
	lastPersLocked    bool
	warned3PPreLock   bool
	warnedPersPreLock bool
	initialized       bool
}

func NewNotifier() *RateLimitNotifier {
	return &RateLimitNotifier{}
}

func (n *RateLimitNotifier) Start(ctx context.Context) {
	ticker := time.NewTicker(45 * time.Second)
	defer ticker.Stop()

	// Initial check
	PollQuotas(ctx)
	n.check()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			PollQuotas(ctx)
			n.check()
		}
	}
}

func (n *RateLimitNotifier) check() {
	state, err := router.LoadPacerState()
	if err != nil {
		return
	}

	pool3P := state.Pools[router.Pool3PClaude]
	poolGem := state.Pools[router.PoolGeminiNative]
	poolPers := state.Pools[router.PoolPersonalClaude]

	is3PLocked := pool3P != nil && pool3P.IsLocked
	isGemLocked := poolGem != nil && poolGem.IsLocked
	isPersLocked := poolPers != nil && poolPers.IsLocked

	if !n.initialized {
		n.last3PLocked = is3PLocked
		n.lastGeminiLocked = isGemLocked
		n.lastPersLocked = isPersLocked
		if pool3P != nil && pool3P.FiveHour.UsedPct >= 85.0 {
			n.warned3PPreLock = true
		}
		if poolPers != nil && poolPers.FiveHour.UsedPct >= 85.0 {
			n.warnedPersPreLock = true
		}
		n.initialized = true
		return
	}

	// Personal Claude Pre-lockout warning (>= 85% used, ~15% left)
	if poolPers != nil && !isPersLocked && poolPers.FiveHour.UsedPct >= 85.0 && !n.warnedPersPreLock {
		resetStr := "soon"
		if !poolPers.LockoutUntil.IsZero() {
			resetStr = router.FormatReset(poolPers.LockoutUntil, time.Now())
		}
		SendQuotaAlert(router.PoolPersonalClaude, QuotaWarning,
			"[Staypoint] Claude 5h Limit Warning (15% left)",
			fmt.Sprintf("Claude Code 5-hour quota at %.0f%% (resets %s). Handoff to Gemini staged in clipboard. Switch via /model gemini-3.8-flash-high or open agy.", poolPers.FiveHour.UsedPct, resetStr),
		)
		n.warnedPersPreLock = true
	} else if poolPers != nil && poolPers.FiveHour.UsedPct < 80.0 {
		n.warnedPersPreLock = false
	}

	// 3P Pre-lockout warning (>= 85% used, ~15% left)
	if pool3P != nil && !is3PLocked && pool3P.FiveHour.UsedPct >= 85.0 && !n.warned3PPreLock {
		resetStr := "soon"
		if !pool3P.LockoutUntil.IsZero() {
			resetStr = router.FormatReset(pool3P.LockoutUntil, time.Now())
		}
		SendQuotaAlert(router.Pool3PClaude, QuotaWarning,
			"[Staypoint] 5h Quota Warning (15% left)",
			fmt.Sprintf("3P Claude quota at %.0f%% (resets %s). Handoff to Gemini staged in clipboard. Switch via /model gemini-3.8-flash-high or paste prompt.", pool3P.FiveHour.UsedPct, resetStr),
		)
		n.warned3PPreLock = true
	} else if pool3P != nil && pool3P.FiveHour.UsedPct < 80.0 {
		n.warned3PPreLock = false
	}

	// 3P Transition: Unlocked -> Locked
	if is3PLocked && !n.last3PLocked {
		n.warned3PPreLock = true
		resetStr := "soon"
		if pool3P != nil && !pool3P.LockoutUntil.IsZero() {
			resetStr = router.FormatReset(pool3P.LockoutUntil, time.Now())
		}
		SendQuotaAlert(router.Pool3PClaude, QuotaLocked,
			"[Switch -> Gemini] 3P Quota Locked",
			fmt.Sprintf("3P 5-hour quota exhausted (resets %s). Switch to Gemini 3.8 Flash for unblocked progress.", resetStr),
		)
	}

	// 3P Transition: Locked -> Reset
	if !is3PLocked && n.last3PLocked {
		SendQuotaAlert(router.Pool3PClaude, QuotaReady,
			"[Switch -> Claude] 3P Quota Ready",
			"3P quota has reset! Ready to switch back to Claude 4.6 for deep architecture.",
		)
	}

	// Personal Claude Transition: Unlocked -> Locked
	if isPersLocked && !n.lastPersLocked {
		n.warnedPersPreLock = true
		resetStr := "soon"
		if poolPers != nil && !poolPers.LockoutUntil.IsZero() {
			resetStr = router.FormatReset(poolPers.LockoutUntil, time.Now())
		}
		SendQuotaAlert(router.PoolPersonalClaude, QuotaLocked,
			"[Switch -> Gemini] Claude Quota Locked",
			fmt.Sprintf("Claude Code quota exhausted (resets %s). Switch to Gemini 3.8 Flash in Antigravity (agy).", resetStr),
		)
	}

	// Personal Claude Transition: Locked -> Reset
	if !isPersLocked && n.lastPersLocked {
		SendQuotaAlert(router.PoolPersonalClaude, QuotaReady,
			"[Switch -> Claude] Claude Quota Ready",
			"Claude Code quota has reset! Ready to switch back to Claude for deep architecture.",
		)
	}

	// Gemini Transition: Unlocked -> Locked
	if isGemLocked && !n.lastGeminiLocked {
		SendQuotaAlert(router.PoolGeminiNative, QuotaLocked,
			"[Switch -> Claude] Gemini Quota Locked",
			"Gemini quota exhausted. Switch to Claude Sonnet or 3P for immediate progress.",
		)
	}

	// Gemini Transition: Locked -> Reset
	if !isGemLocked && n.lastGeminiLocked {
		SendQuotaAlert(router.PoolGeminiNative, QuotaReady,
			"[Switch -> Gemini] Gemini Quota Ready",
			"Gemini quota has reset! Ready to switch back to primary Gemini model.",
		)
	}

	n.last3PLocked = is3PLocked
	n.lastGeminiLocked = isGemLocked
	n.lastPersLocked = isPersLocked
}

// QuotaState is the quota pool transition an alert reports.
type QuotaState string

const (
	QuotaWarning QuotaState = "warning"
	QuotaLocked  QuotaState = "locked"
	QuotaReady   QuotaState = "ready"
)

// quotaAlert builds the Board alert for a quota pool changing state. The
// dedupe key is shared with the hook CLI's pre-lock warning, so both
// processes bump one alert.
func quotaAlert(pool router.PoolID, state QuotaState, title, message string) alerts.Alert {
	a := alerts.Alert{
		Title:     title,
		Message:   message,
		DedupeKey: fmt.Sprintf("quota:%s:%s", pool, state),
	}
	switch state {
	case QuotaWarning:
		a.Kind, a.Severity = "quota_warning", alerts.SeverityWarning
	case QuotaLocked:
		a.Kind, a.Severity = "quota_locked", alerts.SeverityWarning
	default:
		a.Kind, a.Severity = "quota_ready", alerts.SeverityInfo
	}
	return a
}

// SendQuotaAlert raises the Board alert for a quota pool changing state.
func SendQuotaAlert(pool router.PoolID, state QuotaState, title, message string) {
	SendAlert(quotaAlert(pool, state, title, message))
}

// runScript runs AppleScript; tests replace it.
var runScript = osascript.Run

// SendNotification shows a macOS notification. Delivery is best-effort: macOS
// can drop a notification while osascript still exits 0 (see package
// osascript). A non-zero exit, such as no GUI session, is logged with its exit
// status and stderr rather than dropped.
func SendNotification(title, message string) {
	slog.Info("notify", slog.String("title", title), slog.String("message", message))

	script := fmt.Sprintf(`display notification %s with title %s sound name "Glass"`,
		osascript.Quote(message), osascript.Quote(title))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runScript(ctx, script); err != nil {
		slog.Warn("notification failed", slog.String("title", title), slog.String("error", err.Error()))
	}
}
