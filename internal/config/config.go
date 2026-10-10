package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	DataDir               string  `json:"data_dir" toml:"data_dir"`
	DBPath                string  `json:"db_path" toml:"db_path"`
	TelemetryDBPath       string  `json:"telemetry_db_path" toml:"telemetry_db_path"`
	CompanyName           string  `json:"company_name" toml:"company_name"`
	EngineerName          string  `json:"engineer_name" toml:"engineer_name"`
	HourlyRate            float64 `json:"hourly_rate" toml:"hourly_rate"`
	WorkEmail             string  `json:"work_email" toml:"work_email"`
	PersonalEmail         string  `json:"personal_email" toml:"personal_email"`
	WorkRepoRoot          string  `json:"work_repo_root" toml:"work_repo_root"`
	// HarnessRepoRoot is the git repo used for agent worktrees. Populated from
	// STAYPOINT_REPO_ROOT env var (takes precedence) or harness_repo_root in
	// config.toml. Never derived from work_repo_root.
	HarnessRepoRoot string `json:"harness_repo_root" toml:"harness_repo_root"`
	RemoteHost            string  `json:"remote_host" toml:"remote_host"`
	RemoteRepoRoot        string  `json:"remote_repo_root" toml:"remote_repo_root"` // e.g. "~/Documents/dev/managed_solution" or "~/Documents/dev/work"
	MachineRole           string  `json:"machine_role" toml:"machine_role"`         // "hybrid" (default), "work", or "personal"
	GooglePlanTier        string  `json:"google_plan_tier" toml:"google_plan_tier"` // e.g. "Google AI Ultra" or "Ultra"
	ClaudePlanTier        string  `json:"claude_plan_tier" toml:"claude_plan_tier"` // e.g. "Max 5x" or "Pro"
	MaxHandoffsPerRepo    int     `json:"max_handoffs_per_repo" toml:"max_handoffs_per_repo"`
	PreferredPersonalTool string  `json:"preferred_personal_tool" toml:"preferred_personal_tool"` // "auto" (default), "claude", or "agy"
	// MaxConcurrentRuns caps how many agent runs the daemon runs in parallel
	// (STA-773). Zero or unset = 9 (3 organizations x 3 runs).
	// Top-level key: it must appear before any [table] in config.toml.
	MaxConcurrentRuns int `json:"max_concurrent_runs" toml:"max_concurrent_runs"`
	// MaxRunsPerRepo caps parallel runs in one git repo; each run has its own
	// task worktree (STA-867). Zero or unset = 3. A plain (non-git) folder
	// always runs one at a time. Top-level key.
	MaxRunsPerRepo int `json:"max_runs_per_repo" toml:"max_runs_per_repo"`
	// MaxRunsPerOrg caps parallel runs per task organization; tasks with no
	// organization share the "Unassigned" bucket (STA-867). Zero or unset = 3.
	// [run_limits.orgs] overrides it per organization. Top-level key.
	MaxRunsPerOrg int `json:"max_runs_per_org" toml:"max_runs_per_org"`
	// RunLimits holds the optional [run_limits] table.
	RunLimits RunLimitsConfig `json:"run_limits,omitempty" toml:"run_limits"`
	// TurnTimeout caps one agent turn's wall-clock time, as a Go duration
	// ("2h"). Empty or "0" = no limit (the default). Top-level key.
	TurnTimeout string `json:"turn_timeout" toml:"turn_timeout"`
	// StallTimeout stops a turn after this long with no agent output or tool
	// activity, as a Go duration. Empty = 20m; "0" turns the check off.
	// Top-level key.
	StallTimeout string `json:"stall_timeout" toml:"stall_timeout"`
	// Use-it-or-lose-it routing (only active in the last UIOLIWindowHours before the weekly reset; zero = default).
	UIOLIDisabled        bool    `json:"uioli_disabled" toml:"uioli_disabled"`
	UIOLIWindowHours     float64 `json:"uioli_window_hours" toml:"uioli_window_hours"`
	UIOLIMinRemainingPct float64 `json:"uioli_min_remaining_pct" toml:"uioli_min_remaining_pct"`
	UIOLIMinPctPerHour   float64 `json:"uioli_min_pct_per_hour" toml:"uioli_min_pct_per_hour"`
	// Provider CLI binary overrides. Empty = resolve via PATH (exec.LookPath).
	// STAYPOINT_CLAUDE_BIN / STAYPOINT_AGY_BIN / STAYPOINT_CODEX_BIN env vars take precedence over these.
	ClaudeBin string `json:"claude_bin" toml:"claude_bin"`
	AgyBin    string `json:"agy_bin" toml:"agy_bin"`
	CodexBin  string `json:"codex_bin" toml:"codex_bin"`

	// Server section
	// CORSAllowAll enables permissive CORS so browser extensions (e.g. Tampermonkey
	// userscripts via GM_xmlhttpRequest) can reach the local API from any origin.
	// Disabled by default; set [server] cors_allow_all = true in config.toml to opt in.
	CORSAllowAll bool `json:"cors_allow_all" toml:"cors_allow_all"`

	// Routing holds the optional [routing] table.
	// When nil (no table in config.toml), callers fall back to DefaultKindChains.
	// TODO(STA-316): wire LoadConfig to populate this field.
	Routing *RoutingConfig `json:"routing,omitempty" toml:"routing"`

	// Gates holds the optional [gates] table.
	Gates GatesConfig `json:"gates,omitempty" toml:"gates"`
}

