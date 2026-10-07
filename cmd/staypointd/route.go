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
)

// loadPacer and runRoute are package vars so tests can inject quota state and
// a fake CLI without touching the real ~/.config pacer files or binaries.
var (
	loadPacer = router.LoadPacerState
	runRoute  = adapter.RunRoute
	// testSkipGitPreflight lets tests drive the production adapter path in a
	// temp dir that is not a git checkout.
	testSkipGitPreflight = false
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
// work_kind, its repo's seat (work vs personal) and current quota. An empty
// repo_path resolves against repoRoot, matching the harness.
func resolveTaskRoute(db *sql.DB, taskID, repoRoot string, pacer *router.PacerState, now time.Time) router.KindRoute {
	var repoPath, workKind string
	if err := db.QueryRowContext(context.Background(),
		"SELECT COALESCE(repo_path,''), COALESCE(work_kind,'') FROM tasks WHERE id=?", taskID,
	).Scan(&repoPath, &workKind); err != nil && !errors.Is(err, sql.ErrNoRows) {
		slog.Warn("route: task lookup failed; routing as coding on the personal seat",
			slog.String("task", taskID), slog.Any("error", err))
	}
	if repoPath == "" {
		repoPath = repoRoot
	}
	isWork := false
	if repoPath != "" {
		isWork, _, _ = router.IsWorkRepo(repoPath)
	}
	return router.ResolveRoute(workKind, isWork, pacer, "", now)
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

// geminiDocsOnly applies the Board rule (STA-856) to a run: in a work repo the
// harness reverts any non-doc change a Gemini turn makes and fails the run.
func geminiDocsOnly(r router.KindRoute) bool { return router.GeminiCodeForbidden(r.IsWork) }

// TakeGeminiSpawned reports whether a Gemini CLI was spawned since the last
// call, and resets the flag (RunConfig.TurnUsedGemini).
func (t *routeTracker) TakeGeminiSpawned() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	v := t.geminiSpawned
	t.geminiSpawned = false
	return v
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
