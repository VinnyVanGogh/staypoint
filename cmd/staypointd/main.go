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
	"github.com/VinnyVanGogh/staypoint/internal/router"
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
// when it is (STA-805). Go's vcs.modified is not used: Go reads it from the
// nearest .git directory, so a build in a worktree nested in a dirty checkout
// reports the checkout's state, not its own.
func devBuildReason() string {
	return devBuildReasonFrom(DevBuild, GitCommit)
}

func devBuildReasonFrom(flag, gitCommit string) string {
	if flag == "true" {
		return "deployed with --allow-dev-build"
	}
	if gitCommit == "" || gitCommit == "none" {
		return "not built by reinstall-daemon.sh: no commit stamped"
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
			return
		}
		if agentID == "" {
			agentID = "local"
		}

		sessID := "paperclip-" + taskID
		if len(taskID) >= 8 {
			sessID = "paperclip-" + taskID[:8]
		}

		// resolvedProv is set by adapterFn before each adapter turn writes to
		// stdout; parseDelta reads it to select the right stream parser.
		var resolvedProv string

		var adapterFn orchestrator.AdapterRunFunc
		if adapterOverride != nil {
			adapterFn = func(runCtx context.Context, cwd, prov string, rawArgs, extraEnv []string, stdout, stderr io.Writer) error {
				agentType := prov
				if agentType == "" {
					agentType = "claude"
				}
				resolvedProv = agentType
				return adapterOverride(runCtx, cwd, prov, rawArgs, extraEnv, stdout, stderr)
			}
		} else {
			adapterFn = func(runCtx context.Context, cwd, prov string, rawArgs, extraEnv []string, stdout, stderr io.Writer) error {
				agentType := prov
				if agentType == "" {
					agentType = "claude"
				}
				resolvedProv = agentType
				if err := telemetry.HeartbeatSession(dbStore.DB(), telemetry.AgentSession{
					ID:        sessID,
					AgentType: agentType,
					RepoPath:  cwd,
					PID:       os.Getpid(),
				}); err != nil {
					slog.Warn("wake: session heartbeat failed",
						slog.String("task", taskID), slog.Any("error", err))
				}
				if len(extraEnv) > 0 {
					runCtx = adapter.WithExtraEnv(runCtx, extraEnv)
				}
				return adapter.RunAdapter(runCtx, cwd, nil, prov, rawArgs, nil, stdout, stderr)
			}
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

		parseDelta := func(line []byte) ([]orchestrator.StepDelta, error) {
			prov := resolvedProv
			if prov == "" {
				prov = "claude"
			}
			raw, err := adapter.AdapterFor(prov).ParseStreamDelta(line)
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
		db := dbStore.DB()
		result, runErr := h.Run(context.Background(), taskID, orchestrator.RunConfig{
			AgentID:    agentID,
			WakeReason: reason,
			RunAdapter: adapterFn,
			EmitRoute: func(sr *orchestrator.StepRecorder) {
				emitRouteStep(sr, db, taskID)
			},
			StepRecorder:     sr,
			ParseDelta:       parseDelta,
			RunControl:       orchestrator.GlobalRunControl,
			SkipGitPreflight: adapterOverride != nil,
			HookBin:          resolveStaypointCLIBin(),
		})
		if runErr != nil {
			if errors.Is(runErr, orchestrator.ErrConcurrencyCap) {
				// Refused: lock held by another run. No steps were emitted (wake/route
				// are now deferred to after Claim), so nothing to close out.
				slog.Warn("run refused: concurrency cap", slog.String("task", taskID))
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

// emitRouteStep looks up the task's repo_path and work_kind, resolves the
// actual adapter provider chain (the same chain RunAdapter will execute), and
// emits a route step as the first substantive timeline row.
//
// Using adapter.ResolveProviderChain instead of router.DefaultKindChains
// ensures the label matches the provider that actually runs (STA-481).
func emitRouteStep(sr *orchestrator.StepRecorder, dbConn *sql.DB, taskID string) {
	var repoPath, workKind string
	if err := dbConn.QueryRowContext(context.Background(),
		"SELECT COALESCE(repo_path,''), COALESCE(work_kind,'coding') FROM tasks WHERE id=?", taskID,
	).Scan(&repoPath, &workKind); err != nil {
		slog.Warn("route step: task lookup failed", slog.String("task", taskID), slog.Any("err", err))
		return
	}

	isWork, _, _ := router.IsWorkRepo(repoPath)

	pacer, err := router.LoadPacerState()
	if err != nil {
		slog.Warn("route step: pacer state load failed", slog.Any("err", err))
		pacer = &router.PacerState{Pools: make(map[router.PoolID]*router.QuotaPool)}
	}

	res := adapter.ResolveProviderChain(isWork, "", pacer)

	if res.AllLocked {
		sr.EmitRoute("All providers locked", "No viable provider in the chain")
		return
	}
	if res.IsCloud {
		sr.EmitRoute("Running in Claude Cloud", "Kind of work: "+workKind)
		return
	}
	if res.FallbackFromDisplay != "" {
		sr.EmitRoute(
			"Fell back to "+res.SelectedDisplay+": "+res.FallbackFromDisplay+" quota locked",
			"Kind of work: "+workKind,
		)
	} else {
		sr.EmitRoute("Ran on "+res.SelectedDisplay, "Kind of work: "+workKind)
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