// GatesConfig holds security-gate toggles from the [gates] section of config.toml.
// All fields default to enabled (the safe posture).
type GatesConfig struct {
	// MainMergeApproval requires Board approval before any agent command that
	// lands code on main/master. Default true. Set to false to fall back to
	// Yellow (unattended) for those commands.
	MainMergeApproval *bool `json:"main_merge_approval,omitempty" toml:"main_merge_approval"`

	// ShipReview requires Board sign-off on a Ship Review card before an agent
	// branch is merged. Default true. When off, tasks finish the way they do
	// today — no card, no dev server.
	ShipReview *bool `json:"ship_review,omitempty" toml:"ship_review"`

	// Hosts classifies ssh destinations ([gates.hosts] dev = ["mansol-dev"]).
	// A host not listed as dev is treated as prod (task-97b4fa02).
	Hosts HostClasses `json:"hosts,omitempty" toml:"hosts"`

	// Ops configures the typed ops MCP tools ([gates.ops]).
	Ops OpsConfig `json:"ops,omitempty" toml:"ops"`
}

// HostClasses is the [gates.hosts] table.
type HostClasses struct {
	Dev  []string `json:"dev,omitempty" toml:"dev"`
	Prod []string `json:"prod,omitempty" toml:"prod"`
}

// IsDevHost reports whether host is listed in [gates.hosts] dev and not also
// in prod (a host in both is prod: fail closed).
func (h HostClasses) IsDevHost(host string) bool {
	if host == "" {
		return false
	}
	for _, p := range h.Prod {
		if strings.EqualFold(p, host) {
			return false
		}
	}
	for _, d := range h.Dev {
		if strings.EqualFold(d, host) {
			return true
		}
	}
	return false
}

