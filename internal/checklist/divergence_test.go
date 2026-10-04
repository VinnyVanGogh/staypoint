package checklist_test

import (
	"context"
	"testing"

	. "github.com/VinnyVanGogh/staypoint/internal/checklist"
)

// TestEvaluateSprint_UsesRunningCommitForBanner verifies that when
// EvaluateOptions.RunningCommit is non-empty, EvaluationSummary.CommitSHA
// reflects it instead of shelling out to git (which returns "unknown" when
// the repo is unreachable under launchd/TCC).
func TestEvaluateSprint_UsesRunningCommitForBanner(t *testing.T) {
	db := setupTestDB(t)
	const want = "abc1234"

	summary, err := EvaluateSprint(context.Background(), db, "STA-168", EvaluateOptions{
		RepoRoot:      "/nonexistent-repo-sta285",
		RunningCommit: want,
	})
	if err != nil {
		t.Fatalf("EvaluateSprint: %v", err)
	}
	if summary.CommitSHA != want {
		t.Errorf("CommitSHA = %q, want %q (banner shows 'at commit unknown' when RunningCommit is ignored)", summary.CommitSHA, want)
	}
}

// TestEvaluateSprint_FallsBackToGitWhenNoRunningCommit verifies the fallback
// path: when RunningCommit is empty and the repo dir does not exist, CommitSHA
// is "unknown" (not a panic or empty string).
func TestEvaluateSprint_FallsBackToGitWhenNoRunningCommit(t *testing.T) {
	db := setupTestDB(t)

	summary, err := EvaluateSprint(context.Background(), db, "STA-168", EvaluateOptions{
		RepoRoot:      "/nonexistent-repo-sta285",
		RunningCommit: "",
	})
	if err != nil {
		t.Fatalf("EvaluateSprint: %v", err)
	}
	if summary.CommitSHA != "unknown" {
		t.Errorf("CommitSHA = %q, want \"unknown\" when git is unreachable and RunningCommit is empty", summary.CommitSHA)
	}
}
