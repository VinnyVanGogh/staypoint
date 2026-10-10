package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// InferredTask represents the structured engineering issue inferred by the AI engine.
type InferredTask struct {
	Organization     string   `json:"organization"`
	Project          string   `json:"project"`
	Title            string   `json:"title"`
	Description      string   `json:"description"`
	Priority         string   `json:"priority"` // low, medium, high, urgent
	Labels           []string `json:"labels"`
	AssigneeRole     string   `json:"assigneeRole"`
	Status           string   `json:"status,omitempty"`
	AskClarification string   `json:"askClarification,omitempty"`
}

// GenerationResult holds the task and generation telemetry.
type GenerationResult struct {
	Task             InferredTask
	Model            string
	InputTokens      int
	OutputTokens     int
	CachedTokens     int
	EstimatedCostUSD float64
	// Unpriced marks a turn that made no billable API call (local heuristic). The
	// telemetry block must render it as unpriced rather than as a real $0 cost.
	Unpriced         bool
	ReferenceCostUSD float64 // what the same tokens would cost on ReferenceModel
	ReferenceModel   string
	FallbackUsed     bool
	RawResponse      string
}

// EstimateHeuristicTokens approximates token counts for the local heuristic
// engine at ~4 characters per token for the prompt it would have sent, plus a
// fixed synthesized-task output size.
func EstimateHeuristicTokens(prompt string) (in, out int) {
	return (len(prompt) + 3) / 4, heuristicOutputTokens
}

const heuristicOutputTokens = 120

// FormatCost renders the turn cost line value: a priced dollar amount, or an
// explicit "unpriced" with the reference estimate for non-billable turns.
func (r *GenerationResult) FormatCost() string {
	if !r.Unpriced {
		return fmt.Sprintf("$%.6f USD", r.EstimatedCostUSD)
	}
	if r.ReferenceCostUSD > 0 {
		return fmt.Sprintf("unpriced (local heuristic, no API billed; ~$%.6f USD if run on %s)", r.ReferenceCostUSD, r.ReferenceModel)
	}
	return "unpriced (local heuristic, no API billed)"
}

// GeneratorConfig configures model endpoints, keys, and timeout behaviors.
type GeneratorConfig struct {
	GeminiAPIKey     string
	GeminiModel      string
	GeminiBaseURL    string
	AnthropicAPIKey  string
	AnthropicModel   string
	AnthropicBaseURL string
	HTTPClient       *http.Client
}

type TaskGenerator struct {
	cfg GeneratorConfig
}

// DefaultGeneratorConfig resolves configuration from environment variables with sensible defaults.
func DefaultGeneratorConfig() GeneratorConfig {
	geminiKey := os.Getenv("GEMINI_API_KEY")
	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")

	geminiModel := os.Getenv("STAYPOINT_GEMINI_MODEL")
	if geminiModel == "" {
		geminiModel = "gemini-3.8-flash"
	}

	anthropicModel := os.Getenv("STAYPOINT_ANTHROPIC_MODEL")
	if anthropicModel == "" {
		anthropicModel = "claude-sonnet-4-6"
	}

	return GeneratorConfig{
		GeminiAPIKey:     geminiKey,
		GeminiModel:      geminiModel,
		GeminiBaseURL:    "https://generativelanguage.googleapis.com",
		AnthropicAPIKey:  anthropicKey,
		AnthropicModel:   anthropicModel,
		AnthropicBaseURL: "https://api.anthropic.com",
		HTTPClient:       &http.Client{Timeout: 30 * time.Second},
	}
}

// NewGenerator returns a new TaskGenerator.
func NewGenerator(cfg GeneratorConfig) *TaskGenerator {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.GeminiModel == "" {
		cfg.GeminiModel = "gemini-3.8-flash"
	}
	if cfg.AnthropicModel == "" {
		cfg.AnthropicModel = "claude-sonnet-4-6"
	}
	if cfg.GeminiBaseURL == "" {
		cfg.GeminiBaseURL = "https://generativelanguage.googleapis.com"
	}
	if cfg.AnthropicBaseURL == "" {
		cfg.AnthropicBaseURL = "https://api.anthropic.com"
	}
	return &TaskGenerator{cfg: cfg}
}

