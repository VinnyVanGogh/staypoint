package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/adapter"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
	"github.com/VinnyVanGogh/staypoint/internal/router"
)

// fakeCLI writes a stand-in for claude/agy that appends
// "<CLAUDE_CONFIG_DIR>|<args>" to a log per spawn.
// Spawns whose args contain failOn exit 1 before any output.
func fakeCLI(t *testing.T, failOn string) (bin, logPath string) {
	t.Helper()
	return fakeCLIOutput(t, failOn, "")
}

// fakeCLIOutput is fakeCLI whose non-failing spawns print stdoutFile (a
// captured CLI stream) when set, else "done".
func fakeCLIOutput(t *testing.T, failOn, stdoutFile string) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "spawns.log")
	bin = filepath.Join(dir, "fakecli.sh")
	script := "#!/bin/sh\nprintf '%s|%s\\n' \"${CLAUDE_CONFIG_DIR}\" \"$(echo \"$*\" | tr '\\n' ' ')\" >> \"" + logPath + "\"\n"
	if failOn != "" {
		script += "case \"$*\" in *" + failOn + "*) exit 1;; esac\n"
	}
	if stdoutFile != "" {
		script += "cat '" + stdoutFile + "'\n"
	} else {
		script += "echo 'done'\n"
	}
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logPath
}

type wakeResult struct {
	spawns []string // "<CLAUDE_CONFIG_DIR>|<args>"
	routes []string // route row titles in order
	bodies []string
	steps  []string // kinds of the agent's own steps (no bookkeeping rows)
	// interceptorComments counts completion-interceptor feedback rows. The
	// harness writes one only when it saw [[TASK_COMPLETE]] in a turn.
	interceptorComments int
	taskID              string
	messages            []string // message row titles (status error)
	stage               string   // final execution_stage
}

// wakeChoice is the provider/model stored on the next runWake task (STA-838).
// Tests set it with withChoice.
var wakeChoice struct{ provider, model string }

// wakeReuse, when store is set, makes runWakeIn wake an existing task in an
// existing store instead of creating both (multi-run tests).
var wakeReuse struct {
	store  *db.Store
	taskID string
}

func withChoice(t *testing.T, provider, model string) {
	t.Helper()
	wakeChoice.provider, wakeChoice.model = provider, model
	t.Cleanup(func() { wakeChoice.provider, wakeChoice.model = "", "" })
}

// runWake drives the production wake path (adapterOverride == nil) end to end:
// wireOnWake → harness → routed adapter → fake CLI, with quota state injected.
func runWake(t *testing.T, repoRoot, workKind string, pacer *router.PacerState, failOn string) wakeResult {
	t.Helper()
	bin, logPath := fakeCLI(t, failOn)
	return runWakeBin(t, repoRoot, workKind, pacer, bin, logPath)
}

// runWakeBin is runWake with a caller-supplied fake CLI.
func runWakeBin(t *testing.T, repoRoot, workKind string, pacer *router.PacerState, bin, logPath string) wakeResult {
	t.Helper()
	return runWakeIn(t, repoRoot, workKind, pacer, bin, logPath, t.TempDir())
}

