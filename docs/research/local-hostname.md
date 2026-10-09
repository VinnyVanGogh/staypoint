# Research Report: Stable Local Hostname Architecture for StayPoint

**Document Version:** 1.0.0  
**Date:** 2026-10-09  
**Target Branch:** `staypoint/task-f9e79cee`  
**Task ID:** `task-f9e79cee`  
**Classification:** Technical Architecture & Security Analysis  

---

## Executive Summary

StayPoint currently binds its HTTP/SSE daemon to `127.0.0.1:41421`. When accessing the StayPoint Board via `http://127.0.0.1:41421`, WebAuthn passkey operations (e.g. Touch ID approvals for Rhizome tasks, organization trust, and gate overrides) fail with `SecurityError: This is an invalid domain.` (STA-785, `task-f611ea4e`, `task-0e4bc7b1`). Furthermore, raw IP addresses and unbranded localhost URLs lack a cohesive, memorable developer experience.

This report evaluates five architectural options to provide a single, stable, memorable local hostname for StayPoint:
1. **Option (a): `staypoint.localhost` on the existing port (`41421`)** — **RECOMMENDED**
2. **Option (b): Static `/etc/hosts` entry (e.g. `staypoint.test` or `staypoint.local`)**
3. **Option (c): Wildcard resolver via `dnsmasq` or `/etc/resolver/test`**
4. **Option (d): Local reverse proxy (Caddy with local CA) on port 443**
5. **Option (e): Public domain pointing to `127.0.0.1` (e.g. `local.staypoint.io`)**

### Key Findings
- **Option (a) (`staypoint.localhost:41421`) is the decisive winner.** Under RFC 6761 and W3C Secure Contexts, `*.localhost` is recognized as a secure context over plain HTTP, allowing WebAuthn passkeys to work with **zero TLS setup, zero local CAs, zero Keychain modifications, zero sudo privileges, and zero configuration on macOS**.
- Options using custom TLDs (`.test`, `.local`) or public domains over plain HTTP **fail completely for WebAuthn** because browsers refuse to treat them as Secure Contexts without TLS.
- Options requiring local TLS and custom Root CAs (Option d) introduce substantial security attack surfaces (Keychain root trust, private key exposure risks), port 443 binding conflicts with other local dev tooling, and fragile background daemon lifecycles.
- Modern macOS (Darwin 24/25/26) resolves `*.localhost` to `127.0.0.1` and `::1` out of the box via system directory services (`mDNSResponder` / `libsystem_info`), matching internal browser resolvers in Chromium and Firefox.

---

## 1. WebAuthn, Passkeys, and Secure Contexts

### 1.1 WebAuthn RP ID Specification Rules
WebAuthn Relying Party Identifier (RP ID) rules are defined in **W3C Web Authentication: An API for accessing Public Key Credentials Level 2 & Level 3**, specifically Section 5.4.3 ("Relying Party Parameters for Credential Generation") and Section 13.4.4 ("Relying Party Identifier Validation"):

1. **Explicit Ban on IP Addresses:**  
   The specification states:
   > *"The RP ID MUST be a valid domain string and MUST NOT be an IP address."*
   
   When a user visits `http://127.0.0.1:41421/`, the document's effective domain is the IPv4 string `127.0.0.1`. Attempting to register or authenticate a passkey causes browsers to throw a DOM `SecurityError` ("This is an invalid domain.") before any Touch ID prompt can appear.

2. **Effective Domain Matching:**  
   An RP ID must be either:
   - Exactly equal to the caller's origin's effective domain, OR
   - A registrable domain suffix of, and not a public suffix of, the origin's effective domain.

3. **Port and Scheme Exclusion:**  
   An RP ID is strictly a host string; it never includes schemes (`http://`), port numbers (`:41421`), or paths.

### 1.2 Secure Context Requirements (W3C Secure Contexts)
Under the **W3C Secure Contexts Specification (§3.1 "Is origin potentially trustworthy?")**, the Web Authentication API (`navigator.credentials.create()` and `navigator.credentials.get()`) is only exposed in secure contexts:

