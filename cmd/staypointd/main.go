package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/adapter"
	"github.com/VinnyVanGogh/staypoint/internal/config"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/ipc"
	"github.com/VinnyVanGogh/staypoint/internal/logging"
	"github.com/VinnyVanGogh/staypoint/internal/mcp"
	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
	"github.com/VinnyVanGogh/staypoint/internal/repoaccess"
	"github.com/VinnyVanGogh/staypoint/internal/server"
	"github.com/VinnyVanGogh/staypoint/internal/telemetry"
)

var (
	version   = "0.3.0"
	commit    = "none"
	GitCommit = "none"
	date      = "unknown"
	// DevBuild is set to "true" by reinstall-daemon.sh --allow-dev-build.
	DevBuild = "false"
)

// devBuildReason says why this binary is not a reviewed main build, or ""
// when it is (STA-805). The Go VCS stamp is trusted here because
// reinstall-daemon.sh builds clean trees from a throwaway clone, so for a
// script main build vcs.revision is the deployed commit and vcs.modified is
// false. A raw `go build -ldflags "-X main.GitCommit=x"` from a dirty or
// different tree is caught by the stamp even though GitCommit looks real.
func devBuildReason() string {
	rev, modified := vcsStamp()
	return devBuildReasonFrom(DevBuild, GitCommit, rev, modified)
}

// vcsStamp returns the vcs.revision and vcs.modified settings Go embedded in
// this binary, or "" for each one that is missing.
func vcsStamp() (revision, modified string) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", ""
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	return revision, modified
}

func devBuildReasonFrom(flag, gitCommit, vcsRevision, vcsModified string) string {
	if flag == "true" {
		return "deployed with --allow-dev-build"
	}
	if gitCommit == "" || gitCommit == "none" {
		return "not built by reinstall-daemon.sh: no commit stamped"
	}
	if vcsRevision == "" {
		return "no vcs.revision stamped: reinstall-daemon.sh refuses such builds"
	}
	if vcsModified == "true" {
		return "built from a tree with uncommitted changes (vcs.modified=true)"
	}
	if !strings.HasPrefix(vcsRevision, strings.TrimSuffix(gitCommit, "-dirty")) {
		return fmt.Sprintf("vcs.revision %s does not match the stamped commit %s", vcsRevision, gitCommit)
	}
	return ""
}

func init() {
	if GitCommit != "none" && commit == "none" {
		commit = GitCommit
	} else if commit != "none" && GitCommit == "none" {
		GitCommit = commit
	}
}