// BuildPrompt creates the system instruction and context wrapper for the raw comment.
func BuildPrompt(comment string, projects []string) string {
	projectList := strings.Join(projects, ", ")
	if projectList == "" {
		projectList = "None available"
	}
	return fmt.Sprintf(`You are the CTO's autonomous AI parsing engine. A user has dictated or written a raw thought, complaint, or request.

Task Input:
"""
%s
"""

Project Candidates:
[%s]

Your job is to read this raw input and convert it into a highly structured, professional engineering issue.
1. DO NOT just copy and paste the input as the title. You MUST synthesize a crisp, concise title using semantic prefixes (e.g., Feature:, Fix:, Refactor:, Infra:, Idea:).
2. Carefully infer the target Organization and MUST select a Project strictly from the Project Candidates provided, if any match closely.
3. Determine dynamic task sizing: Generate concise Idea/Spike cards for rough thoughts (label: idea) and comprehensive engineering specs (Objectives, Scope, Boundaries, Acceptance Criteria) for concrete requests.
4. Raw Voice Preservation: Embed the exact original voice dictation inside a <details><summary>Original Voice Dictation</summary>... block in the description.
5. If the brief has architectural forks or high ambiguity, write a clarification question in the askClarification field.
6. Semantic Disposition Inference: Detect whether the prompt implies backlog parking ("idea", "someday", "later") vs. active execution ("urgent", "now"). Set status to "backlog" or "todo" accordingly.

Respond ONLY with a valid JSON object matching the requested schema. No markdown wrapping.`, strings.TrimSpace(comment), projectList)
}

// CalculateCost estimates the USD cost for a generation run based on token counts.
func CalculateCost(model string, inputTokens, outputTokens, cachedTokens int) float64 {
	switch {
	case strings.Contains(strings.ToLower(model), "gemini"):
		// Gemini 3.8 Flash rates: $0.15/1M in, $0.60/1M out, $0.0375/1M cached
		inCost := (float64(inputTokens) / 1000000.0) * 0.15
		outCost := (float64(outputTokens) / 1000000.0) * 0.60
		cachedCost := (float64(cachedTokens) / 1000000.0) * 0.0375
		return inCost + outCost + cachedCost

	case strings.Contains(strings.ToLower(model), "claude") || strings.Contains(strings.ToLower(model), "sonnet"):
		// Claude Sonnet rates: $3.00/1M in, $15.00/1M out, $0.30/1M cached
		inCost := (float64(inputTokens) / 1000000.0) * 3.00
		outCost := (float64(outputTokens) / 1000000.0) * 15.00
		cachedCost := (float64(cachedTokens) / 1000000.0) * 0.30
		return inCost + outCost + cachedCost

	default:
		return 0.0
	}
}

