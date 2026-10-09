package fleet

import (
	"time"
)

// TaskStatusCounts contains counts of tasks across states.
type TaskStatusCounts struct {
	Total   int `json:"total"`
	Running int `json:"running"` // a run in flight (TaskItem.Running), not stage in_progress
	Active  int `json:"active"`  // todo / backlog / active
	Stopped int `json:"stopped"` // paused / stopped / cancelled
	Blocked int `json:"blocked"` // blocked
	Errored int `json:"errored"` // error / failed
	Done    int `json:"done"`    // done / closed
}

// add counts one task under its Status.
func (c *TaskStatusCounts) add(t TaskItem) {
	c.Total++
	switch t.Status {
	case "running":
		c.Running++
	case "blocked":
		c.Blocked++
	case "errored":
		c.Errored++
	case "done":
		c.Done++
	case "stopped":
		c.Stopped++
	default:
		c.Active++
	}
}

// TaskItem represents an individual task in the consolidated fleet view.
type TaskItem struct {
	ID             string    `json:"id"`
	Identifier     string    `json:"identifier"`
	Title          string    `json:"title"`
	Description    string    `json:"description,omitempty"`
	Comments       []string  `json:"comments,omitempty"`
	Organization   string    `json:"organization"`
	Project        string    `json:"project,omitempty"`
	Status         string    `json:"status"` // running, active, stopped, blocked, errored, done
	ExecutionStage string    `json:"execution_stage,omitempty"`
	Priority       string    `json:"priority,omitempty"`
	ParentID       string    `json:"parent_id,omitempty"`
	SpentUSD       float64   `json:"spent_usd"`
	SpentTokens    int64     `json:"spent_tokens"`
	IsBlocked       bool      `json:"is_blocked"`
	BlockReason     string    `json:"block_reason,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
	AssigneeAgentID string    `json:"assignee_agent_id,omitempty"`
	CheckoutAgentID string    `json:"checkout_agent_id,omitempty"`
	// Origin is the local task origin (native, paperclip_import, legacy, agent);
	// live Paperclip issues are "legacy" (Paperclip is frozen).
	Origin string `json:"origin,omitempty"`
	// Running is true only while a run is in flight (Aggregator.LiveRuns);
	// RunStartedAt is when it started. Status "running" follows Running.
	Running      bool   `json:"running"`
	RunStartedAt string `json:"run_started_at,omitempty"`
}

// AgentItem represents an active or registered agent in an organization.
type AgentItem struct {
	ID            string              `json:"id"`
	Name          string              `json:"name"`
	Role          string              `json:"role"`
	Organization  string              `json:"organization"`
	Provider      string              `json:"provider"` // gemini, claude, openai (never other)
	Model         string              `json:"model,omitempty"`
	Status        string              `json:"status"` // running, active, idle, closed
	LastHeartbeat time.Time           `json:"last_heartbeat"`
	Quota         *ProviderQuotaGauge `json:"quota,omitempty"`
}

// GlobalAgentMetrics aggregates agent statistics fleet-wide.
type GlobalAgentMetrics struct {
	Total          int            `json:"total"`
	ActiveRunning  int            `json:"active_running"`
	Idle           int            `json:"idle"`
	ByProvider     map[string]int `json:"by_provider"`
	ByOrganization map[string]int `json:"by_organization"`
	Items          []AgentItem    `json:"items,omitempty"`
}

// TokenSummary aggregates token consumption across all providers and orgs.
type TokenSummary struct {
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	TotalTokens         int64   `json:"total_tokens"`
	TotalCostUSD        float64 `json:"total_cost_usd"`
}

// ModelSpendBreakdown summarizes spend by specific model.
type ModelSpendBreakdown struct {
	Model        string  `json:"model"`
	Family       string  `json:"family"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	TotalTokens  int64   `json:"total_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	Percentage   float64 `json:"percentage"`
}

// OrgSpendBreakdown summarizes token and financial spend per organization.
type OrgSpendBreakdown struct {
	Organization string  `json:"organization"`
	TotalTokens  int64   `json:"total_tokens"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	Percentage   float64 `json:"percentage"`
	ActiveTasks  int     `json:"active_tasks"`
	ActiveAgents int     `json:"active_agents"`
}

