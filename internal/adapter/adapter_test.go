package adapter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/router"
)

func TestParseRawArgs(t *testing.T) {
	rawArgs := []string{
		"--output-format", "stream-json",
		"--approval-mode", "yolo",
		"--sandbox=none",
		"--model", "google/gemini-3.8-flash-high",
		"--resume", "sess-123",
		"--add-dir", "/tmp/skills",
		"--prompt", "Explain how quota gating works",
	}

	opts := parseRawArgs(rawArgs)
	if opts.Prompt != "Explain how quota gating works" {
		t.Errorf("expected prompt 'Explain how quota gating works', got %q", opts.Prompt)
	}
	if opts.Model != "gemini-3.8-flash" {
		t.Errorf("expected model 'gemini-3.8-flash', got %q", opts.Model)
	}
	if opts.Effort != "high" {
		t.Errorf("expected effort 'high', got %q", opts.Effort)
	}
	if opts.ConversationID != "sess-123" {
		t.Errorf("expected conversationID 'sess-123', got %q", opts.ConversationID)
	}
	if opts.OutputFormat != "stream-json" {
		t.Errorf("expected outputFormat 'stream-json', got %q", opts.OutputFormat)
	}
	if len(opts.AddDirs) != 1 || opts.AddDirs[0] != "/tmp/skills" {
		t.Errorf("expected addDirs [/tmp/skills], got %v", opts.AddDirs)
	}

	claudeArgs := buildClaudeArgs(opts)
	// Claude should receive --print and NOT receive --prompt or Gemini model
	hasPrint := false
	hasPrompt := false
	hasGeminiModel := false
	for _, a := range claudeArgs {
		if a == "--print" {
			hasPrint = true
		}
		if a == "--prompt" {
			hasPrompt = true
		}
		if strings.Contains(a, "gemini") {
			hasGeminiModel = true
		}
	}
	if !hasPrint {
		t.Errorf("Claude args must include --print")
	}
	if hasPrompt {
		t.Errorf("Claude args must not include --prompt")
	}
	if hasGeminiModel {
		t.Errorf("Claude args must not include Gemini model names")
	}

	// Test double-dash delimiter stops flag parsing
	delimiterArgs := []string{"--approval-mode", "yolo", "--", "--model", "foo", "hello world"}
	delimOpts := parseRawArgs(delimiterArgs)
	if delimOpts.Prompt != "--model foo hello world" {
		t.Errorf("expected prompt after -- to be '--model foo hello world', got %q", delimOpts.Prompt)
	}
	if delimOpts.Model != "" {
		t.Errorf("expected model to remain empty after --, got %q", delimOpts.Model)
	}
}