// OpsConfig is the [gates.ops] table: what the ops MCP tools may touch.
type OpsConfig struct {
	// DevHosts maps a [gates.hosts] dev alias to what dev_host_run may do there.
	DevHosts map[string]DevHostConfig `json:"dev_hosts,omitempty" toml:"dev_hosts"`
	// VerifyRepos are the GitHub repos ("owner/name") dev_deploy_verify may
	// run scripts/verify_dev_deploy.sh from.
	VerifyRepos []string `json:"verify_repos,omitempty" toml:"verify_repos"`
	// VerifyScriptBlobs are git blob ids of scripts/verify_dev_deploy.sh the
	// Board trusts besides the one on the repo's main branch on GitHub:
	// dev_deploy_verify runs the dev-server copy only when its blob is one of
	// these or main's.
	VerifyScriptBlobs []string `json:"verify_script_blobs,omitempty" toml:"verify_script_blobs"`
	// OwnRepos are the GitHub repos ("owner/name") StayPoint tasks work in:
	// pr_body edits a PR there unattended when it is the task's own repo.
	OwnRepos []string `json:"own_repos,omitempty" toml:"own_repos"`
}

// DevHostConfig is one [gates.ops.dev_hosts.<alias>] table.
type DevHostConfig struct {
	// HostName is the address ssh connects to for this alias. It is passed
	// on the ssh command line, so ~/.ssh/config (or an Include in it) cannot
	// redirect the alias elsewhere. Required.
	HostName string `json:"host_name" toml:"host_name"`
	// SSHConfig is an optional StayPoint-owned ssh config file (ssh -F) used
	// instead of ~/.ssh/config, e.g. for User, Port and IdentityFile.
	SSHConfig string `json:"ssh_config,omitempty" toml:"ssh_config"`
	// AppDir is the absolute app checkout; read actions stay under it.
	AppDir string `json:"app_dir" toml:"app_dir"`
	// Services are the systemd units journal_tail, systemctl_status and
	// restart may name.
	Services []string `json:"services,omitempty" toml:"services"`
	// Branches git_ff_pull may pull. Empty = dev-server and dev.
	Branches []string `json:"branches,omitempty" toml:"branches"`
	// SudoRestart runs restart as `sudo -n systemctl restart`.
	SudoRestart bool `json:"sudo_restart,omitempty" toml:"sudo_restart"`
	// Apps are the Django apps collectstatic and pip_sync may name.
	Apps map[string]DevAppConfig `json:"apps,omitempty" toml:"apps"`
}

// DevAppConfig is one [gates.ops.dev_hosts.<alias>.apps.<name>] table.
type DevAppConfig struct {
	// Dir is the absolute directory holding manage.py.
	Dir string `json:"dir" toml:"dir"`
	// Python is the absolute interpreter (usually the app's venv).
	Python string `json:"python" toml:"python"`
	// Requirements is pip_sync's file, relative to Dir. Empty = requirements.txt.
	Requirements string `json:"requirements,omitempty" toml:"requirements"`
}

// Parallel-run caps used when config.toml does not set them (STA-773,
// STA-867). Keep in sync with the orchestrator defaults.
const (
	DefaultMaxConcurrentRuns = 9
	DefaultMaxRunsPerRepo    = 3
	DefaultMaxRunsPerOrg     = 3
)

// RunLimitsConfig is the [run_limits] table of config.toml.
type RunLimitsConfig struct {
	// Orgs overrides max_runs_per_org for named organizations
	// ([run_limits.orgs] "Managed Solution" = 3). Names match
	// case-insensitively; "Unassigned" is the bucket for tasks with no
	// organization. Non-positive values are ignored.
	Orgs map[string]int `json:"orgs,omitempty" toml:"orgs"`
}

// MaxConcurrentRunsOrDefault returns max_concurrent_runs, or 9 when unset or
// not positive.
func (c *Config) MaxConcurrentRunsOrDefault() int {
	if c == nil || c.MaxConcurrentRuns <= 0 {
		return DefaultMaxConcurrentRuns
	}
	return c.MaxConcurrentRuns
}

// MaxRunsPerRepoOrDefault returns max_runs_per_repo, or 3 when unset or not
// positive.
func (c *Config) MaxRunsPerRepoOrDefault() int {
	if c == nil || c.MaxRunsPerRepo <= 0 {
		return DefaultMaxRunsPerRepo
	}
	return c.MaxRunsPerRepo
}

