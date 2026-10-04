package server

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

const sessionCookieName = "staypoint_session"

// boardCookieName carries the board credential, set only during the browser ?token= bootstrap.
// Agents that don't follow the cookie exchange cannot obtain it without an explicit HTTP
// session with a cookie jar (which the pre-tool hook classifies as Red-tier).
const boardCookieName = "staypoint_board"

// sessionCookieMaxAge is 30 days; long enough that a bookmark just works.
const sessionCookieMaxAge = 30 * 24 * 60 * 60

// SecurityMiddleware returns a middleware that validates Host and Origin headers,
// enforces no-wildcard CORS, and verifies the local auth token.
type SecurityMiddleware struct {
	token        string
	boardToken   string // separate credential required for Board-only mutations
	port         int    // actual bound port, if known (or 0 for any local port)
	corsAllowAll bool   // opt-in: skip origin check and emit wildcard CORS headers

	nonceMu  sync.Mutex
	nonce    string // one-time bootstrap nonce; zeroed after first successful use

	db         *sql.DB    // used by WrapBoardAction to count registered passkeys
	verifierMu sync.RWMutex
	webAuthnVerifier func(r *http.Request, assertion string) error // injectable for tests
}

// NewSecurityMiddleware creates a new SecurityMiddleware.
func NewSecurityMiddleware(token string, port int) *SecurityMiddleware {
	return &SecurityMiddleware{
		token: token,
		port:  port,
	}
}

// NewSecurityMiddlewareWithOpts creates a SecurityMiddleware with extended options.
func NewSecurityMiddlewareWithOpts(token string, port int, corsAllowAll bool) *SecurityMiddleware {
	return &SecurityMiddleware{
		token:        token,
		port:         port,
		corsAllowAll: corsAllowAll,
	}
}

// NewSecurityMiddlewareWithBoardToken creates a SecurityMiddleware with a separate
// board token enforced on Board-only mutation endpoints.
func NewSecurityMiddlewareWithBoardToken(token, boardToken string, port int, corsAllowAll bool) *SecurityMiddleware {
	return &SecurityMiddleware{
		token:        token,
		boardToken:   boardToken,
		port:         port,
		corsAllowAll: corsAllowAll,
	}
}

// SetBoardNonce installs a one-time bootstrap nonce. The first request that presents
// this nonce via ?board_nonce= is granted the board session cookie; the nonce is then
// consumed and cannot be reused. This keeps the long-lived board_token out of URLs.
func (sm *SecurityMiddleware) SetBoardNonce(nonce string) {
	sm.nonceMu.Lock()
	defer sm.nonceMu.Unlock()
	sm.nonce = nonce
}

// FreshNonce validates boardToken against the stored board credential and, if correct,
// mints a new single-use nonce, installs it, and returns it. Returns ("", false) if
// boardToken is empty or does not match. The caller (the staypoint board url CLI command)
// must present both the session auth token and the board token to obtain a nonce; this
// keeps the endpoint from being usable by agents that only hold the session auth token.
func (sm *SecurityMiddleware) FreshNonce(boardToken string) (string, bool) {
	if sm.boardToken == "" || boardToken == "" {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(boardToken), []byte(sm.boardToken)) != 1 {
		return "", false
	}
	nonce, err := GenerateAuthToken()
	if err != nil {
		return "", false
	}
	sm.SetBoardNonce(nonce)
	return nonce, true
}

// consumeNonce returns true and clears the nonce if n matches; false otherwise.
func (sm *SecurityMiddleware) consumeNonce(n string) bool {
	if n == "" {
		return false
	}
	sm.nonceMu.Lock()
	defer sm.nonceMu.Unlock()
	if sm.nonce == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(n), []byte(sm.nonce)) == 1 {
		sm.nonce = "" // consume: single-use
		return true
	}
	return false
}

// BoardToken returns the board-only credential.
func (sm *SecurityMiddleware) BoardToken() string { return sm.boardToken }

// SetDB wires the database so WrapBoardAction can count registered passkeys.
func (sm *SecurityMiddleware) SetDB(db *sql.DB) { sm.db = db }

// setWebAuthnVerifier replaces the WebAuthn assertion verifier (test seam).
func (sm *SecurityMiddleware) setWebAuthnVerifier(fn func(r *http.Request, assertion string) error) {
	sm.verifierMu.Lock()
	defer sm.verifierMu.Unlock()
	sm.webAuthnVerifier = fn
}

