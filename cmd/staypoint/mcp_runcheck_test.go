package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/opstools"
	"github.com/VinnyVanGogh/staypoint/internal/server"
)

// Board review #4: staypoint mcp confirms its run token with the daemon,
// which holds live tokens in memory. End to end over the daemon's HTTP API.
func TestDaemonRunCheck(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "staypoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	tokens := opstools.NewRunTokens()
	start := func(t *testing.T, reg *opstools.RunTokens) string {
		t.Helper()
		srv, err := server.New(server.Options{
			BindHost: "127.0.0.1", Port: 0, AuthToken: "runcheck-token-1234567890abcdef01", DB: store.DB(),
			TelemetryDBPath:  filepath.Join(t.TempDir(), "telemetry.db"),
			ReplayBufferSize: 100, SubscriberBufferSize: 16,
			RunTokens: reg,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		})
		return srv.URL()
	}
	old := gateDaemonConn
	t.Cleanup(func() { gateDaemonConn = old })
	url := start(t, tokens)
	gateDaemonConn = func() (string, string) { return url, "runcheck-token-1234567890abcdef01" }

	tok, revoke, err := tokens.Issue("task-a", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := daemonRunCheck(ctx, "task-a", tok); err != nil {
		t.Fatalf("live token refused: %v", err)
	}
	for _, c := range []struct{ task, tok, want string }{
		{"task-b", tok, "does not belong to task task-b"},
		{"task-a", strings.Repeat("0", 64), "not a live run's token"},
	} {
		if err := daemonRunCheck(ctx, c.task, c.tok); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("check(%s): %v, want %q", c.task, err, c.want)
		}
	}
	revoke()
	if err := daemonRunCheck(ctx, "task-a", tok); err == nil || !strings.Contains(err.Error(), "not a live run's token") {
		t.Fatalf("ended run's token: %v", err)
	}

	// Without the daemon's auth token the check is not even answered.
	gateDaemonConn = func() (string, string) { return url, "wrong-token-1234567890abcdef012345" }
	tok2, revoke2, _ := tokens.Issue("task-a", "run-2")
	defer revoke2()
	if err := daemonRunCheck(ctx, "task-a", tok2); err == nil {
		t.Fatal("check passed with a bad daemon auth token")
	}

	// A daemon that issues no tokens, or none at all, confirms nothing.
	bare := start(t, nil)
	gateDaemonConn = func() (string, string) { return bare, "runcheck-token-1234567890abcdef01" }
	if err := daemonRunCheck(ctx, "task-a", tok2); err == nil || !strings.Contains(err.Error(), "issues no run tokens") {
		t.Fatalf("daemon without a registry: %v", err)
	}
	gateDaemonConn = func() (string, string) { return "http://127.0.0.1:1", "x" }
	if err := daemonRunCheck(ctx, "task-a", tok2); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("no daemon: %v", err)
	}
	gateDaemonConn = func() (string, string) { return "", "" }
	if err := daemonRunCheck(ctx, "task-a", tok2); err == nil {
		t.Fatal("no daemon conn: passed")
	}
}