// runWakeIn is runWakeBin with the run's worktree at wtDir.
func runWakeIn(t *testing.T, repoRoot, workKind string, pacer *router.PacerState, bin, logPath, wtDir string) wakeResult {
	t.Helper()

	origPacer, origRun, origSkip := loadPacer, runRoute, testSkipGitPreflight
	t.Cleanup(func() { loadPacer, runRoute, testSkipGitPreflight = origPacer, origRun, origSkip })
	testSkipGitPreflight = true
	loadPacer = func() (*router.PacerState, error) { return pacer, nil }
	// Only the first turn spawns: later turns fail fast so the harness stops
	// after its consecutive-error limit instead of burning its turn budget.
	var turns atomic.Int32
	runRoute = func(ctx context.Context, cwd string, p *router.PacerState, r router.KindRoute, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
		if turns.Add(1) > 1 {
			return errors.New("test: one turn only")
		}
		//nolint:staticcheck // the adapter's existing test hook is a string key.
		ctx = context.WithValue(ctx, "testBin", bin)
		return adapter.RunRoute(ctx, cwd, p, r, args, stdin, stdout, stderr)
	}

	orchestrator.GlobalDispatcher = orchestrator.NewDispatcher()
	store, taskID := wakeReuse.store, wakeReuse.taskID
	if store == nil {
		store = openTestStore(t)
		taskID = "route-" + strings.ReplaceAll(t.Name(), "/", "-")
	}
	if wakeReuse.store != nil {
		// existing task: nothing to insert
	} else if _, err := store.DB().Exec(
		`INSERT INTO tasks (id, name, repo_path, execution_stage, assignee_agent_id, work_kind, provider, model_override) VALUES (?, 'route test', ?, 'todo', 'agent-route', ?, ?, ?)`,
		taskID, repoRoot, workKind, wakeChoice.provider, wakeChoice.model,
	); err != nil {
		t.Fatalf("insert task: %v", err)
	}

	wireOnWake(store, repoRoot, nil, nil, &stubWM{dir: wtDir})
	orchestrator.GlobalDispatcher.Wake(taskID, "test_route", "test:"+taskID)
	done := make(chan struct{})
	go func() { orchestrator.GlobalDispatcher.Drain(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("dispatcher did not drain")
	}

	res := wakeResult{taskID: taskID}
	msgRows, err := store.DB().Query(`SELECT title FROM run_steps WHERE task_id=? AND kind='message' AND status='error' ORDER BY seq`, taskID)
	if err != nil {
		t.Fatalf("query message steps: %v", err)
	}
	for msgRows.Next() {
		var title string
		if err := msgRows.Scan(&title); err != nil {
			t.Fatal(err)
		}
		res.messages = append(res.messages, title)
	}
	msgRows.Close()
	_ = store.DB().QueryRow(`SELECT COALESCE(execution_stage,'') FROM tasks WHERE id=?`, taskID).Scan(&res.stage)
	if data, err := os.ReadFile(logPath); err == nil {
		res.spawns = strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	rows, err := store.DB().Query(`SELECT title, COALESCE(body,'') FROM run_steps WHERE task_id=? AND kind='route' ORDER BY seq`, taskID)
	if err != nil {
		t.Fatalf("query route steps: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var title, body string
		if err := rows.Scan(&title, &body); err != nil {
			t.Fatal(err)
		}
		res.routes = append(res.routes, title)
		res.bodies = append(res.bodies, body)
	}
	if err := store.DB().QueryRow(`SELECT COUNT(1) FROM task_comments WHERE task_id=? AND author='interceptor'`, taskID).Scan(&res.interceptorComments); err != nil {
		t.Fatalf("count interceptor comments: %v", err)
	}
	steps, err := store.DB().Query(`SELECT kind FROM run_steps WHERE task_id=? AND kind IN ('think','read','run','edit') ORDER BY seq`, taskID)
	if err != nil {
		t.Fatalf("query agent steps: %v", err)
	}
	defer steps.Close()
	for steps.Next() {
		var kind string
		if err := steps.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		res.steps = append(res.steps, kind)
	}
	return res
}

func openPacer() *router.PacerState {
	return &router.PacerState{Pools: map[router.PoolID]*router.QuotaPool{}}
}

func lockedPacer(locks map[router.PoolID]string) *router.PacerState {
	p := openPacer()
	for id, why := range locks {
		p.Pools[id] = &router.QuotaPool{ID: id, IsLocked: true, LockoutReason: why}
	}
	return p
}

func personalRepo(t *testing.T) string { return filepath.Join(t.TempDir(), "personal-app") }
func workRepo(t *testing.T) string     { return filepath.Join(t.TempDir(), "mansol-app") }

func isClaude(spawn string) bool { return strings.Contains(spawn, "--print") }

func mustOneSpawn(t *testing.T, r wakeResult) (cfgDir, args string) {
	t.Helper()
	if len(r.spawns) != 1 {
		t.Fatalf("want exactly 1 spawn, got %q", r.spawns)
	}
	cfgDir, args, _ = strings.Cut(r.spawns[0], "|")
	return cfgDir, args
}

func TestWake_PersonalCodingSpawnsPersonalClaudeOpus(t *testing.T) {
	r := runWake(t, personalRepo(t), "coding", openPacer(), "")
	cfgDir, args := mustOneSpawn(t, r)
	if !isClaude(args) || !strings.Contains(args, "--model opus") {
		t.Errorf("spawned %q, want claude --model opus", args)
	}
	if cfgDir != "" {
		t.Errorf("personal seat must not set CLAUDE_CONFIG_DIR, got %q", cfgDir)
	}
	if len(r.routes) != 1 || r.routes[0] != "Ran on Claude Opus · personal seat" {
		t.Errorf("route rows = %q", r.routes)
	}
	if r.bodies[0] != "Kind of work: coding" {
		t.Errorf("route body = %q", r.bodies[0])
	}
}

// STA-772 repro: personal seat locked must not push a work repo off Claude.
func TestWake_WorkRepoPersonalLockedSpawnsWorkClaude(t *testing.T) {
	r := runWake(t, workRepo(t), "coding", lockedPacer(map[router.PoolID]string{router.PoolPersonalClaude: "weekly limit"}), "")
	cfgDir, args := mustOneSpawn(t, r)
	home, _ := os.UserHomeDir()
	if !isClaude(args) || !strings.Contains(args, "--model opus") {
		t.Errorf("spawned %q, want claude --model opus", args)
	}
	if cfgDir != home+"/.claude-work" {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want %s/.claude-work", cfgDir, home)
	}
	if len(r.routes) != 1 || r.routes[0] != "Ran on Claude Opus · work seat" {
		t.Errorf("route rows = %q", r.routes)
	}
}

// STA-856: for a code kind a locked work seat falls back to the personal
// Claude seat (default profile, no CLAUDE_CONFIG_DIR), never to Gemini.
func TestWake_WorkRepoWorkLockedSpawnsPersonalClaudeNeverGemini(t *testing.T) {
	for _, kind := range []string{"coding", "qa", "", "bogus"} {
		t.Run("kind="+kind, func(t *testing.T) {
			r := runWake(t, workRepo(t), kind, lockedPacer(map[router.PoolID]string{router.PoolWorkClaude: "5h limit"}), "")
			cfgDir, args := mustOneSpawn(t, r)
			if !isClaude(args) {
				t.Errorf("spawned %q, want personal Claude", args)
			}
			if cfgDir != "" {
				t.Errorf("personal seat must not set CLAUDE_CONFIG_DIR, got %q", cfgDir)
			}
			if len(r.routes) != 1 || !strings.HasPrefix(r.routes[0], "Fell back to Claude ") ||
				!strings.HasSuffix(r.routes[0], "· personal seat: work seat locked (5h limit)") {
				t.Errorf("route rows = %q", r.routes)
			}
		})
	}
}

// GeminiCodeForbidden (all repos): with every Claude seat locked a code-kind
// run waits in the quota queue. Gemini has quota but is never spawned, even
// when the task's stored provider is gemini.
func TestWake_CodeKindAllSeatsLockedQueuesNeverGemini(t *testing.T) {
	pacer := lockedPacer(map[router.PoolID]string{
		router.PoolWorkClaude:     "weekly limit",
		router.PoolPersonalClaude: "5h limit",
	})
	for _, c := range []struct {
		name, repoKind, kind, provider string
	}{
		{"work coding", "work", "coding", ""},
		{"personal coding", "personal", "coding", ""},
		{"personal qa", "personal", "qa", ""},
		{"personal coding stored gemini", "personal", "coding", "gemini"},
		{"work qa stored gemini", "work", "qa", "gemini"},
	} {
		t.Run(c.name, func(t *testing.T) {
			slots := freshSlots(t, 3)
			withChoice(t, c.provider, "")
			repo := personalRepo(t)
			if c.repoKind == "work" {
				repo = workRepo(t)
			}
			r := runWake(t, repo, c.kind, pacer, "")
			if len(r.spawns) != 0 {
				t.Fatalf("spawned %q while every Claude seat was locked; want a queued run", r.spawns)
			}
			if len(r.routes) != 0 {
				t.Errorf("a queued run must not write route rows, got %q", r.routes)
			}
			pos := slots.Position(r.taskID)
			if !pos.Queued || pos.Wait != orchestrator.WaitQuota {
				t.Fatalf("run not quota-queued: %+v", pos)
			}
			slots.Dequeue(r.taskID)
		})
	}
}

// Non-code kinds run Gemini first automatically in every repo (the harness
// guard reverts any code a Gemini turn writes).
func TestWake_NonCodeKindsSpawnGeminiEveryRepo(t *testing.T) {
	for _, repoKind := range []string{"personal", "work"} {
		for _, kind := range []string{"planning", "architecture", "review", "docs"} {
			t.Run(repoKind+"/"+kind, func(t *testing.T) {
				repo := personalRepo(t)
				if repoKind == "work" {
					repo = workRepo(t)
				}
				r := runWake(t, repo, kind, openPacer(), "")
				_, args := mustOneSpawn(t, r)
				if isClaude(args) || !strings.Contains(args, "--model gemini-") {
					t.Errorf("spawned %q, want agy", args)
				}
				if len(r.routes) != 1 || !strings.HasPrefix(r.routes[0], "Ran on Gemini ") || strings.Contains(r.routes[0], "chosen by Board") {
					t.Errorf("route rows = %q", r.routes)
				}
			})
		}
	}
}

// STA-838: the Board's explicit choice. provider=gemini on a non-code kind
// runs Gemini with the "chosen by Board" label; provider=claude on a non-code
// kind runs Claude; a stored gemini choice on a code kind in a work repo
// still runs Claude (personal repos wait for Touch ID: gemini_code_gate_test).
func TestWake_ExplicitProviderChoice(t *testing.T) {
	cases := []struct {
		name, kind, provider, model string
		wantClaude                  bool
		wantArgs, wantRoute         string
	}{
		{"gemini on docs", "docs", "gemini", "", false, "--model gemini-3.1-pro --effort high", "Ran on Gemini 3.1 Pro · chosen by Board"},
		{"gemini flash on review", "review", "gemini", "gemini-3.8-flash-high", false, "--model gemini-3.8-flash --effort high", "Ran on Gemini 3.8 Flash · chosen by Board"},
		{"claude on planning", "planning", "claude", "", true, "--model sonnet", "Ran on Claude Sonnet · personal seat"},
		{"claude opus on architecture", "architecture", "claude", "opus", true, "--model opus", "Ran on Claude Opus · personal seat"},
		{"gemini stored on work coding", "coding", "gemini", "", true, "--model opus", "Ran on Claude Opus · work seat"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withChoice(t, c.provider, c.model)
			repo := personalRepo(t)
			if strings.Contains(c.name, "work") {
				repo = workRepo(t)
			}
			r := runWake(t, repo, c.kind, openPacer(), "")
			_, args := mustOneSpawn(t, r)
			if isClaude(args) != c.wantClaude || !strings.Contains(args, c.wantArgs) {
				t.Errorf("spawned %q, want claude=%v %s", args, c.wantClaude, c.wantArgs)
			}
			if len(r.routes) != 1 || r.routes[0] != c.wantRoute {
				t.Errorf("route rows = %q, want %q", r.routes, c.wantRoute)
			}
			if c.kind == "coding" && !strings.Contains(r.bodies[0], "Gemini refused") {
				t.Errorf("body %q should say the gemini choice was refused", r.bodies[0])
			}
		})
	}
}

func TestWake_UnknownAndEmptyKindAreClaudeFirst(t *testing.T) {
	for _, kind := range []string{"", "bogus"} {
		t.Run("kind="+kind, func(t *testing.T) {
			r := runWake(t, personalRepo(t), kind, openPacer(), "")
			_, args := mustOneSpawn(t, r)
			if !isClaude(args) || !strings.Contains(args, "--model opus") {
				t.Errorf("spawned %q, want claude --model opus", args)
			}
			if len(r.routes) != 1 || r.routes[0] != "Ran on Claude Opus · personal seat" {
				t.Errorf("route rows = %q", r.routes)
			}
		})
	}
}

// Every kind in the table is honoured by the daemon, and the route row always
// names the provider that was actually spawned.
func TestWake_EveryKindRouteMatchesSpawn(t *testing.T) {
	cases := []struct {
		kind, wantArgs, wantRoute string
		claude                    bool
	}{
		{"coding", "--model opus", "Ran on Claude Opus · personal seat", true},
		{"qa", "--model opus", "Ran on Claude Opus · personal seat", true},
		{"review", "--model gemini-3.1-pro --effort high", "Ran on Gemini 3.1 Pro", false},
		{"architecture", "--model gemini-3.1-pro --effort high", "Ran on Gemini 3.1 Pro", false},
		{"planning", "--model gemini-3.8-flash --effort high", "Ran on Gemini 3.8 Flash", false},
		{"docs", "--model gemini-3.8-flash --effort high", "Ran on Gemini 3.8 Flash", false},
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			r := runWake(t, personalRepo(t), c.kind, openPacer(), "")
			_, args := mustOneSpawn(t, r)
			if isClaude(args) != c.claude || !strings.Contains(args, c.wantArgs) {
				t.Errorf("spawned %q, want claude=%v %s", args, c.claude, c.wantArgs)
			}
			if len(r.routes) != 1 || r.routes[0] != c.wantRoute {
				t.Errorf("route rows = %q, want %q", r.routes, c.wantRoute)
			}
		})
	}
}

