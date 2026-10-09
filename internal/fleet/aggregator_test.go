package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/paperclip"
	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open test in-memory db: %v", err)
	}

	// Insert test tasks across organizations
	_, err = store.DB().Exec(`
		INSERT INTO tasks (id, name, repo_path, organization, project, status, execution_stage, is_blocked, spent_usd, spent_tokens, parent_id, checkout_run_id)
		VALUES
		('task-1', 'Build Fleet UI', '/repo/sta', 'StayPoint', 'Core', 'active', 'in_progress', 0, 12.50, 50000, NULL, 'run-1'),
		('task-2', 'Review PR 122', '/repo/sta', 'StayPoint', 'Core', 'active', 'todo', 0, 0.0, 0, 'task-1', NULL),
		('task-3', 'Blocked Database Migration', '/repo/sta', 'StayPoint', 'Core', 'active', 'blocked', 1, 3.20, 12000, NULL, NULL),
		('task-4', 'Completed Auth Fix', '/repo/sta', 'StayPoint', 'Core', 'done', 'done', 0, 8.40, 30000, NULL, NULL),
		('task-5', 'Deploy Enterprise Gateway', '/repo/man', 'Managed Solution', 'Cloud', 'active', 'in_progress', 0, 45.00, 150000, NULL, 'run-5'),
		('task-6', 'Sync Client Repos', '/repo/man', 'Managed Solution', 'Platform', 'active', 'todo', 0, 0.0, 0, NULL, NULL),
		('task-7', 'Failed Deployment Rollback', '/repo/man', 'Managed Solution', 'Cloud', 'active', 'failed', 0, 5.00, 18000, NULL, NULL),
		('task-8', 'Cancelled Maintenance Run', '/repo/per', 'Personal', 'Infra', 'soft_deleted', 'cancelled', 0, 0.0, 0, NULL, NULL);
	`)
	if err != nil {
		t.Fatalf("failed to seed tasks: %v", err)
	}

	// Insert test agent sessions
	_, err = store.DB().Exec(`
		INSERT INTO agent_sessions (id, agent_type, status, repo_path, last_heartbeat_at)
		VALUES
		('sess-1', 'claude', 'active', '/home/user/staypoint', '2026-09-30T07:00:00Z'),
		('sess-2', 'gemini', 'active', '/home/user/staypoint', '2026-09-30T07:05:00Z'),
		('sess-3', 'codex', 'active', '/home/user/mansol-repo', '2026-09-30T07:02:00Z'),
		('sess-4', 'claude', 'idle', '/home/user/other-repo', '2026-09-30T06:00:00Z');
	`)
	if err != nil {
		t.Fatalf("failed to seed agent sessions: %v", err)
	}

	// Insert quota windows
	_, err = store.DB().Exec(`
		INSERT INTO quota_windows (pool_key, window_type, used_percent, remaining_pct, is_locked, resets_at)
		VALUES
		('gemini', 'rolling_5h', 14.5, 85.5, 0, '2026-09-30T12:00:00Z'),
		('gemini', 'weekly_7d', 42.0, 58.0, 0, '2026-10-05T00:00:00Z'),
		('claude', 'rolling_5h', 92.0, 8.0, 0, '2026-09-30T10:00:00Z'),
		('claude', 'weekly_7d', 85.0, 15.0, 0, '2026-10-04T00:00:00Z'),
		('codex', 'rolling_5h', 100.0, 0.0, 1, '2026-09-30T09:30:00Z'),
		('codex', 'weekly_7d', 70.0, 30.0, 0, '2026-10-03T00:00:00Z');
	`)
	if err != nil {
		t.Fatalf("failed to seed quota windows: %v", err)
	}

	return store.DB()
}

func setupTestTelemetryDBFile(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "telemetry.db")
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to create test telemetry db: %v", err)
	}
	defer conn.Close()

	_, err = conn.Exec(`
		CREATE TABLE requests (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			model TEXT,
			model_family TEXT,
			input_tokens INTEGER,
			output_tokens INTEGER,
			cache_read_tokens INTEGER,
			cache_creation_tokens INTEGER,
			total_tokens INTEGER,
			cost_usd REAL,
			ts TEXT
		);
		INSERT INTO requests (model, model_family, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, total_tokens, cost_usd, ts)
		VALUES
		('claude-3-7-sonnet', 'claude', 100000, 20000, 5000, 1000, 126000, 0.60, '2026-09-30T07:00:00Z'),
		('gemini-2.0-flash', 'gemini', 500000, 50000, 0, 0, 550000, 0.07, '2026-09-30T07:10:00Z'),
		('o1-preview', 'codex', 20000, 5000, 0, 0, 25000, 0.60, '2026-09-30T07:15:00Z');
	`)
	if err != nil {
		t.Fatalf("failed to seed test telemetry requests: %v", err)
	}

	return dbPath
}

