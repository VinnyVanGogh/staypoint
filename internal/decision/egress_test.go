package decision

// Egress allowlist for the advisor path (#241: the Touch ID advisor called
// the paid Together API instead of local Ollama). Two layers:
//
//  1. Wiring: no production code outside this package may build a decision
//     client other than NewLocal, and the advisor/governance files that hold
//     a decision client may not do their own networking.
//  2. Dialing: the NewLocal client connects only to loopback (127.0.0.0/8,
//     ::1), ignores proxy env vars, and refuses everything else before a
//     packet leaves the machine.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const decisionImportPath = "github.com/VinnyVanGogh/staypoint/internal/decision"

// remoteCapableCtors are the decision constructors that can reach a
// non-loopback endpoint. Only tests and this package may call them.
var remoteCapableCtors = map[string]bool{"New": true, "NewTogether": true, "NewWithEndpoint": true}

// advisorFiles hold a decision client and must stay network-free: the client
// is their only way out. gates/review.go (the Board-clicked Gemini batch
// review) is deliberately not listed; it is not the Touch ID advisor.
var advisorFiles = []string{
	"internal/gates/advisor.go",
	"internal/governance/advisory.go",
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestEgress_ProductionWiresOnlyNewLocal(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	var bad []string
	scanned, newLocalCalls := 0, 0
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			if filepath.Dir(rel) == filepath.Join("internal", "decision") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			alias := ""
			for _, imp := range f.Imports {
				if p, _ := strconv.Unquote(imp.Path.Value); p == decisionImportPath {
					alias = "decision"
					if imp.Name != nil {
						alias = imp.Name.Name
					}
				}
			}
			if alias == "" {
				return nil
			}
			if alias == "." || alias == "_" {
				bad = append(bad, rel+": imports decision as "+alias+" (calls cannot be checked)")
				return nil
			}
			f, err = parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			scanned++
			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == alias {
					if remoteCapableCtors[sel.Sel.Name] {
						bad = append(bad, fset.Position(sel.Pos()).String()+": decision."+sel.Sel.Name+" can reach a non-loopback endpoint; use decision.NewLocal")
					}
					if sel.Sel.Name == "NewLocal" {
						newLocalCalls++
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	for _, b := range bad {
		t.Error(b)
	}
	// Guard against the scan silently matching nothing (moved module path,
	// wrong root): staypointd wires the gate advisor with NewLocal today.
	if scanned == 0 || newLocalCalls == 0 {
		t.Fatalf("scan found %d files importing decision and %d NewLocal calls; expected the staypointd wiring", scanned, newLocalCalls)
	}
}

func TestEgress_AdvisorFilesDoNoNetworking(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	for _, rel := range advisorFiles {
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if p == "net" || strings.HasPrefix(p, "net/") || p == "os/exec" {
				t.Errorf("%s imports %q: advisor code must reach the model only through decision.NewLocal", rel, p)
			}
		}
	}
}

// loopbackServer starts an httptest server on addr ("127.0.0.1:0", "[::1]:0").
func loopbackServer(t *testing.T, addr string, h http.Handler) *httptest.Server {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("cannot listen on %s: %v", addr, err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: h}}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func TestEgress_DialsLoopbackV4AndV6(t *testing.T) {
	var remote atomic.Value
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote.Store(r.RemoteAddr)
		_, _ = w.Write([]byte(`{"answers":{"decision":{"choice":"approved","probabilities":{"approved":0.9,"denied":0.1}}}}`))
	})
	v4 := loopbackServer(t, "127.0.0.1:0", h)
	_, v4port, _ := net.SplitHostPort(v4.Listener.Addr().String())
	urls := []string{
		v4.URL + "/v1/systemone",
		"http://localhost:" + v4port + "/v1/systemone",
	}
	if ln, err := net.Listen("tcp", "[::1]:0"); err == nil {
		_ = ln.Close()
		urls = append(urls, loopbackServer(t, "[::1]:0", h).URL+"/v1/systemone")
	} else {
		t.Logf("no IPv6 loopback here, skipping [::1]: %v", err)
	}
	for _, u := range urls {
		t.Setenv("DECISION_LOCAL_URL", u)
		c := NewLocal()
		if c.BaseURL() != u {
			t.Fatalf("loopback URL %q not honoured: %q", u, c.BaseURL())
		}
		res, err := c.DecideWithReason(context.Background(), DecisionRequest{State: "s", Question: "q", Options: approveDeny})
		if err != nil || res.SelectedKey != "approved" {
			t.Fatalf("%s: res %+v err %v", u, res, err)
		}
		host, _, _ := net.SplitHostPort(remote.Load().(string))
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			t.Errorf("%s: server saw client address %q, want loopback", u, host)
		}
	}
}

func TestEgress_RefusesEveryNonLoopbackTarget(t *testing.T) {
	c := NewLocal()
	for _, u := range []string{
		"https://api.together.xyz/v1/chat/completions", // the #241 target
		"http://0.0.0.0:11434/v1/systemone",            // "any" address, not loopback
		"http://[::]:11434/v1/systemone",
		"http://[::ffff:192.0.2.1]:9/x", // v4-mapped non-loopback
		"http://169.254.169.254/latest/meta-data/",
		"http://192.168.1.10:11434/v1/systemone",
		"http://[fe80::1]:11434/x",
		"http://[fd00::1]:11434/x",
		"http://8.8.8.8:53/x",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader("{}"))
		resp, err := c.httpClient.Do(req)
		cancel()
		if err == nil {
			_ = resp.Body.Close()
			t.Errorf("POST %s succeeded; the advisor transport must refuse it", u)
			continue
		}
		// A name that does not resolve (offline CI) never dials either.
		if !strings.Contains(err.Error(), "non-loopback") && !strings.Contains(err.Error(), "decision: resolve") {
			t.Errorf("POST %s: err %v, want a loopback-allowlist refusal", u, err)
		}
	}
}

// HTTP(S)_PROXY in the daemon environment must not route advisor traffic
// through a proxy (which could forward it anywhere).
func TestEgress_IgnoresProxyEnv(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxied.Add(1)
		_, _ = w.Write([]byte(`{"answers":{"decision":{"choice":"approved","probabilities":{"approved":1,"denied":0}}}}`))
	}))
	defer proxy.Close()
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(k, proxy.URL)
	}
	t.Setenv("NO_PROXY", "")
	c := NewLocal()
	// http.ProxyFromEnvironment reads the env once per process, so the
	// behavioural check below can pass by test order alone; check the field.
	tr, ok := c.httpClient.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatalf("advisor transport %T must be an *http.Transport with Proxy nil", c.httpClient.Transport)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://192.0.2.1:9/v1/systemone", strings.NewReader("{}"))
	resp, err := c.httpClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("request to TEST-NET via proxy env succeeded; want a non-loopback refusal")
	}
	if !strings.Contains(err.Error(), "non-loopback") {
		t.Errorf("err %v, want a non-loopback refusal", err)
	}
	if proxied.Load() != 0 {
		t.Fatalf("proxy received %d advisor requests", proxied.Load())
	}
}