- **Loopback IP Addresses:** `127.0.0.0/8` and `::1/128` are defined as potentially trustworthy over HTTP.
- **Localhost Names:** An origin is potentially trustworthy if its host is `localhost` or falls under `.localhost` (per `draft-ietf-dnsop-let-localhost-be-localhost` and W3C Secure Contexts §3.1 Item 4).
- **Custom / Other Domains (`.test`, `.local`, `.internal`, or real domains):** These are **NOT** considered secure contexts over unencrypted HTTP. If accessed via `http://staypoint.test:41421`, `window.isSecureContext` is `false`, and `navigator.credentials` is undefined or throws an immediate security error.

### 1.3 Hostname Comparison for Passkeys

| Hostname Format | Valid WebAuthn RP ID? | Secure Context over HTTP (no TLS)? | WebAuthn Works over Plain HTTP? | Primary Source |
| :--- | :--- | :--- | :--- | :--- |
| `127.0.0.1` | **No** (forbidden IP) | Yes | **No** (Throws `SecurityError: invalid domain`) | W3C WebAuthn Level 3 §5.4.3 |
| `localhost` | **Yes** (`"localhost"`) | Yes | **Yes** | W3C Secure Contexts §3.1, W3C WebAuthn §13.4.4 |
| `staypoint.localhost` | **Yes** (`"staypoint.localhost"`) | Yes | **Yes** | RFC 6761 §6.3, W3C Secure Contexts §3.1 |
| `staypoint.test` | **Yes** (`"staypoint.test"`) | **No** (Requires HTTPS) | **No** (Blocked: Insecure Context) | RFC 2606 §2, W3C Secure Contexts §3.1 |
| `staypoint.local` | **Yes** (`"staypoint.local"`) | **No** (Requires HTTPS) | **No** (Blocked: Insecure Context) | RFC 6762 §3, W3C Secure Contexts §3.1 |
| Real Domain (`local.staypoint.io`) | **Yes** (`"staypoint.io"`) | **No** (Requires HTTPS) | **No** (Blocked: Insecure Context) | W3C Secure Contexts §3.1, CA/B Forum Baseline Requirements |

---

## 2. Loopback Resolution of `*.localhost` on macOS

### 2.1 RFC Standards: RFC 6761 vs `let-localhost-be-localhost`
- **RFC 6761 ("Special-Use Domain Names", Section 6.3):**  
  States that `localhost.` and any name falling within `.localhost.` are special. Name resolution APIs and libraries *SHOULD* recognize localhost names as special and return loopback addresses (`127.0.0.1`, `::1`). Caching DNS servers *SHOULD NOT* send queries for localhost names to upstream forwarders.
- **IETF Draft (`draft-ietf-dnsop-let-localhost-be-localhost`):**  
  Strengthens this by mandating that stub resolvers and applications resolve `*.localhost` strictly to loopback internally without consulting network DNS.

### 2.2 Client-by-Client Resolution Behavior on macOS

