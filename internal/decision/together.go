// Package decision provides a lightweight tev1 decision client for StayPoint.
// It targets a local OpenAI-compatible endpoint (Ollama/llama.cpp) by default;
// Together AI is available via TOGETHER_BASE_URL + TOGETHER_API_KEY env vars.
package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultLocalURL    = "http://localhost:11434/v1/chat/completions"
	defaultTogetherURL = "https://api.together.xyz/v1/chat/completions"
	defaultLocalModel  = "tev1-4b"
	defaultRemoteModel = "tev1-4B-experimental"
)

// ErrNoKey is returned when a Together AI call is attempted but TOGETHER_API_KEY is unset.
var ErrNoKey = errors.New("decision: TOGETHER_API_KEY not set")

// Option is a single labelled choice presented to the model.
type Option struct {
	Key    string // machine key returned in DecisionResult
	Letter string // single capital letter the model should emit (A, B, C…)
	Label  string // human-readable label
}

// DecisionRequest is the input to Decide.
type DecisionRequest struct {
	State    string   // context description
	Question string   // question posed to the model
	Options  []Option // ordered list of choices
}

// DecisionResult is the parsed output from Decide.
type DecisionResult struct {
	SelectedKey    string // Option.Key of the chosen option
	SelectedLetter string // single capital letter the model emitted
	RawResponse    string // raw JSON response body for debugging
	// Reason is the model's one-line justification (DecideWithReason only).
	Reason string
}

// TogetherDecisionClient calls an OpenAI-compatible /v1/chat/completions endpoint
// for structured single-letter advisory decisions.
type TogetherDecisionClient struct {
	baseURL    string
	apiKey     string // empty → no Authorization header (local endpoint)
	model      string
	httpClient *http.Client
}