// ExtractTaskJSON extracts and parses the InferredTask from a raw model string response.
func ExtractTaskJSON(raw string) (InferredTask, error) {
	var task InferredTask
	trimmed := strings.TrimSpace(raw)

	// Strip markdown code fences if present
	fenceRegex := regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)\\s*```")
	if matches := fenceRegex.FindStringSubmatch(trimmed); len(matches) > 1 {
		trimmed = strings.TrimSpace(matches[1])
	}

	// Find enclosing brackets
	firstBrace := strings.Index(trimmed, "{")
	lastBrace := strings.LastIndex(trimmed, "}")
	if firstBrace != -1 && lastBrace != -1 && lastBrace > firstBrace {
		trimmed = trimmed[firstBrace : lastBrace+1]
	}

	if err := json.Unmarshal([]byte(trimmed), &task); err != nil {
		return task, fmt.Errorf("failed to unmarshal JSON: %w", err)
	}

	// Normalize defaults
	if task.Title == "" {
		task.Title = "Untitled Task"
	}
	if task.Priority == "" {
		task.Priority = "medium"
	}
	task.Priority = strings.ToLower(task.Priority)
	if task.Organization == "" {
		task.Organization = "StayPoint"
	}
	if task.Project == "" {
		task.Project = "StayPoint Core Engine & Telemetry Fleet"
	}
	if task.AssigneeRole == "" {
		task.AssigneeRole = "CLI & Statusline Presentation Specialist"
	}
	if task.Description == "" {
		task.Description = fmt.Sprintf("## Objectives\n%s\n\n## Next Steps\n- Implement task requirements.", task.Title)
	}

	return task, nil
}

// GenerateTask orchestrates inference with Gemini 3.8 Flash as primary, falling back to Claude Sonnet.
func (g *TaskGenerator) GenerateTask(ctx context.Context, comment string, projects []string) (*GenerationResult, error) {
	prompt := BuildPrompt(comment, projects)

	// 1. Try Gemini (Primary) if key configured
	var geminiErr error
	if g.cfg.GeminiAPIKey != "" {
		res, err := g.CallGemini(ctx, prompt)
		if err == nil {
			return res, nil
		}
		geminiErr = err
	} else {
		geminiErr = errors.New("GEMINI_API_KEY not configured")
	}

	// 2. Downshift to Claude Sonnet (Fallback)
	var claudeErr error
	if g.cfg.AnthropicAPIKey != "" {
		res, err := g.CallClaude(ctx, prompt)
		if err == nil {
			res.FallbackUsed = true
			return res, nil
		}
		claudeErr = err
	} else {
		claudeErr = errors.New("ANTHROPIC_API_KEY not configured")
	}

	// 3. If both remote APIs fail or are unconfigured, use deterministic heuristic generator
	heuristicTask := g.GenerateHeuristicTask(comment)
	inTokens, outTokens := EstimateHeuristicTokens(prompt)
	return &GenerationResult{
		Task:         heuristicTask,
		Model:        "heuristic-fallback",
		InputTokens:  inTokens,
		OutputTokens: outTokens,
		CachedTokens: 0,
		// No API call is billed, so the real cost is zero. Flag it as unpriced and
		// carry the reference cost the same tokens would have incurred on the
		// primary model so the report never shows a bare $0.000000.
		EstimatedCostUSD: 0.0,
		Unpriced:         true,
		ReferenceCostUSD: CalculateCost(g.cfg.GeminiModel, inTokens, outTokens, 0),
		ReferenceModel:   g.cfg.GeminiModel,
		FallbackUsed:     true,
		RawResponse:      "Synthesized via StayPoint local heuristic engine (remote APIs unavailable: Gemini: " + geminiErr.Error() + "; Claude: " + claudeErr.Error() + ")",
	}, nil
}

// CallGemini executes an HTTP call to the Google Gemini generateContent endpoint.
func (g *TaskGenerator) CallGemini(ctx context.Context, prompt string) (*GenerationResult, error) {
	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent?key=%s",
		strings.TrimRight(g.cfg.GeminiBaseURL, "/"),
		g.cfg.GeminiModel,
		g.cfg.GeminiAPIKey,
	)

	reqBody := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"parts": []map[string]interface{}{
					{"text": prompt},
				},
			},
		},
		"systemInstruction": map[string]interface{}{
			"parts": []map[string]interface{}{
				{"text": "You are an expert autonomous software engineer and task coordinator for the Paperclip & StayPoint ecosystem. Analyze the following natural language task request or dictated comment and synthesize a structured engineering issue."},
			},
		},
		"generationConfig": map[string]interface{}{
			"temperature":      0.2,
			"responseMimeType": "application/json",
			"responseSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"organization": map[string]interface{}{
						"type":        "string",
						"description": "Target Organization (e.g. StayPoint, Managed Solution, RuneLite, Maintenance, Research)",
					},
					"project": map[string]interface{}{
						"type":        "string",
						"description": "Target Project name (e.g. StayPoint Core Engine & Telemetry Fleet)",
					},
					"title": map[string]interface{}{
						"type":        "string",
						"description": "Crisp, concise issue title in imperative mood (e.g. 'Implement dynamic quota router')",
					},
					"description": map[string]interface{}{
						"type":        "string",
						"description": "Structured Markdown description with sections: ## Objectives, ## Core Specs, ## Next Steps",
					},
					"priority": map[string]interface{}{
						"type": "string",
						"enum": []string{"low", "medium", "high", "urgent"},
					},
					"labels": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"description": "array of lowercase tags",
					},
					"status": map[string]interface{}{
						"type":        "string",
						"enum":        []string{"backlog", "todo", "in_progress"},
						"description": "If prompt implies parking (e.g. idea, someday) use 'backlog'. If active (e.g. urgent, now) use 'todo'.",
					},
					"askClarification": map[string]interface{}{
						"type":        "string",
						"description": "If brief has architectural forks or high ambiguity, write a clarification question here.",
					},
					"assigneeRole": map[string]interface{}{
						"type":        "string",
						"description": "Recommended assignee role (e.g. CLI & Statusline Presentation Specialist, Architecture Lead, Senior PR Reviewer)",
					},
				},
				"required": []string{"organization", "project", "title", "description", "priority", "labels", "assigneeRole"},
			},
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to encode gemini request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create gemini request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini network error: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read gemini response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gemini API error (HTTP %d): %s", resp.StatusCode, string(respBytes))
	}

	var geminiResp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount        int `json:"promptTokenCount"`
			CandidatesTokenCount    int `json:"candidatesTokenCount"`
			CachedContentTokenCount int `json:"cachedContentTokenCount"`
		} `json:"usageMetadata"`
	}

	if err := json.Unmarshal(respBytes, &geminiResp); err != nil {
		return nil, fmt.Errorf("failed to decode gemini response JSON: %w", err)
	}

	if len(geminiResp.Candidates) == 0 || len(geminiResp.Candidates[0].Content.Parts) == 0 {
		return nil, errors.New("empty candidates returned from gemini")
	}

	rawText := geminiResp.Candidates[0].Content.Parts[0].Text
	task, err := ExtractTaskJSON(rawText)
	if err != nil {
		return nil, fmt.Errorf("failed to parse structured task from gemini response: %w", err)
	}

	inTokens := geminiResp.UsageMetadata.PromptTokenCount
	outTokens := geminiResp.UsageMetadata.CandidatesTokenCount
	cachedTokens := geminiResp.UsageMetadata.CachedContentTokenCount
	cost := CalculateCost(g.cfg.GeminiModel, inTokens, outTokens, cachedTokens)

	return &GenerationResult{
		Task:             task,
		Model:            g.cfg.GeminiModel,
		InputTokens:      inTokens,
		OutputTokens:     outTokens,
		CachedTokens:     cachedTokens,
		EstimatedCostUSD: cost,
		FallbackUsed:     false,
		RawResponse:      rawText,
	}, nil
}

// CallClaude executes an HTTP call to the Anthropic Messages endpoint.
func (g *TaskGenerator) CallClaude(ctx context.Context, prompt string) (*GenerationResult, error) {
	url := fmt.Sprintf("%s/v1/messages", strings.TrimRight(g.cfg.AnthropicBaseURL, "/"))

	reqBody := map[string]interface{}{
		"model":      g.cfg.AnthropicModel,
		"max_tokens": 4096,
		"system":     "You are an expert autonomous software engineer and task coordinator. Return ONLY a valid JSON object matching the requested schema.",
		"messages": []map[string]interface{}{
			{
				"role":    "user",
				"content": prompt,
			},
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to encode claude request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create claude request: %w", err)
	}
	req.Header.Set("x-api-key", g.cfg.AnthropicAPIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude network error: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read claude response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("claude API error (HTTP %d): %s", resp.StatusCode, string(respBytes))
	}

	var claudeResp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens          int `json:"input_tokens"`
			OutputTokens         int `json:"output_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(respBytes, &claudeResp); err != nil {
		return nil, fmt.Errorf("failed to decode claude response JSON: %w", err)
	}

	if len(claudeResp.Content) == 0 {
		return nil, errors.New("empty content returned from claude")
	}

	rawText := claudeResp.Content[0].Text
	task, err := ExtractTaskJSON(rawText)
	if err != nil {
		return nil, fmt.Errorf("failed to parse structured task from claude response: %w", err)
	}

	inTokens := claudeResp.Usage.InputTokens
	outTokens := claudeResp.Usage.OutputTokens
	cachedTokens := claudeResp.Usage.CacheReadInputTokens
	cost := CalculateCost(g.cfg.AnthropicModel, inTokens, outTokens, cachedTokens)

	return &GenerationResult{
		Task:             task,
		Model:            g.cfg.AnthropicModel,
		InputTokens:      inTokens,
		OutputTokens:     outTokens,
		CachedTokens:     cachedTokens,
		EstimatedCostUSD: cost,
		FallbackUsed:     true,
		RawResponse:      rawText,
	}, nil
}

