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

	// Case 2: the last tool used in the repo being agy no longer keeps a
	// session on agy: the router never picks it (GeminiCodeForbidden).
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
	if dec2.Tool != "claude" {
		t.Errorf("expected claude even after an agy session in this repo, got %s", dec2.Tool)
	}
	// A remembered preference for agy is ignored (with a warning), and so is
	// Gemini having far more headroom.
	for _, pref := range []string{"agy", "gemini"} {
		gemRich := &PacerState{Pools: map[PoolID]*QuotaPool{
			PoolGeminiNative:   {TurnsRunway: 100, FiveHour: QuotaWindow{RemainingPct: 100}, Weekly: QuotaWindow{RemainingPct: 100}},
			PoolPersonalClaude: {TurnsRunway: 5, FiveHour: QuotaWindow{RemainingPct: 10}, Weekly: QuotaWindow{RemainingPct: 5}},
		}}
		d, err := Route(ctx, "/Users/vincevasile/Documents/dev/personal-app", gemRich, RouteOptions{PreferredPersonalTool: pref, LastUsedTool: "agy"})
		if err != nil {
			t.Fatal(err)
		}
		if d.Tool != "claude" || d.Target != TargetClaudePersonal || len(d.Warnings) == 0 {
			t.Errorf("pref %s: got tool=%s target=%s warnings=%v, want claude with a warning", pref, d.Tool, d.Target, d.Warnings)
		}
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

	// 1. When Claude (Work) is locked out -> personal Claude seat, never agy
	//    (STA-856: Gemini never writes code in a work repo).
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
	if decWork.Tool != "claude" || decWork.Target != TargetClaudePersonal || decWork.Waiting {
		t.Errorf("expected personal Claude for locked work seat, got tool=%s target=%s waiting=%v", decWork.Tool, decWork.Target, decWork.Waiting)
	}

	// 1b. Both seats locked -> wait on the work seat, still never agy.
	bothLocked := &PacerState{Pools: map[PoolID]*QuotaPool{
		PoolWorkClaude:     {IsLocked: true, LockoutReason: "weekly limit"},
		PoolPersonalClaude: {IsLocked: true, LockoutReason: "5h limit"},
		PoolGeminiNative:   {TurnsRunway: 100, FiveHour: QuotaWindow{RemainingPct: 80}, Weekly: QuotaWindow{RemainingPct: 60}},
	}}
	decWait, err := Route(ctx, "/Users/vincevasile/Documents/dev/mansol-apps-server/github_repo-prod", bothLocked, RouteOptions{PreferredPersonalTool: "agy"})
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	if decWait.Tool == "agy" || !decWait.Waiting || decWait.Target != TargetLocalClaudeWork {
		t.Errorf("expected waiting on work Claude, got tool=%s target=%s waiting=%v", decWait.Tool, decWait.Target, decWait.Waiting)
	}

	// 2. Personal repo: Claude locked -> waits on personal Claude, never agy.
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
	if decOpus.Tool != "claude" || decOpus.Model != "claude-opus-5" || !decOpus.Waiting || decOpus.Target != TargetClaudePersonal {
		t.Errorf("expected waiting on personal Claude Opus, got tool=%s model=%s waiting=%v", decOpus.Tool, decOpus.Model, decOpus.Waiting)
	}

	// 3. Same with Sonnet preferred.
	decSonnet, err := Route(ctx, "/Users/vincevasile/Documents/dev/personal-app", lockedPersonalPacer, RouteOptions{
		PreferredPersonalTool: "claude",
		PreferredModel:        "claude-sonnet-4-6",
		PreferredEffort:       "high",
	})
	if err != nil {
		t.Fatalf("Route failed: %v", err)
	}
	if decSonnet.Tool != "claude" || decSonnet.Model != "claude-sonnet-4-6" || !decSonnet.Waiting {
		t.Errorf("expected waiting on personal Claude Sonnet, got tool=%s model=%s waiting=%v", decSonnet.Tool, decSonnet.Model, decSonnet.Waiting)
	}

	// 3b. Every personal pool locked (including 3P Claude in Antigravity):
	// still Claude, waiting, never agy.
	allLocked := &PacerState{Pools: map[PoolID]*QuotaPool{
		PoolPersonalClaude: {IsLocked: true, LockoutReason: "5h"},
		PoolGeminiNative:   {IsLocked: true},
		Pool3PClaude:       {TurnsRunway: 50, FiveHour: QuotaWindow{RemainingPct: 100}, Weekly: QuotaWindow{RemainingPct: 100}},
	}}
	decAll, err := Route(ctx, "/Users/vincevasile/Documents/dev/personal-app", allLocked, RouteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if decAll.Tool != "claude" || !decAll.Waiting {
		t.Errorf("all locked: want waiting Claude, got tool=%s waiting=%v", decAll.Tool, decAll.Waiting)
	}

	// 3c. No pacer data at all: Claude, not agy.
	decNone, err := Route(ctx, "/Users/vincevasile/Documents/dev/personal-app", &PacerState{Pools: map[PoolID]*QuotaPool{}}, RouteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if decNone.Tool != "claude" || decNone.Waiting {
		t.Errorf("no data: want Claude, got tool=%s waiting=%v", decNone.Tool, decNone.Waiting)
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
