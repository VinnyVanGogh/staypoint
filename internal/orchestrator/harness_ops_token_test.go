package orchestrator

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/opstools"
)

// task-7d279c9d Board review #2 H1: each run gets STAYPOINT_RUN_MAC, the
// token the staypoint MCP server checks before any ops tool runs, valid for
// this run's task only.
func TestHarness_IssuesOpsRunToken(t *testing.T) {
	h, _ := seatLimitHarness(t, "task-ops-token")
	dataDir := t.TempDir()
	var env []string
	_, err := h.Run(context.Background(), "task-ops-token", RunConfig{
		MaxTurns:   1,
		OpsDataDir: dataDir,
		RunAdapter: func(ctx context.Context, cwd, provider string, rawArgs, extraEnv []string, stdout, stderr io.Writer) error {
			env = extraEnv
			return fakeSeatLimit{all: true}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var tok, task string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, opstools.RunTokenEnv+"="); ok {
			tok = v
		}
		if v, ok := strings.CutPrefix(kv, "STAYPOINT_TASK_ID="); ok {
			task = v
		}
	}
	if tok == "" || task != "task-ops-token" {
		t.Fatalf("env lacks the run token or task id: task=%q token set=%v", task, tok != "")
	}
	if err := opstools.VerifyRunToken(dataDir, "task-ops-token", tok); err != nil {
		t.Fatalf("issued token does not verify: %v", err)
	}
	if err := opstools.VerifyRunToken(dataDir, "task-other", tok); err == nil {
		t.Fatal("issued token verifies for another task")
	}

	// No ops data dir (tests, an old daemon): no token, so ops tools refuse.
	env = nil
	if _, err := h.Run(context.Background(), "task-ops-token", RunConfig{
		MaxTurns: 1,
		RunAdapter: func(ctx context.Context, cwd, provider string, rawArgs, extraEnv []string, stdout, stderr io.Writer) error {
			env = extraEnv
			return fakeSeatLimit{all: true}
		},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, opstools.RunTokenEnv+"=") {
			t.Fatal("token issued without an ops data dir")
		}
	}
}
