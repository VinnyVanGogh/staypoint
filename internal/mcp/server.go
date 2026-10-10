package mcp

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/VinnyVanGogh/staypoint/internal/config"
	"github.com/VinnyVanGogh/staypoint/internal/db"
)

// Server implements a pure Go Model Context Protocol stdio server.
type Server struct {
	cfg     *config.Config
	db      *sql.DB
	store   *db.Store
	workDir string
	// boardPlanDaemonURL overrides the daemon URL for board plan proposals (tests).
	boardPlanDaemonURL string
	mu                 sync.Mutex
	outMu              sync.Mutex
}

// Option configures Server behavior.
type Option func(*Server)

// WithConfig sets the agent-mesh configuration.
func WithConfig(cfg *config.Config) Option {
	return func(s *Server) {
		s.cfg = cfg
	}
}

// WithDB sets an existing database connection.
func WithDB(database *sql.DB) Option {
	return func(s *Server) {
		s.db = database
	}
}

// WithWorkDir sets the server working directory.
func WithWorkDir(dir string) Option {
	return func(s *Server) {
		s.workDir = dir
	}
}

// NewServer creates a new MCP server instance.
func NewServer(opts ...Option) *Server {
	s := &Server{}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Close releases any database resources opened by the server.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.store != nil {
		return s.store.Close()
	}
	return nil
}

func (s *Server) getDB() (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.db != nil {
		return s.db, nil
	}

	if s.cfg == nil {
		loaded, err := config.LoadConfig()
		if err == nil {
			s.cfg = loaded
		} else {
			s.cfg = config.DefaultConfig()
		}
	}

	dbPath := s.cfg.DBPath
	if dbPath == "" {
		dbPath = config.DefaultConfig().DBPath
	}

	store, err := db.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database at %s: %w", dbPath, err)
	}

	s.store = store
	s.db = store.DB()
	return s.db, nil
}

func (s *Server) getWorkDir() string {
	if s.workDir != "" {
		return s.workDir
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}

// Serve reads JSON-RPC requests line-by-line from r and writes responses to w.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 16*1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Bytes()
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		respBytes, err := s.HandleMessage(ctx, line)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mcp: internal processing error: %v\n", err)
		}
		if len(respBytes) > 0 {
			s.outMu.Lock()
			_, writeErr := w.Write(respBytes)
			if writeErr == nil {
				_, writeErr = w.Write([]byte("\n"))
			}
			s.outMu.Unlock()
			if writeErr != nil {
				return fmt.Errorf("mcp: write failed: %w", writeErr)
			}
		}
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		return fmt.Errorf("mcp: scanner error: %w", err)
	}
	return nil
}

// HandleMessage parses a single JSON-RPC message and produces a response if required.
func (s *Server) HandleMessage(ctx context.Context, msg []byte) ([]byte, error) {
	var req Request
	if err := json.Unmarshal(msg, &req); err != nil {
		nullID := json.RawMessage("null")
		return s.makeErrorResponse(&nullID, -32700, fmt.Sprintf("Parse error: %v", err))
	}

	switch req.Method {
	case "initialize":
		result := InitializeResult{
			ProtocolVersion: "2024-11-05",
			Capabilities: ServerCapabilities{
				Tools: map[string]any{},
			},
			ServerInfo: ServerInfo{
				Name:    "staypoint",
				Version: "0.1.0",
			},
		}
		return s.makeResultResponse(req.ID, result)

	case "notifications/initialized":
		if req.ID == nil {
			return nil, nil
		}
		return s.makeResultResponse(req.ID, map[string]any{})

	case "ping":
		return s.makeResultResponse(req.ID, map[string]any{})

	case "tools/list":
		result := ListToolsResult{
			Tools: s.getToolsList(),
		}
		return s.makeResultResponse(req.ID, result)

	case "tools/call":
		var params CallToolParams
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &params); err != nil {
				return s.makeErrorResponse(req.ID, -32602, fmt.Sprintf("Invalid params: %v", err))
			}
		} else {
			return s.makeErrorResponse(req.ID, -32602, "Missing params for tools/call")
		}

		result := s.handleCallTool(ctx, params)
		return s.makeResultResponse(req.ID, result)

	default:
		if req.ID == nil {
			return nil, nil
		}
		return s.makeErrorResponse(req.ID, -32601, fmt.Sprintf("Method not found: %s", req.Method))
	}
}

func (s *Server) makeResultResponse(id *json.RawMessage, result any) ([]byte, error) {
	if id == nil {
		nullID := json.RawMessage("null")
		id = &nullID
	}
	resp := Response{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	return json.Marshal(resp)
}

func (s *Server) makeErrorResponse(id *json.RawMessage, code int, message string) ([]byte, error) {
	if id == nil {
		nullID := json.RawMessage("null")
		id = &nullID
	}
	resp := Response{
		JSONRPC: "2.0",
		ID:      id,
		Error: &RPCError{
			Code:    code,
			Message: message,
		},
	}
	return json.Marshal(resp)
}
