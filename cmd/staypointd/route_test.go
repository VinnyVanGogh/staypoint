package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/adapter"
	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
	"github.com/VinnyVanGogh/staypoint/internal/router"
)

// fakeCLI writes a stand-in for claude/agy that appends
// "<CLAUDE_CONFIG_DIR>|<args>" to a log per spawn.
// Spawns whose args contain failOn exit 1 before any output.
func fakeCLI(t *testing.T, failOn string) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "spawns.log")
	bin = filepath.Join(dir, "fakecli.sh")
	script := "#!/bin/sh\nprintf '%s|%s\\n' \"${CLAUDE_CONFIG_DIR}\" \"$(echo \"$*\" | tr '\\n' ' ')\" >> \"" + logPath + "\"\n"
	if failOn != "" {
		script += "case \"$*\" in *" + failOn + "*) exit 1;; esac\n"
	}
	script += "echo 'done'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logPath
}

type wakeResult struct {
	spawns []string // "<CLAUDE_CONFIG_DIR>|<args>"
	routes []string // route row titles in order
	bodies []string
	// interceptorComments counts completion-interceptor feedback rows. The
	// harness writes one only when it saw [[TASK_COMPLETE]] in a turn.
	interceptorComments int
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
	store := openTestStore(t)
	taskID := "route-" + strings.ReplaceAll(t.Name(), "/", "-")
	if _, err := store.DB().Exec(
		`INSERT INTO tasks (id, name, repo_path, execution_stage, assignee_agent_id, work_kind) VALUES (?, 'route test', '', 'todo', 'agent-route', ?)`,
		taskID, workKind,
	); err != nil {
		t.Fatalf("insert task: %v", err)
	}

	wireOnWake(store, repoRoot, nil, nil, &stubWM{dir: t.TempDir()})
	orchestrator.GlobalDispatcher.Wake(taskID, "test_route", "test:"+taskID)
	done := make(chan struct{})
	go func() { orchestrator.GlobalDispatcher.Drain(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("dispatcher did not drain")
	}

	var res wakeResult
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

func TestWake_WorkRepoWorkLockedSpawnsGeminiWithReason(t *testing.T) {
	r := runWake(t, workRepo(t), "coding", lockedPacer(map[router.PoolID]string{router.PoolWorkClaude: "weekly limit"}), "")
	_, args := mustOneSpawn(t, r)
	if isClaude(args) || !strings.Contains(args, "--model gemini-3.1-pro --effort high") {
		t.Errorf("spawned %q, want agy --model gemini-3.1-pro --effort high", args)
	}
	if len(r.routes) != 1 || r.routes[0] != "Fell back to Gemini 3.1 Pro: work seat locked (weekly limit)" {
		t.Errorf("route rows = %q", r.routes)
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
		{"review", "--model opus", "Ran on Claude Opus · personal seat", true},
		{"architecture", "--model gemini-3.1-pro --effort high", "Ran on Gemini 3.1 Pro", false},
		{"planning", "--model gemini-3.8-flash --effort high", "Ran on Gemini 3.8 Flash", false},
		{"qa", "--model gemini-3.8-flash --effort high", "Ran on Gemini 3.8 Flash", false},
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

// A Claude spawn that dies before output falls over to Gemini, and a second
// route row says so: the timeline never claims Claude ran when Gemini did.
func TestWake_RuntimeFailoverRelabelsRoute(t *testing.T) {
	r := runWake(t, personalRepo(t), "coding", openPacer(), "--print")
	if len(r.spawns) < 2 || !isClaude(r.spawns[0]) || isClaude(r.spawns[1]) {
		t.Fatalf("want claude then gemini spawns, got %q", r.spawns)
	}
	if len(r.routes) != 2 {
		t.Fatalf("want planned + fallback route rows, got %q", r.routes)
	}
	if r.routes[0] != "Ran on Claude Opus · personal seat" {
		t.Errorf("planned row = %q", r.routes[0])
	}
	if !strings.HasPrefix(r.routes[1], "Fell back to Gemini 3.1 Pro: Claude Opus · personal seat failed") {
		t.Errorf("fallback row = %q", r.routes[1])
	}
}

func TestRouteTracker_OnlyEmitsOnSwitch(t *testing.T) {
	route := router.ResolveRoute("coding", false, openPacer(), "", time.Now())
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