// ProviderQuotaGauge displays 5-hour rolling limits, burn rates, and pacing projections.
type ProviderQuotaGauge struct {
	Provider             string     `json:"provider"` // gemini, claude, openai
	DisplayName          string     `json:"display_name"`
	FiveHourUsedPct      float64    `json:"five_hour_used_pct"`
	FiveHourRemainingPct float64    `json:"five_hour_remaining_pct"`
	FiveHourResetsAt     *time.Time `json:"five_hour_resets_at,omitempty"`
	BurnRate5h           float64    `json:"burn_rate_5h"` // % per turn / hour
	LockoutThresholdPct  float64    `json:"lockout_threshold_pct"`
	IsLocked             bool       `json:"is_locked"`
	LockoutReason        string     `json:"lockout_reason,omitempty"`
	LockoutUntil         *time.Time `json:"lockout_until,omitempty"`
	WeeklyUsedPct        float64    `json:"weekly_used_pct"`
	WeeklyRemainingPct   float64    `json:"weekly_remaining_pct"`
	WeeklyResetsAt       *time.Time `json:"weekly_resets_at,omitempty"`
	BurnRateWeekly       float64    `json:"burn_rate_weekly"`
	ProjectionStatus     string     `json:"projection_status"` // on_track, overpaced, locked_out, unknown
	ProjectionMessage    string     `json:"projection_message"`
	RunwayTurns          int        `json:"runway_turns"`
	// Measured is false when no source reported this provider/seat at all;
	// the percentages are then placeholders and the UI must show "No data",
	// never "0% used". The per-window flags say which window was reported.
	Measured         bool `json:"measured"`
	FiveHourMeasured bool `json:"five_hour_measured"`
	WeeklyMeasured   bool `json:"weekly_measured"`

	measuredAt time.Time // newest reading applied so far; older overlays are skipped
}

// OrgFleetSummary summarizes an individual organization's fleet metrics.
type OrgFleetSummary struct {
	ID                     string           `json:"id"`
	Name                   string           `json:"name"`
	IssuePrefix            string           `json:"issue_prefix"`
	TaskCounts             TaskStatusCounts `json:"task_counts"`
	ActiveAgents           int              `json:"active_agents"`
	ActiveAgentsByProvider map[string]int   `json:"active_agents_by_provider"`
	SpentUSD               float64                        `json:"spent_usd"`
	SpentTokens            int64                          `json:"spent_tokens"`
	ProviderQuotas         map[string]*ProviderQuotaGauge `json:"provider_quotas,omitempty"`
	Tasks                  []TaskItem                     `json:"tasks,omitempty"`
	Agents                 []AgentItem                    `json:"agents,omitempty"`
	Projects               []string                       `json:"projects,omitempty"`
}

// FleetOverview is the unified root response for the All Organizations overview screen.
type FleetOverview struct {
	Timestamp      time.Time                      `json:"timestamp"`
	Organizations  []OrgFleetSummary              `json:"organizations"`
	GlobalTasks    TaskStatusCounts               `json:"global_tasks"`
	GlobalAgents   GlobalAgentMetrics             `json:"global_agents"`
	TokenTelemetry TokenSummary                   `json:"token_telemetry"`
	ModelSpend     []ModelSpendBreakdown          `json:"model_spend"`
	OrgSpend       []OrgSpendBreakdown            `json:"org_spend"`
	ProviderQuotas map[string]*ProviderQuotaGauge `json:"provider_quotas"`
	Tasks          []TaskItem                     `json:"tasks,omitempty"`
}