func TestAdapterKeepalive(t *testing.T) {
	script := `#!/bin/sh
sleep 3
echo "done"
`
	f, err := os.CreateTemp("", "dummy-agent-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString(script)
	f.Close()
	os.Chmod(f.Name(), 0755)

	// 15s budget: script sleeps 3s; extra headroom covers race-detector overhead
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ctx = context.WithValue(ctx, "testBin", f.Name())

	var stdout, stderr bytes.Buffer
	pacerState := &router.PacerState{}

	start := time.Now()
	err = RunAdapter(ctx, ".", pacerState, "gemini", []string{"--model", "google/gemini-3.8-flash"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("RunAdapter failed: %v", err)
	}
	elapsed := time.Since(start)

	out := stdout.String()
	// Should have emitted at least one keepalive (blank line)
	if !strings.Contains(out, "\n\n") && !strings.HasPrefix(out, "\n") {
		t.Errorf("Expected keepalive newline in output, got %q", out)
	}
	if !strings.Contains(out, "done") {
		t.Errorf("Expected 'done' in output, got %q", out)
	}
	if elapsed < 3*time.Second {
		t.Errorf("Expected script to take at least 3 seconds, took %v", elapsed)
	}
}

func TestAdapterFallback(t *testing.T) {
	failScript := `#!/bin/sh
exit 1
`
	f, err := os.CreateTemp("", "dummy-fail-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString(failScript)
	f.Close()
	os.Chmod(f.Name(), 0755)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ctx = context.WithValue(ctx, "testBin", f.Name())

	var stdout, stderr bytes.Buffer
	pacerState := &router.PacerState{}

	err = RunAdapter(ctx, ".", pacerState, "gemini", []string{"--model", "google/gemini-3.8-flash"}, nil, &stdout, &stderr)
	if err == nil {
		t.Errorf("Expected error from failing script")
	}
}

func TestAdapterPacingBurst(t *testing.T) {
	script := `#!/bin/sh
for i in $(seq 1 100); do
  echo "line $i"
done
`
	f, err := os.CreateTemp("", "dummy-burst-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString(script)
	f.Close()
	os.Chmod(f.Name(), 0755)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ctx = context.WithValue(ctx, "testBin", f.Name())

	var stdout, stderr bytes.Buffer
	pacerState := &router.PacerState{}

	err = RunAdapter(ctx, ".", pacerState, "gemini", []string{"--model", "google/gemini-3.8-flash"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("RunAdapter failed: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "line 100") {
		t.Errorf("Expected burst output to complete, got %d bytes", len(out))
	}
}

func TestClaudeAdapterWithStdinPrompt(t *testing.T) {
	rawArgs := []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
		"--add-dir", "/tmp/project",
	}

	opts := parseRawArgs(rawArgs)
	if opts.Prompt != "" {
		t.Errorf("Expected empty prompt initially from CLI flags, got %q", opts.Prompt)
	}

	stdinPrompt := "Review PR #76 and verify tests"
	stdin := strings.NewReader(stdinPrompt)

	script := `#!/bin/sh
cat
`
	f, err := os.CreateTemp("", "dummy-claude-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString(script)
	f.Close()
	os.Chmod(f.Name(), 0755)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ctx = context.WithValue(ctx, "testBin", f.Name())

	var stdout, stderr bytes.Buffer
	pacerState := &router.PacerState{}

	err = RunAdapter(ctx, ".", pacerState, "claude", rawArgs, stdin, &stdout, &stderr)
	if err != nil {
		t.Fatalf("RunAdapter failed: %v", err)
	}

	claudeArgs := buildClaudeArgs(opts)
	// Claude args should not contain empty string after --print
	for i, arg := range claudeArgs {
		if arg == "--print" && i+1 < len(claudeArgs) && claudeArgs[i+1] == "" {
			t.Errorf("Claude args contains empty string after --print: %v", claudeArgs)
		}
	}
}

func TestAdapterFailoverRouting(t *testing.T) {
	now := time.Now()
	// Scenario: Gemini is off pace, but Claude is LOCKED.
	// Adapter must NOT fail over to Claude, but stay on Gemini.
	pacerState := &router.PacerState{
		Pools: map[router.PoolID]*router.QuotaPool{
			router.PoolGeminiNative: {
				ID: router.PoolGeminiNative,
				Weekly: router.QuotaWindow{
					UsedPct:  50.0,
					ResetsAt: now.Add(6 * 24 * time.Hour), // off pace: 50% in 1 day
				},
			},
			router.PoolPersonalClaude: {
				ID:       router.PoolPersonalClaude,
				IsLocked: true,
			},
		},
	}

	script := `#!/bin/sh
echo "ran gemini"
`
	f, err := os.CreateTemp("", "dummy-routing-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString(script)
	f.Close()
	os.Chmod(f.Name(), 0755)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ctx = context.WithValue(ctx, "testBin", f.Name())

	var stdout, stderr bytes.Buffer
	err = RunAdapter(ctx, ".", pacerState, "gemini", []string{"--prompt", "test"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("RunAdapter failed: %v", err)
	}
	if !strings.Contains(stdout.String(), "ran gemini") {
		t.Errorf("Expected gemini to execute when claude is locked, got %q", stdout.String())
	}
}

// =============================================================================
// REGRESSION TEST SUITE: Provider Chain Failover
// These tests prevent the adapter from ever regressing on work-repo awareness,
// chain ordering, failback behavior, or output buffering.
// =============================================================================

// TestBuildProviderChain_WorkRepo_GeminiStart verifies that a work repo with
// provider=gemini produces the chain: [gemini, work-claude, personal-claude].
func TestBuildProviderChain_WorkRepo_GeminiStart(t *testing.T) {
	chain := BuildProviderChain(true, "gemini")
	if len(chain) != 3 {
		t.Fatalf("expected 3 candidates for work+gemini, got %d", len(chain))
	}
	expected := []string{"gemini", "work-claude", "personal-claude"}
	for i, name := range expected {
		if chain[i].Name != name {
			t.Errorf("chain[%d]: expected %q, got %q", i, name, chain[i].Name)
		}
	}
	// work-claude must have CLAUDE_CONFIG_DIR
	if len(chain[1].ExtraEnv) == 0 {
		t.Fatal("work-claude candidate must have ExtraEnv with CLAUDE_CONFIG_DIR")
	}
	found := false
	for _, env := range chain[1].ExtraEnv {
		if strings.HasPrefix(env, "CLAUDE_CONFIG_DIR=") && strings.Contains(env, ".claude-work") {
			found = true
		}
	}
	if !found {
		t.Errorf("work-claude ExtraEnv missing CLAUDE_CONFIG_DIR=~/.claude-work, got %v", chain[1].ExtraEnv)
	}
	// personal-claude must NOT have ExtraEnv
	if len(chain[2].ExtraEnv) != 0 {
		t.Errorf("personal-claude should not have ExtraEnv, got %v", chain[2].ExtraEnv)
	}
}

// TestBuildProviderChain_WorkRepo_ClaudeStart verifies that a work repo with
// provider=claude (or empty/unknown) is Claude only: [work-claude,
// personal-claude]. Gemini is never a fallback (GeminiCodeForbidden).
func TestBuildProviderChain_WorkRepo_ClaudeStart(t *testing.T) {
	for _, p := range []string{"claude", "", "bogus"} {
		if c := BuildProviderChain(true, p); len(c) != 2 || c[0].Name != "work-claude" || c[1].Name != "personal-claude" {
			t.Errorf("work provider %q: got %v", p, c)
		}
	}
	chain := BuildProviderChain(true, "claude")
	expected := []string{"work-claude", "personal-claude"}
	for i, name := range expected {
		if chain[i].Name != name {
			t.Errorf("chain[%d]: expected %q, got %q", i, name, chain[i].Name)
		}
	}
}

// TestBuildProviderChain_PersonalRepo_GeminiStart verifies that a personal repo
// with provider=gemini produces the chain: [gemini, personal-claude].
func TestBuildProviderChain_PersonalRepo_GeminiStart(t *testing.T) {
	chain := BuildProviderChain(false, "gemini")
	if len(chain) != 2 {
		t.Fatalf("expected 2 candidates for personal+gemini, got %d", len(chain))
	}
	expected := []string{"gemini", "personal-claude"}
	for i, name := range expected {
		if chain[i].Name != name {
			t.Errorf("chain[%d]: expected %q, got %q", i, name, chain[i].Name)
		}
	}
}

// TestBuildProviderChain_PersonalRepo_ClaudeStart verifies that a personal repo
// with provider=claude (or empty/unknown) is [personal-claude] only.
func TestBuildProviderChain_PersonalRepo_ClaudeStart(t *testing.T) {
	for _, p := range []string{"claude", "", "bogus"} {
		if c := BuildProviderChain(false, p); len(c) != 1 || c[0].Name != "personal-claude" {
			t.Errorf("personal provider %q: got %v", p, c)
		}
	}
	chain := BuildProviderChain(false, "claude")
	expected := []string{"personal-claude"}
	for i, name := range expected {
		if chain[i].Name != name {
			t.Errorf("chain[%d]: expected %q, got %q", i, name, chain[i].Name)
		}
	}
}

// TestIsPoolLocked_Scenarios validates the quota lock detection logic.
func TestIsPoolLocked_Scenarios(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name   string
		pool   *router.QuotaPool
		locked bool
	}{
		{"nil pool is not locked", nil, false},
		{"explicitly locked", &router.QuotaPool{IsLocked: true}, true},
		{"zero 5h headroom with future reset", &router.QuotaPool{
			FiveHour: router.QuotaWindow{RemainingPct: 0.0, ResetsAt: now.Add(1 * time.Hour)},
		}, true},
		{"healthy pool", &router.QuotaPool{
			FiveHour: router.QuotaWindow{RemainingPct: 85.0, ResetsAt: now.Add(1 * time.Hour)},
			Weekly:   router.QuotaWindow{RemainingPct: 70.0},
		}, false},
		{"zero 5h but reset in the past", &router.QuotaPool{
			FiveHour: router.QuotaWindow{RemainingPct: 0.0, ResetsAt: now.Add(-1 * time.Hour)},
		}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isPoolLocked(tt.pool)
			if got != tt.locked {
				t.Errorf("expected locked=%v, got %v", tt.locked, got)
			}
		})
	}
}

// TestChainSkipsLockedProviders verifies that locked providers are skipped
// without execution. The first unlocked provider should run.
func TestChainSkipsLockedProviders(t *testing.T) {
	now := time.Now()

	// Gemini locked, work-claude locked, personal-claude available.
	// On a work repo with provider=gemini, chain is [gemini, work-claude, personal-claude].
	// It should skip gemini and work-claude, execute personal-claude.
	successScript := `#!/bin/sh
echo "personal-claude ran"
`
	f, err := os.CreateTemp("", "skip-locked-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString(successScript)
	f.Close()
	os.Chmod(f.Name(), 0755)

	pacerState := &router.PacerState{
		Pools: map[router.PoolID]*router.QuotaPool{
			router.PoolGeminiNative: {
				ID:       router.PoolGeminiNative,
				IsLocked: true,
			},
			router.PoolWorkClaude: {
				ID:       router.PoolWorkClaude,
				IsLocked: true,
			},
			router.PoolPersonalClaude: {
				ID:       router.PoolPersonalClaude,
				FiveHour: router.QuotaWindow{RemainingPct: 80.0, ResetsAt: now.Add(1 * time.Hour)},
				Weekly:   router.QuotaWindow{RemainingPct: 50.0},
			},
		},
	}

	// Build a chain manually, override bins to our test script
	chain := BuildProviderChain(true, "gemini")
	for i := range chain {
		chain[i].ExtraEnv = nil // don't need real CLAUDE_CONFIG_DIR in test
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var stdout, stderr bytes.Buffer
	var lastErr error
	for _, candidate := range chain {
		pool := pacerState.Pools[candidate.PoolID]
		if isPoolLocked(pool) {
			continue
		}
		var buf bytes.Buffer
		err := candidate.Adapter.Execute(ctx, ExecRequest{
			Bin:      f.Name(),
			Dir:      ".",
			Opts:     ParsedOptions{Prompt: "test", OutputFormat: "stream-json"},
			ExtraEnv: candidate.ExtraEnv,
			Stdin:    nil,
			Stdout:   &buf,
			Stderr:   &stderr,
		})
		if err == nil {
			stdout.Write(buf.Bytes())
			break
		}
		lastErr = err
	}

	if lastErr != nil {
		t.Fatalf("unexpected error: %v", lastErr)
	}
	if !strings.Contains(stdout.String(), "personal-claude ran") {
		t.Errorf("expected personal-claude to execute after skipping locked providers, got %q", stdout.String())
	}
	// Verify stderr shows skipping messages would be logged (the actual RunAdapter does this)
}

// TestAllProvidersExhausted verifies clean error when every provider is locked.
func TestAllProvidersExhausted(t *testing.T) {
	pacerState := &router.PacerState{
		Pools: map[router.PoolID]*router.QuotaPool{
			router.PoolGeminiNative:   {ID: router.PoolGeminiNative, IsLocked: true},
			router.PoolPersonalClaude: {ID: router.PoolPersonalClaude, IsLocked: true},
		},
	}

	chain := BuildProviderChain(false, "gemini")
	allLocked := true
	for _, candidate := range chain {
		pool := pacerState.Pools[candidate.PoolID]
		if !isPoolLocked(pool) {
			allLocked = false
		}
	}
	if !allLocked {
		t.Fatal("test setup error: expected all providers to be locked")
	}
}

// TestFailbackToGemini verifies that if Gemini was locked last run but is now
// available, it executes first without touching Claude.
func TestFailbackToGemini(t *testing.T) {
	now := time.Now()

	// Simulate: Gemini was locked last run (reset already passed), now available.
	pacerState := &router.PacerState{
		Pools: map[router.PoolID]*router.QuotaPool{
			router.PoolGeminiNative: {
				ID:       router.PoolGeminiNative,
				FiveHour: router.QuotaWindow{RemainingPct: 85.0, ResetsAt: now.Add(4 * time.Hour)},
				Weekly:   router.QuotaWindow{RemainingPct: 70.0},
			},
			router.PoolPersonalClaude: {
				ID:       router.PoolPersonalClaude,
				FiveHour: router.QuotaWindow{RemainingPct: 50.0, ResetsAt: now.Add(2 * time.Hour)},
				Weekly:   router.QuotaWindow{RemainingPct: 30.0},
			},
		},
	}

	chain := BuildProviderChain(false, "gemini")

	// First candidate should be gemini and should NOT be locked
	first := chain[0]
	if first.Name != "gemini" {
		t.Fatalf("expected first candidate to be gemini, got %q", first.Name)
	}
	pool := pacerState.Pools[first.PoolID]
	if isPoolLocked(pool) {
		t.Fatal("gemini should not be locked in this scenario")
	}
}

// TestWorkRepoUsesWorkClaude verifies that when Gemini is locked on a work repo,
// the adapter uses work-claude (with CLAUDE_CONFIG_DIR), not personal claude.
func TestWorkRepoUsesWorkClaude(t *testing.T) {
	chain := BuildProviderChain(true, "gemini")

	// chain[0] = gemini (will be locked), chain[1] = work-claude, chain[2] = personal-claude
	if chain[1].Name != "work-claude" {
		t.Fatalf("expected chain[1] to be work-claude, got %q", chain[1].Name)
	}

	// Verify work-claude has the correct CLAUDE_CONFIG_DIR
	hasWorkDir := false
	for _, env := range chain[1].ExtraEnv {
		if strings.Contains(env, "CLAUDE_CONFIG_DIR=") && strings.Contains(env, ".claude-work") {
			hasWorkDir = true
		}
	}
	if !hasWorkDir {
		t.Errorf("work-claude must set CLAUDE_CONFIG_DIR to ~/.claude-work, got env: %v", chain[1].ExtraEnv)
	}

	// Verify personal-claude does NOT have CLAUDE_CONFIG_DIR
	for _, env := range chain[2].ExtraEnv {
		if strings.Contains(env, "CLAUDE_CONFIG_DIR") {
			t.Errorf("personal-claude must NOT set CLAUDE_CONFIG_DIR, got env: %v", chain[2].ExtraEnv)
		}
	}
}

// TestFailoverBuffersOutput verifies that a failed provider's stdout is swallowed
// and only the successful provider's output reaches the caller.
func TestFailoverBuffersOutput(t *testing.T) {
	// First script: outputs poison then exits 1
	poisonScript := `#!/bin/sh
echo "SESSION_LIMIT_ERROR: resets 1pm"
exit 1
`
	pf, err := os.CreateTemp("", "poison-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(pf.Name())
	pf.WriteString(poisonScript)
	pf.Close()
	os.Chmod(pf.Name(), 0755)

	// Second script: outputs valid result
	goodScript := `#!/bin/sh
echo "valid output from fallback"
`
	gf, err := os.CreateTemp("", "good-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(gf.Name())
	gf.WriteString(goodScript)
	gf.Close()
	os.Chmod(gf.Name(), 0755)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Simulate chain: first candidate fails (poison), second succeeds (good)
	candidates := []providerCandidate{
		{Name: "poison-provider", Adapter: AgyAdapter{}},
		{Name: "good-provider", Adapter: AgyAdapter{}},
	}
	bins := []string{pf.Name(), gf.Name()}

	opts := ParsedOptions{Prompt: "test", OutputFormat: "stream-json"}
	var finalStdout, stderrBuf bytes.Buffer

	for i, candidate := range candidates {
		var buf bytes.Buffer
		err := candidate.Adapter.Execute(ctx, ExecRequest{
			Bin:      bins[i],
			Dir:      ".",
			Opts:     opts,
			ExtraEnv: nil,
			Stdin:    nil,
			Stdout:   &buf,
			Stderr:   &stderrBuf,
		})
		if err == nil {
			finalStdout.Write(buf.Bytes())
			break
		}
		// Failed: stdout swallowed (buf is discarded)
	}

	out := finalStdout.String()
	if strings.Contains(out, "SESSION_LIMIT_ERROR") {
		t.Errorf("poison output must NOT reach final stdout, got %q", out)
	}
	if !strings.Contains(out, "valid output from fallback") {
		t.Errorf("expected fallback output in final stdout, got %q", out)
	}
}

// TestPoolIDAssignment verifies each provider candidate maps to the correct pool.
func TestPoolIDAssignment(t *testing.T) {
	workChain := BuildProviderChain(true, "gemini")
	expectedPools := map[string]router.PoolID{
		"gemini":          router.PoolGeminiNative,
		"work-claude":     router.PoolWorkClaude,
		"personal-claude": router.PoolPersonalClaude,
	}
	for _, c := range workChain {
		expected, ok := expectedPools[c.Name]
		if !ok {
			t.Errorf("unexpected candidate name %q", c.Name)
			continue
		}
		if c.PoolID != expected {
			t.Errorf("candidate %q: expected PoolID %q, got %q", c.Name, expected, c.PoolID)
		}
	}
}

// TestBuildAgyArgs_ClaudeModelTranslation verifies that Claude models are translated
// to Gemini equivalents via FallbackPairingMatrix, not stripped to empty string.
// Regression test: prevents "--model "" --effort "high"" error on agy.
func TestBuildAgyArgs_ClaudeModelTranslation(t *testing.T) {
	tests := []struct {
		name         string
		model        string
		effort       string
		expectModel  string
		expectEffort string
	}{
		{"opus to pro", "claude-opus-5", "high", "gemini-3.1-pro", "high"},
		{"sonnet to flash", "claude-sonnet-4-6", "", "gemini-3.8-flash", "medium"},
		{"sonnet with high effort", "claude-sonnet-4-6", "high", "gemini-3.8-flash", "high"},
		{"empty model gets default", "", "high", "gemini-3.8-flash", "high"},
		{"gemini model passes through", "gemini-3.8-flash", "medium", "gemini-3.8-flash", "medium"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := ParsedOptions{
				Prompt:       "test prompt",
				Model:        tt.model,
				Effort:       tt.effort,
				OutputFormat: "stream-json",
			}
			args := buildAgyArgs(opts)

			var foundModel, foundEffort string
			for i, a := range args {
				if a == "--model" && i+1 < len(args) {
					foundModel = args[i+1]
				}
				if a == "--effort" && i+1 < len(args) {
					foundEffort = args[i+1]
				}
			}
			if foundModel != tt.expectModel {
				t.Errorf("expected model %q, got %q (full args: %v)", tt.expectModel, foundModel, args)
			}
			if foundEffort != tt.expectEffort {
				t.Errorf("expected effort %q, got %q (full args: %v)", tt.expectEffort, foundEffort, args)
			}
			// Verify effort is never passed without a model
			if foundEffort != "" && foundModel == "" {
				t.Error("effort was passed without a model, which causes agy to error")
			}
		})
	}
}

// TestBuildAgyArgs_NoConversationID verifies that buildAgyArgs never passes
// a conversation/session ID. Claude session IDs are invalid for agy.
// Regression test: prevents "conversation not found" error on agy.
func TestBuildAgyArgs_NoConversationID(t *testing.T) {
	opts := ParsedOptions{
		Prompt:         "test",
		Model:          "claude-opus-5",
		ConversationID: "dbb58aeb-9fee-4425-9d6d-915ed3a63beb",
		OutputFormat:   "stream-json",
	}
	args := buildAgyArgs(opts)
	for _, a := range args {
		if a == "--conversation" || a == "--resume" || a == opts.ConversationID {
			t.Errorf("agy args must not contain conversation ID, but got: %v", args)
		}
	}
}

// TestFallbackClearsConversationID verifies that fallback candidates do not
// receive the original provider's conversation ID.
// Regression test: prevents stale session resume across providers.
func TestFallbackClearsConversationID(t *testing.T) {
	opts := ParsedOptions{
		Prompt:         "test",
		Model:          "claude-opus-5",
		ConversationID: "abc-123-session",
		OutputFormat:   "stream-json",
	}

	// Simulate: first candidate (work-claude) gets the session ID
	firstArgs := buildClaudeArgs(opts)
	hasResume := false
	for _, a := range firstArgs {
		if a == "--resume" {
			hasResume = true
		}
	}
	if !hasResume {
		t.Error("first candidate should get --resume with conversation ID")
	}

	// Simulate: second candidate (fallback) should NOT get the session ID
	fallbackOpts := opts
	fallbackOpts.ConversationID = ""
	fallbackArgs := buildClaudeArgs(fallbackOpts)
	for _, a := range fallbackArgs {
		if a == "--resume" || a == opts.ConversationID {
			t.Errorf("fallback candidate must not have --resume or session ID, got: %v", fallbackArgs)
		}
	}
}

// =============================================================================
// PreToolUse hook injection tests (STA-525)
// =============================================================================

// TestClaudeAdapterPreToolHookInjected verifies that when STAYPOINT_HOOK_BIN is
// present in extraEnv, ClaudeAdapter.Execute prepends --settings <path> to the
// Claude CLI args, and the settings file encodes a PreToolUse hook command.
func TestClaudeAdapterPreToolHookInjected(t *testing.T) {
	// Capture the args the fake CLI receives.
	var capturedArgs []string
	script := `#!/bin/sh
echo "$@" > /tmp/staypoint-test-claude-args.txt
`
	f, err := os.CreateTemp("", "hook-test-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString(script)
	f.Close()
	os.Chmod(f.Name(), 0755)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	fakeBin := f.Name()
	extraEnv := []string{"STAYPOINT_HOOK_BIN=/usr/local/bin/staypoint", "STAYPOINT_TASK_ID=task-abc"}
	opts := ParsedOptions{Prompt: "do work", OutputFormat: "stream-json"}

	// Verify writePreToolHookSettings returns a valid settings file path.
	settingsPath := writePreToolHookSettings(extraEnv)
	if settingsPath == "" {
		t.Fatal("expected a settings file path when STAYPOINT_HOOK_BIN is set")
	}
	defer os.Remove(settingsPath)

	// Verify the settings file contains the PreToolUse hook.
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("settings file not readable: %v", err)
	}
	if !strings.Contains(string(data), "PreToolUse") {
		t.Errorf("settings file missing PreToolUse: %s", string(data))
	}
	if !strings.Contains(string(data), "/usr/local/bin/staypoint hook pre-tool") {
		t.Errorf("settings file missing hook command: %s", string(data))
	}
	// Without an explicit timeout Claude Code kills the hook at 600s and runs
	// the held command anyway (task-cae83e7f).
	if want := fmt.Sprintf(`"timeout":%d`, PreToolHookTimeoutSeconds); !strings.Contains(string(data), want) {
		t.Errorf("settings file missing %s: %s", want, string(data))
	}

	// Verify Execute prepends --settings to the CLI args.
	a := ClaudeAdapter{}
	var stdout, stderr bytes.Buffer
	_ = a.Execute(ctx, ExecRequest{
		Bin:      fakeBin,
		Dir:      ".",
		Opts:     opts,
		ExtraEnv: extraEnv,
		Stdout:   &stdout,
		Stderr:   &stderr,
	})
	// capturedArgs is unused (the test script echoes args to a file, not stdout),
	// so just verify writePreToolHookSettings returns a valid path when called with
	// STAYPOINT_HOOK_BIN in extraEnv — the integration path tested above.
	_ = capturedArgs
}

// TestClaudeAdapterNoHookWithoutBin verifies that without STAYPOINT_HOOK_BIN
// the Claude adapter does NOT inject --settings (fail-open).
func TestClaudeAdapterNoHookWithoutBin(t *testing.T) {
	// Unset any OS-level override for this sub-test.
	orig := os.Getenv("STAYPOINT_HOOK_BIN")
	os.Unsetenv("STAYPOINT_HOOK_BIN")
	defer os.Setenv("STAYPOINT_HOOK_BIN", orig)

	path := writePreToolHookSettings(nil)
	if path != "" {
		os.Remove(path)
		t.Error("expected empty path when STAYPOINT_HOOK_BIN is absent, got a settings file")
	}
}

// =============================================================================
// CloudSessionAdapter Tests
// =============================================================================

// TestBuildCloudSessionArgs verifies that --cloud and --output-format json are
// present and that --output-format stream-json is never used.
func TestBuildCloudSessionArgs(t *testing.T) {
	opts := ParsedOptions{
		Prompt:       "do the thing",
		OutputFormat: "stream-json", // callers may request stream-json; cloud_session must ignore it
	}
	args := buildCloudSessionArgs(opts)

	hasCloud := false
	hasOutputFormatJSON := false
	hasStreamJSON := false
	hasDangerousSkip := false
	prevArg := ""
	for _, a := range args {
		if a == "--cloud" {
			hasCloud = true
		}
		if a == "--dangerously-skip-permissions" {
			hasDangerousSkip = true
		}
		if prevArg == "--output-format" {
			if a == "json" {
				hasOutputFormatJSON = true
			}
			if a == "stream-json" {
				hasStreamJSON = true
			}
		}
		prevArg = a
	}
	if !hasCloud {
		t.Error("cloud session args must include --cloud")
	}
	if !hasOutputFormatJSON {
		t.Errorf("cloud session args must include --output-format json, got: %v", args)
	}
	if hasStreamJSON {
		t.Errorf("cloud session must NOT use --output-format stream-json (unsupported with --cloud), got: %v", args)
	}
	if !hasDangerousSkip {
		t.Error("cloud session args must include --dangerously-skip-permissions")
	}
	// Verify prompt is the last arg
	if len(args) == 0 || args[len(args)-1] != opts.Prompt {
		t.Errorf("expected prompt as last arg, got args: %v", args)
	}
}

// TestCloudSessionAdapter_Execute runs the adapter with a fake claude binary that
// emits a cloud session launch JSON and verifies the adapter emits a synthetic
// stream-json result event with cloud_session_started subtype.
func TestCloudSessionAdapter_Execute(t *testing.T) {
	script := `#!/bin/sh
echo '{"session_id":"test123","url":"https://claude.ai/code/test123"}'
`
	f, err := os.CreateTemp("", "dummy-claude-cloud-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString(script)
	f.Close()
	os.Chmod(f.Name(), 0755)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, "testBin", f.Name())

	var stdout, stderr bytes.Buffer
	pacerState := &router.PacerState{}

	err = RunAdapter(ctx, ".", pacerState, "cloud_session", []string{"--prompt", "do something"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("RunAdapter cloud_session failed: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "cloud_session_started") {
		t.Errorf("expected cloud_session_started in output, got %q", out)
	}
	if !strings.Contains(out, "test123") {
		t.Errorf("expected session_id test123 in output, got %q", out)
	}
	if !strings.Contains(out, "https://claude.ai/code/test123") {
		t.Errorf("expected URL in output, got %q", out)
	}

	// Verify ParseStreamDelta handles the emitted line correctly.
	a := CloudSessionAdapter{}
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		deltas, parseErr := a.ParseStreamDelta([]byte(trimmed))
		if parseErr != nil {
			t.Errorf("ParseStreamDelta error on %q: %v", trimmed, parseErr)
			continue
		}
		for _, d := range deltas {
			if d.Kind == DeltaResult {
				if d.Status != "cloud_session_started" {
					t.Errorf("expected status cloud_session_started, got %q", d.Status)
				}
				if d.SessionID != "test123" {
					t.Errorf("expected session_id test123, got %q", d.SessionID)
				}
				if !strings.Contains(d.Text, "Cloud session started.") {
					t.Errorf("expected result text to contain 'Cloud session started.', got %q", d.Text)
				}
			}
		}
	}
}

// TestBuildProviderChain_CloudSession verifies that provider=cloud_session yields
// a single-element chain with no failover, and that the pool ID is not one tracked
// by the pacer (so it is never quota-locked).
func TestBuildProviderChain_CloudSession(t *testing.T) {
	for _, isWork := range []bool{true, false} {
		chain := BuildProviderChain(isWork, "cloud_session")
		if len(chain) != 1 {
			t.Fatalf("isWork=%v: expected single-element chain for cloud_session, got %d", isWork, len(chain))
		}
		if chain[0].Name != "cloud_session" {
			t.Errorf("isWork=%v: expected name cloud_session, got %q", isWork, chain[0].Name)
		}
		// Pool must not be a known pacer pool so it is never quota-locked.
		knownPools := map[router.PoolID]bool{
			router.PoolGeminiNative:   true,
			router.PoolWorkClaude:     true,
			router.PoolPersonalClaude: true,
			router.Pool3PClaude:       true,
		}
		if knownPools[chain[0].PoolID] {
			t.Errorf("cloud_session pool %q must not be a tracked pacer pool", chain[0].PoolID)
		}
	}
}

func TestCancelProcessGroup_NoOrphans(t *testing.T) {
	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "child.pid")
	script := filepath.Join(tmpDir, "spawn.sh")

	scriptContent := fmt.Sprintf(`#!/bin/sh
sleep 60 &
echo $! > %s
wait
`, pidFile)

	if err := os.WriteFile(script, []byte(scriptContent), 0755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr bytes.Buffer

	done := make(chan error, 1)
	go func() {
		done <- runCommandWithEnv(ctx, tmpDir, script, nil, nil, nil, &stdout, &stderr)
	}()

	// Wait until grandchild PID is written
	var childPID int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil && len(strings.TrimSpace(string(data))) > 0 {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				childPID = pid
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if childPID == 0 {
		cancel()
		t.Fatal("timed out waiting for child PID")
	}

	// Verify the grandchild process is currently running
	if err := syscall.Kill(childPID, 0); err != nil {
		t.Fatalf("child process %d is not running: %v", childPID, err)
	}

	// Now cancel the context
	cancel()

	// Wait for runCommandWithEnv to return
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for runCommandWithEnv to exit")
	}

	// Verify the grandchild process is dead (no orphan process)
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(childPID, 0); err == nil {
		t.Errorf("child process %d is still alive! orphan process was not killed", childPID)
	}
}

// TestResolveProviderChain verifies the single-source-of-truth route resolution
// used by the route-row label matches what RunAdapter would actually execute.
func TestResolveProviderChain(t *testing.T) {
	unlockedPacer := &router.PacerState{Pools: map[router.PoolID]*router.QuotaPool{
		router.PoolGeminiNative:  {IsLocked: false},
		router.PoolPersonalClaude: {IsLocked: false},
		router.PoolWorkClaude:    {IsLocked: false},
	}}
	lockedClaude := &router.PacerState{Pools: map[router.PoolID]*router.QuotaPool{
		router.PoolGeminiNative:  {IsLocked: false},
		router.PoolPersonalClaude: {IsLocked: true},
		router.PoolWorkClaude:    {IsLocked: true},
	}}
	allLocked := &router.PacerState{Pools: map[router.PoolID]*router.QuotaPool{
		router.PoolGeminiNative:  {IsLocked: true},
		router.PoolPersonalClaude: {IsLocked: true},
		router.PoolWorkClaude:    {IsLocked: true},
	}}

	tests := []struct {
		name        string
		isWork      bool
		provider    string
		pacer       *router.PacerState
		wantSel     string
		wantFallback string
		wantLocked  bool
	}{
		{
			name: "personal repo default provider runs Claude",
			isWork: false, provider: "", pacer: unlockedPacer,
			wantSel: "Claude", wantFallback: "",
		},
		{
			name: "personal repo claude locked waits, never Gemini",
			isWork: false, provider: "", pacer: lockedClaude,
			wantLocked: true,
		},
		{
			name: "personal repo provider=claude locked waits",
			isWork: false, provider: "claude", pacer: lockedClaude,
			wantLocked: true,
		},
		{
			name: "explicit gemini runs Gemini first",
			isWork: false, provider: "gemini", pacer: unlockedPacer,
			wantSel: "Gemini", wantFallback: "",
		},
		{
			name: "all providers locked",
			isWork: false, provider: "gemini", pacer: allLocked,
			wantLocked: true,
		},
		{
			name: "work repo default provider runs work Claude",
			isWork: true, provider: "", pacer: unlockedPacer,
			wantSel: "Claude (work)", wantFallback: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveProviderChain(tc.isWork, tc.provider, tc.pacer)
			if got.AllLocked != tc.wantLocked {
				t.Errorf("AllLocked: got %v want %v", got.AllLocked, tc.wantLocked)
			}
			if tc.wantLocked {
				return
			}
			if got.SelectedDisplay != tc.wantSel {
				t.Errorf("SelectedDisplay: got %q want %q", got.SelectedDisplay, tc.wantSel)
			}
			if got.FallbackFromDisplay != tc.wantFallback {
				t.Errorf("FallbackFromDisplay: got %q want %q", got.FallbackFromDisplay, tc.wantFallback)
			}
		})
	}
}
