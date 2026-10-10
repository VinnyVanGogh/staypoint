package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildPrompt(t *testing.T) {
	prompt := BuildPrompt(BuildPrompt("Add speech-to-text safety to textarea", nil), nil)
	if !strings.Contains(prompt, "Add speech-to-text safety to textarea") {
		t.Errorf("expected prompt to contain input comment")
	}
	if !strings.Contains(prompt, "schema") || !strings.Contains(prompt, "Organization") {
		t.Errorf("expected prompt to specify schema requirements")
	}
}

func TestExtractTaskJSON(t *testing.T) {
	// Case 1: Plain JSON
	raw1 := `{
		"organization": "StayPoint",
		"project": "StayPoint Core Engine & Telemetry Fleet",
		"title": "Implement CLI Task Generator",
		"description": "## Objectives\nAdd new task command",
		"priority": "high",
		"labels": ["cli", "tui"],
		"assigneeRole": "CLI & Statusline Presentation Specialist"
	}`
	task1, err := ExtractTaskJSON(raw1)
	if err != nil {
		t.Fatalf("unexpected error parsing plain JSON: %v", err)
	}
	if task1.Title != "Implement CLI Task Generator" || task1.Priority != "high" {
		t.Errorf("unexpected task fields: %+v", task1)
	}

	// Case 2: Markdown fenced JSON with commentary
	raw2 := "Here is the structured task:\n```json\n" + raw1 + "\n```\nLet me know if you need changes."
	task2, err := ExtractTaskJSON(raw2)
	if err != nil {
		t.Fatalf("unexpected error parsing fenced JSON: %v", err)
	}
	if task2.Title != "Implement CLI Task Generator" {
		t.Errorf("unexpected task title: %s", task2.Title)
	}
}

func TestCalculateCost(t *testing.T) {
	geminiCost := CalculateCost("gemini-3.8-flash", 1000, 2000, 500)
	if geminiCost <= 0.0 {
		t.Errorf("expected positive gemini cost, got %f", geminiCost)
	}

	claudeCost := CalculateCost("claude-sonnet-4-6", 1000, 2000, 500)
	if claudeCost <= 0.0 || claudeCost <= geminiCost {
		t.Errorf("expected claude cost to exceed gemini cost, got %f vs %f", claudeCost, geminiCost)
	}
}

