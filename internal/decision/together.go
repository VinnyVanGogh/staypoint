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
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultLocalURL = "http://localhost:11434/v1/chat/completions"
	// defaultSystemOneURL is Ollama's decision-model endpoint (0.35+). tev1
	// only answers here: it has the "decision" capability, not chat.
	defaultSystemOneURL = "http://localhost:11434/v1/systemone"
	defaultTogetherURL  = "https://api.together.xyz/v1/chat/completions"
	defaultLocalModel   = "tev1-4b"
	defaultRemoteModel  = "tev1-4B-experimental"
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
	// Probability is the model's probability for the selected option, from
	// /v1/systemone only; 0 when the endpoint does not report one.
	Probability float64
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

// NewLocal returns a client for the local model only (Board rule, 2026-10-07:
// decision traffic stays on this machine and is never billed). It ignores
// TOGETHER_API_KEY / TOGETHER_BASE_URL, sends no API key, and accepts
// DECISION_LOCAL_URL only when it points at a loopback host; anything else
// falls back to the default Ollama endpoint.
func NewLocal() *TogetherDecisionClient {
	baseURL := defaultSystemOneURL
	if u := os.Getenv("DECISION_LOCAL_URL"); u != "" && isLoopbackURL(u) {
		baseURL = u
	}
	return &TogetherDecisionClient{
		baseURL:    baseURL,
		model:      firstNonEmpty(os.Getenv("DECISION_LOCAL_MODEL"), defaultLocalModel),
		httpClient: newLoopbackHTTPClient(),
	}
}

// newLoopbackHTTPClient is an http.Client that cannot leave this machine
// (#243-1): it never follows redirects (a 3xx is returned as the response),
// uses no proxy, and its dialer resolves the host and refuses to connect
// unless every address is loopback.
func newLoopbackHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
				if err != nil {
					return nil, fmt.Errorf("decision: resolve %q: %w", host, err)
				}
				if len(ips) == 0 {
					return nil, fmt.Errorf("decision: refusing non-loopback host %q: no addresses", host)
				}
				for _, ip := range ips {
					if !ip.IP.IsLoopback() {
						return nil, fmt.Errorf("decision: refusing non-loopback address %s for host %q", ip.IP, host)
					}
				}
				// Dial the checked addresses, not the name, so a second
				// lookup cannot answer differently. "localhost" may resolve
				// to ::1 first while the server listens on 127.0.0.1 only.
				var lastErr error
				for _, ip := range ips {
					conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
					if err == nil {
						return conn, nil
					}
					lastErr = err
				}
				return nil, lastErr
			},
			ForceAttemptHTTP2:   false,
			TLSHandshakeTimeout: 5 * time.Second,
			MaxIdleConns:        2,
			IdleConnTimeout:     30 * time.Second,
		},
	}
}

// isLoopbackURL reports whether raw is an http(s) URL whose host is
// localhost or a loopback IP, with no userinfo.
func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Opaque != "" {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// BaseURL returns the endpoint the client calls.
func (c *TogetherDecisionClient) BaseURL() string { return c.baseURL }

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
	if c.isSystemOne() {
		return c.callSystemOne(ctx, req)
	}
	return c.call(ctx, buildPrompt(req), 8, req.Options)
}

// DecideWithReason is Decide plus a one-line reason after the letter
// ("A - read-only du/git survey"), for advisories shown to the Board (STA-868).
func (c *TogetherDecisionClient) DecideWithReason(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	if c.isSystemOne() {
		// Decision models return probabilities, not prose: the reason is
		// the winning probability and the model's confidence.
		return c.callSystemOne(ctx, req)
	}
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

// isSystemOne reports whether the client targets Ollama's decision endpoint.
func (c *TogetherDecisionClient) isSystemOne() bool {
	u, err := url.Parse(c.baseURL)
	return err == nil && strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/v1/systemone")
}

// systemOneResponse is the part of a /v1/systemone reply we use.
type systemOneResponse struct {
	Answers map[string]struct {
		Choice        string             `json:"choice"`
		Probabilities map[string]float64 `json:"probabilities"`
		Confidence    float64            `json:"confidence"`
	} `json:"answers"`
}

// callSystemOne asks one "choice" question whose criteria are the option
// keys, and maps the winning key back to its option.
func (c *TogetherDecisionClient) callSystemOne(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	criteria := make(map[string]string, len(req.Options))
	for _, o := range req.Options {
		criteria[o.Key] = o.Label
	}
	body := map[string]any{
		"model": c.model,
		"state": map[string]string{"context": req.State},
		"questions": map[string]any{
			"decision": map[string]any{
				"type":         "choice",
				"instructions": req.Question,
				"criteria":     criteria,
			},
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
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return DecisionResult{}, fmt.Errorf("decision: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return DecisionResult{}, fmt.Errorf("decision: server %d: %s", resp.StatusCode, raw)
	}
	var sr systemOneResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		return DecisionResult{RawResponse: string(raw)}, fmt.Errorf("decision: parse response: %w", err)
	}
	ans, ok := sr.Answers["decision"]
	if !ok || ans.Choice == "" {
		return DecisionResult{RawResponse: string(raw)}, fmt.Errorf("decision: no answer in response")
	}
	if err := validateSystemOneAnswer(ans.Choice, ans.Probabilities, req.Options); err != nil {
		return DecisionResult{RawResponse: string(raw)}, err
	}
	for _, o := range req.Options {
		if o.Key == ans.Choice {
			return DecisionResult{
				SelectedKey:    o.Key,
				SelectedLetter: o.Letter,
				RawResponse:    string(raw),
				Reason:         fmt.Sprintf("p=%.2f, confidence %.2f", ans.Probabilities[o.Key], ans.Confidence),
				Probability:    ans.Probabilities[o.Key],
			}, nil
		}
	}
	return DecisionResult{RawResponse: string(raw)}, fmt.Errorf("decision: unknown choice %q", ans.Choice)
}

// validateSystemOneAnswer rejects a /v1/systemone answer that is not a
// probability distribution over the options with the choice as its mode
// (#243-2): every probability finite and in [0,1], one per option and no
// others, summing to 1 within 0.05, and the chosen option an option with the
// highest probability. Callers compare the probability to a trust threshold,
// so an unchecked p=7 would clear any threshold.
func validateSystemOneAnswer(choice string, probs map[string]float64, options []Option) error {
	if len(probs) == 0 {
		return fmt.Errorf("decision: answer has no probabilities")
	}
	known := make(map[string]bool, len(options))
	for _, o := range options {
		known[o.Key] = true
	}
	if !known[choice] {
		return fmt.Errorf("decision: unknown choice %q", choice)
	}
	sum, maxP := 0.0, math.Inf(-1)
	for k, p := range probs {
		if !known[k] {
			return fmt.Errorf("decision: probability for unknown option %q", k)
		}
		if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 || p > 1 {
			return fmt.Errorf("decision: probability %v for %q is outside [0,1]", p, k)
		}
		sum += p
		maxP = math.Max(maxP, p)
	}
	for k := range known {
		if _, ok := probs[k]; !ok {
			return fmt.Errorf("decision: no probability for option %q", k)
		}
	}
	if sum < 0.95 || sum > 1.05 {
		return fmt.Errorf("decision: probabilities sum to %.3f, not 1", sum)
	}
	if probs[choice] < maxP {
		return fmt.Errorf("decision: choice %q (p=%.2f) is not the most probable option (p=%.2f)", choice, probs[choice], maxP)
	}
	return nil
}