func TestAggregatorGather(t *testing.T) {
	testDB := setupTestDB(t)
	defer testDB.Close()

	agg := &Aggregator{
		DB:              testDB,
		TelemetryDBPath: "", // will use fallback task spend
		PaperclipClient: nil,
		RateLimitsPath:  "",
		Now: func() time.Time {
			return time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
		},
	}

	overview, err := agg.Gather(context.Background())
	if err != nil {
		t.Fatalf("unexpected error gathering fleet overview: %v", err)
	}

	// 1. Verify Global Tasks
	if overview.GlobalTasks.Running != 2 {
		t.Errorf("expected 2 running tasks, got %d", overview.GlobalTasks.Running)
	}
	if overview.GlobalTasks.Active != 2 {
		t.Errorf("expected 2 active tasks, got %d", overview.GlobalTasks.Active)
	}
	if overview.GlobalTasks.Blocked != 1 {
		t.Errorf("expected 1 blocked task, got %d", overview.GlobalTasks.Blocked)
	}
	if overview.GlobalTasks.Errored != 1 {
		t.Errorf("expected 1 errored task, got %d", overview.GlobalTasks.Errored)
	}
	if overview.GlobalTasks.Done != 1 {
		t.Errorf("expected 1 done task, got %d", overview.GlobalTasks.Done)
	}

	// 2. Verify Organizations presence
	var foundStayPoint, foundManaged bool
	for _, org := range overview.Organizations {
		if org.Name == "StayPoint" {
			foundStayPoint = true
			if org.TaskCounts.Running != 1 {
				t.Errorf("expected 1 running task in StayPoint, got %d", org.TaskCounts.Running)
			}
			if org.TaskCounts.Blocked != 1 {
				t.Errorf("expected 1 blocked task in StayPoint, got %d", org.TaskCounts.Blocked)
			}
		}
		if org.Name == "Managed Solution" {
			foundManaged = true
			if org.TaskCounts.Running != 1 {
				t.Errorf("expected 1 running task in Managed Solution, got %d", org.TaskCounts.Running)
			}
			if org.TaskCounts.Errored != 1 {
				t.Errorf("expected 1 errored task in Managed Solution, got %d", org.TaskCounts.Errored)
			}
		}
	}
	if !foundStayPoint {
		t.Errorf("expected StayPoint organization in fleet summary")
	}
	if !foundManaged {
		t.Errorf("expected Managed Solution organization in fleet summary")
	}

	// 3. Verify Active Running Agents
	if overview.GlobalAgents.ActiveRunning != 3 {
		t.Errorf("expected 3 active running agents, got %d", overview.GlobalAgents.ActiveRunning)
	}
	if overview.GlobalAgents.ByProvider["claude"] != 1 {
		t.Errorf("expected 1 active claude agent, got %d", overview.GlobalAgents.ByProvider["claude"])
	}
	if overview.GlobalAgents.ByProvider["gemini"] != 1 {
		t.Errorf("expected 1 active gemini agent, got %d", overview.GlobalAgents.ByProvider["gemini"])
	}
	if overview.GlobalAgents.ByProvider["openai"] != 1 {
		t.Errorf("expected 1 active openai agent, got %d", overview.GlobalAgents.ByProvider["openai"])
	}

	// 4. Verify 5-Hour Rolling Quotas & Lockout Gauges
	gemGauge := overview.ProviderQuotas["gemini"]
	if gemGauge == nil {
		t.Fatalf("missing gemini quota gauge")
	}
	if gemGauge.FiveHourRemainingPct != 85.5 {
		t.Errorf("expected 85.5%% 5h remaining for gemini, got %.1f", gemGauge.FiveHourRemainingPct)
	}
	if gemGauge.ProjectionStatus != "on_track" {
		t.Errorf("expected on_track status for gemini, got %s", gemGauge.ProjectionStatus)
	}

	claudeGauge := overview.ProviderQuotas["claude"]
	if claudeGauge == nil {
		t.Fatalf("missing claude quota gauge")
	}
	if claudeGauge.FiveHourRemainingPct != 8.0 {
		t.Errorf("expected 8.0%% 5h remaining for claude, got %.1f", claudeGauge.FiveHourRemainingPct)
	}
	if claudeGauge.ProjectionStatus != "overpaced" {
		t.Errorf("expected overpaced status for claude, got %s", claudeGauge.ProjectionStatus)
	}

	openaiGauge := overview.ProviderQuotas["openai"]
	if openaiGauge == nil {
		t.Fatalf("missing openai quota gauge")
	}
	if !openaiGauge.IsLocked {
		t.Errorf("expected openai gauge to be locked")
	}
	if openaiGauge.ProjectionStatus != "locked_out" {
		t.Errorf("expected locked_out status for openai, got %s", openaiGauge.ProjectionStatus)
	}

	// 5. Verify ParentID propagation
	var foundTask2 bool
	for _, it := range overview.Tasks {
		if it.ID == "task-2" {
			foundTask2 = true
			if it.ParentID != "task-1" {
				t.Errorf("expected task-2 ParentID to be 'task-1', got %q", it.ParentID)
			}
		}
	}
	if !foundTask2 {
		t.Errorf("expected task-2 to be present in overview.Tasks")
	}
}