// WrapBoardSession wraps a handler requiring only the staypoint_board cookie (no assertion).
// Used for WebAuthn management endpoints that the Board must reach before any passkey exists.
func (sm *SecurityMiddleware) WrapBoardSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !sm.hasBoardCookie(r) {
			writeBoardError(w, "board_session_required", "forbidden: board action requires a Board session")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// WrapBoardAction wraps a handler so it requires the staypoint_board session cookie plus,
// once a passkey is registered, a valid X-WebAuthn-Assertion header. Fail-closed: once any
// passkey is stored, cookie-only requests are rejected even if assertion verification is
// temporarily misconfigured.
func (sm *SecurityMiddleware) WrapBoardAction(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !sm.hasBoardCookie(r) {
			writeBoardError(w, "board_session_required", "forbidden: board action requires a Board session (agent auth token is not sufficient)")
			return
		}

		count := sm.credentialCount()
		if count > 0 {
			assertion := r.Header.Get("X-WebAuthn-Assertion")
			if assertion == "" {
				writeBoardError(w, "board_passkey_assertion_required", "forbidden: Board action requires a WebAuthn assertion")
				return
			}
			sm.verifierMu.RLock()
			verifier := sm.webAuthnVerifier
			sm.verifierMu.RUnlock()
			// Fail-closed: if no verifier is wired, reject all assertions.
			// Tests inject a stub via SetWebAuthnVerifier; production wires the real verifier.
			if verifier == nil {
				writeBoardError(w, "board_passkey_verifier_unavailable", "forbidden: WebAuthn verifier not configured")
				return
			}
			if err := verifier(r, assertion); err != nil {
				writeBoardError(w, "board_passkey_assertion_invalid", "forbidden: WebAuthn assertion verification failed")
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// credentialCount returns the number of rows in board_webauthn_credentials. Returns 0 on any
// error so the fail-open (no passkey enrolled) path remains reachable during bootstrap.
func (sm *SecurityMiddleware) credentialCount() int {
	if sm.db == nil {
		return 0
	}
	var n int
	_ = sm.db.QueryRow(`SELECT COUNT(*) FROM board_webauthn_credentials`).Scan(&n)
	return n
}

// hasBoardCookie checks whether the request carries the staypoint_board cookie.
// When boardToken is empty (test / no credential configured), any cookie presence passes.
func (sm *SecurityMiddleware) hasBoardCookie(r *http.Request) bool {
	c, err := r.Cookie(boardCookieName)
	if err != nil {
		return false
	}
	if sm.boardToken == "" {
		return true // test mode or unconfigured — cookie presence is sufficient
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(sm.boardToken)) == 1
}

// writeBoardError writes a JSON 403 with an error code field.
func writeBoardError(w http.ResponseWriter, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": msg})
}

// SetPort updates the port once the server is listening.
func (sm *SecurityMiddleware) SetPort(port int) {
	sm.port = port
}

// Wrap wraps an http.Handler with full security protections.
func (sm *SecurityMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 1. DNS Rebinding Protection (Host header check)
		// Attacker web pages use DNS rebinding to point attacker domain to 127.0.0.1.
		// The browser sends Host: evil.com. We MUST reject this.
		host := r.Host
		if host == "" {
			host = r.Header.Get("Host")
		}
		if !sm.isValidHost(host) {
			writeError(w, http.StatusForbidden, "forbidden: host not allowed (DNS rebinding protection)")
			return
		}

		// 2. Cross-Origin Protection (Origin header check)
		origin := r.Header.Get("Origin")
		if origin != "" {
			if sm.corsAllowAll {
				// Opt-in permissive mode for browser extensions (e.g. Tampermonkey).
				// Wildcard '*' is intentional here; auth is still enforced in step 3.
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, X-StayPoint-Token, Content-Type, Last-Event-ID")
			} else {
				if !sm.isValidOrigin(origin) {
					writeError(w, http.StatusForbidden, "forbidden: cross-origin request rejected")
					return
				}

				// Valid loopback origin: reflect exact origin, never wildcard.
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, X-StayPoint-Token, Content-Type, Last-Event-ID")
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}

			// Handle preflight OPTIONS request
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}

		// 3. Auth Token Verification
		if !sm.isAuthorized(r) {
			writeError(w, http.StatusUnauthorized, "unauthorized: invalid or missing auth token")
			return
		}

		// 4. Cookie exchange: if browser hit a UI route with ?token=, set the session
		// cookie and redirect to the clean URL. After this, the bookmark works.
		//
		// The board cookie is issued only via ?board_nonce=<nonce> — a one-time value
		// minted per-session. The ?board_token= fallback has been removed (STA-583).
		//
		// Additionally, the board cookie is only granted when the request looks like a
		// browser navigation (Sec-Fetch-Mode: navigate) or the Sec-Fetch headers are
		// absent (direct URL paste). Programmatic fetches (Sec-Fetch-Mode: cors /
		// no-cors / same-origin) are rejected: those come from script-triggered fetch()
		// calls, not a human clicking the URL.
		if qToken := r.URL.Query().Get("token"); qToken != "" && !strings.HasPrefix(r.URL.Path, "/api/") {
			if subtle.ConstantTimeCompare([]byte(qToken), []byte(sm.token)) == 1 {
				http.SetCookie(w, &http.Cookie{
					Name:     sessionCookieName,
					Value:    qToken,
					Path:     "/",
					HttpOnly: true,
					SameSite: http.SameSiteStrictMode,
					MaxAge:   sessionCookieMaxAge,
				})
				if sm.boardToken != "" && sm.consumeNonce(r.URL.Query().Get("board_nonce")) {
					// Only grant the board cookie on browser-like navigations.
					// Sec-Fetch-Mode values for script-triggered requests (cors, no-cors,
					// same-origin) indicate a programmatic call, not a human navigating.
					sfm := strings.ToLower(r.Header.Get("Sec-Fetch-Mode"))
					grantBoard := sfm == "" || sfm == "navigate"
					if grantBoard {
						http.SetCookie(w, &http.Cookie{
							Name:     boardCookieName,
							Value:    sm.boardToken,
							Path:     "/",
							HttpOnly: true,
							SameSite: http.SameSiteStrictMode,
							MaxAge:   sessionCookieMaxAge,
						})
					}
				}
				cleanURL := *r.URL
				q := cleanURL.Query()
				q.Del("token")
				q.Del("board_nonce")
				cleanURL.RawQuery = q.Encode()
				http.Redirect(w, r, cleanURL.String(), http.StatusFound)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// isValidHost checks if the Host header is loopback (127.0.0.1 or localhost).
func (sm *SecurityMiddleware) isValidHost(hostHeader string) bool {
	if hostHeader == "" {
		return false
	}

	h := hostHeader
	p := 0
	if strings.Contains(hostHeader, ":") {
		var portStr string
		var err error
		h, portStr, err = net.SplitHostPort(hostHeader)
		if err != nil {
			return false
		}
		p, _ = strconv.Atoi(portStr)
	}

	h = strings.ToLower(strings.TrimSpace(h))
	if h != "127.0.0.1" && h != "localhost" {
		return false
	}

	// If a specific server port is known and the request included a port, verify it matches
	if sm.port > 0 && p > 0 && p != sm.port {
		return false
	}

	return true
}

// isValidOrigin checks if the Origin header is a valid local loopback origin.
func (sm *SecurityMiddleware) isValidOrigin(originHeader string) bool {
	if originHeader == "" {
		return true
	}

	u, err := url.Parse(originHeader)
	if err != nil {
		return false
	}

	// Must be http:// (or https:// only if local TLS is enabled, but local daemon uses http)
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}

	hostname := strings.ToLower(u.Hostname())
	if hostname != "127.0.0.1" && hostname != "localhost" {
		return false
	}

	// If port is specified and our port is configured, check port match
	if sm.port > 0 && u.Port() != "" {
		p, err := strconv.Atoi(u.Port())
		if err == nil && p != sm.port {
			return false
		}
	}

	return true
}

// isAuthorized verifies the auth token from Bearer header, X-StayPoint-Token header, or ?token= query param.
func (sm *SecurityMiddleware) isAuthorized(r *http.Request) bool {
	if sm.token == "" {
		return false
	}

	// 1. Authorization: Bearer <token>
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(sm.token)) == 1 {
			return true
		}
	}

	// 2. X-StayPoint-Token: <token>
	if xToken := r.Header.Get("X-StayPoint-Token"); xToken != "" {
		if subtle.ConstantTimeCompare([]byte(xToken), []byte(sm.token)) == 1 {
			return true
		}
	}

	// 3. Query param ?token=<token> (needed for browser EventSource / SSE)
	if qToken := r.URL.Query().Get("token"); qToken != "" {
		if subtle.ConstantTimeCompare([]byte(qToken), []byte(sm.token)) == 1 {
			return true
		}
	}

	// 4. Session cookie (set after first successful ?token= visit)
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		if subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(sm.token)) == 1 {
			return true
		}
	}

	return false
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": msg,
	})
}