// MaxRunsPerOrgOrDefault returns max_runs_per_org, or 3 when unset or not
// positive.
func (c *Config) MaxRunsPerOrgOrDefault() int {
	if c == nil || c.MaxRunsPerOrg <= 0 {
		return DefaultMaxRunsPerOrg
	}
	return c.MaxRunsPerOrg
}

// OrgRunLimits returns the [run_limits.orgs] overrides (may be nil).
func (c *Config) OrgRunLimits() map[string]int {
	if c == nil {
		return nil
	}
	return c.RunLimits.Orgs
}

// DefaultStallTimeout is the stall_timeout used when config.toml does not set it.
const DefaultStallTimeout = 20 * time.Minute

// TurnTimeoutOrDefault returns turn_timeout; 0 = no limit. An unparseable or
// negative value is reported and treated as no limit.
func (c *Config) TurnTimeoutOrDefault() (time.Duration, error) {
	if c == nil {
		return 0, nil
	}
	d, err := parseTimeout(c.TurnTimeout, 0)
	if err != nil {
		return 0, fmt.Errorf("turn_timeout: %w", err)
	}
	return d, nil
}

// StallTimeoutOrDefault returns stall_timeout: 20m when unset, 0 when off.
// An unparseable or negative value is reported and the default is used.
func (c *Config) StallTimeoutOrDefault() (time.Duration, error) {
	if c == nil {
		return DefaultStallTimeout, nil
	}
	d, err := parseTimeout(c.StallTimeout, DefaultStallTimeout)
	if err != nil {
		return DefaultStallTimeout, fmt.Errorf("stall_timeout: %w", err)
	}
	return d, nil
}

// parseTimeout parses a duration key; empty returns def.
func parseTimeout(v string, def time.Duration) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def, err
	}
	if d < 0 {
		return def, fmt.Errorf("negative duration %q", v)
	}
	return d, nil
}

// MainMergeApprovalEnabled returns true unless explicitly disabled.
func (g GatesConfig) MainMergeApprovalEnabled() bool {
	if g.MainMergeApproval == nil {
		return true // default on
	}
	return *g.MainMergeApproval
}

// ShipReviewEnabled returns true unless explicitly disabled.
func (g GatesConfig) ShipReviewEnabled() bool {
	if g.ShipReview == nil {
		return true // default on
	}
	return *g.ShipReview
}

// DefaultConfig returns the default configuration.
func DefaultConfig() *Config {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}

	dataDir := filepath.Join(home, ".staypoint")
	return &Config{
		DataDir:               dataDir,
		DBPath:                filepath.Join(dataDir, "staypoint.db"),
		TelemetryDBPath:       filepath.Join(home, ".config", "token-telemetry", "telemetry.db"),
		CompanyName:           "",
		EngineerName:          "",
		HourlyRate:            0.0,
		WorkEmail:             "engineer@company.com",
		PersonalEmail:         "personal@gmail.com",
		WorkRepoRoot:          filepath.Join(home, "Documents", "dev", "work"),
		RemoteHost:            "company-mbp",
		RemoteRepoRoot:        "~/Documents/dev/work",
		MachineRole:           "hybrid",
		GooglePlanTier:        "Google AI Ultra",
		ClaudePlanTier:        "Pro",
		MaxHandoffsPerRepo:    3,
		PreferredPersonalTool: "auto",
	}
}

