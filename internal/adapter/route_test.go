package adapter

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/router"
)

// spawnRecorder writes a fake CLI that appends one line per invocation to
// logPath: "<CLAUDE_CONFIG_DIR>|<args...>". When failOn is non-empty and the
// args contain it, the script exits 1 (to exercise runtime failover).
func spawnRecorder(t *testing.T, failOn string) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "spawns.log")
	bin = filepath.Join(dir, "fakecli.sh")
	script := `#!/bin/sh
echo "${CLAUDE_CONFIG_DIR}|$*" >> "` + logPath + `"
`
	if failOn != "" {
		script += `case "$*" in *` + failOn + `*) exit 1;; esac
`
	}
	script += `echo '{"type":"result","result":"ok"}'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, logPath
}

func readSpawns(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read spawn log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

type attemptLog struct{ attempts []AttemptInfo }

func (a *attemptLog) observe(i AttemptInfo) { a.attempts = append(a.attempts, i) }

func runRouteWithFake(t *testing.T, d router.KindRoute, pacer *router.PacerState, failOn string) ([]string, *attemptLog, error) {
	t.Helper()
	bin, logPath := spawnRecorder(t, failOn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, testBinKey, bin)
	log := &attemptLog{}
	ctx = WithAttemptObserver(ctx, log.observe)
	var stdout, stderr bytes.Buffer
	err := RunRoute(ctx, t.TempDir(), pacer, d, []string{"--prompt", "do the thing"}, nil, &stdout, &stderr)
	return readSpawns(t, logPath), log, err
}

func isClaudeSpawn(line string) bool { return strings.Contains(line, "--print") }

func TestRunRoute_PersonalCodingSpawnsPersonalClaudeOpus(t *testing.T) {
	pacer := &router.PacerState{Pools: map[router.PoolID]*router.QuotaPool{}}
	d := router.ResolveRoute("coding", false, pacer, "", time.Now())
	spawns, log, err := runRouteWithFake(t, d, pacer, "")
	if err != nil {
		t.Fatalf("RunRoute: %v", err)
	}
	if len(spawns) != 1 {
		t.Fatalf("want 1 spawn, got %v", spawns)
	}
	cfgDir, args, _ := strings.Cut(spawns[0], "|")
	if !isClaudeSpawn(args) || !strings.Contains(args, "--model opus") {
		t.Errorf("want claude --model opus, got args %q", args)
	}
	if cfgDir != "" {
		t.Errorf("personal seat must not set CLAUDE_CONFIG_DIR, got %q", cfgDir)
	}
	if len(log.attempts) != 1 || log.attempts[0].Slot != mustSlot(t, d) || log.attempts[0].FallbackReason != "" {
		t.Errorf("observer must report the routed slot with no fallback, got %+v", log.attempts)
	}
}

func TestRunRoute_WorkRepoPersonalLockedSpawnsWorkClaude(t *testing.T) {
	pacer := &router.PacerState{Pools: map[router.PoolID]*router.QuotaPool{
		router.PoolPersonalClaude: {IsLocked: true, LockoutReason: "weekly limit"},
	}}
	d := router.ResolveRoute("coding", true, pacer, "", time.Now())
	spawns, _, err := runRouteWithFake(t, d, pacer, "")
	if err != nil {
		t.Fatalf("RunRoute: %v", err)
	}
	home, _ := os.UserHomeDir()
	cfgDir, args, _ := strings.Cut(spawns[0], "|")
	if !isClaudeSpawn(args) || !strings.Contains(args, "--model opus") {
		t.Errorf("want claude --model opus, got %q", args)
	}
	if cfgDir != home+"/.claude-work" {
		t.Errorf("work seat must set CLAUDE_CONFIG_DIR=%s/.claude-work, got %q", home, cfgDir)
	}
	if d.Title() != "Ran on Claude Opus · work seat" {
		t.Errorf("route title %q does not match the spawned work seat", d.Title())
	}
}

// STA-856: a locked work seat falls back to personal Claude, never Gemini.
func TestRunRoute_WorkRepoWorkLockedSpawnsPersonalClaude(t *testing.T) {
	pacer := &router.PacerState{Pools: map[router.PoolID]*router.QuotaPool{
		router.PoolWorkClaude: {IsLocked: true, LockoutReason: "weekly limit"},
	}}
	d := router.ResolveRoute("coding", true, pacer, "", time.Now())
	spawns, log, err := runRouteWithFake(t, d, pacer, "")
	if err != nil {
		t.Fatalf("RunRoute: %v", err)
	}
	if len(spawns) != 1 {
		t.Fatalf("locked work seat must not be spawned; got %v", spawns)
	}
	cfgDir, args, _ := strings.Cut(spawns[0], "|")
	if !isClaudeSpawn(args) || !strings.Contains(args, "--model opus") || cfgDir != "" {
		t.Errorf("want personal claude --model opus, got cfg=%q args=%q", cfgDir, args)
	}
	if d.Title() != "Fell back to Claude Opus · personal seat: work seat locked (weekly limit)" {
		t.Errorf("title = %q", d.Title())
	}
	if len(log.attempts) != 1 || log.attempts[0].Slot.Seat != router.SeatPersonal {
		t.Errorf("observer must report the personal seat, got %+v", log.attempts)
	}
}

func TestRunRoute_ModelOverrideSpawnsSonnet(t *testing.T) {
	pacer := &router.PacerState{Pools: map[router.PoolID]*router.QuotaPool{}}
	d := router.ResolveRoute("coding", false, pacer, "sonnet", time.Now())
	spawns, _, err := runRouteWithFake(t, d, pacer, "")
	if err != nil {
		t.Fatalf("RunRoute: %v", err)
	}
	if _, args, _ := strings.Cut(spawns[0], "|"); !strings.Contains(args, "--model sonnet") {
		t.Errorf("want --model sonnet, got %q", args)
	}
}

// A Gemini spawn (non-code kind) that fails before committing output falls
// over to Claude, and the observer reports the switch with a reason so the
// timeline can say so.
func TestRunRoute_RuntimeFailoverReportsReason(t *testing.T) {
	pacer := &router.PacerState{Pools: map[router.PoolID]*router.QuotaPool{}}
	d := router.ResolveRoute("planning", false, pacer, "", time.Now())
	spawns, log, err := runRouteWithFake(t, d, pacer, "gemini")
	if err != nil {
		t.Fatalf("RunRoute: %v", err)
	}
	if len(spawns) != 2 || isClaudeSpawn(spawns[0]) || !isClaudeSpawn(spawns[1]) {
		t.Fatalf("want gemini then claude spawns, got %v", spawns)
	}
	if len(log.attempts) != 2 {
		t.Fatalf("want 2 attempts, got %+v", log.attempts)
	}
	second := log.attempts[1]
	if second.Slot.Family != router.FamilyClaude || !strings.Contains(second.FallbackReason, "Gemini 3.8 Flash failed") {
		t.Errorf("second attempt = %+v", second)
	}
}

// A failing Claude spawn on a code kind never falls over to Gemini: the run
// fails (and the queue retries on Claude).
func TestRunRoute_CodingClaudeFailureNeverSpawnsGemini(t *testing.T) {
	pacer := &router.PacerState{Pools: map[router.PoolID]*router.QuotaPool{}}
	d := router.ResolveRoute("coding", false, pacer, "", time.Now())
	spawns, _, err := runRouteWithFake(t, d, pacer, "--print")
	if err == nil {
		t.Fatal("want an error when the only Claude slot fails")
	}
	for _, s := range spawns {
		if !isClaudeSpawn(s) {
			t.Fatalf("coding run spawned a non-Claude CLI: %v", spawns)
		}
	}
}

// The adapter re-checks quota at spawn time; a seat that locked between route
// resolution and spawn is skipped and the reason surfaces through the observer.
func TestRunRoute_SeatLockedAtSpawnTimeReportsReason(t *testing.T) {
	open := &router.PacerState{Pools: map[router.PoolID]*router.QuotaPool{}}
	d := router.ResolveRoute("coding", true, open, "", time.Now())
	nowLocked := &router.PacerState{Pools: map[router.PoolID]*router.QuotaPool{
		router.PoolWorkClaude: {IsLocked: true, LockoutReason: "5h limit"},
	}}
	spawns, log, err := runRouteWithFake(t, d, nowLocked, "")
	if err != nil {
		t.Fatalf("RunRoute: %v", err)
	}
	// STA-856: the fallback is the personal Claude seat, never Gemini.
	if len(spawns) != 1 || !isClaudeSpawn(spawns[0]) || !strings.HasPrefix(spawns[0], "|") {
		t.Fatalf("want only a personal claude spawn, got %v", spawns)
	}
	if len(log.attempts) != 1 || log.attempts[0].FallbackReason != "work seat locked (5h limit)" {
		t.Errorf("attempts = %+v", log.attempts)
	}
}

func TestRunRoute_AllLockedErrors(t *testing.T) {
	pacer := &router.PacerState{Pools: map[router.PoolID]*router.QuotaPool{
		router.PoolPersonalClaude: {IsLocked: true},
		router.PoolGeminiNative:   {IsLocked: true},
	}}
	d := router.ResolveRoute("coding", false, pacer, "", time.Now())
	bin, _ := spawnRecorder(t, "")
	ctx := context.WithValue(context.Background(), testBinKey, bin)
	var stdout, stderr bytes.Buffer
	if err := RunRoute(ctx, t.TempDir(), pacer, d, []string{"--prompt", "x"}, nil, &stdout, &stderr); err == nil {
		t.Fatal("want error when every slot is locked")
	}
}

func mustSlot(t *testing.T, d router.KindRoute) router.RouteSlot {
	t.Helper()
	s, ok := d.Chosen()
	if !ok {
		t.Fatal("no chosen slot")
	}
	return s
}
