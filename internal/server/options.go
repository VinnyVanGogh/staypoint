package server

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrNonLoopbackBind is returned when an attempt is made to bind to a non-127.0.0.1 address.
	ErrNonLoopbackBind = errors.New("server only binds to 127.0.0.1 (loopback)")
	// ErrInvalidAuthToken is returned when an invalid or missing auth token is presented.
	ErrInvalidAuthToken = errors.New("unauthorized: invalid or missing auth token")
	// ErrDNSRebindingBlocked is returned when Host header validation fails.
	ErrDNSRebindingBlocked = errors.New("forbidden: host not allowed (DNS rebinding protection)")
	// ErrCrossOriginBlocked is returned when Origin header validation fails.
	ErrCrossOriginBlocked = errors.New("forbidden: cross-origin request rejected")
)

// Options holds configuration settings for the HTTP and SSE server.
type Options struct {
	// BindHost MUST be "127.0.0.1". If empty, defaults to "127.0.0.1".
	// Any other value will cause New() to return ErrNonLoopbackBind.
	BindHost string

	// Port to listen on. If 0, an ephemeral port is chosen automatically.
	Port int

	// AuthToken is the shared secret required for all API calls.
	// If empty, New() will load from TokenPath or generate a cryptographically random token.
	AuthToken string

	// TokenPath is the filesystem path to store/read the auth token file.
	// If empty and AuthToken is empty, a random token is generated in memory.
	TokenPath string

	// BoardToken is a separate secret required for Board-only mutation endpoints
	// (ship-review approve/reject/send-back, gate decide, settings POSTs).
	// Agents that only hold AuthToken cannot call these endpoints; only the Board UI
	// (which receives BoardToken embedded in the served HTML) can call them.
	// If empty, Validate() will load from BoardTokenPath or generate one.
	BoardToken string

	// BoardTokenPath is the filesystem path to store/read the board token.
	// If empty and BoardToken is empty, a random board token is generated in memory.
	BoardTokenPath string

	// BoardNonce is a one-time bootstrap nonce generated at server startup.
	// It is set by New() after the middleware is created; callers may read it via
	// Server.BoardNonce() to build the board bootstrap URL without exposing BoardToken in a URL.
	BoardNonce string

	// DB is the SQLite database connection backing the orchestrator.
	DB *sql.DB

	// Hub is an optional pre-existing EventHub. If nil, one will be created.
	Hub *EventHub

	// ReplayBufferSize is the capacity of the SSE replay buffer (default 1000).
	ReplayBufferSize int

	// SubscriberBufferSize is the bounded buffer size per SSE client channel (default 128).
	SubscriberBufferSize int

	// TelemetryDBPath is the path to telemetry.db (default ~/.config/token-telemetry/telemetry.db).
	TelemetryDBPath string

	// GitCommit is the injected git commit hash of the running binary.
	GitCommit string

	// DevBuildReason is non-empty when the running binary is not a reviewed
	// main build (deployed with --allow-dev-build, or built from a tree with
	// uncommitted changes). /api/health reports dev_build and this reason, and
	// the web UI header shows a DEV BUILD badge (STA-805).
	DevBuildReason string

	// RepoAccess, when set, supplies the latest repo access check for
	// /api/health so redeploy scripts can flag repos macOS has blocked.
	RepoAccess RepoAccessReporter

	// CORSAllowAll disables Origin validation so browser extensions (e.g. Tampermonkey
	// userscripts via GM_xmlhttpRequest) can reach the local API from any page origin.
	// Disabled by default; enable via [server] cors_allow_all = true in config.toml.
	CORSAllowAll bool

	// TestMode enables test-only routes (e.g. ship-review seed endpoint).
	// Must never be set in production.
	TestMode bool
}

// GenerateAuthToken generates a 32-byte (64-character hex) cryptographically secure random token.
func GenerateAuthToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate random token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// LoadOrCreateBoardToken loads a board token from disk or generates and saves a new one.
// The board token is a second credential required only for Board-only mutation endpoints.
func LoadOrCreateBoardToken(tokenPath string) (string, error) {
	return LoadOrCreateAuthToken(tokenPath)
}

// LoadOrCreateAuthToken loads an auth token from disk or generates and saves a new one.
func LoadOrCreateAuthToken(tokenPath string) (string, error) {
	if tokenPath != "" {
		if data, err := os.ReadFile(tokenPath); err == nil {
			t := strings.TrimSpace(string(data))
			if len(t) >= 16 {
				return t, nil
			}
		}
	}

	token, err := GenerateAuthToken()
	if err != nil {
		return "", err
	}

	if tokenPath != "" {
		dir := filepath.Dir(tokenPath)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", fmt.Errorf("failed to create token dir: %w", err)
		}
		// Write with 0600 permissions so only owner can read
		if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0600); err != nil {
			return "", fmt.Errorf("failed to write auth token: %w", err)
		}
	}

	return token, nil
}

// Validate validates and sets default options.
func (o *Options) Validate() error {
	if o.BindHost == "" {
		o.BindHost = "127.0.0.1"
	}

	// Strictly enforce 127.0.0.1 binding
	if o.BindHost != "127.0.0.1" {
		// Even localhost or 127.0.0.x is constrained: explicitly require 127.0.0.1
		return fmt.Errorf("%w: attempted to bind to %q", ErrNonLoopbackBind, o.BindHost)
	}

	if o.AuthToken == "" {
		if o.TokenPath != "" {
			tok, err := LoadOrCreateAuthToken(o.TokenPath)
			if err != nil {
				return fmt.Errorf("auth token error: %w", err)
			}
			o.AuthToken = tok
		} else {
			tok, err := GenerateAuthToken()
			if err != nil {
				return fmt.Errorf("generate auth token: %w", err)
			}
			o.AuthToken = tok
		}
	}

	if o.BoardToken == "" {
		if o.BoardTokenPath != "" {
			tok, err := LoadOrCreateBoardToken(o.BoardTokenPath)
			if err != nil {
				return fmt.Errorf("board token error: %w", err)
			}
			o.BoardToken = tok
		} else {
			tok, err := GenerateAuthToken()
			if err != nil {
				return fmt.Errorf("generate board token: %w", err)
			}
			o.BoardToken = tok
		}
	}

	if o.ReplayBufferSize <= 0 {
		o.ReplayBufferSize = 1000
	}

	if o.SubscriberBufferSize <= 0 {
		o.SubscriberBufferSize = 128
	}

	return nil
}

// CheckHostIsLoopback returns true if the host string represents 127.0.0.1 or localhost.
func CheckHostIsLoopback(hostHeader string) bool {
	h := hostHeader
	if strings.Contains(hostHeader, ":") {
		var err error
		h, _, err = net.SplitHostPort(hostHeader)
		if err != nil {
			// Malformed host header with colon
			return false
		}
	}
	h = strings.ToLower(strings.TrimSpace(h))
	return h == "127.0.0.1" || h == "localhost"
}
