package quota

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// KeychainServiceClaude is the Keychain item Claude Code stores OAuth creds
	// in for the default profile (CLAUDE_CONFIG_DIR unset).
	KeychainServiceClaude = "Claude Code-credentials"

	// Pool keys the per-seat Claude fetchers write to quota_windows.
	ProviderClaudePersonal = "claude_personal"
	ProviderClaudeWork     = "claude_work"
	// ProviderClaudeLegacy is the single-seat key older binaries read. Only
	// the personal seat writes it, so it can never carry work-seat numbers.
	ProviderClaudeLegacy = "claude"

	// ClaudeWorkConfigDirName is the work seat's CLAUDE_CONFIG_DIR under $HOME.
	ClaudeWorkConfigDirName = ".claude-work"
	claudeUsageURL        = "https://api.anthropic.com/api/oauth/usage"
	claudeBetaHeader      = "oauth-2025-04-20"
)

type anthropicWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

type anthropicUsage struct {
	FiveHour       *anthropicWindow `json:"five_hour"`
	SevenDay       *anthropicWindow `json:"seven_day"`
	SevenDaySonnet *anthropicWindow `json:"seven_day_sonnet"`
	ExtraUsage     *struct {
		IsEnabled    bool    `json:"is_enabled"`
		UsedCredits  float64 `json:"used_credits"`
		MonthlyLimit float64 `json:"monthly_limit"`
	} `json:"extra_usage"`
}

// ClaudeKeychainService returns the Keychain service Claude Code uses for a
// profile. The default profile (configDir == "") uses the bare service name;
// a CLAUDE_CONFIG_DIR profile appends the first 8 hex chars of the SHA-256 of
// the absolute config dir path (no trailing slash), so each profile keeps its
// own login.
func ClaudeKeychainService(configDir string) string {
	if configDir == "" {
		return KeychainServiceClaude
	}
	dir := filepath.Clean(configDir)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	sum := sha256.Sum256([]byte(dir))
	return KeychainServiceClaude + "-" + hex.EncodeToString(sum[:])[:8]
}

// ClaudeFetcher probes Anthropic's OAuth usage endpoint for one Claude Code
// seat, using the token that seat keeps in the macOS Keychain and falling
// back to its .credentials.json.
type ClaudeFetcher struct {
	Keychain  KeychainReader
	Client    HTTPDoer
	URL       string
	CredsFile string // fallback path; empty disables the file fallback
	Now       func() time.Time

	// Service is the Keychain service to read; empty means the default profile.
	Service string
	// Pool is the quota_windows pool key this seat writes; empty means "claude".
	Pool string
	// Legacy is an extra pool key the same snapshot is also saved under, for
	// readers that predate per-seat keys. Empty disables it.
	Legacy string
}

// NewClaudeFetcher returns the personal-seat fetcher (default profile) wired
// to the real Keychain and endpoint.
func NewClaudeFetcher() *ClaudeFetcher {
	home, _ := os.UserHomeDir()
	return newClaudeSeatFetcher(home, "")
}

// NewClaudeSeatFetchers returns one fetcher per Claude Code seat on this
// machine: personal (default profile) always, and work (CLAUDE_CONFIG_DIR =
// $HOME/.claude-work) only when that directory exists.
func NewClaudeSeatFetchers(home string) []Fetcher {
	out := []Fetcher{newClaudeSeatFetcher(home, "")}
	if home == "" {
		return out
	}
	workDir := filepath.Join(home, ClaudeWorkConfigDirName)
	if fi, err := os.Stat(workDir); err == nil && fi.IsDir() {
		out = append(out, newClaudeSeatFetcher(home, workDir))
	}
	return out
}

// newClaudeSeatFetcher builds the fetcher for configDir ("" = personal).
func newClaudeSeatFetcher(home, configDir string) *ClaudeFetcher {
	f := &ClaudeFetcher{
		Keychain: SecurityKeychain{},
		Client:   newHTTPClient(),
		URL:      claudeUsageURL,
		Now:      time.Now,
		Service:  ClaudeKeychainService(configDir),
	}
	if configDir == "" {
		f.Pool, f.Legacy = ProviderClaudePersonal, ProviderClaudeLegacy
		if home != "" {
			f.CredsFile = filepath.Join(home, ".claude", ".credentials.json")
		}
	} else {
		f.Pool = ProviderClaudeWork
		f.CredsFile = filepath.Join(configDir, ".credentials.json")
	}
	return f
}

func (f *ClaudeFetcher) Provider() string {
	if f.Pool != "" {
		return f.Pool
	}
	return ProviderClaudeLegacy
}

// LegacyProvider is the extra pool key the poller also saves this seat under.
func (f *ClaudeFetcher) LegacyProvider() string { return f.Legacy }

func (f *ClaudeFetcher) service() string {
	if strings.TrimSpace(f.Service) != "" {
		return f.Service
	}
	return KeychainServiceClaude
}

// parseClaudeCreds extracts the access token from Claude Code's credential JSON.
func parseClaudeCreds(raw []byte) string {
	var c struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(raw, &c) != nil {
		return ""
	}
	return c.ClaudeAiOauth.AccessToken
}

func (f *ClaudeFetcher) accessToken(ctx context.Context) (string, error) {
	if f.Keychain != nil {
		if s, err := f.Keychain.Read(ctx, f.service()); err == nil {
			if tok := parseClaudeCreds([]byte(s)); tok != "" {
				return tok, nil
			}
		}
	}
	if f.CredsFile != "" {
		if b, err := os.ReadFile(f.CredsFile); err == nil {
			if tok := parseClaudeCreds(b); tok != "" {
				return tok, nil
			}
		}
	}
	return "", fmt.Errorf("%w: %s", ErrNoCredentials, f.Provider())
}

func (f *ClaudeFetcher) Fetch(ctx context.Context) (*Snapshot, error) {
	tok, err := f.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("quota: claude build request: %w", sanitizeErr(err))
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("anthropic-beta", claudeBetaHeader)
	req.Header.Set("User-Agent", userAgent)
	body, err := doJSON(f.Client, f.Provider(), req)
	if err != nil {
		return nil, err
	}
	var u anthropicUsage
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, fmt.Errorf("quota: claude decode usage: %w", err)
	}
	conv := func(w *anthropicWindow) *Window {
		if w == nil || w.Utilization == nil {
			return nil
		}
		return &Window{Utilization: *w.Utilization, ResetsAt: parseTime(w.ResetsAt)}
	}
	snap := &Snapshot{
		Provider:     f.Provider(),
		FiveHour:     conv(u.FiveHour),
		Weekly:       conv(u.SevenDay),
		WeeklySonnet: conv(u.SevenDaySonnet),
		FetchedAt:    f.Now(),
	}
	if u.ExtraUsage != nil {
		snap.Extra = &Extra{Enabled: u.ExtraUsage.IsEnabled, UsedCredits: u.ExtraUsage.UsedCredits, MonthlyLimit: u.ExtraUsage.MonthlyLimit}
	}
	return snap, nil
}