// New returns a client configured from environment variables.
//
// Priority:
//  1. DECISION_BASE_URL overrides the endpoint.
//  2. If TOGETHER_API_KEY is set (with optional TOGETHER_BASE_URL), use Together AI.
//  3. Otherwise use the local Ollama endpoint (DECISION_BASE_URL or localhost:11434).
//
// DECISION_MODEL overrides the model name.
func New() (*TogetherDecisionClient, error) {
	apiKey := os.Getenv("TOGETHER_API_KEY")
	baseURL := firstNonEmpty(
		os.Getenv("DECISION_BASE_URL"),
		os.Getenv("TOGETHER_BASE_URL"),
	)

	if baseURL == "" {
		if apiKey != "" {
			baseURL = defaultTogetherURL
		} else {
			baseURL = defaultLocalURL
		}
	}

	model := os.Getenv("DECISION_MODEL")
	if model == "" {
		if apiKey != "" {
			model = defaultRemoteModel
		} else {
			model = defaultLocalModel
		}
	}

	return &TogetherDecisionClient{
		baseURL:    baseURL,
		apiKey:     apiKey,
		model:      model,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// NewTogether returns a client pinned to the Together AI endpoint.
// Returns ErrNoKey if TOGETHER_API_KEY is unset.
func NewTogether() (*TogetherDecisionClient, error) {
	apiKey := os.Getenv("TOGETHER_API_KEY")
	if apiKey == "" {
		return nil, ErrNoKey
	}
	baseURL := firstNonEmpty(os.Getenv("TOGETHER_BASE_URL"), defaultTogetherURL)
	model := firstNonEmpty(os.Getenv("DECISION_MODEL"), defaultRemoteModel)
	return &TogetherDecisionClient{
		baseURL:    baseURL,
		apiKey:     apiKey,
		model:      model,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// NewWithEndpoint returns a client for an explicit endpoint (tests, fakes).
func NewWithEndpoint(baseURL, apiKey, model string, hc *http.Client) *TogetherDecisionClient {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &TogetherDecisionClient{baseURL: baseURL, apiKey: apiKey, model: model, httpClient: hc}
}

// Model returns the model name requests are sent to.
func (c *TogetherDecisionClient) Model() string { return c.model }

// Decide sends req to the configured endpoint and returns the parsed single-letter pick.
func (c *TogetherDecisionClient) Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	return c.call(ctx, buildPrompt(req), 8, req.Options)
}

// DecideWithReason is Decide plus a one-line reason after the letter
// ("A - read-only du/git survey"), for advisories shown to the Board (STA-868).
func (c *TogetherDecisionClient) DecideWithReason(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	prompt := buildPrompt(req) + "\nReply as: <letter> - <one-line reason, at most 15 words>\n"
	res, err := c.call(ctx, prompt, 60, req.Options)
	if err != nil {
		return res, err
	}
	res.Reason = parseReason(res.RawResponse)
	return res, nil
}

// parseReason extracts the text after the option letter in a chat response.
func parseReason(raw string) string {
	var cr chatResponse
	if json.Unmarshal([]byte(raw), &cr) != nil || len(cr.Choices) == 0 {
		return ""
	}
	content := strings.TrimSpace(cr.Choices[0].Message.Content)
	if r := []rune(content); len(r) > 0 {
		content = string(r[1:])
	}
	content = strings.TrimLeft(content, " .):-\u2013\u2014\t")
	if i := strings.IndexByte(content, '\n'); i >= 0 {
		content = content[:i]
	}
	if r := []rune(content); len(r) > 200 {
		content = string(r[:200])
	}
	return strings.TrimSpace(content)
}

func (c *TogetherDecisionClient) call(ctx context.Context, prompt string, maxTokens int, options []Option) (DecisionResult, error) {
	body := map[string]any{
		"model": c.model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"temperature": 0,
		"max_tokens":  maxTokens,
		"stream":      false,
		"chat_template_kwargs": map[string]any{
			"enable_thinking": false,
		},
	}

	b, err := json.Marshal(body)
	if err != nil {
		return DecisionResult{}, fmt.Errorf("decision: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(b))
	if err != nil {
		return DecisionResult{}, fmt.Errorf("decision: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return DecisionResult{}, fmt.Errorf("decision: http: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return DecisionResult{}, fmt.Errorf("decision: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return DecisionResult{}, fmt.Errorf("decision: server %d: %s", resp.StatusCode, raw)
	}

	return parseResponse(raw, options)
}

// chatResponse is the minimal OpenAI chat completions shape we need.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func parseResponse(raw []byte, options []Option) (DecisionResult, error) {
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return DecisionResult{}, fmt.Errorf("decision: parse response: %w", err)
	}
	if len(cr.Choices) == 0 || cr.Choices[0].Message.Content == "" {
		return DecisionResult{RawResponse: string(raw)}, fmt.Errorf("decision: empty response")
	}

	content := strings.TrimSpace(cr.Choices[0].Message.Content)
	letter := strings.ToUpper(string([]rune(content)[0]))

	for _, opt := range options {
		if strings.EqualFold(opt.Letter, letter) {
			return DecisionResult{
				SelectedKey:    opt.Key,
				SelectedLetter: letter,
				RawResponse:    string(raw),
			}, nil
		}
	}
	return DecisionResult{RawResponse: string(raw)},
		fmt.Errorf("decision: response %q did not match any option letter", content)
}

func buildPrompt(req DecisionRequest) string {
	var sb strings.Builder
	sb.WriteString("State: ")
	sb.WriteString(req.State)
	sb.WriteString("\n\nQuestion: ")
	sb.WriteString(req.Question)
	sb.WriteString("\n\nOptions (reply with the single capital letter only):\n")
	for _, opt := range req.Options {
		sb.WriteString(opt.Letter)
		sb.WriteString(". ")
		sb.WriteString(opt.Label)
		sb.WriteString(" [key=")
		sb.WriteString(opt.Key)
		sb.WriteString("]\n")
	}
	return sb.String()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