func TestAggregatorWithTelemetryDB(t *testing.T) {
	testDB := setupTestDB(t)
	defer testDB.Close()

	telemDBPath := setupTestTelemetryDBFile(t)

	agg := &Aggregator{
		DB:              testDB,
		TelemetryDBPath: telemDBPath,
		Now: func() time.Time {
			return time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
		},
	}

	overview, err := agg.Gather(context.Background())
	if err != nil {
		t.Fatalf("unexpected error gathering fleet overview: %v", err)
	}

	// Verify token telemetry aggregation
	if overview.TokenTelemetry.TotalTokens != 701000 {
		t.Errorf("expected 701,000 total tokens, got %d", overview.TokenTelemetry.TotalTokens)
	}
	if overview.TokenTelemetry.InputTokens != 620000 {
		t.Errorf("expected 620,000 input tokens, got %d", overview.TokenTelemetry.InputTokens)
	}
	if overview.TokenTelemetry.OutputTokens != 75000 {
		t.Errorf("expected 75,000 output tokens, got %d", overview.TokenTelemetry.OutputTokens)
	}
	if overview.TokenTelemetry.TotalCostUSD <= 0 {
		t.Errorf("expected positive total cost USD, got %.2f", overview.TokenTelemetry.TotalCostUSD)
	}

	// Verify model breakdown
	if len(overview.ModelSpend) != 3 {
		t.Fatalf("expected 3 models in spend breakdown, got %d", len(overview.ModelSpend))
	}
}

func TestResolveAgentProvider(t *testing.T) {
	tests := []struct {
		name          string
		adapterType   string
		adapterConfig map[string]interface{}
		runtimeConfig map[string]interface{}
		model         string
		agentName     string
		role          string
		title         string
		expected      string
	}{
		{
			name:        "gemini_local adapter type",
			adapterType: "gemini_local",
			agentName:   "Telemetry & Quota Pacing Engineer",
			role:        "engineer",
			expected:    "gemini",
		},
		{
			name:        "claude_local adapter type",
			adapterType: "claude_local",
			agentName:   "Lead Systems & Daemon Architect",
			role:        "cto",
			expected:    "claude",
		},
		{
			name:        "codex_local adapter type",
			adapterType: "codex_local",
			agentName:   "Code Synthesis Specialist",
			role:        "engineer",
			expected:    "openai",
		},
		{
			name:          "runtimeConfig with model claude",
			adapterType:   "",
			runtimeConfig: map[string]interface{}{"model": "claude-sonnet-4-6"},
			agentName:     "Research Engineer",
			role:          "engineer",
			expected:      "claude",
		},
		{
			name:          "runtimeConfig with provider openai",
			adapterType:   "",
			runtimeConfig: map[string]interface{}{"provider": "openai"},
			agentName:     "General Assistant",
			role:          "general",
			expected:      "openai",
		},
		{
			name:          "adapterConfig with defaultModel gemini",
			adapterType:   "",
			adapterConfig: map[string]interface{}{"defaultModel": "gemini-2.5-flash"},
			agentName:     "Build Worker",
			role:          "devops",
			expected:      "gemini",
		},
		{
			name:        "name contains Claude Fable",
			adapterType: "",
			agentName:   "Claude Fable Engineer",
			role:        "engineer",
			expected:    "claude",
		},
		{
			name:        "completely unknown agent with no indicators defaults to gemini, never other",
			adapterType: "",
			agentName:   "Generic Helper",
			role:        "assistant",
			expected:    "gemini",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveAgentProvider(tc.adapterType, tc.adapterConfig, tc.runtimeConfig, tc.model, tc.agentName, tc.role, tc.title)
			if got != tc.expected {
				t.Errorf("ResolveAgentProvider() = %q, expected %q", got, tc.expected)
			}
			if got == "other" {
				t.Errorf("ResolveAgentProvider() returned 'other' which is strictly forbidden")
			}
		})
	}
}