func main() {
	// A repo access probe child (STA-687) exits here, before any daemon setup.
	repoaccess.RunProbeChild()

	// Subcommands dispatch before flag.Parse so they own their own flag sets.
	if len(os.Args) > 1 && os.Args[1] == "eval-contracts" {
		if err := runEvalContracts(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "eval-contracts:", err)
			os.Exit(1)
		}
		return
	}

	var (
		flagService    = flag.Bool("service", false, "Run as a Windows service (SCM-managed)")
		flagInstallSvc = flag.Bool("install-service", false, "Register staypointd with the Windows SCM")
		flagRemoveSvc  = flag.Bool("remove-service", false, "Unregister staypointd from the Windows SCM")
	)
	flag.Parse()

	if *flagInstallSvc {
		exe, err := filepath.Abs(os.Args[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "install-service: %v\n", err)
			os.Exit(1)
		}
		if err := installService(exe); err != nil {
			fmt.Fprintf(os.Stderr, "install-service: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *flagRemoveSvc {
		if err := removeService(); err != nil {
			fmt.Fprintf(os.Stderr, "remove-service: %v\n", err)
			os.Exit(1)
		}
		return
	}

	logLevel := os.Getenv("STAYPOINT_LOG_LEVEL")
	if logLevel == "" {
		if legacy := os.Getenv("MESH_LOG_LEVEL"); legacy != "" {
			fmt.Fprintf(os.Stderr, "DEPRECATION WARNING: MESH_LOG_LEVEL is deprecated and will be removed in v0.3.0. Use STAYPOINT_LOG_LEVEL instead.\n")
			logLevel = legacy
		} else {
			logLevel = "INFO"
		}
	}
	logFormat := os.Getenv("STAYPOINT_LOG_FORMAT")
	if logFormat == "" {
		if legacy := os.Getenv("MESH_LOG_FORMAT"); legacy != "" {
			fmt.Fprintf(os.Stderr, "DEPRECATION WARNING: MESH_LOG_FORMAT is deprecated and will be removed in v0.3.0. Use STAYPOINT_LOG_FORMAT instead.\n")
			logFormat = legacy
		} else {
			logFormat = "text"
		}
	}

	// When running under the Windows SCM, log as JSON so the Windows Event Log
	// or a log collector can parse structured fields.
	isSvc, _ := isWindowsService()
	if isSvc || *flagService {
		logFormat = "json"
	}
	logging.SetupLogger(logLevel, logFormat, os.Stderr)

	slog.Info("Starting Staypoint Background Daemon...",
		slog.String("version", version),
		slog.String("commit", commit),
		slog.String("build_date", date),
	)

	if isSvc || *flagService {
		if err := runAsService(runDaemon); err != nil {
			slog.Error("Service exited with error", slog.Any("error", err))
			os.Exit(1)
		}
		return
	}

	// Interactive / console path: wire signal handler.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		slog.Warn("Received signal, shutting down...", slog.Any("signal", sig))
		cancel()
	}()

	if err := runDaemon(ctx); err != nil {
		slog.Error("Daemon exited with error", slog.Any("error", err))
	}
	slog.Info("Daemon shutdown complete.")
}