// isMailRouter reports whether lowercased text names the Mail Router or Power
// Platform work. It maps to org Managed Solution: emitting any org the work-org
// list may not hold would land work in a personal org.
func isMailRouter(lower string) bool {
	return strings.Contains(lower, "power platform") || strings.Contains(lower, "mail router") ||
		strings.Contains(lower, "mail-router")
}

// GenerateHeuristicTask provides a deterministic fallback task synthesis when remote AI APIs are offline.
func (g *TaskGenerator) GenerateHeuristicTask(comment string) InferredTask {
	cleanComment := strings.TrimSpace(comment)

	// Correct common phonetic STT artifacts
	replacer := strings.NewReplacer(
		"sharepoint", "StayPoint",
		"SharePoint", "StayPoint",
		"share point", "StayPoint",
		"grab ", "grep ",
		"length ", "lint ",
		"batch ", "bash ",
	)
	normalized := replacer.Replace(cleanComment)
	lowerNorm := strings.ToLower(normalized)

	// 1. Infer Organization
	org := "StayPoint"
	switch {
	case strings.Contains(lowerNorm, "managed solution") || isMailRouter(lowerNorm) || strings.Contains(lowerNorm, "mansol") ||
		strings.Contains(lowerNorm, "azure") || strings.Contains(lowerNorm, "m365") ||
		strings.Contains(lowerNorm, "client portal") || strings.Contains(lowerNorm, "client acme") ||
		strings.Contains(lowerNorm, "msp"):
		org = "Managed Solution"
	case strings.Contains(lowerNorm, "runelite") || strings.Contains(lowerNorm, "osrs") ||
		strings.Contains(lowerNorm, "runescape") || strings.Contains(lowerNorm, "prayer flick") ||
		strings.Contains(lowerNorm, "tile indicator"):
		org = "RuneLite"
	case strings.Contains(lowerNorm, "dotfile") || strings.Contains(lowerNorm, "zshrc") ||
		strings.Contains(lowerNorm, "homebrew") || strings.Contains(lowerNorm, "prune") ||
		strings.Contains(lowerNorm, "maintenance") || strings.Contains(lowerNorm, "cleanup my") ||
		strings.Contains(lowerNorm, "clean up my"):
		org = "Maintenance"
	case strings.Contains(lowerNorm, "arxiv") || strings.Contains(lowerNorm, "paper") ||
		strings.Contains(lowerNorm, "benchmark") || strings.Contains(lowerNorm, "research") ||
		strings.Contains(lowerNorm, "eval") || strings.Contains(lowerNorm, "needle retrieval"):
		org = "Research"
	}

	// 2. Infer Project
	project := "StayPoint Core Engine & Telemetry Fleet"
	switch org {
	case "Managed Solution":
		if isMailRouter(lowerNorm) {
			project = "Managed Solution Mail Router"
		} else if strings.Contains(lowerNorm, "migration") || strings.Contains(lowerNorm, "cloud") {
			project = "Managed Solution Cloud Migration"
		} else {
			project = "Managed Solution Client Services"
		}
	case "RuneLite":
		project = "RuneLite Plugin Suite"
	case "Maintenance":
		project = "System Maintenance & Infrastructure"
	case "Research":
		if strings.Contains(lowerNorm, "tooling") || strings.Contains(lowerNorm, "observability") ||
			strings.Contains(lowerNorm, "github") || strings.Contains(lowerNorm, "copilot") ||
			strings.Contains(lowerNorm, "appsec") {
			project = "Tooling, AppSec & Observability Intelligence"
		} else {
			project = "AI Model Benchmarking & Research"
		}
	case "StayPoint":
		if strings.Contains(lowerNorm, "statusline") || strings.Contains(lowerNorm, "tui") ||
			strings.Contains(lowerNorm, "bubbletea") || strings.Contains(lowerNorm, "textarea") ||
			strings.Contains(lowerNorm, "glamour") {
			project = "StayPoint Statusline & TUI Presentation"
		} else if strings.Contains(lowerNorm, "wire") || strings.Contains(lowerNorm, "daemon") ||
			strings.Contains(lowerNorm, "socket") || strings.Contains(lowerNorm, "ipc") {
			project = "StayPoint Wire Protocol & Daemon"
		} else if strings.Contains(lowerNorm, "quota") || strings.Contains(lowerNorm, "pacing") ||
			strings.Contains(lowerNorm, "rate limit") {
			project = "StayPoint Dynamic Quota & Fleet Engine"
		}
	}

	// 3. Infer Priority
	priority := "medium"
	if strings.Contains(lowerNorm, "urgent") || strings.Contains(lowerNorm, "asap") ||
		strings.Contains(lowerNorm, "critical") || strings.Contains(lowerNorm, "blocker") ||
		strings.Contains(lowerNorm, "outage") {
		priority = "urgent"
	} else if strings.Contains(lowerNorm, "bug") || strings.Contains(lowerNorm, "broken") ||
		strings.Contains(lowerNorm, "fix") || strings.Contains(lowerNorm, "failure") ||
		strings.Contains(lowerNorm, "race condition") || strings.Contains(lowerNorm, "leak") {
		priority = "high"
	} else if strings.Contains(lowerNorm, "doc") || strings.Contains(lowerNorm, "readme") ||
		strings.Contains(lowerNorm, "typo") || strings.Contains(lowerNorm, "minor") {
		priority = "low"
	}

	// 4. Synthesize Crisp Imperative Title (strip conversational filler)
	title := cleanImperativeTitle(normalized, org)

	// 5. Infer Labels
	labels := []string{"cli", "task"}
	switch org {
	case "Managed Solution":
		labels = []string{"managed-solution", "client"}
		if isMailRouter(lowerNorm) {
			labels = append(labels, "mail-router")
		}
		if strings.Contains(lowerNorm, "azure") {
			labels = append(labels, "azure")
		}
		if strings.Contains(lowerNorm, "portal") {
			labels = append(labels, "portal")
		}
	case "RuneLite":
		labels = []string{"runelite", "plugin", "osrs"}
	case "Maintenance":
		labels = []string{"maintenance", "infrastructure"}
		if strings.Contains(lowerNorm, "dotfile") || strings.Contains(lowerNorm, "zshrc") {
			labels = append(labels, "dotfiles")
		}
	case "Research":
		labels = []string{"research", "ai"}
		if strings.Contains(lowerNorm, "github") || strings.Contains(lowerNorm, "copilot") {
			labels = append(labels, "github", "copilot", "agents")
		} else {
			labels = append(labels, "benchmark")
		}
	case "StayPoint":
		labels = []string{"staypoint"}
		if strings.Contains(lowerNorm, "tui") || strings.Contains(lowerNorm, "bubbletea") {
			labels = append(labels, "tui")
		}
		if strings.Contains(lowerNorm, "wire") || strings.Contains(lowerNorm, "daemon") {
			labels = append(labels, "wire")
		}
		if strings.Contains(lowerNorm, "quota") || strings.Contains(lowerNorm, "pacing") {
			labels = append(labels, "pacing")
		}
	}
	if priority == "urgent" || priority == "high" {
		labels = append(labels, "bug")
	}

	// 6. Assignee Role
	role := "CLI & Statusline Presentation Specialist"

	// Mask out negative directives (e.g. "not PR Reviewer", "don't assign to Security", "instead of Chief of Staff")
	reNegative := regexp.MustCompile(`(?i)(?:not|don't|do not|never|instead of)\s+(?:a\s+|the\s+)?(?:senior\s+)?(?:pr\s+reviewer|pull\s+request|pr\b|code\s+review|chief\s+of\s+staff|cos\b|cto\b|devops|qa|test|security|ci\b)`)
	roleNorm := reNegative.ReplaceAllString(lowerNorm, " ")

	// Explicit user directives take top priority
	if strings.Contains(roleNorm, "chief of staff") || strings.Contains(roleNorm, "cos") {
		role = "Chief of Staff"
	} else if strings.Contains(roleNorm, "cto") || strings.Contains(roleNorm, "chief technology officer") {
		role = "CTO"
	} else if strings.Contains(roleNorm, "qa") || strings.Contains(roleNorm, "quality assurance") {
		role = "QA & Automated Test Engineer"
	} else if strings.Contains(roleNorm, "devops") {
		role = "DevOps & Release Engineer"
	} else {
		switch {
		case strings.Contains(roleNorm, "security") || strings.Contains(roleNorm, "auth") || strings.Contains(roleNorm, "secret"):
			role = "Security & Deep Remediation Fixer"
		case strings.Contains(roleNorm, "pull request") || strings.Contains(roleNorm, "pr ") || strings.Contains(roleNorm, "git diff") || strings.Contains(roleNorm, "code review"):
			role = "Senior PR Reviewer"
		case strings.Contains(roleNorm, "research") || strings.Contains(roleNorm, "copilot") || strings.Contains(roleNorm, "eval"):
			role = "Research & Architecture Specialist"
		case strings.Contains(roleNorm, "wire") || strings.Contains(roleNorm, "architecture") || strings.Contains(roleNorm, "rfc") || strings.Contains(roleNorm, "daemon"):
			role = "Architecture Lead"
		case strings.Contains(roleNorm, "ci") || strings.Contains(roleNorm, "lint") || strings.Contains(roleNorm, "hook") || strings.Contains(roleNorm, "test"):
			role = "CI/CD Engineer"
		case strings.Contains(roleNorm, "release") || strings.Contains(roleNorm, "deploy") || strings.Contains(roleNorm, "package") || strings.Contains(roleNorm, "homebrew"):
			role = "DevOps & Release Engineer"
		}
	}

	// Extract questions if present
	var questions []string
	reQuestion := regexp.MustCompile(`(?m)^\s*(\d+\.|\*|-)\s*(.+)`)
	for _, match := range reQuestion.FindAllStringSubmatch(cleanComment, -1) {
		if len(match) > 2 {
			q := strings.TrimSpace(match[2])
			if q != "" {
				questions = append(questions, fmt.Sprintf("%s %s", match[1], q))
			}
		}
	}

	questionBlock := ""
	if len(questions) > 0 {
		questionBlock = fmt.Sprintf("\n\n## Key Inquiries & Questions\n%s", strings.Join(questions, "\n"))
	}

	status := "todo"
	if strings.Contains(lowerNorm, "idea") || strings.Contains(lowerNorm, "someday") || strings.Contains(lowerNorm, "look into later") || strings.Contains(lowerNorm, "passing thought") || strings.Contains(lowerNorm, "note to self") || strings.Contains(lowerNorm, "backlog") {
		status = "backlog"
	}

	// 7. Synthesize Markdown Description with Dynamic Task Sizing
	var description string
	if status == "backlog" || strings.Contains(lowerNorm, "idea") || strings.Contains(lowerNorm, "passing thought") {
		labels = append(labels, "idea")
		description = fmt.Sprintf(`## Concept Overview
- **Passing Thought / Research Idea:** %s
- **Target Subsystem:** %s (%s)

## Open Research Questions
- What are the architectural tradeoffs and runtime performance implications?
- How does this integrate with the existing database and indexing schema?

<details><summary>Original Voice Dictation</summary>
%s
</details>`, cleanComment, project, org, cleanComment)
	} else {
		description = fmt.Sprintf(`## Objectives
- Synthesize requirements and deliver engineering solution for %s.
- Address specifications and prevent regressions.

## Scope
- **Target Organization:** %s
- **Target Project:** %s
- **Priority Tier:** %s
- **Scope Summary:** %s%s

## Boundaries & Acceptance Criteria
- [ ] Triage codebase and inspect relevant source files.
- [ ] Implement solution following architectural specifications.
- [ ] Validate against StayPoint Definition of Done under race detection.

<details><summary>Original Voice Dictation</summary>
%s
</details>`, title, org, project, strings.ToUpper(priority), title, questionBlock, cleanComment)
	}

	var askClarification string
	if strings.Contains(lowerNorm, "ambiguous") || strings.Contains(lowerNorm, "maybe") || strings.Contains(lowerNorm, "not sure") || strings.Contains(lowerNorm, "vague") || (strings.Contains(lowerNorm, " or ") && strings.Contains(lowerNorm, "?")) {
		askClarification = "The brief is ambiguous. Which specific subsystem or implementation approach should be prioritized?"
	}

	return InferredTask{
		Organization:     org,
		Project:          project,
		Title:            title,
		Description:      description,
		Priority:         priority,
		Labels:           labels,
		AssigneeRole:     role,
		Status:           status,
		AskClarification: askClarification,
	}
}