func TestAgentQuotaBinding(t *testing.T) {
	testDB := setupTestDB(t)
	defer testDB.Close()

	agg := &Aggregator{
		DB: testDB,
		Now: func() time.Time {
			return time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
		},
	}

	overview, err := agg.Gather(context.Background())
	if err != nil {
		t.Fatalf("unexpected error gathering fleet overview: %v", err)
	}

	if len(overview.GlobalAgents.Items) == 0 {
		t.Fatalf("expected aggregated agents in overview")
	}

	for _, ag := range overview.GlobalAgents.Items {
		if ag.Provider == "other" || ag.Provider == "" {
			t.Errorf("agent %s has invalid provider %q", ag.Name, ag.Provider)
		}
		if ag.Quota == nil {
			t.Errorf("agent %s (%s) does not have 5h quota bound", ag.Name, ag.Provider)
		} else {
			if ag.Quota.Provider != ag.Provider && (ag.Provider == "openai" && ag.Quota.Provider != "openai") {
				t.Errorf("agent %s provider %q mismatched with quota %q", ag.Name, ag.Provider, ag.Quota.Provider)
			}
		}
	}
}

func TestClaudePersonalQuotaAndOrgPrioritization(t *testing.T) {
	testDB := setupTestDB(t)
	defer testDB.Close()

	// Seed work and personal quotas
	_, err := testDB.Exec(`
		DELETE FROM quota_windows WHERE pool_key IN ('claude', 'claude_work', 'claude_personal');
		INSERT INTO quota_windows (pool_key, window_type, used_percent, remaining_pct, is_locked, resets_at)
		VALUES
		('claude_work', 'rolling_5h', 15.0, 85.0, 0, '2026-09-30T10:00:00Z'),
		('claude_work', 'weekly_7d', 20.0, 80.0, 0, '2026-10-04T00:00:00Z'),
		('claude', 'rolling_5h', 54.0, 46.0, 0, '2026-09-30T10:00:00Z'),
		('claude', 'weekly_7d', 31.0, 69.0, 0, '2026-10-04T00:00:00Z');
	`)
	if err != nil {
		t.Fatalf("failed to seed quota windows: %v", err)
	}

	agg := &Aggregator{
		DB: testDB,
		Now: func() time.Time {
			return time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
		},
	}

	overview, err := agg.Gather(context.Background())
	if err != nil {
		t.Fatalf("unexpected error gathering overview: %v", err)
	}

	// 1. Verify global fleet provider quotas
	persQ := overview.ProviderQuotas["claude_personal"]
	if persQ == nil {
		t.Fatalf("missing claude_personal in ProviderQuotas")
	}
	if persQ.FiveHourUsedPct != 54.0 || persQ.FiveHourRemainingPct != 46.0 {
		t.Errorf("expected claude_personal 54%% used / 46%% left, got %.1f%% used / %.1f%% left",
			persQ.FiveHourUsedPct, persQ.FiveHourRemainingPct)
	}
	if persQ.WeeklyUsedPct != 31.0 || persQ.WeeklyRemainingPct != 69.0 {
		t.Errorf("expected claude_personal weekly 31%% used / 69%% left, got %.1f%% used / %.1f%% left",
			persQ.WeeklyUsedPct, persQ.WeeklyRemainingPct)
	}

	workQ := overview.ProviderQuotas["claude_work"]
	if workQ == nil {
		t.Fatalf("missing claude_work in ProviderQuotas")
	}
	if workQ.FiveHourUsedPct != 15.0 || workQ.FiveHourRemainingPct != 85.0 {
		t.Errorf("expected claude_work 15%% used / 85%% left, got %.1f%% used / %.1f%% left",
			workQ.FiveHourUsedPct, workQ.FiveHourRemainingPct)
	}

	// 2. Verify organization-specific prioritization and headroom
	var stayPointOrg, managedOrg *OrgFleetSummary
	for i := range overview.Organizations {
		if overview.Organizations[i].Name == "StayPoint" {
			stayPointOrg = &overview.Organizations[i]
		} else if overview.Organizations[i].Name == "Managed Solution" {
			managedOrg = &overview.Organizations[i]
		}
	}
	if stayPointOrg == nil || managedOrg == nil {
		t.Fatalf("missing StayPoint or Managed Solution orgs in overview")
	}

	// StayPoint (Personal org): prioritizes Claude Personal
	spPers := stayPointOrg.ProviderQuotas["claude_personal"]
	if spPers == nil {
		t.Fatalf("missing claude_personal in StayPoint ProviderQuotas")
	}
	if spPers.FiveHourRemainingPct != 46.0 || spPers.FiveHourUsedPct != 54.0 {
		t.Errorf("expected StayPoint claude_personal 46%% headroom, got %.1f%%", spPers.FiveHourRemainingPct)
	}
	if spPers.WeeklyRemainingPct != 69.0 || spPers.WeeklyUsedPct != 31.0 {
		t.Errorf("expected StayPoint claude_personal weekly 69%% headroom, got %.1f%%", spPers.WeeklyRemainingPct)
	}
	if spPers.ProjectionMessage != "Claude Personal prioritized (healthy headroom)" {
		t.Errorf("expected StayPoint claude_personal message 'Claude Personal prioritized (healthy headroom)', got %q", spPers.ProjectionMessage)
	}
	spWork := stayPointOrg.ProviderQuotas["claude_work"]
	if spWork == nil || spWork.FiveHourRemainingPct != 100.0 {
		t.Errorf("expected StayPoint claude_work 100%% remaining (not used by personal org), got %v", spWork)
	}

	// Managed Solution (Work org): prioritizes Claude Work
	msWork := managedOrg.ProviderQuotas["claude_work"]
	if msWork == nil {
		t.Fatalf("missing claude_work in Managed Solution ProviderQuotas")
	}
	if msWork.FiveHourRemainingPct != 85.0 || msWork.FiveHourUsedPct != 15.0 {
		t.Errorf("expected Managed Solution claude_work 85%% headroom, got %.1f%%", msWork.FiveHourRemainingPct)
	}
	if msWork.WeeklyRemainingPct != 80.0 || msWork.WeeklyUsedPct != 20.0 {
		t.Errorf("expected Managed Solution claude_work weekly 80%% headroom, got %.1f%%", msWork.WeeklyRemainingPct)
	}
	if msWork.ProjectionMessage != "Claude Work prioritized (healthy headroom)" {
		t.Errorf("expected Managed Solution claude_work message 'Claude Work prioritized (healthy headroom)', got %q", msWork.ProjectionMessage)
	}
	msPers := managedOrg.ProviderQuotas["claude_personal"]
	if msPers == nil || msPers.FiveHourRemainingPct != 100.0 {
		t.Errorf("expected Managed Solution claude_personal 100%% remaining (retained for personal), got %v", msPers)
	}

	// 3. Verify getProviderQuotaGauge prioritization per org
	spClaudeGauge := getProviderQuotaGauge(overview.ProviderQuotas, "claude", "StayPoint")
	if spClaudeGauge == nil || spClaudeGauge.FiveHourRemainingPct != 46.0 {
		t.Errorf("expected StayPoint claude gauge to bind to personal (46%% remaining), got %v", spClaudeGauge)
	}
	msClaudeGauge := getProviderQuotaGauge(overview.ProviderQuotas, "claude", "Managed Solution")
	if msClaudeGauge == nil || msClaudeGauge.FiveHourRemainingPct != 85.0 {
		t.Errorf("expected Managed Solution claude gauge to bind to work (85%% remaining), got %v", msClaudeGauge)
	}
}

