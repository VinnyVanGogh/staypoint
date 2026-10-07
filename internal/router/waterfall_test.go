package router

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIsWorkRepoDetection(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("unexpected error getting home dir: %v", err)
	}

	testCases := []struct {
		path     string
		expected bool
	}{
		// The shared worktrees folder holds personal and work worktrees, so its
		// name alone says nothing; a worktree there is work only by git remote.
		{filepath.Join(home, "Documents/dev/worktrees/feat-auth"), false},
		{filepath.Join(home, "Documents/dev/worktrees/staypointd-main"), false},
		{filepath.Join(home, "Documents/dev/work"), true},
		{filepath.Join(home, "Documents/dev/work/client-x"), true},
		{filepath.Join(home, "Documents/dev/work-repos/client-x"), true},
		{filepath.Join(home, "Documents/dev/workbench"), false},
		{filepath.Join(home, "Documents/dev/mansol-apps-server/github_repo-prod"), true},
		{filepath.Join(home, "Documents/dev/mansol/vps-hr-automation"), true},
		{filepath.Join(home, "Documents/dev/managed-solution-dashboard"), true},
		{filepath.Join(home, "Documents/dev/personal-game"), false},
		{filepath.Join(home, "Documents/dev/bassline"), false},
	}

	for _, tc := range testCases {
		isWork, src, err := IsWorkRepo(tc.path)
		if err != nil {
			t.Fatalf("unexpected error for %s: %v", tc.path, err)
		}
		if isWork != tc.expected {
			t.Errorf("path %s: expected isWork=%v, got %v (source: %s)", tc.path, tc.expected, isWork, src)
		}
	}

	// Test git config with branch name containing mansol does not trigger work repo
	tmpRepo := t.TempDir()
	gitDir := filepath.Join(tmpRepo, ".git")
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		t.Fatal(err)
	}
	cfgContent := `[core]
	repositoryformatversion = 0
[remote "origin"]
	url = https://github.com/VinnyVanGogh/personal-tool.git
[branch "feature/scrub-mansol-mbp"]
	remote = origin
	merge = refs/heads/feature/scrub-mansol-mbp
`
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(cfgContent), 0644); err != nil {
		t.Fatal(err)
	}
	isWork, src, err := IsWorkRepo(tmpRepo)
	if err != nil {
		t.Fatal(err)
	}
	if isWork {
		t.Errorf("expected personal repo with mansol in branch name not to be work repo, but got isWork=true (source: %s)", src)
	}
}

