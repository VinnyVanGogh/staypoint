package quota

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mapKeychain answers by service name; services not in the map are missing,
// which is what `security` reports for an absent item.
type mapKeychain struct {
	items map[string]string
	asked []string
}

func (k *mapKeychain) Read(_ context.Context, service string) (string, error) {
	k.asked = append(k.asked, service)
	if v, ok := k.items[service]; ok {
		return v, nil
	}
	return "", ErrNoCredentials
}

func TestClaudeKeychainService(t *testing.T) {
	if got := ClaudeKeychainService(""); got != "Claude Code-credentials" {
		t.Errorf("default profile service = %q", got)
	}
	// Known value on the dev Mac: printf %s /Users/vincevasile/.claude-work | shasum -a 256 -> 4b3e4e6a...
	if got := ClaudeKeychainService("/Users/vincevasile/.claude-work"); got != "Claude Code-credentials-4b3e4e6a" {
		t.Errorf("work service = %q, want Claude Code-credentials-4b3e4e6a", got)
	}
	dir := "/tmp/some/where/.claude-work"
	sum := sha256.Sum256([]byte(dir))
	want := "Claude Code-credentials-" + hex.EncodeToString(sum[:])[:8]
	if got := ClaudeKeychainService(dir); got != want {
		t.Errorf("service(%q) = %q, want %q", dir, got, want)
	}
	if got := ClaudeKeychainService(dir + "/"); got != want {
		t.Errorf("trailing slash must not change the hash: %q vs %q", got, want)
	}
}

func TestNewClaudeSeatFetchers_WorkOnlyWhenDirExists(t *testing.T) {
	home := t.TempDir()
	fs := NewClaudeSeatFetchers(home)
	if len(fs) != 1 || fs[0].Provider() != ProviderClaudePersonal {
		t.Fatalf("without ~/.claude-work want only personal, got %d fetchers", len(fs))
	}
	pers := fs[0].(*ClaudeFetcher)
	if pers.Service != KeychainServiceClaude || pers.LegacyProvider() != ProviderClaudeLegacy {
		t.Errorf("personal service=%q legacy=%q", pers.Service, pers.LegacyProvider())
	}

	workDir := filepath.Join(home, ".claude-work")
	if err := os.Mkdir(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fs = NewClaudeSeatFetchers(home)
	if len(fs) != 2 || fs[1].Provider() != ProviderClaudeWork {
		t.Fatalf("with ~/.claude-work want personal+work, got %d", len(fs))
	}
	work := fs[1].(*ClaudeFetcher)
	if work.Service != ClaudeKeychainService(workDir) || !strings.HasPrefix(work.Service, "Claude Code-credentials-") {
		t.Errorf("work service = %q", work.Service)
	}
	if work.CredsFile != filepath.Join(workDir, ".credentials.json") {
		t.Errorf("work creds file = %q", work.CredsFile)
	}
	if work.LegacyProvider() != "" {
		t.Errorf("work seat must never write the legacy key, got %q", work.LegacyProvider())
	}
}

// seatServer answers the usage endpoint per bearer token so each seat gets
// its own numbers.
func seatServer(t *testing.T, byToken map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := byToken[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func seatFetchers(t *testing.T, home string, kc KeychainReader, srv *httptest.Server) []Fetcher {
	t.Helper()
	fs := NewClaudeSeatFetchers(home)
	for _, f := range fs {
		cf := f.(*ClaudeFetcher)
		cf.Keychain, cf.Client, cf.URL, cf.Now = kc, srv.Client(), srv.URL, now
	}
	return fs
}

func loadPct(t *testing.T, st Store, pool, window string) (float64, bool) {
	t.Helper()
	rows, err := st.Load(pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.WindowType == window {
			return r.UsedPct, true
		}
	}
	return 0, false
}

func TestPoller_BothClaudeSeatsToSeparatePools(t *testing.T) {
	home := t.TempDir()
	workDir := filepath.Join(home, ".claude-work")
	if err := os.Mkdir(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	kc := &mapKeychain{items: map[string]string{
		KeychainServiceClaude:          claudeCreds("FAKE-PERSONAL"),
		ClaudeKeychainService(workDir): claudeCreds("FAKE-WORK"),
	}}
	srv := seatServer(t, map[string]string{
		"FAKE-PERSONAL": `{"five_hour":{"utilization":12,"resets_at":"2026-09-29T15:00:00Z"},"seven_day":{"utilization":30}}`,
		"FAKE-WORK":     `{"five_hour":{"utilization":100,"resets_at":"2026-09-29T14:00:00Z"},"seven_day":{"utilization":64}}`,
	})
	st := newTestStore(t)
	p := &Poller{Store: st, Fetchers: seatFetchers(t, home, kc, srv), Now: now}

	got := p.PollOnce(context.Background())
	if strings.Join(got, ",") != "claude_personal,claude_work" {
		t.Fatalf("fetched %v", got)
	}
	if v, ok := loadPct(t, st, ProviderClaudePersonal, WindowFiveHour); !ok || v != 12 {
		t.Errorf("personal 5h = %v (%v), want 12", v, ok)
	}
	if v, ok := loadPct(t, st, ProviderClaudeWork, WindowFiveHour); !ok || v != 100 {
		t.Errorf("work 5h = %v (%v), want 100", v, ok)
	}
	if v, ok := loadPct(t, st, ProviderClaudeWork, WindowWeekly); !ok || v != 64 {
		t.Errorf("work weekly = %v (%v), want 64", v, ok)
	}
	// Legacy key mirrors personal only.
	if v, ok := loadPct(t, st, ProviderClaudeLegacy, WindowFiveHour); !ok || v != 12 {
		t.Errorf("legacy claude 5h = %v (%v), want personal's 12", v, ok)
	}
	var locked int
	if err := st.DB.QueryRow(`SELECT is_locked FROM quota_windows WHERE pool_key='claude_work' AND window_type='rolling_5h'`).Scan(&locked); err != nil || locked != 1 {
		t.Errorf("work 5h at 100%% must be stored locked, got %d (%v)", locked, err)
	}
}

func TestPoller_MissingWorkCredsLeavesPersonalAlone(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".claude-work"), 0o700); err != nil {
		t.Fatal(err)
	}
	kc := &mapKeychain{items: map[string]string{KeychainServiceClaude: claudeCreds("FAKE-PERSONAL")}}
	srv := seatServer(t, map[string]string{"FAKE-PERSONAL": `{"five_hour":{"utilization":7},"seven_day":{"utilization":9}}`})
	st := newTestStore(t)
	fs := seatFetchers(t, home, kc, srv)
	p := &Poller{Store: st, Fetchers: fs, Now: now}
	p.PollOnce(context.Background())

	if v, ok := loadPct(t, st, ProviderClaudePersonal, WindowFiveHour); !ok || v != 7 {
		t.Errorf("personal 5h = %v (%v), want 7", v, ok)
	}
	if rows, _ := st.Load(ProviderClaudeWork); len(rows) != 0 {
		t.Errorf("work must have no rows without creds, got %+v", rows)
	}
	ws, ok, err := st.State(ProviderClaudeWork)
	if err != nil || !ok || ws.Status != "rejected" {
		t.Errorf("work fetch state = %+v ok=%v err=%v, want rejected", ws, ok, err)
	}
	if _, err := fs[1].Fetch(context.Background()); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("work fetch err = %v, want ErrNoCredentials", err)
	}
}
