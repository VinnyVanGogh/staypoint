package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/repoaccess"
)

// RepoAccessReporter supplies the latest repo access check (repoaccess.Checker).
type RepoAccessReporter interface {
	Snapshot() repoaccess.Snapshot
}

// Server is the StayPoint local HTTP and SSE daemon server.
type Server struct {
	opts       Options
	listener   net.Listener
	httpServer *http.Server
	hub        *EventHub
	secMid     *SecurityMiddleware
	webAuthnH  *WebAuthnHandler
	prH        *PullRequestsHandler
	addr       string
	port       int
	mu         sync.Mutex
	running    bool
	stopAlerts chan struct{}
}

// New creates and configures a new Server instance.
func New(opts Options) (*Server, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}

	hub := opts.Hub
	if hub == nil {
		hub = NewEventHub(opts.ReplayBufferSize, opts.SubscriberBufferSize)
	}

	secMid := NewSecurityMiddlewareWithBoardToken(opts.AuthToken, opts.BoardToken, opts.Port, opts.CORSAllowAll)
	// Generate a one-time bootstrap nonce so the board_token never needs to appear in a URL.
	// The nonce is consumed on first successful ?board_nonce= use; ?board_token= is no longer accepted.
	if nonce, err := GenerateAuthToken(); err == nil {
		secMid.SetBoardNonce(nonce)
		opts.BoardNonce = nonce
	}

	// Gate testMode on opts.TestMode only — never call SetTestMode from production paths.
	if opts.TestMode {
		secMid.SetTestMode(true)
	}

	s := &Server{
		opts:   opts,
		hub:    hub,
		secMid: secMid,
	}
	if opts.DB != nil {
		secMid.SetDB(opts.DB)
		// Wire the default WebAuthn verifier (challenge-store validation).
		// Tests may override this via SetWebAuthnVerifier.
		s.webAuthnH = NewWebAuthnHandler(opts.DB, hub)
		secMid.setWebAuthnVerifier(s.webAuthnH.VerifyAssertion)
		if opts.TestMode {
			// Never pop a real macOS dialog from e2e; the code is read back
			// through the TestMode last-pairing-code endpoint instead.
			s.webAuthnH.SetPairingNotifier(func(string) error { return nil })
		}
	}

	mux := http.NewServeMux()
	s.registerRoutes(mux)

	// Wrap entire mux with security middleware (DNS rebinding, Origin, CORS, Auth)
	secureHandler := secMid.Wrap(mux)

	s.httpServer = &http.Server{
		Handler:      secureHandler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // Zero for SSE streaming
		IdleTimeout:  60 * time.Second,
	}

	return s, nil
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	// SSE endpoint
	mux.HandleFunc("GET /api/events", s.hub.HandleSSE())
	mux.HandleFunc("GET /api/sse", s.hub.HandleSSE())

	// Board nonce endpoint: mints a fresh single-use bootstrap nonce.
	// Requires the session auth token (normal auth) AND the board token as
	// X-Board-Token header. This keeps the endpoint from being callable by
	// agents that only hold the session auth token.
	mux.HandleFunc("POST /api/board/fresh-nonce", func(w http.ResponseWriter, r *http.Request) {
		bt := r.Header.Get("X-Board-Token")
		nonce, ok := s.secMid.FreshNonce(bt)
		if !ok {
			writeError(w, http.StatusForbidden, "forbidden: X-Board-Token required and must match the board credential")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"nonce":%q}`, nonce)
	})

	// Health check (within security wrapper)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		snap := s.repoAccessSnapshot()
		writeJSONUnescaped(w, struct {
			Status         string           `json:"status"`
			Version        string           `json:"version"`
			GitCommit      string           `json:"git_commit"`
			Commit         string           `json:"commit"`
			DevBuild       bool             `json:"dev_build"`
			DevBuildReason string           `json:"dev_build_reason,omitempty"`
			RepoAccess     healthRepoAccess `json:"repo_access"`
		}{
			Status: "ok", Version: "1.0", GitCommit: s.opts.GitCommit, Commit: s.opts.GitCommit,
			DevBuild: s.opts.DevBuildReason != "", DevBuildReason: s.opts.DevBuildReason,
			RepoAccess: healthRepoAccess{snap.Checked, snap.CheckedAt, snap.Failing()},
		})
	})

	// Per-repo check results (STA-687): every configured repo, with the step
	// that failed, the raw error and the daemon's uid and executable.
	mux.HandleFunc("GET /api/health/repos", func(w http.ResponseWriter, r *http.Request) {
		writeJSONUnescaped(w, s.repoAccessSnapshot())
	})

	// Tasks REST API
	if s.opts.DB != nil {
		tasksH := NewTasksHandler(s.opts.DB, s.hub)
		// STA-859: override / allow_deep are Board-only; they need the passkey gate.
		tasksH.SetBoardGate(s.secMid.WrapBoardAction)
		// Tasks created without a Board session are agent tasks (backlog,
		// Board-only to start).
		tasksH.SetBoardSession(s.secMid.IsBoardSession)
		mux.HandleFunc("GET /api/tasks", tasksH.ListTasks)
		mux.HandleFunc("POST /api/tasks", tasksH.CreateTask)
		mux.HandleFunc("GET /api/tasks/{id}", tasksH.GetTask)
		mux.HandleFunc("PUT /api/tasks/{id}/description", tasksH.UpdateTaskDescription)
		mux.HandleFunc("GET /api/tasks/{id}/comments", tasksH.GetComments)
		mux.HandleFunc("POST /api/tasks/{id}/comments", tasksH.AddComment)
		mux.HandleFunc("POST /api/tasks/{id}/done", tasksH.MarkDone)
		mux.HandleFunc("POST /api/tasks/{id}/block", tasksH.BlockTask)
		mux.HandleFunc("POST /api/tasks/{id}/unblock", tasksH.UnblockTask)
		mux.HandleFunc("GET /api/tasks/{id}/dependencies", tasksH.GetTaskDependencies)
		mux.HandleFunc("POST /api/tasks/{id}/blockers", tasksH.AddBlocker)
		mux.HandleFunc("DELETE /api/tasks/{id}/blockers/{bid}", tasksH.RemoveBlocker)
		mux.HandleFunc("POST /api/tasks/{id}/stage", tasksH.SetStage)
		// task-f6777004: Run all children is one Board action (session + passkey).
		mux.Handle("POST /api/tasks/{id}/run-children", s.secMid.WrapBoardAction(http.HandlerFunc(tasksH.RunChildren)))
		mux.HandleFunc("PUT /api/tasks/{id}/repo", tasksH.SetRepo)
		mux.HandleFunc("PUT /api/tasks/{id}/project", tasksH.SetProject)
		mux.HandleFunc("PUT /api/tasks/{id}/provider", tasksH.SetProvider)
		mux.HandleFunc("PUT /api/tasks/{id}/kind", tasksH.SetKind)
		mux.HandleFunc("POST /api/tasks/{id}/work-products", tasksH.AddWorkProduct)
		mux.HandleFunc("GET /api/tasks/{id}/documents", tasksH.ListTaskDocuments)
		mux.HandleFunc("GET /api/tasks/{id}/documents/{key}", tasksH.GetTaskDocument)
		mux.HandleFunc("GET /api/documents", tasksH.ListDocuments)
		mux.HandleFunc("GET /api/tasks/{id}/run-steps", tasksH.GetRunSteps)
		mux.HandleFunc("GET /api/tasks/{id}/run-errors", tasksH.GetRunErrors)
		mux.HandleFunc("GET /api/run-errors", tasksH.GetAllRunErrors)
		mux.HandleFunc("GET /api/tasks/{id}/checkpoints", tasksH.GetTaskCheckpoints)
		mux.HandleFunc("GET /api/tasks/{id}/diff", tasksH.GetTaskDiff)
		mux.HandleFunc("GET /api/tasks/{id}/diff/file", tasksH.GetTaskFileDiff)
		mux.HandleFunc("GET /api/tasks/{id}/migrations", tasksH.GetTaskMigrations)
		mux.HandleFunc("POST /api/tasks/{id}/migrations/mark-applied", tasksH.MarkMigrationApplied)
		mux.HandleFunc("POST /api/tasks/{id}/checkpoint-undo", tasksH.UndoTaskCheckpoint)
		mux.HandleFunc("POST /api/tasks/{id}/checkpoint-restore-file", tasksH.RestoreFileHandler)
		mux.HandleFunc("POST /api/tasks/{id}/run-control", tasksH.RunControl)
		mux.HandleFunc("GET /api/tasks/{id}/run-control-state", tasksH.GetRunControlState)
		mux.HandleFunc("GET /api/tasks/{id}/interactions", tasksH.ListInteractions)
		mux.HandleFunc("POST /api/tasks/{id}/interactions", tasksH.CreateInteraction)
		mux.HandleFunc("POST /api/tasks/{id}/interactions/{iid}/resolve", tasksH.ResolveInteraction)

		// Threads REST API
		threadsH := NewThreadsHandler(s.opts.DB, s.hub)
		mux.HandleFunc("GET /api/threads", threadsH.ListThreads)
		mux.HandleFunc("POST /api/threads", threadsH.CreateThread)
		mux.HandleFunc("GET /api/threads/{id}", threadsH.GetThread)
		mux.HandleFunc("GET /api/threads/{id}/messages", threadsH.GetMessages)
		mux.HandleFunc("POST /api/threads/{id}/messages", threadsH.AppendMessage)

		// Sessions REST API
		sessionsH := NewSessionsHandler(s.opts.DB, s.hub)
		mux.HandleFunc("GET /api/agents", sessionsH.ListAgents)
		mux.HandleFunc("GET /api/sessions", sessionsH.ListSessions)
		mux.HandleFunc("POST /api/sessions", sessionsH.RegisterSession)
		mux.HandleFunc("GET /api/sessions/{id}", sessionsH.GetSession)
		mux.HandleFunc("POST /api/sessions/{id}/heartbeat", sessionsH.Heartbeat)
		mux.HandleFunc("POST /api/sessions/{id}/close", sessionsH.CloseSession)

		// Governance REST API
		govH := NewGovernanceHandler(s.opts.DB, s.hub)
		mux.HandleFunc("GET /api/tasks/{id}/governance", govH.GetGovernance)
		mux.HandleFunc("POST /api/tasks/{id}/governance", govH.SetGovernance)
		mux.HandleFunc("POST /api/tasks/{id}/reviewers", govH.AddReviewer)
		mux.HandleFunc("DELETE /api/tasks/{id}/reviewers/{rid}", govH.RemoveReviewer)
		mux.HandleFunc("POST /api/tasks/{id}/approvers", govH.AddApprover)
		mux.HandleFunc("DELETE /api/tasks/{id}/approvers/{aid}", govH.RemoveApprover)
		mux.HandleFunc("POST /api/tasks/{id}/watchdog", govH.SetWatchdog)
		mux.HandleFunc("POST /api/tasks/{id}/review", govH.SubmitReview)
		mux.HandleFunc("POST /api/tasks/{id}/approve", govH.SubmitApproval)
		mux.HandleFunc("POST /api/tasks/{id}/transition", govH.Transition)
		mux.HandleFunc("GET /api/tasks/{id}/audit", govH.GetAuditLog)

		// Telemetry & Fleet REST API
		telemetryH := NewTelemetryHandler(s.opts.DB, s.hub, s.opts.TelemetryDBPath)
		mux.HandleFunc("GET /api/telemetry", telemetryH.GetTelemetry)
		mux.HandleFunc("GET /api/fleet/overview", telemetryH.GetFleetOverview)
		mux.HandleFunc("GET /api/fleet/tasks/{id}", telemetryH.GetFleetTask)
		mux.HandleFunc("GET /api/fleet/tasks/{id}/comments", telemetryH.GetFleetTaskComments)
		mux.HandleFunc("POST /api/fleet/tasks/{id}/comments", telemetryH.PostFleetTaskComment)
		mux.HandleFunc("GET /api/report", telemetryH.GetReport)

		// Checklist REST API
		checklistH := NewChecklistHandler(s.opts.DB, s.hub, s.opts.GitCommit)
		mux.HandleFunc("GET /api/checklist", checklistH.ListItems)
		mux.HandleFunc("GET /api/checklist/sprints", checklistH.ListSprints)
		mux.HandleFunc("PATCH /api/checklist/{id}", checklistH.UpdateItem)
		mux.HandleFunc("GET /api/checklist/{id}/history", checklistH.GetHistory)
		mux.HandleFunc("POST /api/checklist/seed", checklistH.Seed)
		mux.HandleFunc("POST /api/checklist/evaluate", checklistH.Evaluate)

		// Security Gate REST API (Board approval for Red-tier agent commands)
		gateH := NewSecurityGateHandler(s.opts.DB, s.hub)
		gateH.advisor, gateH.reviewer, gateH.resolver = s.opts.GateAdvisor, s.opts.GateReviewer, s.opts.GateResolver
		mux.HandleFunc("GET /api/security/gate-requests", gateH.ListGateRequests)
		mux.HandleFunc("POST /api/security/gate-requests", gateH.CreateGateRequest)
		mux.HandleFunc("GET /api/security/gate-requests/{id}", gateH.GetGateRequest)
		mux.HandleFunc("GET /api/security/gate-requests/{id}/audit-log", gateH.ListGateAuditLog)
		// Board-only: deciding a gate request requires the board token so agents cannot self-approve.
		// Gate actions accept the passkey grace window (STA-868).
		mux.Handle("POST /api/security/gate-requests/{id}/decide", s.secMid.WrapBoardGateAction(http.HandlerFunc(gateH.DecideGateRequest)))
		mux.Handle("POST /api/security/gate-requests/decide-batch", s.secMid.WrapBoardGateAction(http.HandlerFunc(gateH.DecideBatch)))
		// AI review is advisory and spends quota: Board session, no passkey.
		mux.Handle("POST /api/security/gate-requests/review", s.secMid.WrapBoardSession(http.HandlerFunc(gateH.ReviewPending)))
		mux.HandleFunc("GET /api/security/gate-rules", gateH.ListRules)
		mux.Handle("POST /api/security/gate-rules", s.secMid.WrapBoardGateAction(http.HandlerFunc(gateH.CreateRule)))
		mux.Handle("DELETE /api/security/gate-rules/{id}", s.secMid.WrapBoardGateAction(http.HandlerFunc(gateH.DeleteRule)))
		mux.HandleFunc("GET /api/security/gate-stats", gateH.Stats)
		// task-6c1ed91f: "Trust this task until…". Creating one is a fresh
		// Touch ID (no grace); revoking only removes power, so a session will do.
		mux.HandleFunc("GET /api/security/trusts", gateH.ListTrusts)
		mux.HandleFunc("GET /api/tasks/{id}/trust", gateH.GetTaskTrust)
		mux.Handle("POST /api/tasks/{id}/trust", s.secMid.WrapBoardAction(http.HandlerFunc(gateH.CreateTaskTrust)))
		mux.Handle("POST /api/tasks/{id}/trust/revoke", s.secMid.WrapBoardSession(http.HandlerFunc(gateH.RevokeTaskTrust)))
		mux.Handle("GET /api/board/passkey-grace", s.secMid.WrapBoardSession(http.HandlerFunc(s.secMid.GraceStatus)))
		mux.HandleFunc("GET /api/settings/security-gate", gateH.GetSecurityGateSettings)
		// Board-only: toggling the gate itself requires the board token.
		mux.Handle("POST /api/settings/security-gate", s.secMid.WrapBoardAction(http.HandlerFunc(gateH.UpdateSecurityGateSettings)))
		// STA-854: per-company tracking gate; toggling is Board-only.
		mux.HandleFunc("GET /api/settings/tracking-gate", gateH.GetTrackingGateSettings)
		mux.Handle("POST /api/settings/tracking-gate", s.secMid.WrapBoardAction(http.HandlerFunc(gateH.UpdateTrackingGateSettings)))
		// Org hold: Board-only (session + passkey); agents can read it, never set it.
		mux.HandleFunc("GET /api/settings/org-hold", gateH.GetOrgHolds)
		mux.Handle("POST /api/settings/org-hold", s.secMid.WrapBoardAction(http.HandlerFunc(gateH.UpdateOrgHold)))
		mux.HandleFunc("GET /api/settings/org-trust", gateH.ListOrgTrusts)
		mux.Handle("POST /api/settings/org-trust", s.secMid.WrapBoardAction(http.HandlerFunc(gateH.CreateOrgTrust)))
		mux.Handle("POST /api/settings/org-trust/revoke", s.secMid.WrapBoardSession(http.HandlerFunc(gateH.RevokeOrgTrust)))

		// Ship Review REST API (Board-approval gate for agent branch merges)
		shipH := NewShipReviewHandler(s.opts.DB, s.hub)
		mux.HandleFunc("GET /api/tasks/{id}/ship-review", shipH.GetCard)
		mux.HandleFunc("PUT /api/tasks/{id}/ship-review", shipH.UpsertCard)
		// live_credentials projects need the Board session + passkey (STA-727); others stay agent-callable.
		mux.Handle("POST /api/tasks/{id}/ship-review/start-dev", shipH.StartDevGated(s.secMid.WrapBoardAction))
		mux.HandleFunc("POST /api/tasks/{id}/ship-review/stop-dev", shipH.StopDev)
		// Board-only: these actions merge / push / reject / revise the branch — agents cannot call them.
		mux.Handle("POST /api/tasks/{id}/ship-review/push-branch", s.secMid.WrapBoardAction(http.HandlerFunc(shipH.PushBranch)))
		mux.Handle("POST /api/tasks/{id}/ship-review/approve", s.secMid.WrapBoardAction(http.HandlerFunc(shipH.Approve)))
		mux.Handle("POST /api/tasks/{id}/ship-review/send-back", s.secMid.WrapBoardAction(http.HandlerFunc(shipH.SendBack)))
		mux.Handle("POST /api/tasks/{id}/ship-review/reject", s.secMid.WrapBoardAction(http.HandlerFunc(shipH.Reject)))
		mux.Handle("POST /api/tasks/{id}/ship-review/delete-branch", s.secMid.WrapBoardAction(http.HandlerFunc(shipH.DeleteMergedBranch)))
		// STA-717: PR-mode checks are read-only polls; merging the PR is a Board action.
		mux.HandleFunc("GET /api/tasks/{id}/ship-review/checks", shipH.Checks)
		mux.HandleFunc("GET /api/tasks/{id}/ship-review/check-failures", shipH.CheckFailures)
		mux.HandleFunc("GET /api/tasks/{id}/ship-review/test-coverage", shipH.TestCoverage)
		mux.Handle("POST /api/tasks/{id}/ship-review/merge", s.secMid.WrapBoardAction(http.HandlerFunc(shipH.MergePR)))
		mux.HandleFunc("GET /api/settings/ship-review", shipH.GetSettings)
		// Board-only: disabling ship review is a Board action.
		mux.Handle("POST /api/settings/ship-review", s.secMid.WrapBoardAction(http.HandlerFunc(shipH.SetSettings)))
		mux.HandleFunc("GET /api/project-dev-configs", shipH.ListProjectDevConfigs)

		// task-9fb380ef: Pull Requests page. Listing is read-only; merging and
		// combining PRs are Board actions behind the same gate as Approve.
		if s.prH == nil {
			s.prH = NewPullRequestsHandler(s.opts.DB)
		}
		mux.HandleFunc("GET /api/pull-requests", s.prH.List)
		mux.Handle("POST /api/pull-requests/merge", s.secMid.WrapBoardAction(http.HandlerFunc(s.prH.Merge)))
		mux.Handle("POST /api/pull-requests/combine", s.secMid.WrapBoardAction(http.HandlerFunc(s.prH.Combine)))
		// Board-only: setting dev_command/setup_steps is a Board action (STA-520).
		mux.Handle("PUT /api/project-dev-configs", s.secMid.WrapBoardAction(http.HandlerFunc(shipH.UpsertProjectDevConfig)))
		if s.opts.TestMode {
			mux.HandleFunc("PUT /api/tasks/{id}/ship-review/seed", shipH.SeedCard)
		}

		// Board alert feed (STA-705). Reading is open to the session token;
		// dismissing needs a Board session so agents can't silence alerts.
		alertsH := NewBoardAlertsHandler(s.opts.DB, s.hub)
		mux.HandleFunc("GET /api/board/alerts", alertsH.List)
		mux.Handle("POST /api/board/alerts/{id}/ack", s.secMid.WrapBoardSession(http.HandlerFunc(alertsH.Ack)))
		if s.opts.TestMode {
			mux.HandleFunc("POST /api/board/alerts/test/seed", alertsH.Seed)
		}

		// WebAuthn / passkey endpoints (Board session required; assertion enforced on delete)
		webAuthnH := s.webAuthnH
		if webAuthnH == nil {
			webAuthnH = NewWebAuthnHandler(s.opts.DB, s.hub)
		}
		mux.Handle("GET /api/board/webauthn/status", s.secMid.WrapBoardSession(http.HandlerFunc(webAuthnH.Status)))
		mux.Handle("POST /api/board/webauthn/register/begin", s.secMid.WrapBoardSession(http.HandlerFunc(webAuthnH.RegisterBegin)))
		mux.Handle("POST /api/board/webauthn/register/finish", s.secMid.WrapBoardSession(http.HandlerFunc(webAuthnH.RegisterFinish)))
		mux.Handle("POST /api/board/webauthn/challenge", s.secMid.WrapBoardSession(http.HandlerFunc(webAuthnH.Challenge)))
		mux.Handle("GET /api/board/webauthn/credentials", s.secMid.WrapBoardSession(http.HandlerFunc(webAuthnH.ListCredentials)))
		mux.Handle("DELETE /api/board/webauthn/credentials/{id}", s.secMid.WrapBoardAction(http.HandlerFunc(webAuthnH.DeleteCredential)))
		if s.opts.TestMode {
			// Test-only: expose the last generated pairing code so Playwright's CDP
			// enrollment helper can finish registration without the macOS dialog.
			mux.Handle("GET /api/board/webauthn/test/last-pairing-code", s.secMid.WrapBoardSession(http.HandlerFunc(webAuthnH.TestLastPairingCode)))
			// Test-only: wipe all passkeys so boardPage fixture can re-enroll on each test.
			mux.Handle("DELETE /api/board/webauthn/test/clear-credentials", s.secMid.WrapBoardSession(http.HandlerFunc(webAuthnH.TestClearCredentials)))
			// Test-only: make the pairing notifier fail like a denied osascript (STA-696).
			mux.Handle("PUT /api/board/webauthn/test/pairing-notifier", s.secMid.WrapBoardSession(http.HandlerFunc(webAuthnH.TestSetPairingNotifierError)))
		}
	}

	// Embedded web UI (must be registered last so /api/* patterns take precedence)
	RegisterUIRoutes(mux, s.opts.AuthToken)
}

// Start binds to 127.0.0.1 and starts serving requests in the background.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return fmt.Errorf("server already running")
	}

	// Strictly verify loopback bind address
	if s.opts.BindHost != "127.0.0.1" {
		return fmt.Errorf("%w: attempted to bind %q", ErrNonLoopbackBind, s.opts.BindHost)
	}

	addr := fmt.Sprintf("%s:%d", s.opts.BindHost, s.opts.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	s.listener = ln
	s.addr = ln.Addr().String()

	// Extract the actual port if dynamically assigned (port 0)
	if tcpAddr, ok := ln.Addr().(*net.TCPAddr); ok {
		s.port = tcpAddr.Port
		s.secMid.SetPort(s.port)
		if s.webAuthnH != nil {
			s.webAuthnH.SetPort(s.port)
		}
	}

	s.running = true

	if s.opts.DB != nil {
		s.stopAlerts = make(chan struct{})
		go s.pollAlerts(s.stopAlerts, time.Now(), s.opts.AlertPollInterval)
	}

	go func() {
		if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			// server closed unexpectedly
		}
	}()

	return nil
}

// Shutdown gracefully terminates the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return nil
	}

	s.running = false
	if s.stopAlerts != nil {
		close(s.stopAlerts)
		s.stopAlerts = nil
	}
	return s.httpServer.Shutdown(ctx)
}

// Addr returns the network address the server is listening on.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Port returns the TCP port the server is listening on.
func (s *Server) Port() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.port
}

// URL returns the base URL of the running server (e.g. http://127.0.0.1:41421).
func (s *Server) URL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", s.Port())
}

// Token returns the authentication token required by the server.
func (s *Server) Token() string {
	return s.opts.AuthToken
}

// BoardToken returns the board-only credential required for Board-action endpoints.
func (s *Server) BoardToken() string {
	return s.opts.BoardToken
}

// BoardNonce returns the one-time bootstrap nonce for the board session.
// Use this to construct the Board URL: /?token=<Token>&board_nonce=<BoardNonce>
// The nonce is single-use and is consumed by the first successful bootstrap request.
func (s *Server) BoardNonce() string {
	return s.opts.BoardNonce
}

// Hub returns the server's EventHub for publishing events.
func (s *Server) Hub() *EventHub {
	return s.hub
}

// SetPRClientFactory replaces the gh-backed client behind the Pull Requests
// page. Tests use it so they never reach GitHub; nil restores the default.
func (s *Server) SetPRClientFactory(f PRClientFactory) {
	if s.prH != nil {
		s.prH.setFactory(f)
	}
}

// SetWebAuthnVerifier replaces the WebAuthn assertion verifier on the security middleware.
// This is the test seam: tests inject a stub so they can exercise WrapBoardAction without
// real Touch ID hardware.
func (s *Server) SetWebAuthnVerifier(fn func(r *http.Request, assertion string) error) {
	s.secMid.setWebAuthnVerifier(fn)
}

// SetPairingNotifier replaces the macOS pairing-code dialog with a custom function
// for the WebAuthn registration pairing code. Used in tests and the Playwright e2e suite.
func (s *Server) SetPairingNotifier(fn func(code string) error) {
	if s.webAuthnH != nil {
		s.webAuthnH.SetPairingNotifier(fn)
	}
}

// healthRepoAccess is the repo check summary in /api/health: only the repos
// that failed. /api/health/repos has all of them.
type healthRepoAccess struct {
	Checked      bool                `json:"checked"`
	CheckedAt    *time.Time          `json:"checked_at,omitempty"`
	Inaccessible []repoaccess.Result `json:"inaccessible"`
}

// repoAccessSnapshot returns the latest repo check, or an unchecked snapshot
// when no checker is wired.
func (s *Server) repoAccessSnapshot() repoaccess.Snapshot {
	if s.opts.RepoAccess == nil {
		return repoaccess.Snapshot{Repos: []repoaccess.Result{}}
	}
	snap := s.opts.RepoAccess.Snapshot()
	if snap.Repos == nil {
		snap.Repos = []repoaccess.Result{}
	}
	return snap
}

// writeJSONUnescaped writes v as JSON without HTML escaping, so paths and
// messages stay readable from curl and shell scripts.
func writeJSONUnescaped(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