func TestGenerator_GeminiPrimarySuccess(t *testing.T) {
	geminiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"candidates": []map[string]interface{}{
				{
					"content": map[string]interface{}{
						"parts": []map[string]interface{}{
							{
								"text": `{
									"organization": "StayPoint",
									"project": "StayPoint Core Engine",
									"title": "Implement Gemini Primary Inference",
									"description": "## Objectives\nRun Gemini inference",
									"priority": "high",
									"labels": ["ai", "gemini"],
									"assigneeRole": "CLI & Statusline Presentation Specialist"
								}`,
							},
						},
					},
				},
			},
			"usageMetadata": map[string]interface{}{
				"promptTokenCount":        150,
				"candidatesTokenCount":    220,
				"cachedContentTokenCount": 0,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer geminiServer.Close()

	cfg := GeneratorConfig{
		GeminiAPIKey:  "test-gemini-key",
		GeminiModel:   "gemini-3.8-flash",
		GeminiBaseURL: geminiServer.URL,
		HTTPClient:    geminiServer.Client(),
	}

	gen := NewGenerator(cfg)
	res, err := gen.GenerateTask(context.Background(), "Implement Gemini primary inference", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.FallbackUsed {
		t.Errorf("expected primary model to be used, not fallback")
	}
	if res.Model != "gemini-3.8-flash" {
		t.Errorf("expected model gemini-3.8-flash, got %s", res.Model)
	}
	if res.Task.Title != "Implement Gemini Primary Inference" {
		t.Errorf("unexpected task title: %s", res.Task.Title)
	}
	if res.InputTokens != 150 || res.OutputTokens != 220 {
		t.Errorf("unexpected token counts: in=%d, out=%d", res.InputTokens, res.OutputTokens)
	}
}

func TestGenerator_FallbackToClaudeOnGeminiFailure(t *testing.T) {
	// Gemini server returns 429 Too Many Requests
	geminiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":429,"message":"Resource exhausted"}}`, http.StatusTooManyRequests)
	}))
	defer geminiServer.Close()

	// Claude server succeeds
	claudeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"content": []map[string]interface{}{
				{
					"type": "text",
					"text": `{
						"organization": "StayPoint",
						"project": "StayPoint Core Engine",
						"title": "Fallback Issue Synthesized by Claude",
						"description": "## Objectives\nHandled via Sonnet fallback",
						"priority": "urgent",
						"labels": ["claude", "fallback"],
						"assigneeRole": "Senior PR Reviewer"
					}`,
				},
			},
			"usage": map[string]interface{}{
				"input_tokens":            180,
				"output_tokens":           250,
				"cache_read_input_tokens": 10,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer claudeServer.Close()

	cfg := GeneratorConfig{
		GeminiAPIKey:     "test-gemini-key",
		GeminiModel:      "gemini-3.8-flash",
		GeminiBaseURL:    geminiServer.URL,
		AnthropicAPIKey:  "test-claude-key",
		AnthropicModel:   "claude-sonnet-4-6",
		AnthropicBaseURL: claudeServer.URL,
		HTTPClient:       http.DefaultClient,
	}

	gen := NewGenerator(cfg)
	res, err := gen.GenerateTask(context.Background(), "Urgent bug in rate limiter", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !res.FallbackUsed {
		t.Errorf("expected FallbackUsed to be true")
	}
	if res.Model != "claude-sonnet-4-6" {
		t.Errorf("expected model claude-sonnet-4-6, got %s", res.Model)
	}
	if res.Task.Title != "Fallback Issue Synthesized by Claude" {
		t.Errorf("unexpected title: %s", res.Task.Title)
	}
	if res.Task.Priority != "urgent" {
		t.Errorf("expected priority urgent, got %s", res.Task.Priority)
	}
}

func TestGenerator_HeuristicFallbackWhenOffline(t *testing.T) {
	cfg := GeneratorConfig{
		GeminiAPIKey:    "",
		AnthropicAPIKey: "",
	}
	gen := NewGenerator(cfg)
	res, err := gen.GenerateTask(context.Background(), "Fix critical race condition in watcher", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !res.FallbackUsed {
		t.Errorf("expected FallbackUsed to be true for offline heuristic")
	}
	if res.Model != "heuristic-fallback" {
		t.Errorf("expected model heuristic-fallback, got %s", res.Model)
	}
	if !strings.Contains(res.Task.Title, "race condition in watcher") {
		t.Errorf("unexpected task title: %s", res.Task.Title)
	}
	if res.Task.Priority != "urgent" {
		t.Errorf("expected urgent priority for 'critical', got %s", res.Task.Priority)
	}
}

// Mail Router / Power Platform text stays in Managed Solution: work_orgs may not
// list "Power Platform", and an unlisted org is personal (Board 2026-10-09).
func TestHeuristicTask_MailRouterIsManagedSolution(t *testing.T) {
	gen := NewGenerator(GeneratorConfig{})
	for _, input := range []string{
		"fix the mail router retry loop dropping messages",
		"update the mail-router flow for the new shared inbox",
		"add a Power Platform connector for approvals",
	} {
		task := gen.GenerateHeuristicTask(input)
		if task.Organization != "Managed Solution" {
			t.Errorf("%q: Organization = %q, want Managed Solution", input, task.Organization)
		}
		if task.Project != "Managed Solution Mail Router" {
			t.Errorf("%q: Project = %q, want Managed Solution Mail Router", input, task.Project)
		}
	}
}

func TestComplexDictationInference_ManagedSolution(t *testing.T) {
	input := "hey can you please um fix the azure active directory sync issue on the managed solution portal for client acme ASAP because the login is completely broken"
	gen := NewGenerator(GeneratorConfig{})
	res, err := gen.GenerateTask(context.Background(), input, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	task := res.Task
	if task.Organization != "Managed Solution" {
		t.Errorf("expected Organization 'Managed Solution', got '%s'", task.Organization)
	}
	if !strings.Contains(task.Project, "Managed Solution") {
		t.Errorf("expected Project to contain 'Managed Solution', got '%s'", task.Project)
	}
	if task.Priority != "urgent" && task.Priority != "high" {
		t.Errorf("expected urgent or high priority, got '%s'", task.Priority)
	}
	if strings.Contains(strings.ToLower(task.Title), "hey can you please") || strings.Contains(strings.ToLower(task.Title), "um") {
		t.Errorf("expected title to strip conversational filler, got '%s'", task.Title)
	}
	if len(task.Title) > 72 {
		t.Errorf("expected title under 72 chars, got %d ('%s')", len(task.Title), task.Title)
	}
	if !strings.Contains(task.Description, "## Objectives") || !strings.Contains(task.Description, "## Scope") {
		t.Errorf("expected structured markdown description, got: %s", task.Description)
	}
}

func TestComplexDictationInference_RuneLite_WithPhoneticSTT(t *testing.T) {
	input := "yo vinny so basically for runelite we need to add a prayer flicking indicator plugin with sound alerts when offensive prayers are active"
	gen := NewGenerator(GeneratorConfig{})
	res, err := gen.GenerateTask(context.Background(), input, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	task := res.Task
	if task.Organization != "RuneLite" {
		t.Errorf("expected Organization 'RuneLite', got '%s'", task.Organization)
	}
	if task.Project != "RuneLite Plugin Suite" {
		t.Errorf("expected Project 'RuneLite Plugin Suite', got '%s'", task.Project)
	}
	if strings.Contains(strings.ToLower(task.Title), "yo vinny") || strings.Contains(strings.ToLower(task.Title), "so basically") {
		t.Errorf("expected title to strip conversational filler, got '%s'", task.Title)
	}
	if !strings.HasPrefix(task.Title, "Feature:") && !strings.HasPrefix(task.Title, "Fix:") && !strings.HasPrefix(task.Title, "Idea:") && !strings.HasPrefix(task.Title, "Infra:") && !strings.HasPrefix(task.Title, "Refactor:") {
		t.Errorf("expected title to start with semantic prefix, got '%s'", task.Title)
	}
	hasPluginLabel := false
	for _, l := range task.Labels {
		if l == "plugin" || l == "runelite" {
			hasPluginLabel = true
			break
		}
	}
	if !hasPluginLabel {
		t.Errorf("expected runelite or plugin label in %+v", task.Labels)
	}
}

func TestComplexDictationInference_StayPoint_PhoneticMisrecognition(t *testing.T) {
	// User says "sharepoint" due to speech-to-text lisp and "grab" for grep/fetch
	input := "in sharepoint we gotta update the wire daemon to grab the latest token metrics and fix the race condition in the statusline"
	gen := NewGenerator(GeneratorConfig{})
	res, err := gen.GenerateTask(context.Background(), input, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	task := res.Task
	if task.Organization != "StayPoint" {
		t.Errorf("expected Organization 'StayPoint' (mapped from sharepoint), got '%s'", task.Organization)
	}
	if !strings.Contains(task.Project, "StayPoint") {
		t.Errorf("expected Project to contain 'StayPoint', got '%s'", task.Project)
	}
	if strings.Contains(strings.ToLower(task.Title), "sharepoint") {
		t.Errorf("expected title not to mention phonetic 'sharepoint', got '%s'", task.Title)
	}
	if task.Priority != "high" {
		t.Errorf("expected high priority for race condition/bug, got '%s'", task.Priority)
	}
}

func TestComplexDictationInference_Maintenance_Dotfiles(t *testing.T) {
	input := "hey vinny could you please clean up my zshrc dotfiles and prune the unused homebrew packages on the local machine"
	gen := NewGenerator(GeneratorConfig{})
	res, err := gen.GenerateTask(context.Background(), input, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	task := res.Task
	if task.Organization != "Maintenance" {
		t.Errorf("expected Organization 'Maintenance', got '%s'", task.Organization)
	}
	if task.Project != "System Maintenance & Infrastructure" {
		t.Errorf("expected Project 'System Maintenance & Infrastructure', got '%s'", task.Project)
	}
	if strings.Contains(strings.ToLower(task.Title), "hey vinny") || strings.Contains(strings.ToLower(task.Title), "could you please") {
		t.Errorf("expected title to strip conversational filler, got '%s'", task.Title)
	}
}

func TestComplexDictationInference_Research_Benchmarking(t *testing.T) {
	input := "we need to run an evaluation benchmark on arxiv papers comparing gemini flash and claude sonnet latency across long context needle retrieval"
	gen := NewGenerator(GeneratorConfig{})
	res, err := gen.GenerateTask(context.Background(), input, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	task := res.Task
	if task.Organization != "Research" {
		t.Errorf("expected Organization 'Research', got '%s'", task.Organization)
	}
	if task.Project != "AI Model Benchmarking & Research" {
		t.Errorf("expected Project 'AI Model Benchmarking & Research', got '%s'", task.Project)
	}
	if strings.Contains(strings.ToLower(task.Title), "we need to") {
		t.Errorf("expected title to strip 'we need to', got '%s'", task.Title)
	}
}

func TestGenerator_GeminiInferenceComplexDictation(t *testing.T) {
	geminiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"candidates": []map[string]interface{}{
				{
					"content": map[string]interface{}{
						"parts": []map[string]interface{}{
							{
								"text": `{
									"organization": "Managed Solution",
									"project": "Managed Solution Client Services",
									"title": "Fix Azure AD sync on client portal",
									"description": "## Objectives\n- Resolve authentication timeout during user sync\n\n## Core Specs\n- Azure AD Graph API token validation\n\n## Next Steps\n1. Review token refresh logic\n2. Run unit tests\n\n## Original Request\n> Fix the portal sync issue",
									"priority": "urgent",
									"labels": ["managed-solution", "azure", "auth", "bug"],
									"assigneeRole": "Security & Deep Remediation Fixer"
								}`,
							},
						},
					},
				},
			},
			"usageMetadata": map[string]interface{}{
				"promptTokenCount":        210,
				"candidatesTokenCount":    165,
				"cachedContentTokenCount": 0,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer geminiServer.Close()

	cfg := GeneratorConfig{
		GeminiAPIKey:  "test-gemini-key",
		GeminiModel:   "gemini-3.8-flash",
		GeminiBaseURL: geminiServer.URL,
		HTTPClient:    geminiServer.Client(),
	}

	gen := NewGenerator(cfg)
	res, err := gen.GenerateTask(context.Background(), "hey can you please fix the azure sync on managed solution portal ASAP", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if res.FallbackUsed {
		t.Errorf("expected primary model to be used without fallback")
	}
	if res.Task.Organization != "Managed Solution" {
		t.Errorf("expected Organization 'Managed Solution', got '%s'", res.Task.Organization)
	}
	if res.Task.Project != "Managed Solution Client Services" {
		t.Errorf("expected Project 'Managed Solution Client Services', got '%s'", res.Task.Project)
	}
	if res.Task.Title != "Fix Azure AD sync on client portal" {
		t.Errorf("unexpected task title: %s", res.Task.Title)
	}
	if res.Task.Priority != "urgent" {
		t.Errorf("expected priority urgent, got %s", res.Task.Priority)
	}
	if res.Task.AssigneeRole != "Security & Deep Remediation Fixer" {
		t.Errorf("expected role Security & Deep Remediation Fixer, got %s", res.Task.AssigneeRole)
	}
}

func TestComplexDictationInference_UserResearchPrompt(t *testing.T) {
	input := `Can you please open a new task under Research and Organization: Research and Implementation?

The task is to research GitHub agents and see if I can start using GitHub Copilot agents with my account. It can use the GitHub CLI or do whatever else it needs to do, as long as it's read-only (though if it needs to activate an agent, that's fine).

I am curious about a few things:

1. Since I have both an organization account and a personal account, is there a separation or difference between the two?
2. Do both of them have free usage or maximum usage limits? What is going on there, and how much usage can I actually get out of them?`

	gen := NewGenerator(GeneratorConfig{})
	res, err := gen.GenerateTask(context.Background(), input, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	task := res.Task
	if task.Organization != "Research" {
		t.Errorf("expected Organization 'Research', got '%s'", task.Organization)
	}
	if task.Project != "Tooling, AppSec & Observability Intelligence" {
		t.Errorf("expected Project 'Tooling, AppSec & Observability Intelligence', got '%s'", task.Project)
	}
	if strings.Contains(strings.ToLower(task.Title), "open a new task") {
		t.Errorf("expected title to strip meta-dictation 'open a new task', got '%s'", task.Title)
	}
	if !strings.HasPrefix(task.Title, "Feature: Research") {
		t.Errorf("expected title to start with 'Feature: Research', got '%s'", task.Title)
	}
	if len(task.Title) > 72 {
		t.Errorf("expected title length <= 72, got %d ('%s')", len(task.Title), task.Title)
	}
	if !strings.Contains(task.Description, "## Key Inquiries & Questions") {
		t.Errorf("expected description to extract inquiries, got: %s", task.Description)
	}
}

func TestComplexDictationInference_MultilineWithQuotesAndBackticks(t *testing.T) {
	input := `We need to fix the critical bug where 'quotes', "double quotes", and backticks like ` + "`" + `git status` + "`" + `
or environment variables like $PAPERCLIP_API_KEY crash the textarea parser.
Ensure that multi-line piping:
echo "line 1
line 2" | staypoint task create
works cleanly without escaping artifacts!`

	gen := NewGenerator(GeneratorConfig{})
	res, err := gen.GenerateTask(context.Background(), input, nil)
	if err != nil {
		t.Fatalf("unexpected error parsing complex multiline input: %v", err)
	}

	task := res.Task
	if task.Title == "" {
		t.Errorf("expected non-empty title")
	}
	if task.Priority != "urgent" && task.Priority != "high" {
		t.Errorf("expected high or urgent priority for 'critical bug', got '%s'", task.Priority)
	}
	if !strings.Contains(task.Description, "Objectives") {
		t.Errorf("expected structured description with Objectives, got: %s", task.Description)
	}
}

func TestGenerator_RoleInferenceAndMapping(t *testing.T) {
	cases := []struct {
		prompt       string
		expectedRole string
	}{
		{"Write unit and automated tests for the test matrix", "QA & Automated Test Engineer"},
		{"Setup CI/CD pipeline and release system packaging for Linux", "DevOps & Release Systems Engineer"},
		{"Audit authentication tokens, secret leaks, and OS privilege boundaries", "Security & Deep Remediation Fixer"},
	}

	gen := NewGenerator(GeneratorConfig{})
	for _, c := range cases {
		res, err := gen.GenerateTask(context.Background(), c.prompt, nil)
		if err != nil {
			t.Fatalf("unexpected error for prompt %q: %v", c.prompt, err)
		}
		if res.Task.AssigneeRole == "" {
			t.Errorf("expected non-empty AssigneeRole for prompt %q", c.prompt)
		}
	}
}

func TestGenerator_CleanImperativeTitle_FilePathStripping(t *testing.T) {
	boardPrompt := `/Users/vincevasile/Documents/me/teams-archive/ /Users/vincevasile/Documents/cleanshot/Chat\ \ Bill\ Furlong\ \ Managed\\ Solution\ \ vvasile\@managedsolution.com\ \ Microsoft\\ Teams\_September-28-2026\_013363\@2x.png I need help creating a task to review everything that was done today, specifically in relation to the conversation I had with Bill earlier. The goal is to make sure I got everything done that I said I was going to do, and that whatever was a bug or issue got fixed.`

	gen := NewGenerator(GeneratorConfig{})
	res, err := gen.GenerateTask(context.Background(), boardPrompt, nil)
	if err != nil {
		t.Fatalf("GenerateTask failed: %v", err)
	}

	title := res.Task.Title
	if strings.Contains(title, "/Users") || strings.Contains(title, "cleanshot") ||
		strings.Contains(title, ".png") || strings.Contains(title, "\\") {
		t.Errorf("title contains file path or escape artifacts: %q", title)
	}

	if !strings.HasPrefix(title, "Feature: Review everything that was done today") {
		t.Errorf("expected crisp title starting with 'Feature: Review everything that was done today', got %q", title)
	}

	if res.Task.AssigneeRole == "Senior PR Reviewer" {
		t.Errorf("expected role not to be Senior PR Reviewer, got %q", res.Task.AssigneeRole)
	}
}

func TestGenerator_NegativeKeywordRoleRouting(t *testing.T) {
	gen := NewGenerator(GeneratorConfig{})

	tests := []struct {
		name         string
		prompt       string
		expectedRole string
	}{
		{
			name:         "Override PR Reviewer with Chief of Staff",
			prompt:       "Review the pull request diff for PR #12. This should go to Chief of Staff, not PR Reviewer",
			expectedRole: "Chief of Staff",
		},
		{
			name:         "Override PR Reviewer with QA",
			prompt:       "Review the pull request diff for PR #12. This should go to QA, not PR Reviewer",
			expectedRole: "QA & Automated Test Engineer",
		},
		{
			name:         "Override Security with DevOps",
			prompt:       "Fix the auth bug, not security fixer, assign to DevOps",
			expectedRole: "DevOps & Release Engineer",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := gen.GenerateTask(context.Background(), tc.prompt, nil)
			if err != nil {
				t.Fatalf("GenerateTask failed: %v", err)
			}
			if res.Task.AssigneeRole != tc.expectedRole {
				t.Errorf("expected role %q, got %q", tc.expectedRole, res.Task.AssigneeRole)
			}
		})
	}
}
