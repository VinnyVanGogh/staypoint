package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/adapter"
	"github.com/VinnyVanGogh/staypoint/internal/router"
	"github.com/VinnyVanGogh/staypoint/internal/workorgs"
)

// loadPacer and runRoute are package vars so tests can inject quota state and
// a fake CLI without touching the real ~/.config pacer files or binaries.
var (
	loadPacer = router.LoadPacerState
	runRoute  = adapter.RunRoute
	// testSkipGitPreflight lets tests drive the production adapter path in a
	// temp dir that is not a git checkout.
	testSkipGitPreflight = false
	// routeUIOLI is the use-it-or-lose-it tuning for run routing, set from
	// config at startup.
	routeUIOLI router.UIOLIConfig
)

// currentPacer loads live quota state, degrading to "no data" (nothing locked)
// when the pacer files cannot be read.
func currentPacer() *router.PacerState {
	p, err := loadPacer()
	if err != nil || p == nil {
		if err != nil {
			slog.Warn("pacer state load failed; routing without quota data", slog.Any("error", err))
		}
		return &router.PacerState{Pools: make(map[router.PoolID]*router.QuotaPool)}
	}
	if p.Pools == nil {
		p.Pools = make(map[router.PoolID]*router.QuotaPool)
	}
	return p
}

// resolveTaskRoute picks the routing chain for one run of taskID from its
// work_kind, the Board's explicit provider/model choice (STA-838), its repo's
// seat (work vs personal) and current quota. An empty repo_path resolves
// against repoRoot, matching the harness. Code kinds never get Gemini, even
// with provider=gemini stored (router.GeminiCodeForbidden).
func resolveTaskRoute(db *sql.DB, taskID, repoRoot string, pacer *router.PacerState, now time.Time) router.KindRoute {
	return resolveTaskRouteApproved(db, taskID, repoRoot, pacer, now, "")
}

// resolveTaskRouteApproved is resolveTaskRoute for a run that consumed the
// Board Touch ID approval approvalID (geminiCodeGate); "" for none.
func resolveTaskRouteApproved(db *sql.DB, taskID, repoRoot string, pacer *router.PacerState, now time.Time, approvalID string) router.KindRoute {
	var repoPath, workKind, provider, model, org, priority string
	if err := db.QueryRowContext(context.Background(),
		"SELECT COALESCE(repo_path,''), COALESCE(work_kind,''), COALESCE(provider,''), COALESCE(model_override,''), COALESCE(organization,''), COALESCE(priority,'') FROM tasks WHERE id=?", taskID,
	).Scan(&repoPath, &workKind, &provider, &model, &org, &priority); err != nil && !errors.Is(err, sql.ErrNoRows) {
		slog.Warn("route: task lookup failed; routing as coding on the personal seat",
			slog.String("task", taskID), slog.Any("error", err))
	}
	rawRepoPath := repoPath
	if repoPath == "" {
		repoPath = repoRoot
	}
	// A work-org task (work_orgs) routes as work wherever its repo lives:
	// work seat, no Gemini code.
	isWork := workorgs.IsWork(org)
	if repoPath != "" && !isWork {
		isWork, _, _ = router.IsWorkRepo(repoPath)
	}
	// Non-git dir (STA-864): no checkpoint to revert, so a Board Touch ID
	// approval never lets Gemini write code there, and code kinds drop Gemini.
	if !taskDirIsGit(rawRepoPath, taskID) {
		approvalID = ""
	}
	choice := router.ChoiceFromStored(provider, model)
	choice.CodeApprovalID = approvalID
	uioli := routeUIOLI
	choice.UIOLI = &uioli
	choice.HighPriority = router.IsHighPriority(priority)
	route := router.ResolveRouteChoice(workKind, isWork, pacer, choice, now)
	return barGeminiOutsideGit(route, rawRepoPath, taskID)
}

// slotProvider is the adapter/stream-parser key for a routed slot.
func slotProvider(s router.RouteSlot) string {
	switch s.Family {
	case router.FamilyClaude:
		return "claude"
	case router.FamilyCloud:
		return "cloud_session"
	default:
		return "gemini"
	}
}

// routeTracker keeps the route row honest: it announces the planned slot once,
// then announces again whenever a turn actually spawns a different slot (a seat
// locked after planning, or a pre-output failure), with the reason.
type routeTracker struct {
	mu        sync.Mutex
	route     router.KindRoute
	announced router.RouteSlot
	provider  string
	emit      func(title, body string)
	// geminiSpawned is set whenever a Gemini CLI is about to stream; the
	// harness consumes it after each turn for the STA-856 guard.
	geminiSpawned bool
	// lastSeat labels the most recent spawn ("Claude Opus · personal seat");
	// the harness consumes it after each turn to record the seat used.
	lastSeat string
}

func newRouteTracker(route router.KindRoute, emit func(title, body string)) *routeTracker {
	t := &routeTracker{route: route, emit: emit, provider: "claude"}
	if s, ok := route.Chosen(); ok {
		t.announced = s
		t.provider = slotProvider(s)
	}
	return t
}

// EmitPlanned emits the route row for the planned slot.
func (t *routeTracker) EmitPlanned() {
	if t.emit != nil {
		t.emit(t.route.Title(), t.route.Body())
	}
}

// Observe is the adapter attempt observer: it records which provider is about
// to stream (for the stream parser) and emits a new route row on a switch.
func (t *routeTracker) Observe(a adapter.AttemptInfo) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.provider = a.Provider
	t.lastSeat = a.Slot.Label()
	if a.Slot == (router.RouteSlot{}) {
		t.lastSeat = a.Name
	}
	if a.Provider == "gemini" || a.Slot.Family == router.FamilyGemini {
		t.geminiSpawned = true
	}
	if a.Slot == t.announced {
		return
	}
	t.announced = a.Slot
	title := "Ran on " + a.Slot.Label()
	if a.FallbackReason != "" {
		title = "Fell back to " + a.Slot.Label() + ": " + a.FallbackReason
	}
	if t.emit != nil {
		t.emit(title, t.route.Body())
	}
}

// geminiDocsOnly applies the Board rule (STA-856, all repos) to a run: the
// harness reverts any code change a Gemini turn makes and fails the run. It
// is on whenever the route can spawn Gemini; a Claude-only route skips the
// per-turn snapshot. TurnUsedGemini still gates enforcement per turn.
func geminiDocsOnly(r router.KindRoute) bool {
	return router.GeminiCodeForbidden(r.IsWork) && r.HasGemini()
}

// TakeGeminiSpawned reports whether a Gemini CLI was spawned since the last
// call, and resets the flag (RunConfig.TurnUsedGemini).
func (t *routeTracker) TakeGeminiSpawned() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	v := t.geminiSpawned
	t.geminiSpawned = false
	return v
}

// TakeSeat returns the label of the seat spawned since the last call ("" when
// nothing spawned, e.g. every seat locked), and resets (RunConfig.TurnSeat).
func (t *routeTracker) TakeSeat() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.lastSeat
	t.lastSeat = ""
	return s
}

// Provider is the provider of the most recent (or planned) spawn.
func (t *routeTracker) Provider() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.provider
}

// runRouted runs one harness turn on the task's routed chain, re-reading quota
// at spawn time so a seat that locked mid-run is skipped with a reason.
func (t *routeTracker) runRouted(ctx context.Context, cwd string, rawArgs []string, stdout, stderr io.Writer) error {
	ctx = adapter.WithAttemptObserver(ctx, t.Observe)
	return runRoute(ctx, cwd, currentPacer(), t.route, rawArgs, nil, stdout, stderr)
}