func TestWake_GeminiFirstKindFallsBackToClaudeWhenGeminiLocked(t *testing.T) {
	for _, kind := range []string{"architecture", "planning"} {
		t.Run(kind, func(t *testing.T) {
			r := runWake(t, workRepo(t), kind, lockedPacer(map[router.PoolID]string{router.PoolGeminiNative: "5h limit reached"}), "")
			cfgDir, args := mustOneSpawn(t, r)
			if !isClaude(args) {
				t.Errorf("spawned %q, want Claude fallback", args)
			}
			home, _ := os.UserHomeDir()
			if cfgDir != home+"/.claude-work" {
				t.Errorf("work repo fallback must use the work seat, CLAUDE_CONFIG_DIR=%q", cfgDir)
			}
			if len(r.routes) != 1 || !strings.HasPrefix(r.routes[0], "Fell back to Claude") || !strings.HasSuffix(r.routes[0], "Gemini locked (5h limit reached)") {
				t.Errorf("route rows = %q", r.routes)
			}
		})
	}
}

// A Gemini spawn (non-code kind) that dies before output falls over to
// Claude, and a second route row says so: the timeline never claims Gemini
// ran when Claude did.
func TestWake_RuntimeFailoverRelabelsRoute(t *testing.T) {
	r := runWake(t, personalRepo(t), "architecture", openPacer(), "gemini-3.1-pro")
	if len(r.spawns) < 2 || isClaude(r.spawns[0]) || !isClaude(r.spawns[1]) {
		t.Fatalf("want gemini then claude spawns, got %q", r.spawns)
	}
	if len(r.routes) != 2 {
		t.Fatalf("want planned + fallback route rows, got %q", r.routes)
	}
	if r.routes[0] != "Ran on Gemini 3.1 Pro" {
		t.Errorf("planned row = %q", r.routes[0])
	}
	if !strings.HasPrefix(r.routes[1], "Fell back to Claude Opus · personal seat: Gemini 3.1 Pro failed") {
		t.Errorf("fallback row = %q", r.routes[1])
	}
}

