package gates

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/security"
)

// Batch AI review of pending gate requests (STA-868). Advisory only: the
// result pre-checks boxes in the UI and the Board still confirms with Touch ID.
// Gemini is used because this is non-code work (Board rule).

// ReviewItem is one pending request as the reviewer sees it.
type ReviewItem struct {
	Request *security.GateRequest
	Scripts []security.ScriptRef
}

// ItemReview is the reviewer's call on one request.
type ItemReview struct {
	ID             string `json:"id"`
	Recommendation string `json:"recommendation"` // approved | denied
	Reason         string `json:"reason"`
}

// ReviewResult is a whole batch review.
type ReviewResult struct {
	Summary    string       `json:"summary"`
	Suggestion string       `json:"suggestion"` // e.g. "approve 3, deny 1"
	Items      []ItemReview `json:"items"`
	Model      string       `json:"model,omitempty"`
	LatencyMS  int64        `json:"latency_ms"`
}

// Reviewer reviews a batch of requests.
type Reviewer interface {
	Review(ctx context.Context, items []ReviewItem) (ReviewResult, error)
}

// GeminiReviewer calls the Gemini generateContent REST API.
type GeminiReviewer struct {
	APIKey  string
	Model   string
	BaseURL string
	HTTP    *http.Client
}

// NewGeminiReviewerFromEnv uses GEMINI_API_KEY and STAYPOINT_GEMINI_MODEL.
// It returns nil when no key is set (the review endpoint then reports that).
func NewGeminiReviewerFromEnv() *GeminiReviewer {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		return nil
	}
	model := os.Getenv("STAYPOINT_GEMINI_MODEL")
	if model == "" {
		model = "gemini-3.8-flash"
	}
	return &GeminiReviewer{APIKey: key, Model: model, BaseURL: "https://generativelanguage.googleapis.com",
		HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// ReviewPrompt is the text sent to the reviewer.
func ReviewPrompt(items []ReviewItem) string {
	var b strings.Builder
	b.WriteString("You review shell commands that autonomous coding agents want to run. StayPoint's classifier held each one as Red-tier ")
	b.WriteString("(possibly destructive, secret-touching or exfiltrating) for the human Board. For each request decide whether the Board ")
	b.WriteString("should approve (safe: read-only surveys, writes only to temp/scratch dirs, routine work) or deny (destroys data, touches ")
	b.WriteString("credentials, pushes or publishes, sends data out, or is unclear). Be conservative: deny when unsure.\n")
	b.WriteString("Return JSON: summary (2-3 sentences), suggestion (e.g. \"approve 3, deny 1\"), items[{id, recommendation: approved|denied, reason (one line)}].\n")
	for i, it := range items {
		fmt.Fprintf(&b, "\n=== Request %d, id %s ===\n%s", i+1, it.Request.ID, DescribeRequest(it.Request, it.Scripts))
	}
	return b.String()
}

// Review implements Reviewer.
func (g *GeminiReviewer) Review(ctx context.Context, items []ReviewItem) (ReviewResult, error) {
	if g == nil || g.APIKey == "" {
		return ReviewResult{}, errors.New("Gemini review not configured (GEMINI_API_KEY unset in the daemon environment)")
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"summary":    map[string]any{"type": "string"},
			"suggestion": map[string]any{"type": "string"},
			"items": map[string]any{"type": "array", "items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":             map[string]any{"type": "string"},
					"recommendation": map[string]any{"type": "string", "enum": []string{RecApprove, RecDeny}},
					"reason":         map[string]any{"type": "string"},
				},
				"required": []string{"id", "recommendation", "reason"},
			}},
		},
		"required": []string{"summary", "suggestion", "items"},
	}
	body, _ := json.Marshal(map[string]any{
		"contents": []map[string]any{{"parts": []map[string]any{{"text": ReviewPrompt(items)}}}},
		"generationConfig": map[string]any{
			"temperature": 0.1, "responseMimeType": "application/json", "responseSchema": schema,
		},
	})
	u := fmt.Sprintf("%s/v1beta/models/%s:generateContent", strings.TrimRight(g.BaseURL, "/"), url.PathEscape(g.Model))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return ReviewResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", g.APIKey)
	hc := g.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	start := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		return ReviewResult{}, fmt.Errorf("gemini: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return ReviewResult{}, fmt.Errorf("gemini: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var gr struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &gr); err != nil || len(gr.Candidates) == 0 || len(gr.Candidates[0].Content.Parts) == 0 {
		return ReviewResult{}, errors.New("gemini: empty or malformed response")
	}
	var out ReviewResult
	if err := json.Unmarshal([]byte(gr.Candidates[0].Content.Parts[0].Text), &out); err != nil {
		return ReviewResult{}, fmt.Errorf("gemini: response is not the requested JSON: %w", err)
	}
	out.Model, out.LatencyMS = g.Model, time.Since(start).Milliseconds()
	return out, nil
}

// RunReview reviews items and logs each recommendation in decision_log.
// Recommendations for ids not in the batch, or with unknown values, are
// dropped; a request the reviewer skipped gets no recommendation.
func RunReview(ctx context.Context, db *sql.DB, rv Reviewer, items []ReviewItem) (ReviewResult, error) {
	if rv == nil {
		return ReviewResult{}, errors.New("Gemini review not configured (GEMINI_API_KEY unset in the daemon environment)")
	}
	if len(items) == 0 {
		return ReviewResult{Summary: "No pending requests.", Items: []ItemReview{}}, nil
	}
	res, err := rv.Review(ctx, items)
	if err != nil {
		return ReviewResult{}, err
	}
	known := map[string]bool{}
	for _, it := range items {
		known[it.Request.ID] = true
	}
	clean := []ItemReview{}
	seen := map[string]bool{}
	for _, it := range res.Items {
		if !known[it.ID] || seen[it.ID] || (it.Recommendation != RecApprove && it.Recommendation != RecDeny) {
			continue
		}
		seen[it.ID] = true
		clean = append(clean, it)
		_ = LogAdvice(db, it.ID, Advice{Advisor: AdvisorGemini, Model: res.Model, Recommendation: it.Recommendation,
			Reason: truncate(it.Reason, 300), LatencyMS: res.LatencyMS})
	}
	res.Items = clean
	return res, nil
}