// runDaemon is the core daemon logic shared by interactive and service modes.
func runDaemon(ctx context.Context) error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := config.EnsureDataDir(cfg); err != nil {
		return fmt.Errorf("ensure data dir: %w", err)
	}

	// 0. Open persistent DB; used by recovery scan, harness, and HTTP server.
	// defer guarantees Close on every return path including early errors below.
	dbStore, err := db.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	defer dbStore.Close()
	_ = orchestrator.RecoveryScan(ctx, dbStore.DB())

	// 1. Start Rate Limit Notifier
	notifier := telemetry.NewNotifier()
	go notifier.Start(ctx)
	slog.Info("Rate limit monitoring active")

	// 2. Start IPC listener (Unix socket on POSIX, named pipe on Windows).
	socketPath := ipc.SocketPath()
	mcpServer := mcp.NewServer(mcp.WithConfig(cfg))
	go func() {
		handleConn := func(conn net.Conn) {
			defer conn.Close()
			if err := mcpServer.Serve(ctx, conn, conn); err != nil {
				slog.Debug("IPC connection closed", slog.Any("error", err))
			}
		}
		if err := ipc.Listen(ctx, socketPath, handleConn); err != nil && ctx.Err() == nil {
			slog.Warn("IPC listener exited", slog.Any("error", err))
		}
	}()
	slog.Info("IPC listener active", slog.String("path", socketPath))

	// 3. Start File Watcher and Ingestion Engine
	watcher, err := telemetry.NewWatcher(cfg)
	if err != nil {
		return fmt.Errorf("init watcher: %w", err)
	}

	// 4. Start HTTP & SSE Local Daemon Server (127.0.0.1 only)
	repoChecker := &repoaccess.Checker{
		Options: repoaccess.Options{Timeout: repoaccess.DefaultTimeout},
		Notify:  telemetry.SendNotification,
	}
	var httpServer *server.Server
	tokenPath := filepath.Join(cfg.DataDir, "auth_token")
	boardTokenPath := filepath.Join(cfg.DataDir, "board_token")
	if s, err := server.New(server.Options{
		BindHost:       "127.0.0.1",
		Port:           41421,
		TokenPath:      tokenPath,
		BoardTokenPath: boardTokenPath,
		DB:             dbStore.DB(),
		GitCommit:      GitCommit,
		DevBuildReason: devBuildReason(),
		CORSAllowAll:   cfg.CORSAllowAll,
		RepoAccess:     repoChecker,
	}); err != nil {
		slog.Warn("Failed to initialize HTTP server", slog.Any("error", err))
	} else if err := s.Start(); err != nil {
		slog.Warn("Failed to start HTTP server", slog.Any("error", err))
		// s is not started; leave httpServer nil so shutdown skips it
	} else {
		httpServer = s
		slog.Info("HTTP and SSE server active",
			slog.String("url", httpServer.URL()),
			slog.String("token_path", tokenPath),
			slog.String("board_token_path", boardTokenPath),
		)
		// Print Board URL only to a TTY so the credential is not captured by
		// log collectors or piped output that an agent process could read.
		fi, _ := os.Stdout.Stat()
		if fi != nil && (fi.Mode()&os.ModeCharDevice) != 0 {
			boardURL := fmt.Sprintf("%s/?token=%s&board_nonce=%s", httpServer.URL(), httpServer.Token(), httpServer.BoardNonce())
			fmt.Printf("  Board URL:  %s\n", boardURL)
		}
	}

	// 4b. Repo self-check (STA-687). After a redeploy, git children in a repo
	// have hung in open() with no error (2026-10-04). Probe every repo now and
	// every 10 minutes, step by step, and tell the Board which step failed and
	// the raw error, instead of letting requests hang.
	if httpServer != nil {
		hub := httpServer.Hub()
		repoChecker.Publish = func(eventType string, data any) { hub.Publish(eventType, data) }
	}
	go repoChecker.Run(ctx, 10*time.Minute, func() ([]repoaccess.Target, error) {
		return repoaccess.RepoTargets(dbStore.DB(), cfg.HarnessRepoRoot)
	})

	// 5. Wire GlobalDispatcher.OnWake to launch harness runs.
	// HarnessRepoRoot comes from STAYPOINT_REPO_ROOT env or harness_repo_root config key.
	// work_repo_root is intentionally NOT used here — it belongs to billing/bridge.
	orchestrator.GlobalRunControl.SetDB(dbStore.DB())
	wireRunQueue(ctx, cfg.MaxConcurrentRunsOrDefault(), httpServer)

	repoRoot := cfg.HarnessRepoRoot
	if repoRoot == "" {
		slog.Warn("harness_repo_root not configured and STAYPOINT_REPO_ROOT not set; wake harness disabled")
	} else {
		wireOnWake(dbStore, repoRoot, httpServer, nil)
		slog.Info("agent wake dispatcher wired", slog.String("repo_root", repoRoot))
	}

	// Shutdown: drain in-flight harness runs before closing HTTP server and DB.
	go func() {
		<-ctx.Done()

		drainDone := make(chan struct{})
		go func() { orchestrator.GlobalDispatcher.Drain(); close(drainDone) }()
		select {
		case <-drainDone:
		case <-time.After(30 * time.Second):
			slog.Warn("harness drain timeout; some runs may be incomplete")
		}

		if httpServer != nil {
			shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = httpServer.Shutdown(shutCtx)
		}
		// dbStore closed by defer above once runDaemon returns.
	}()

	slog.Info("Background daemon ready and running")
	return watcher.Start(ctx)
}

