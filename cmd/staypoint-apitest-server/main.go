// Command staypoint-apitest-server runs the StayPoint HTTP API against a
// throwaway SQLite database so the Postman/newman end-to-end suite
// (tests/api) can exercise every route without touching real data.
//
// It deliberately skips everything staypointd does besides serving HTTP:
// no IPC socket, no telemetry watcher, no recovery scan, no token file.
// scripts/api-e2e.sh is the normal entry point.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/config"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
	"github.com/VinnyVanGogh/staypoint/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "staypoint-apitest-server:", err)
		os.Exit(1)
	}
}

func run() error {
	dbPath := flag.String("db", "", "path of a NEW SQLite file to create (required; must not exist)")
	telemetryDB := flag.String("telemetry-db", "", "telemetry.db path (default: next to --db)")
	port := flag.Int("port", 0, "port to bind on 127.0.0.1 (0 = ephemeral)")
	paperclipURL := flag.String("paperclip-url", "", "Paperclip stub base URL for /api/fleet/* proxies (required)")
	flag.Parse()

	token := os.Getenv("STAYPOINT_API_TOKEN")
	if len(token) < 16 {
		return errors.New("STAYPOINT_API_TOKEN must be set (16+ chars)")
	}

	// Generate a distinct board token so Playwright tests can exercise Board-only
	// endpoints (gate decide, ship-review approve/send-back/reject).
	boardToken := os.Getenv("STAYPOINT_BOARD_TOKEN")
	if boardToken == "" {
		boardToken = token + "-board"
	}

	// The fleet handlers proxy to whatever PAPERCLIP_API_URL names, with
	// PAPERCLIP_API_KEY attached, and default to the live control plane.
	// Pin them to the stub and drop the credentials so a test POST can never
	// land on a real issue.
	if *paperclipURL == "" {
		return errors.New("--paperclip-url is required (point it at tests/api/paperclip_stub.py)")
	}
	if strings.Contains(*paperclipURL, ":3100") {
		return fmt.Errorf("refusing --paperclip-url %s: that is the live Paperclip port", *paperclipURL)
	}
	for _, k := range []string{"PAPERCLIP_API_KEY", "PAPERCLIP_COMPANY_ID"} {
		_ = os.Unsetenv(k)
	}
	_ = os.Setenv("PAPERCLIP_API_URL", *paperclipURL)
	if *dbPath == "" {
		return errors.New("--db is required")
	}
	abs, err := filepath.Abs(*dbPath)
	if err != nil {
		return err
	}
	if err := refuseRealDB(abs); err != nil {
		return err
	}
	if _, err := os.Stat(abs); err == nil {
		return fmt.Errorf("refusing to reuse existing database %s: the suite needs a fresh file", abs)
	}
	if *telemetryDB == "" {
		*telemetryDB = filepath.Join(filepath.Dir(abs), "telemetry.db")
	}

	// Handlers resolve config, the report's telemetry DB and the staypointd IPC
	// socket (comments call orchestrator.NotifyDaemon) from HOME at request
	// time. Re-home the process next to the throwaway DB so none of them can
	// reach the real install.
	fakeHome := filepath.Join(filepath.Dir(abs), "home")
	if err := os.MkdirAll(fakeHome, 0o700); err != nil {
		return err
	}
	_ = os.Setenv("HOME", fakeHome)
	for _, k := range []string{"XDG_RUNTIME_DIR", "RUNTIME_DIRECTORY", "STAYPOINT_REPO_ROOT"} {
		_ = os.Unsetenv(k)
	}

	store, err := db.Open(abs)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer store.Close()
	orchestrator.GlobalRunControl.SetDB(store.DB())

	srv, err := server.New(server.Options{
		BindHost:        "127.0.0.1",
		Port:            *port,
		AuthToken:       token,
		BoardToken:      boardToken,
		DB:              store.DB(),
		TelemetryDBPath: *telemetryDB,
		GitCommit:       "apitest",
		TestMode:        true,
	})
	if err != nil {
		return err
	}
	if err := srv.Start(); err != nil {
		return err
	}
	// scripts/api-e2e.sh and scripts/ui-e2e.sh wait for this exact line.
	fmt.Printf("READY %s\n", srv.URL())
	// Board token for Playwright tests that need Board-only endpoints.
	fmt.Printf("BOARD_TOKEN %s\n", boardToken)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return srv.Shutdown(shutCtx)
}

// refuseRealDB rejects the configured and default StayPoint database paths.
// Callers normally also point HOME at a temp dir, but this check must hold
// even when they forget.
func refuseRealDB(abs string) error {
	candidates := []string{config.DefaultConfig().DBPath}
	if cfg, err := config.LoadConfig(); err == nil {
		candidates = append(candidates, cfg.DBPath)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".staypoint", "staypoint.db"),
			filepath.Join(home, ".agent-mesh", "agent-mesh.db"))
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if cAbs, err := filepath.Abs(c); err == nil && cAbs == abs {
			return fmt.Errorf("refusing to open real StayPoint database %s", abs)
		}
	}
	return nil
}
