package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	// (STA-773). Each repo still runs one at a time. Zero or unset = 3.
	// Top-level key: it must appear before any [table] in config.toml.
	MaxConcurrentRuns int `json:"max_concurrent_runs" toml:"max_concurrent_runs"`
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
}

// DefaultMaxConcurrentRuns is the parallel-run cap when config.toml does not
// set max_concurrent_runs.
const DefaultMaxConcurrentRuns = 3

// MaxConcurrentRunsOrDefault returns max_concurrent_runs, or 3 when unset or
// not positive.
func (c *Config) MaxConcurrentRunsOrDefault() int {
	if c == nil || c.MaxConcurrentRuns <= 0 {
		return DefaultMaxConcurrentRuns
	}
	return c.MaxConcurrentRuns
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