// wireOnWake assigns GlobalDispatcher.OnWake so every Wake call launches a
// harness run for the woken task. Extracted for testability.
//
// srv, adapterOverride, and wmOverride, when non-nil, replace the production
// SSE hub, adapter, and worktree manager respectively. Pass nil for all in tests
// that don't need step recording or a real adapter.
func wireOnWake(dbStore *db.Store, repoRoot string, srv *server.Server, adapterOverride orchestrator.AdapterRunFunc, wmOverride ...orchestrator.WorktreeManagerIface) {
	h := orchestrator.NewHarness(dbStore.DB(), repoRoot)
	if len(wmOverride) > 0 && wmOverride[0] != nil {
		h.WM = wmOverride[0]
	}
	orchestrator.GlobalDispatcher.OnWake = func(taskID, reason string) {
		var agentID string
		if err := dbStore.DB().QueryRowContext(context.Background(),
			"SELECT COALESCE(assignee_agent_id,'') FROM tasks WHERE id=?", taskID,
		).Scan(&agentID); err != nil {
			lvl := slog.LevelError
			if errors.Is(err, sql.ErrNoRows) {
				lvl = slog.LevelWarn
			}
			slog.Log(context.Background(), lvl, "wake: agent id lookup failed; skipping run",
				slog.String("task", taskID), slog.Any("error", err))
			// A queued run whose task is gone must not hold its queue place.
			orchestrator.GlobalRunSlots.Dequeue(taskID)
			return
		}
		// Respect pacer locks per pool (STA-773): a run whose whole provider
		// chain is quota-locked waits in the queue instead of taking a slot.
		if taskQuotaLocked(dbStore.DB(), taskID) {
			queueRun(h, taskID, reason, orchestrator.WaitQuota)
			return
		}
		if agentID == "" {
			agentID = "local"
		}

		sessID := "paperclip-" + taskID
		if len(taskID) >= 8 {
			sessID = "paperclip-" + taskID[:8]
		}

		// Build a per-run StepRecorder when a live EventHub is available.
		// ParseDelta bridges adapter.StreamDelta → orchestrator.StepDelta without
		// importing the adapter package from inside the orchestrator.
		var publishFn orchestrator.PublishFunc
		if srv != nil {
			hub := srv.Hub()
			publishFn = func(eventType string, data any) { hub.Publish(eventType, data) }
		} else {
			publishFn = func(string, any) {}
		}
		idPrefix := taskID
		if len(taskID) >= 8 {
			idPrefix = taskID[:8]
		}
		runID := fmt.Sprintf("run-%s-%d", idPrefix, time.Now().UnixMilli())
		sr := orchestrator.NewStepRecorder(dbStore.DB(), publishFn, runID, taskID)
		// Wake and route steps are now emitted from inside harness.Run() after
		// Claim() succeeds, so refused runs (ErrConcurrencyCap) never write steps.

		// One routing decision per run (STA-772): work_kind + repo seat + quota
		// pick the chain. The spawned CLI and the route row both come from it,
		// and the tracker re-labels the row if a turn spawns a different slot.
		route := resolveTaskRoute(dbStore.DB(), taskID, repoRoot, currentPacer(), time.Now())
		tracker := newRouteTracker(route, sr.EmitRoute)

		var adapterFn orchestrator.AdapterRunFunc
		if adapterOverride != nil {
			adapterFn = adapterOverride
		} else {
			adapterFn = func(runCtx context.Context, cwd, _ string, rawArgs, extraEnv []string, stdout, stderr io.Writer) error {
				if err := telemetry.HeartbeatSession(dbStore.DB(), telemetry.AgentSession{
					ID:        sessID,
					AgentType: tracker.Provider(),
					RepoPath:  cwd,
					PID:       os.Getpid(),
				}); err != nil {
					slog.Warn("wake: session heartbeat failed",
						slog.String("task", taskID), slog.Any("error", err))
				}
				if len(extraEnv) > 0 {
					runCtx = adapter.WithExtraEnv(runCtx, extraEnv)
				}
				return tracker.runRouted(runCtx, cwd, rawArgs, stdout, stderr)
			}
		}

		parseDelta := func(line []byte) ([]orchestrator.StepDelta, error) {
			raw, err := adapter.AdapterFor(tracker.Provider()).ParseStreamDelta(line)
			if err != nil {
				return nil, err
			}
			out := make([]orchestrator.StepDelta, 0, len(raw))
			for _, d := range raw {
				sd := orchestrator.StepDelta{
					Kind:      orchestrator.StepDeltaKind(d.Kind),
					Text:      d.Text,
					ToolName:  d.ToolName,
					ToolID:    d.ToolID,
					ToolInput: d.ToolInput,
					IsError:   d.IsError,
				}
				if d.Usage != nil {
					sd.Usage = &orchestrator.StepUsage{
						InputTokens:         d.Usage.InputTokens,
						OutputTokens:        d.Usage.OutputTokens,
						CacheReadTokens:     d.Usage.CacheReadTokens,
						CacheCreationTokens: d.Usage.CacheCreationTokens,
						Model:               d.Model,
					}
				}
				out = append(out, sd)
			}
			return out, nil
		}

		// Use context.Background() so daemon shutdown does not abruptly kill
		// in-flight harness work; the dispatcher's Drain() provides the graceful
		// drain window during shutdown.
		result, runErr := h.Run(context.Background(), taskID, orchestrator.RunConfig{
			AgentID:    agentID,
			WakeReason: reason,
			RunAdapter: adapterFn,
			EmitRoute: func(*orchestrator.StepRecorder) {
				tracker.EmitPlanned()
			},
			StepRecorder:     sr,
			ParseDelta:       parseDelta,
			RunControl:       orchestrator.GlobalRunControl,
			SkipGitPreflight: adapterOverride != nil || testSkipGitPreflight,
			HookBin:          resolveStaypointCLIBin(),
		})
		if runErr != nil {
			if errors.Is(runErr, orchestrator.ErrConcurrencyCap) {
				// Refused for capacity (global cap or repo busy). No steps were
				// emitted (wake/route come after Claim). Queue it so it starts on
				// its own when a slot or its repo frees (STA-773).
				queueRun(h, taskID, reason, orchestrator.WaitFor(runErr))
				return
			}
			if errors.Is(runErr, orchestrator.ErrAlreadyClaimed) {
				// The task is already running (e.g. Run Now pressed again). With
				// parallel slots this no longer hits the global cap first; it must
				// stay quiet and not post an error state over the live run.
				slog.Info("run refused: task already running", slog.String("task", taskID))
				return
			}
			slog.Error("harness run failed", slog.String("task", taskID), slog.Any("error", runErr))
			sr.EmitState("error")
			return
		}
		sr.EmitState(result.Disposition)
		slog.Info("harness run complete",
			slog.String("task", taskID),
			slog.String("disposition", result.Disposition),
			slog.Int("turns", result.Turns),
		)
	}
}

// resolveStaypointCLIBin returns the absolute path of the staypoint CLI binary
// for injection as STAYPOINT_HOOK_BIN into adapter runs (STA-525).
// Precedence: STAYPOINT_CLI_BIN env → ~/.local/bin/staypoint.
// PATH lookup is deliberately omitted: a controlled install path prevents an
// adversarial PATH entry from substituting a different binary as the hook gate.
// Returns "" (fail-open) with a warning log if none can be found; runs still
// proceed but pause will not hold at step boundaries until the CLI is installed.
func resolveStaypointCLIBin() string {
	if v := os.Getenv("STAYPOINT_CLI_BIN"); v != "" {
		if _, err := os.Stat(v); err == nil {
			return v
		}
		slog.Warn("STAYPOINT_CLI_BIN set but binary not found; PreToolUse pause gate will not fire",
			slog.String("path", v))
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".local", "bin", "staypoint")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	slog.Warn("staypoint CLI binary not found at ~/.local/bin/staypoint; " +
		"run reinstall-daemon.sh to build it. PreToolUse pause gate will not fire until installed.")
	return ""
}
