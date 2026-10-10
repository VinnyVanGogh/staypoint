package orchestrator

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/opstools"
)

// task-7d279c9d Board reviews #2 H1 and #4: each run gets a random token the
// daemon holds in memory, valid for this run's task only and only while the
// run lasts.
func TestHarness_IssuesOpsRunToken(t *testing.T) {
	h, _ := seatLimitHarness(t, "task-ops-token")
	tokens := opstools.NewRunTokens()
	var env []string
	var liveDuringRun error
	_, err := h.Run(context.Background(), "task-ops-token", RunConfig{
		MaxTurns:  1,
		OpsTokens: tokens,
		RunAdapter: func(ctx context.Context, cwd, provider string, rawArgs, extraEnv []string, stdout, stderr io.Writer) error {
			env = extraEnv
			for _, kv := range extraEnv {
				if v, ok := strings.CutPrefix(kv, opstools.RunTokenEnv+"="); ok {
					_, _, liveDuringRun = tokens.CheckHash("task-ops-token", opstools.TokenHash(v), "")
				}
			}
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
	if liveDuringRun != nil {
		t.Fatalf("issued token did not check out during the run: %v", liveDuringRun)
	}
	if _, _, err := tokens.CheckHash("task-ops-token", opstools.TokenHash(tok), ""); err == nil {
		t.Fatal("token still live after the run ended")
	}

	// Two runs of the same task never share a token.
	env = nil
	if _, err := h.Run(context.Background(), "task-ops-token", RunConfig{
		MaxTurns:  1,
		OpsTokens: tokens,
		RunAdapter: func(ctx context.Context, cwd, provider string, rawArgs, extraEnv []string, stdout, stderr io.Writer) error {
			env = extraEnv
			return fakeSeatLimit{all: true}
		},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, opstools.RunTokenEnv+"="); ok && v == tok {
			t.Fatal("second run reused the first run's token")
		}
	}

	// No registry (tests, an old daemon): no token, so ops tools refuse.
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
			t.Fatal("token issued without a registry")
		}
	}
}