func TestBalancedPeerPacingRouting(t *testing.T) {
	ctx := context.Background()

	// Base pacer state with healthy pools
	basePacer := &PacerState{
		Pools: map[PoolID]*QuotaPool{
			PoolGeminiNative: {
				TurnsRunway: 100,
				FiveHour:    QuotaWindow{RemainingPct: 80.0},
				Weekly:      QuotaWindow{RemainingPct: 40.0}, // 40% left
			},
			PoolPersonalClaude: {
				TurnsRunway: 80,
				FiveHour:    QuotaWindow{RemainingPct: 100.0},
				Weekly:      QuotaWindow{RemainingPct: 90.0}, // 90% left
			},
			Pool3PClaude: {
				TurnsRunway: 50,
				FiveHour:    QuotaWindow{RemainingPct: 100.0},
				Weekly:      QuotaWindow{RemainingPct: 100.0},
			},
		},
	}

	// Case 1: Claude has significantly more weekly headroom (90% vs 40%) -> favors Claude
	dec1, err := Route(ctx, "/Users/vincevasile/Documents/dev/personal-app", basePacer, RouteOptions{
		CheckSSH: false,
	})
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	if dec1.Tool != "claude" {
		t.Errorf("expected tool claude for higher headroom, got %s (reason: %s)", dec1.Tool, dec1.Reason)
	}

	// Case 2: Repo continuity within 20% margin: LastUsedTool is agy sticks with agy even if Claude has slightly more
	continuityPacer := &PacerState{
		Pools: map[PoolID]*QuotaPool{
			PoolGeminiNative: {
				TurnsRunway: 80,
				FiveHour:    QuotaWindow{RemainingPct: 80.0},
				Weekly:      QuotaWindow{RemainingPct: 75.0},
			},
			PoolPersonalClaude: {
				TurnsRunway: 80,
				FiveHour:    QuotaWindow{RemainingPct: 90.0},
				Weekly:      QuotaWindow{RemainingPct: 85.0}, // 10% difference, within 20% margin
			},
		},
	}
	dec2, err := Route(ctx, "/Users/vincevasile/Documents/dev/personal-app", continuityPacer, RouteOptions{
		CheckSSH:     false,
		LastUsedTool: "agy",
	})
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	if dec2.Tool != "agy" {
		t.Errorf("expected tool agy for repo continuity within 20%% margin, got %s", dec2.Tool)
	}

	// Case 2b: Repo continuity overridden when tool is >20% starved compared to peer
	dec2b, err := Route(ctx, "/Users/vincevasile/Documents/dev/personal-app", basePacer, RouteOptions{
		CheckSSH:     false,
		LastUsedTool: "agy", // Gemini is at 40% while Claude is at 90% (50% gap)
	})
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	if dec2b.Tool != "claude" {
		t.Errorf("expected tool claude when continuity tool is starved by >20%% margin, got %s", dec2b.Tool)
	}

	// Case 3: User explicit preference: PreferredPersonalTool is claude
	dec3, err := Route(ctx, "/Users/vincevasile/Documents/dev/personal-app", basePacer, RouteOptions{
		CheckSSH:              false,
		PreferredPersonalTool: "claude",
		LastUsedTool:          "agy",
	})
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	if dec3.Tool != "claude" {
		t.Errorf("expected tool claude for user preference, got %s", dec3.Tool)
	}

	// Case 4: Gemini Native is locked -> routes to Claude Code
	lockedGeminiPacer := &PacerState{
		Pools: map[PoolID]*QuotaPool{
			PoolGeminiNative: {
				IsLocked:      true,
				LockoutReason: "5h rate limit reached",
				LockoutUntil:  time.Now().Add(1 * time.Hour),
				FiveHour:      QuotaWindow{RemainingPct: 0.0},
				Weekly:        QuotaWindow{RemainingPct: 50.0},
			},
			PoolPersonalClaude: {
				TurnsRunway: 60,
				FiveHour:    QuotaWindow{RemainingPct: 90.0},
				Weekly:      QuotaWindow{RemainingPct: 70.0},
			},
		},
	}
	dec4, err := Route(ctx, "/Users/vincevasile/Documents/dev/personal-app", lockedGeminiPacer, RouteOptions{
		CheckSSH: false,
	})
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	if dec4.Tool != "claude" {
		t.Errorf("expected tool claude when Gemini locked, got %s", dec4.Tool)
	}
}