// cleanImperativeTitle strips conversational dictation filler and ensures imperative mood under 72 chars.
func cleanImperativeTitle(input, org string) string {
	raw := strings.TrimSpace(input)

	// Repeatedly strip conversational and meta-dictation preambles from beginning
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)^(?:can\s+you\s+(?:please\s+)?)?(?:open|create|add|make|file)\s+(?:a\s+)?(?:new\s+)?task\s+(?:under|for|in|titled|about)\s+[^\n\?]+[\?\.]?\s*`),
		regexp.MustCompile(`(?i)^(?:hey|yo|hi|hello)\s+(?:vinny\s+|there\s+|assistant\s+)?`),
		regexp.MustCompile(`(?i)^(?:so\s+)?basically\s+`),
		regexp.MustCompile(`(?i)^(?:can\s+you|could\s+you|would\s+you)\s+(?:please\s+)?`),
		regexp.MustCompile(`(?i)^please\s+`),
		regexp.MustCompile(`(?i)^(?:um+|uh+|er+|ah+)\s+`),
		regexp.MustCompile(`(?i)^(?:in|for)\s+(?:sharepoint|staypoint|runelite|managed\s+solution|research)\s+`),
		regexp.MustCompile(`(?i)^(?:we\s+(?:need\s+to|gotta|should|have\s+to)|i\s+(?:need\s+to|want\s+to|would\s+like\s+to|need\s+help\s+(?:creating|making)\s+a\s+task\s+to))\s+`),
		regexp.MustCompile(`(?i)^(?:the\s+)?task\s+(?:is\s+to|should\s+be\s+to|is)\s+`),
		regexp.MustCompile(`(?i)^(?:urgent|asap|critical):\s*`),
		regexp.MustCompile(`(?i)^(?:(?:~?/(?:\\+[\s\S]|[^ \t\r\n\\])+)\s*)+`),
		regexp.MustCompile(`(?i)^!\[[^\]]*\]\([^\)]+\)\s*`),
		regexp.MustCompile(`(?i)^(?:passing\s+thought|idea|note\s+to\s+self):\s*`),
	}

	stripped := raw
	changed := true
	for changed {
		changed = false
		for _, re := range patterns {
			if loc := re.FindStringIndex(stripped); loc != nil && loc[0] == 0 {
				stripped = strings.TrimSpace(stripped[loc[1]:])
				changed = true
			}
		}
	}

	// Use first substantive line or sentence
	if idx := strings.Index(stripped, "\n"); idx != -1 {
		stripped = strings.TrimSpace(stripped[:idx])
	}
	if idx := strings.Index(stripped, ". "); idx != -1 && idx > 20 {
		stripped = strings.TrimSpace(stripped[:idx])
	}

	// Strip conversational trailing clauses
	if idx := strings.Index(strings.ToLower(stripped), " because "); idx != -1 {
		stripped = strings.TrimSpace(stripped[:idx])
	}
	reTrailing := regexp.MustCompile(`(?i)\s+(?:asap|urgently|please|right now)$`)
	stripped = reTrailing.ReplaceAllString(stripped, "")
	stripped = strings.TrimSpace(stripped)

	// Capitalize first character
	if len(stripped) > 0 {
		stripped = strings.ToUpper(stripped[:1]) + stripped[1:]
	}

	// Ensure semantic prefix if missing
	prefixes := []string{"Feature:", "Fix:", "Refactor:", "Infra:", "Idea:"}
	hasPrefix := false
	for _, p := range prefixes {
		if strings.HasPrefix(stripped, p) {
			hasPrefix = true
			break
		}
	}
	if !hasPrefix {
		lowerStripped := strings.ToLower(stripped)
		if strings.HasPrefix(lowerStripped, "fix ") || strings.Contains(lowerStripped, "bug") || strings.Contains(lowerStripped, "fix") {
			if strings.HasPrefix(lowerStripped, "fix ") {
				stripped = "Fix: " + strings.TrimSpace(stripped[4:])
			} else {
				stripped = "Fix: " + stripped
			}
		} else if strings.Contains(lowerStripped, "idea") || strings.Contains(lowerStripped, "passing thought") || strings.Contains(lowerStripped, "note to self") {
			stripped = "Idea: " + stripped
		} else if strings.Contains(lowerStripped, "refactor") {
			stripped = "Refactor: " + stripped
		} else if org == "Maintenance" {
			stripped = "Infra: " + stripped
		} else {
			stripped = "Feature: " + stripped
		}
	}

	// Remove trailing punctuation
	stripped = strings.TrimRight(stripped, ".!? \t\r\n")

	// Limit to 72 characters
	if len(stripped) > 72 {
		stripped = strings.TrimSpace(stripped[:69]) + "..."
	}

	return stripped
}