1. **Google Chrome & Microsoft Edge (Chromium):**  
   - **Behavior:** Resolves `*.localhost` to `127.0.0.1` and `::1` internally with **zero setup**.  
   - **Implementation:** Chromium's `HostResolverManager` (`net/dns/host_resolver_manager.cc`) checks if the hostname ends with `.localhost`. If true, it intercepts the query before issuing any system call or network request and immediately yields loopback addresses.
   - **Source:** [Chromium Source: HostResolverManager](https://source.chromium.org/chromium/chromium/src/+/main:net/dns/host_resolver_manager.cc).

2. **Mozilla Firefox (Gecko):**  
   - **Behavior:** Resolves `*.localhost` to loopback internally with **zero setup**.  
   - **Implementation:** Firefox controls this via `network.dns.native-is-localhost` (default `true`), resolving all `.localhost` domains internally without issuing OS DNS queries.
   - **Source:** [Mozilla Bugzilla #1220885](https://bugzilla.mozilla.org/show_bug.cgi?id=1220885).

3. **Apple Safari (WebKit on macOS):**  
   - **Behavior:** Safari delegates all name resolution to the macOS system network framework (`CFNetwork` / `NSURLSession` / `getaddrinfo`).  
   - **System Verification on macOS (Darwin 25/26):**  
     Running `dscacheutil -q host -a name staypoint.localhost` outputs:
     ```text
     name: localhost
     ipv6_address: ::1
     name: localhost
     ip_address: 127.0.0.1
     ```
     macOS Directory Services and `mDNSResponder` synthesize loopback responses for `*.localhost`. Therefore, Safari successfully navigates to `http://staypoint.localhost:41421` without configuration.  
   - **Historical Gap:** On older macOS releases (macOS 12 Monterey and earlier), `mDNSResponder` did not synthesize `*.localhost` wildcard records (WebKit Bug 160504). Modern macOS versions natively resolve it.

4. **cURL (`/usr/bin/curl`):**  
   - **Behavior:** Uses system `getaddrinfo()`. On modern macOS, `curl http://staypoint.localhost:41421/api/health` immediately resolves `staypoint.localhost` to `::1` and `127.0.0.1` without manual hosts configuration.
   - **Dual-Stack Order:** cURL queries both AAAA and A records. If the server only listens on IPv4 (`127.0.0.1`), cURL attempts `::1` first, receives `connection refused`, and falls back to `127.0.0.1` within < 1 ms (Happy Eyeballs / RFC 8305).

5. **Go Standard Library (`net` package):**  
   - **Default cgo resolver (macOS default):** Calls `getaddrinfo()` in `libSystem.B.dylib`, successfully resolving `staypoint.localhost`.
   - **Pure Go resolver (`CGO_ENABLED=0` or `GODEBUG=netdns=go`):**  
     **NOTABLE GAP:** Go's pure Go resolver parses `/etc/hosts` (which contains only `127.0.0.1 localhost` without wildcards) and then queries the upstream DNS nameservers from `/etc/resolv.conf`. Because public DNS returns `NXDOMAIN` for `.localhost`, pure Go resolution fails with `no such host` (see Go Issue #78485).
   - **Impact on StayPoint:** The `staypoint` binary is compiled dynamically with cgo linked against `/usr/lib/libSystem.B.dylib`, so native Go calls succeed. However, internal daemon connections from hooks and CLI tools should continue targeting `127.0.0.1:41421` directly to avoid pure-Go resolver edge cases.

---

## 3. Comparison of Architectural Options

| Evaluation Criteria | (a) `staypoint.localhost:41421` | (b) `/etc/hosts` Entry (`staypoint.test`) | (c) `dnsmasq` / `/etc/resolver` (`*.test`) | (d) Reverse Proxy (Caddy) on Port 443 | (e) Public Domain (`local.staypoint.io`) |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **Setup Steps** | **Zero OS setup** (software update only) | Edit `/etc/hosts`, flush DNS cache | Install `dnsmasq`, configure daemon, create `/etc/resolver/test` | Install Caddy, configure Caddyfile, install Root CA in Keychain, start launchd service | Register domain, configure public DNS A/AAAA records to loopback |
| **Requires `sudo`?** | **No** (0% root) | **Yes** (root write to `/etc/hosts`) | **Yes** (root write to `/etc/resolver/` and port 53 daemon) | **Yes** (port 443 binding, Keychain CA trust) | **No** |
| **TLS Story** | **No TLS needed** (Secure Context over plain HTTP) | Needs local CA + HTTPS for passkeys | Needs local CA + HTTPS for passkeys | Full HTTPS via local CA | Broken: Needs public CA + private key distribution |
| **Passkey Compatibility** | **100% Native** | Fails over HTTP; works over HTTPS | Fails over HTTP; works over HTTPS | Works over HTTPS | Fails over HTTP; fragile over HTTPS |
| **Survives OS Updates?** | **100% Yes** | Often overwritten by macOS updates | Flaky; `/etc/resolver` often breaks on upgrades | CA may need re-trusting; launchd plist maintenance | Yes (subject to internet connectivity) |
| **Survives Offline / Airplanes?** | **100% Yes** | Yes | Yes | Yes | **No** (Fails when DNS cache expires while offline) |
| **Impact on Hooks / CLI** | None (hooks use `127.0.0.1:41421`) | None | Depends on dnsmasq health | Incurs TLS handshake overhead & proxy hop | Leaks DNS queries; fails offline |
| **Failure Modes** | Port collision on 41421 (same as today) | Hosts file wiped; typo breaks resolution | Daemon crash; port 53 conflict; VPN collision | Port 443 in use by Docker/Apache; CA expired/untrusted | DNS rebinding blockers; PNA blocks; CA revocation |

---

## 4. StayPoint Codebase Audit: Current URL & Hostname References

The following comprehensive code map identifies every location where StayPoint builds, parses, or validates its URL, hostname, port, and WebAuthn origin today:

### 4.1 Startup Banners and URL Construction
1. `internal/server/server.go:422-425`:
   ```go
   // URL returns the base URL of the running server (e.g. http://127.0.0.1:41421).
   func (s *Server) URL() string {
       return fmt.Sprintf("http://127.0.0.1:%d", s.Port())
   }
   ```
   *Analysis:* Hardcoded to `127.0.0.1`. Must be updated to return the canonical hostname (e.g. `http://staypoint.localhost:41421`).

2. `cmd/staypointd/main.go:285-296`:
   ```go
   slog.Info("HTTP and SSE server active", slog.String("url", httpServer.URL()), ...)
   boardURL := fmt.Sprintf("%s/?token=%s&board_nonce=%s", httpServer.URL(), httpServer.Token(), httpServer.BoardNonce())
   fmt.Printf("  Board URL:  %s\n", boardURL)
   ```
   *Analysis:* Prints `httpServer.URL()`. Changes automatically once `server.URL()` is updated.

3. `cmd/staypoint/daemon_cmd.go:188-199`:
   ```go
   fmt.Printf("  URL:        %s\n", srv.URL())
   boardURL := fmt.Sprintf("%s/?token=%s&board_nonce=%s", srv.URL(), srv.Token(), srv.BoardNonce())
   fmt.Printf("  Board URL:  %s\n", boardURL)
   ```
   *Analysis:* Prints `srv.URL()`.

4. `cmd/staypoint/board_cmd.go:160-169`:
   ```go
   // Emit localhost (not 127.0.0.1) so the RPID "localhost" matches the WebAuthn origin.
   u, err := url.Parse(daemonURL)
   localhostURL := "http://localhost"
   if port := u.Port(); port != "" {
       localhostURL = fmt.Sprintf("http://localhost:%s", port)
   }
   fmt.Printf("%s/?token=%s&board_nonce=%s\n", localhostURL, authToken, result.Nonce)
   ```
   *Analysis:* Explicitly rewrites the URL to `localhost` to work around passkey errors. Must be updated to emit `staypoint.localhost`.

5. `cmd/staypoint/web_cmd.go:36-41, 52`:
   ```go
   webCmd.Flags().String("daemon-url", "http://127.0.0.1:41421", "StayPoint daemon base URL")
   openURL := daemonURL + "/?token=" + token
   bookmarkURL := daemonURL + "/"
   ```
   *Analysis:* Defaults to `127.0.0.1:41421`. Launches the browser to that URL.

### 4.2 Web UI Links & Navigation
1. `internal/server/webui/app.js:212, 259, 296, 317, 4880`:
   All API fetches use relative URLs (`/api/...`). They naturally inherit whatever hostname is in the browser address bar (`staypoint.localhost:41421`).
2. `internal/server/webui/app.js:4968`:
   ```javascript
   { label: 'API Endpoint', val: window.location.host }
   ```
   Displays the current host in the Settings UI.
3. `internal/server/webui/app.js:9418`:
   ```javascript
   (u.hostname === 'localhost' || u.hostname === '127.0.0.1' || u.hostname === '::1' || u.hostname.startsWith('127.'))
   ```
   Ship Review preview link validation.

### 4.3 Security Middleware: Host, Origin, and CORS Checks
1. `internal/server/middleware.go:470-499` (`isValidHost`):
   ```go
   h = strings.ToLower(strings.TrimSpace(h))
   if h != "127.0.0.1" && h != "localhost" {
       return false
   }
   if sm.port > 0 && p > 0 && p != sm.port {
       return false
   }
   ```
   *Analysis:* **CRITICAL BLOCKER.** `isValidHost()` currently rejects `staypoint.localhost` with HTTP 403 `{"error":"forbidden: host not allowed (DNS rebinding protection)"}`. Must be updated to allow `staypoint.localhost`.

2. `internal/server/middleware.go:502-531` (`isValidOrigin`):
   ```go
   hostname := strings.ToLower(u.Hostname())
   if hostname != "127.0.0.1" && hostname != "localhost" {
       return false
   }
   ```
   *Analysis:* **CRITICAL BLOCKER.** `isValidOrigin()` rejects `Origin: http://staypoint.localhost:41421`. Must be updated to allow `staypoint.localhost`.

3. `internal/server/middleware.go:431-462` (Cookie Exchange):
   Issues host-only cookies and redirects to `cleanURL` (preserving the requested Host header).

### 4.4 WebAuthn RP ID and Origin Configuration
1. `internal/server/handlers_webauthn.go:201-209`:
   ```go
   origin := "http://localhost"
   if h.port > 0 {
       origin = fmt.Sprintf("http://localhost:%d", h.port)
   }
   wa, err := webauthn.New(&webauthn.Config{
       RPDisplayName: "StayPoint Board",
       RPID:          "localhost",
       RPOrigins:     []string{origin},
   })
   ```
   *Analysis:* **CRITICAL BLOCKER.** 
   - `RPID` is hardcoded to `"localhost"`.
   - `RPOrigins` contains only `"http://localhost:<port>"`.
   - When loaded on `staypoint.localhost:41421`, the browser sends `origin: "http://staypoint.localhost:41421"`, causing `go-webauthn` to abort with an origin mismatch error.
   - For `staypoint.localhost`, `RPID` must be configured to `"staypoint.localhost"` and `RPOrigins` must include `http://staypoint.localhost:41421`.

### 4.5 CLI and Hook Clients
1. `cmd/staypoint/hook_cmd.go:820-829`:
   ```go
   func resolveDaemonConn() (daemonURL, token string) {
       port := 41421
       daemonURL = fmt.Sprintf("http://127.0.0.1:%d", port)
       ...
   }
   ```
   Used by pre-tool hooks (`createGateRequest`, `waitGateDecision`).
2. `cmd/staypoint/gate_gemini_code.go:36`, `cmd/staypoint/gate_override.go:49`, `cmd/staypoint/hook_gemini_gate.go:271`:
   All call `resolveDaemonConn()`.
3. `internal/security/boardrules.go:90-93, 200-216`:
   ```go
   const StayPointPort = "41421"
   var bareStayPointRe = regexp.MustCompile(`(?i)(127\.0\.0\.1|localhost|\[::1\]|0\.0\.0\.0):41421\b`)
   ```
   `isLocalURL()` already supports `.localhost` subdomains (`strings.HasSuffix(h, ".localhost")`), but `bareStayPointRe` requires updating to detect `staypoint.localhost:41421` if written without a scheme.

### 4.6 MCP Configuration
1. `~/.gemini/config/mcp_config.json:77-83`:
   The MCP server is launched as a sub-process via `staypoint mcp` communicating over stdio. In addition, the daemon runs an IPC listener on a local Unix socket (`cmd/staypointd/main.go:226-240`). **MCP does not use HTTP.** It is completely decoupled from the HTTP hostname.

### 4.7 Notifications
1. `internal/telemetry/notifier.go:204-218`:
   macOS notifications (`display notification ...`) currently deliver short text strings for quota resets and breaker events without URLs.

---

## 5. Security & Threat Modeling

### 5.1 DNS Rebinding Protection
- **The Threat:** A malicious web page visited by the user executes JavaScript that targets `attacker.com`. Attacker DNS returns an external IP with a 1-second TTL, followed by `127.0.0.1`. The browser re-queries the attacker domain, sends the request to `127.0.0.1:41421`, and transmits `Host: attacker.com`. Without strict validation, the attacker page could execute arbitrary gate decisions.
- **StayPoint Defense:** `middleware.go:isValidHost()` enforces an explicit whitelist.
- **Rules for `staypoint.localhost`:**
  - Whitelist **exact** matching only: `127.0.0.1`, `localhost`, `staypoint.localhost`, and `[::1]`.
  - **NEVER** use permissive prefix/suffix matching (e.g. `strings.Contains(host, "localhost")`), which would allow attacker domains like `evil-localhost.com` or `staypoint.localhost.attacker.com`.
  - Under RFC 6761 and browser security models, `.localhost` names are never queried over public DNS, preventing external DNS rebinding.

### 5.2 Cookie Scoping and Cross-App Isolation
- **The Vulnerability with `localhost`:**  
  Under RFC 6265 §8.5, browsers do not isolate cookies by port. Any local development server running on `localhost:3000`, `localhost:8080`, or `localhost:5173` can read and overwrite non-HttpOnly cookies set on `localhost:41421`.
- **The Advantage of `staypoint.localhost`:**  
  StayPoint sets host-only cookies (no `Domain` attribute). A host-only cookie set on `staypoint.localhost:41421` is **strictly isolated** to the `staypoint.localhost` host. Other local applications running on `localhost:<port>` or `otherapp.localhost:<port>` cannot access or poison StayPoint's session or board cookies.

### 5.3 Risks of Local Certificate Authorities (Evaluating Option d)
Installing a local Root CA into the macOS Keychain (required by Caddy or mkcert) introduces severe risks:
1. **System-Wide Trust:** A root CA in the macOS System Keychain is trusted to sign certificates for any domain on the internet (`google.com`, `apple.com`, `github.com`).
2. **Key Compromise:** If an agent or untrusted script gains read access to the local CA private key on disk, the machine is vulnerable to silent local TLS interception.
3. **Keychain Pollution:** CAs do not clean up automatically on uninstall and require manual administrative intervention.
*Conclusion:* Avoiding local TLS/CAs entirely by using `staypoint.localhost` eliminates this critical attack surface.

### 5.4 Binding Exclusively to Loopback
The StayPoint HTTP daemon must continue listening strictly on `127.0.0.1:41421` (and optionally `[::1]:41421` for dual-stack loopback). It must **never** bind to `0.0.0.0` or public network interfaces. This ensures the daemon is completely unreachable from the local area network (LAN), even on untrusted public Wi-Fi networks.

---

## 6. Migration Plan

To ensure zero downtime, existing bookmarks and automation must continue functioning seamlessly during the rollout.

### Phase 1: Dual-Origin Backend Readiness
1. **Middleware Whitelisting:** Update `isValidHost()` and `isValidOrigin()` in `internal/server/middleware.go` to accept `staypoint.localhost` in addition to `127.0.0.1` and `localhost`.
2. **Multi-Origin WebAuthn:**
   Configure `internal/server/handlers_webauthn.go` with:
   - `RPOrigins`: `[]string{"http://localhost:41421", "http://staypoint.localhost:41421"}`.
   - Dynamic RP ID handling based on request Host:
     - If Host is `staypoint.localhost[:port]`, use `RPID: "staypoint.localhost"`.
     - If Host is `localhost[:port]`, use `RPID: "localhost"`.
   This ensures that existing passkeys enrolled under `"localhost"` continue working while new passkeys can be enrolled under `"staypoint.localhost"`.

### Phase 2: Canonical Browser Redirection
1. **Frontend / Middleware Redirect:**
   When a user navigates to `http://127.0.0.1:41421/` or `http://localhost:41421/` in a browser (`Sec-Fetch-Mode: navigate`), the server issues an HTTP 307 redirect to `http://staypoint.localhost:41421/`, preserving query parameters (`?token=...&board_nonce=...`) and paths.
2. **API Exemption:**
   Requests with `/api/` prefixes or explicit authorization headers (`Authorization: Bearer`, `X-StayPoint-Token`) are **never redirected**. Hook scripts and CLI clients communicating with `http://127.0.0.1:41421` remain untouched and execute with zero latency.

### Phase 3: URL Output & Tooling Updates
1. Update `server.URL()` in `internal/server/server.go` to return `http://staypoint.localhost:41421`.
2. Update `staypoint web` (`cmd/staypoint/web_cmd.go`) to open `http://staypoint.localhost:41421`.
3. Update `staypoint board url` (`cmd/staypoint/board_cmd.go`) to emit `http://staypoint.localhost:41421`.
4. Update `bareStayPointRe` in `internal/security/boardrules.go` to recognize `staypoint.localhost:41421`.

---

## 7. Comprehensive Test Plan

The test plan prioritizes failure modes and adversarial cases before happy-path verification.

### 7.1 Adversarial & Failure Cases
1. **Adversarial Host Header (DNS Rebinding):**  
   - Request: `curl -H "Host: evil.com" http://127.0.0.1:41421/api/health`  
   - Expected: `403 Forbidden` (`forbidden: host not allowed (DNS rebinding protection)`).
2. **Adversarial Suffix Spoofing:**  
   - Request: `curl -H "Host: staypoint.localhost.evil.com" http://127.0.0.1:41421/api/health`  
   - Expected: `403 Forbidden`.
3. **Cross-Origin Attack from Untrusted Local App:**  
   - Request: `curl -H "Origin: http://malicious.localhost:3000" http://127.0.0.1:41421/api/health`  
   - Expected: `403 Forbidden` (`forbidden: cross-origin request rejected`).
4. **Port Mismatch in Host Header:**  
   - Request: `curl -H "Host: staypoint.localhost:9999" http://127.0.0.1:41421/api/health`  
   - Expected: `403 Forbidden`.
5. **WebAuthn Ceremony Attempted from Direct IP:**  
   - Verification: Navigating directly to `http://127.0.0.1:41421` redirects to `http://staypoint.localhost:41421`. If an assertion is forced on `127.0.0.1`, browser throws expected domain error without crashing backend.
6. **Pure-Go Resolver / Static Binary Simulation:**  
   - Verification: Running a client with `GODEBUG=netdns=go` against `http://staypoint.localhost:41421` verifies failure behavior; confirm that default CLI/hook transport using `127.0.0.1:41421` bypasses this limitation.
7. **Offline / Airplane Mode Resolution:**  
   - Verification: Disconnect network interfaces (`networksetup -setairportpower en0 off`). Confirm `dscacheutil -q host -a name staypoint.localhost` and Safari still resolve to loopback.

### 7.2 Functional & Regression Cases
8. **Host Whitelist Acceptance:**  
   - Request: `curl -H "Host: staypoint.localhost:41421" http://127.0.0.1:41421/api/health`  
   - Expected: `200 OK` `{"status":"ok"}`.
9. **Origin Header Acceptance:**  
   - Request: `curl -H "Origin: http://staypoint.localhost:41421" http://127.0.0.1:41421/api/health`  
   - Expected: `200 OK` with `Access-Control-Allow-Origin: http://staypoint.localhost:41421`.
10. **WebAuthn Registration & Touch ID Assertion:**  
    - Execute `boardEnrollPasskey` and `boardWebAuthnGetAssertion` on `http://staypoint.localhost:41421`.
    - Verify registration succeeds and Touch ID prompt fires without error.
11. **Cookie Isolation:**  
    - Inspect `Set-Cookie` headers on bootstrap; verify cookies are host-only on `staypoint.localhost` without `Domain=` attribute.
12. **CLI & Hook Backward Compatibility:**  
    - Run `staypoint hook` and verify gate request creation/polling against `http://127.0.0.1:41421` succeeds with zero regressions.
13. **Daemon Startup Logging:**  
    - Verify `cmd/staypointd/main.go` and `cmd/staypoint/daemon_cmd.go` print `http://staypoint.localhost:41421` in terminal output while masking secrets in log files.

---

## 8. Summary of Primary Citations

1. **W3C Web Authentication Level 2 & Level 3:**  
   - [§5.4.3 Relying Party Parameters](https://www.w3.org/TR/webauthn-2/#sctn-rp-credential-params)  
   - [§13.4.4 Validating Relying Party Identifier](https://www.w3.org/TR/webauthn-2/#sctn-validating-rp-id)
2. **W3C Secure Contexts:**  
   - [§3.1 Is origin potentially trustworthy?](https://w3c.github.io/webappsec-secure-contexts/#is-origin-trustworthy)
3. **IETF RFCs & Standards Drafts:**  
   - [RFC 6761: Special-Use Domain Names (§6.3 Domain Name "localhost")](https://datatracker.ietf.org/doc/html/rfc6761#section-6.3)  
   - [RFC 2606: Reserved Top Level DNS Names (§2 Reserved TLDs)](https://datatracker.ietf.org/doc/html/rfc2606)  
   - [RFC 6762: Multicast DNS (§3 .local Domain)](https://datatracker.ietf.org/doc/html/rfc6762)  
   - [RFC 6265: HTTP State Management Mechanism (§8.5 Weak Confidentiality and Integrity)](https://datatracker.ietf.org/doc/html/rfc6265)  
   - [draft-ietf-dnsop-let-localhost-be-localhost](https://datatracker.ietf.org/doc/html/draft-ietf-dnsop-let-localhost-be-localhost)
4. **Browser & OS Implementations:**  
   - [Chromium HostResolverManager](https://source.chromium.org/chromium/chromium/src/+/main:net/dns/host_resolver_manager.cc)  
   - [Mozilla Bugzilla #1220885: Treat *.localhost as loopback](https://bugzilla.mozilla.org/show_bug.cgi?id=1220885)  
   - [WebKit Bugzilla #160504: Localhost subdomains resolution](https://bugs.webkit.org/show_bug.cgi?id=160504)  
   - [Apple Open Source mDNSResponder](https://github.com/apple-oss-distributions/mDNSResponder)
