package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// Server is the StayPoint local HTTP and SSE daemon server.
type Server struct {
	opts       Options
	listener   net.Listener
	httpServer *http.Server
	hub        *EventHub
	secMid     *SecurityMiddleware
	addr       string
	port       int
	mu         sync.Mutex
	running    bool
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

	s := &Server{
		opts:   opts,
		hub:    hub,
		secMid: secMid,
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
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","version":"1.0","git_commit":%q,"commit":%q}`, s.opts.GitCommit, s.opts.GitCommit)
	})

	// Tasks REST API
	if s.opts.DB != nil {
		tasksH := NewTasksHandler(s.opts.DB, s.hub)
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
		mux.HandleFunc("GET /api/security/gate-requests", gateH.ListGateRequests)
		mux.HandleFunc("POST /api/security/gate-requests", gateH.CreateGateRequest)
		mux.HandleFunc("GET /api/security/gate-requests/{id}", gateH.GetGateRequest)
		mux.HandleFunc("GET /api/security/gate-requests/{id}/audit-log", gateH.ListGateAuditLog)
		// Board-only: deciding a gate request requires the board token so agents cannot self-approve.
		mux.Handle("POST /api/security/gate-requests/{id}/decide", s.secMid.WrapBoardAction(http.HandlerFunc(gateH.DecideGateRequest)))
		mux.HandleFunc("GET /api/settings/security-gate", gateH.GetSecurityGateSettings)
		// Board-only: toggling the gate itself requires the board token.
		mux.Handle("POST /api/settings/security-gate", s.secMid.WrapBoardAction(http.HandlerFunc(gateH.UpdateSecurityGateSettings)))

		// Ship Review REST API (Board-approval gate for agent branch merges)
		shipH := NewShipReviewHandler(s.opts.DB, s.hub)
		mux.HandleFunc("GET /api/tasks/{id}/ship-review", shipH.GetCard)
		mux.HandleFunc("PUT /api/tasks/{id}/ship-review", shipH.UpsertCard)
		mux.HandleFunc("POST /api/tasks/{id}/ship-review/start-dev", shipH.StartDev)
		mux.HandleFunc("POST /api/tasks/{id}/ship-review/stop-dev", shipH.StopDev)
		// Board-only: these three actions merge / reject / revise the branch — agents cannot call them.
		mux.Handle("POST /api/tasks/{id}/ship-review/approve", s.secMid.WrapBoardAction(http.HandlerFunc(shipH.Approve)))
		mux.Handle("POST /api/tasks/{id}/ship-review/send-back", s.secMid.WrapBoardAction(http.HandlerFunc(shipH.SendBack)))
		mux.Handle("POST /api/tasks/{id}/ship-review/reject", s.secMid.WrapBoardAction(http.HandlerFunc(shipH.Reject)))
		mux.HandleFunc("GET /api/settings/ship-review", shipH.GetSettings)
		// Board-only: disabling ship review is a Board action.
		mux.Handle("POST /api/settings/ship-review", s.secMid.WrapBoardAction(http.HandlerFunc(shipH.SetSettings)))
		mux.HandleFunc("GET /api/project-dev-configs", shipH.ListProjectDevConfigs)
		mux.HandleFunc("PUT /api/project-dev-configs", shipH.UpsertProjectDevConfig)
		if s.opts.TestMode {
			mux.HandleFunc("PUT /api/tasks/{id}/ship-review/seed", shipH.SeedCard)
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
	}

	s.running = true

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