// LoadConfig loads configuration from ~/.staypoint/config.toml, then config.json if toml does not exist.
// Automatically falls back to legacy ~/.agent-mesh/ if ~/.staypoint does not exist.
// If neither exists, DefaultConfig() is returned.
func LoadConfig() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}

	cfg := DefaultConfig()
	dataDir := filepath.Join(home, ".staypoint")
	tomlPath := filepath.Join(dataDir, "config.toml")
	jsonPath := filepath.Join(dataDir, "config.json")

	var raw []byte
	var isTOML bool

	if data, err := os.ReadFile(tomlPath); err == nil {
		raw = data
		isTOML = true
	} else if data, err := os.ReadFile(jsonPath); err == nil {
		raw = data
		isTOML = false
	} else {
		// Check legacy ~/.agent-mesh directory
		legacyDir := filepath.Join(home, ".agent-mesh")
		legacyTOML := filepath.Join(legacyDir, "config.toml")
		legacyJSON := filepath.Join(legacyDir, "config.json")
		if data, err := os.ReadFile(legacyTOML); err == nil {
			raw = data
			isTOML = true
		} else if data, err := os.ReadFile(legacyJSON); err == nil {
			raw = data
			isTOML = false
		} else {
			return cfg, nil
		}
	}

	if isTOML {
		if err := toml.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("failed to parse config.toml: %w", err)
		}
	} else {
		if err := json.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("failed to parse config.json: %w", err)
		}
	}

	cfg.WorkRepoRoot = expandPath(cfg.WorkRepoRoot, home)
	cfg.DataDir = expandPath(cfg.DataDir, home)
	cfg.DBPath = expandPath(cfg.DBPath, home)
	cfg.TelemetryDBPath = expandPath(cfg.TelemetryDBPath, home)
	cfg.ClaudeBin = expandPath(cfg.ClaudeBin, home)
	cfg.AgyBin = expandPath(cfg.AgyBin, home)
	cfg.CodexBin = expandPath(cfg.CodexBin, home)
	cfg.HarnessRepoRoot = expandPath(cfg.HarnessRepoRoot, home)
	// STAYPOINT_REPO_ROOT env var takes precedence over config file value.
	if v := os.Getenv("STAYPOINT_REPO_ROOT"); v != "" {
		cfg.HarnessRepoRoot = expandPath(v, home)
	}
	// Mansol guard: harness must never create worktrees inside the billing repo.
	if strings.Contains(strings.ToLower(cfg.HarnessRepoRoot), "mansol") {
		cfg.HarnessRepoRoot = ""
	}

	if cfg.RemoteRepoRoot == "" {
		if strings.Contains(strings.ToLower(cfg.CompanyName), "managed solution") ||
			strings.Contains(strings.ToLower(cfg.WorkRepoRoot), "mansol") {
			cfg.RemoteRepoRoot = "~/Documents/dev/managed_solution"
		} else {
			cfg.RemoteRepoRoot = "~/Documents/dev/work"
		}
	}

	return cfg, nil
}

func expandPath(path, home string) string {
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	if path == "~" {
		return home
	}
	return path
}

// EnsureDataDir creates cfg.DataDir and automatically migrates legacy ~/.agent-mesh data.
func EnsureDataDir(cfg *Config) error {
	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	legacyDir := filepath.Join(home, ".agent-mesh")
	if _, err := os.Stat(legacyDir); err == nil && legacyDir != cfg.DataDir {
		// Migrate legacy mesh.db to staypoint.db if staypoint.db does not exist
		legacyDB := filepath.Join(legacyDir, "mesh.db")
		newDB := filepath.Join(cfg.DataDir, "staypoint.db")
		if _, err := os.Stat(newDB); os.IsNotExist(err) {
			if _, err := os.Stat(legacyDB); err == nil {
				_ = copyFileContents(legacyDB, newDB)
			}
		}
		// Migrate legacy cursor file if new cursor file does not exist
		legacyCursor := filepath.Join(legacyDir, "ingest-cursors.json")
		newCursor := filepath.Join(cfg.DataDir, "ingest-cursors.json")
		if _, err := os.Stat(newCursor); os.IsNotExist(err) {
			if _, err := os.Stat(legacyCursor); err == nil {
				_ = copyFileContents(legacyCursor, newCursor)
			}
		}
	}
	return nil
}

func copyFileContents(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}