func TestAggregatorGather_PaperclipProjectsAndAssignees(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/companies":
			_ = json.NewEncoder(w).Encode([]paperclip.CompanyResponse{
				{ID: "comp-1", Name: "StayPoint", IssuePrefix: "STA", Status: "active"},
			})
		case r.URL.Path == "/api/companies/comp-1/projects":
			_ = json.NewEncoder(w).Encode([]paperclip.ProjectResponse{
				{ID: "proj-10", Name: "Core Engine & Telemetry Fleet"},
				{ID: "proj-20", Name: "Native Orchestrator"},
			})
		case strings.HasPrefix(r.URL.Path, "/api/companies/comp-1/issues"):
			rawJSON := `[
				{
					"id": "iss-1",
					"identifier": "STA-212",
					"title": "Fix: Cascading Project Filter",
					"status": "in_progress",
					"priority": "high",
					"projectId": "proj-10",
					"assigneeAgentId": "agent-cto-1",
					"labels": [{"id": "lbl-1", "name": "Bug"}]
				},
				{
					"id": "iss-2",
					"identifier": "STA-213",
					"title": "Boss Card Caching",
					"status": "todo",
					"priority": "medium",
					"projectId": "proj-20",
					"assigneeAgentId": "agent-dev-2"
				}
			]`
			_, _ = w.Write([]byte(rawJSON))
		case r.URL.Path == "/api/companies/comp-1/agents":
			_ = json.NewEncoder(w).Encode([]paperclip.AgentResponse{
				{ID: "agent-cto-1", Name: "Chief Technology Officer", Role: "cto", AdapterType: "claude_local"},
				{ID: "agent-dev-2", Name: "Core Platform Engineer", Role: "engineer", AdapterType: "gemini_local"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	pClient := paperclip.NewClient(ts.URL, "test-key")
	agg := &Aggregator{
		PaperclipClient: pClient,
		Now: func() time.Time {
			return time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
		},
	}

	overview, err := agg.Gather(context.Background())
	if err != nil {
		t.Fatalf("unexpected error gathering fleet: %v", err)
	}

	var stayPointOrg *OrgFleetSummary
	for i := range overview.Organizations {
		if overview.Organizations[i].Name == "StayPoint" {
			stayPointOrg = &overview.Organizations[i]
			break
		}
	}
	if stayPointOrg == nil {
		t.Fatalf("expected StayPoint organization, got %+v", overview.Organizations)
	}

	org := *stayPointOrg
	if len(org.Projects) != 2 {
		t.Fatalf("expected 2 projects on org, got %d: %+v", len(org.Projects), org.Projects)
	}
	if org.Projects[0] != "Core Engine & Telemetry Fleet" || org.Projects[1] != "Native Orchestrator" {
		t.Errorf("unexpected org projects: %+v", org.Projects)
	}

	if len(org.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(org.Tasks))
	}

	t1 := org.Tasks[0]
	if t1.Project != "Core Engine & Telemetry Fleet" {
		t.Errorf("expected t1 project 'Core Engine & Telemetry Fleet', got %q", t1.Project)
	}
	if t1.AssigneeAgentID != "agent-cto-1" {
		t.Errorf("expected t1 AssigneeAgentID 'agent-cto-1', got %q", t1.AssigneeAgentID)
	}
	// Paperclip is frozen: live issues are legacy, hidden behind "Show
	// archive & legacy" with the imported legacy tasks.
	for _, ti := range org.Tasks {
		if ti.Origin != "legacy" {
			t.Errorf("live Paperclip issue %s origin = %q, want legacy", ti.Identifier, ti.Origin)
		}
	}

	t2 := org.Tasks[1]
	if t2.Project != "Native Orchestrator" {
		t.Errorf("expected t2 project 'Native Orchestrator', got %q", t2.Project)
	}
	if t2.AssigneeAgentID != "agent-dev-2" {
		t.Errorf("expected t2 AssigneeAgentID 'agent-dev-2', got %q", t2.AssigneeAgentID)
	}
}