func TestDynamicQuotaAwareFallback(t *testing.T) {
	ctx := context.Background()

	// 1. When Claude (Work) is locked out -> Opus tier falls back to Gemini 3.1 Pro
	lockedWorkPacer := &PacerState{
		Pools: map[PoolID]*QuotaPool{
			PoolWorkClaude: {
				IsLocked:      true,
				LockoutUntil:  time.Now().Add(2 * time.Hour),
				LockoutReason: "429 rate limit exceeded",
				FiveHour:      QuotaWindow{RemainingPct: 0.0},
			},
			PoolGeminiNative: {
				TurnsRunway: 100,
				FiveHour:    QuotaWindow{RemainingPct: 80.0},
				Weekly:      QuotaWindow{RemainingPct: 60.0},
			},
		},
	}
	decWork, err := Route(ctx, "/Users/vincevasile/Documents/dev/mansol-apps-server/github_repo-prod", lockedWorkPacer, RouteOptions{})
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	if decWork.Tool != "agy" || decWork.Model != "gemini-3.1-pro" {
		t.Errorf("expected agy / gemini-3.1-pro for locked work Opus tier, got tool=%s, model=%s", decWork.Tool, decWork.Model)
	}

	// 2. Personal repo: Preferred Claude (Opus) locked -> falls back to Gemini 3.1 Pro
	lockedPersonalPacer := &PacerState{
		Pools: map[PoolID]*QuotaPool{
			PoolPersonalClaude: {
				IsLocked:      true,
				LockoutUntil:  time.Now().Add(2 * time.Hour),
				LockoutReason: "Session quota exhausted (resets 10:40pm)",
				FiveHour:      QuotaWindow{RemainingPct: 0.0},
			},
			PoolGeminiNative: {
				TurnsRunway: 90,
				FiveHour:    QuotaWindow{RemainingPct: 90.0},
				Weekly:      QuotaWindow{RemainingPct: 70.0},
			},
		},
	}
	decOpus, err := Route(ctx, "/Users/vincevasile/Documents/dev/personal-app", lockedPersonalPacer, RouteOptions{
		PreferredPersonalTool: "claude",
		PreferredModel:        "claude-opus-5",
	})
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	if decOpus.Tool != "agy" || decOpus.Model != "gemini-3.1-pro" {
		t.Errorf("expected agy / gemini-3.1-pro for locked Claude Opus, got tool=%s, model=%s", decOpus.Tool, decOpus.Model)
	}

	// 3. Personal repo: Preferred Claude (Sonnet) locked -> falls back to Gemini 3.8 Flash
	decSonnet, err := Route(ctx, "/Users/vincevasile/Documents/dev/personal-app", lockedPersonalPacer, RouteOptions{
		PreferredPersonalTool: "claude",
		PreferredModel:        "claude-sonnet-4-6",
		PreferredEffort:       "high",
	})
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	if decSonnet.Tool != "agy" || decSonnet.Model != "gemini-3.8-flash" {
		t.Errorf("expected agy / gemini-3.8-flash for locked Claude Sonnet, got tool=%s, model=%s", decSonnet.Tool, decSonnet.Model)
	}

	// 4. When Claude lock resets -> routes back to primary Claude model
	unlockedPacer := &PacerState{
		Pools: map[PoolID]*QuotaPool{
			PoolPersonalClaude: {
				IsLocked:    false,
				TurnsRunway: 80,
				FiveHour:    QuotaWindow{RemainingPct: 95.0},
				Weekly:      QuotaWindow{RemainingPct: 80.0},
			},
			PoolGeminiNative: {
				TurnsRunway: 90,
				FiveHour:    QuotaWindow{RemainingPct: 90.0},
				Weekly:      QuotaWindow{RemainingPct: 70.0},
			},
		},
	}
	decReset, err := Route(ctx, "/Users/vincevasile/Documents/dev/personal-app", unlockedPacer, RouteOptions{
		PreferredPersonalTool: "claude",
		PreferredModel:        "claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	if decReset.Tool != "claude" || decReset.Model != "claude-sonnet-4-6" {
		t.Errorf("expected route to return to primary Claude model on reset, got tool=%s, model=%s", decReset.Tool, decReset.Model)
	}

	// 5. Work repo with Claude Work NOT locked -> routes to Claude Code (work seat / SSH)
	unlockedWorkPacer := &PacerState{
		Pools: map[PoolID]*QuotaPool{
			PoolWorkClaude: {
				IsLocked: false,
			},
		},
	}
	decWorkUnchecked, err := Route(ctx, "/Users/vincevasile/Documents/dev/mansol-apps-server/github_repo-prod", unlockedWorkPacer, RouteOptions{
		CheckSSH: false,
	})
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	if decWorkUnchecked.Tool != "claude" || decWorkUnchecked.Target != TargetLocalClaudeWork {
		t.Errorf("expected local claude work seat when ssh not checked, got %s / %s", decWorkUnchecked.Tool, decWorkUnchecked.Target)
	}
}

func TestFallbackPairingMatrix(t *testing.T) {
	testCases := []struct {
		claudeModel    string
		effort         string
		expectedModel  string
		expectedEffort string
	}{
		{"claude-opus-5", "high", "gemini-3.1-pro", "high"},
		{"claude-opus-5", "", "gemini-3.1-pro", "high"},
		{"opus", "low", "gemini-3.1-pro", "low"},
		{"claude-sonnet-4-6", "medium", "gemini-3.8-flash", "medium"},
		{"claude-sonnet-4-6", "high", "gemini-3.8-flash", "high"},
		{"sonnet", "", "gemini-3.8-flash", "medium"},
	}

	for _, tc := range testCases {
		model, effort, cmd := FallbackPairingMatrix(tc.claudeModel, tc.effort)
		if model != tc.expectedModel {
			t.Errorf("model mismatch for %s (effort %s): expected %s, got %s", tc.claudeModel, tc.effort, tc.expectedModel, model)
		}
		if effort != tc.expectedEffort {
			t.Errorf("effort mismatch for %s (effort %s): expected %s, got %s", tc.claudeModel, tc.effort, tc.expectedEffort, effort)
		}
		expectedCmd := "agy --model " + tc.expectedModel + " --effort " + tc.expectedEffort
		if cmd != expectedCmd {
			t.Errorf("command mismatch: expected %q, got %q", expectedCmd, cmd)
		}
	}
}