// A Claude coding spawn that dies never falls over to Gemini.
func TestWake_CodingClaudeFailureNeverSpawnsGemini(t *testing.T) {
	r := runWake(t, personalRepo(t), "coding", openPacer(), "--print")
	for _, s := range r.spawns {
		if !isClaude(s) {
			t.Fatalf("coding run spawned a non-Claude CLI: %q", r.spawns)
		}
	}
	for _, title := range r.routes {
		if strings.Contains(title, "Gemini") {
			t.Errorf("route row names Gemini: %q", title)
		}
	}
}

// A Gemini-routed run's agy stream is parsed with the agy parser, so its tool
// calls reach the timeline (STA-775: they were parsed as Claude and dropped,
// leaving only wake/route/checkpoint rows).
func TestWake_GeminiRunRecordsAgySteps(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("..", "..", "internal", "adapter", "testdata", "agy_stream_success.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	bin, logPath := fakeCLIOutput(t, "", fixture)
	r := runWakeBin(t, personalRepo(t), "planning", openPacer(), bin, logPath)
	if _, args := mustOneSpawn(t, r); isClaude(args) {
		t.Fatalf("spawned %q, want agy", args)
	}
	if !slices.Contains(r.steps, "read") {
		t.Fatalf("agent steps = %q, want the fixture's view_file as a read step", r.steps)
	}
}

func TestRouteTracker_OnlyEmitsOnSwitch(t *testing.T) {
	route := router.ResolveRoute("planning", false, openPacer(), "opus", time.Now())
	var titles []string
	tr := newRouteTracker(route, func(title, _ string) { titles = append(titles, title) })
	if tr.Provider() != "claude" {
		t.Fatalf("planned provider = %q", tr.Provider())
	}
	tr.EmitPlanned()
	tr.Observe(adapter.AttemptInfo{Provider: "claude", Slot: route.Candidates[0]})
	tr.Observe(adapter.AttemptInfo{Provider: "gemini", Slot: route.Candidates[1], FallbackReason: "personal seat locked (5h limit reached)"})
	tr.Observe(adapter.AttemptInfo{Provider: "gemini", Slot: route.Candidates[1], FallbackReason: "personal seat locked (5h limit reached)"})
	tr.Observe(adapter.AttemptInfo{Provider: "claude", Slot: route.Candidates[0]})
	want := []string{
		"Ran on Claude Opus · personal seat",
		"Fell back to Gemini 3.1 Pro: personal seat locked (5h limit reached)",
		"Ran on Claude Opus · personal seat",
	}
	if strings.Join(titles, "\n") != strings.Join(want, "\n") {
		t.Errorf("titles = %q, want %q", titles, want)
	}
	if tr.Provider() != "claude" {
		t.Errorf("provider = %q", tr.Provider())
	}
}
