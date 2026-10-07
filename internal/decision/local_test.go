package decision

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Board rule: decision traffic never leaves the machine, even with a
// Together key and a remote URL in the environment.
func TestNewLocal_IgnoresTogetherAndRemoteURLs(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "sk-should-never-be-sent")
	t.Setenv("TOGETHER_BASE_URL", "https://api.together.xyz/v1/chat/completions")
	for _, remote := range []string{
		"https://api.together.xyz/v1/chat/completions",
		"http://10.0.0.5:11434/v1/chat/completions",
		"http://localhost.evil.com/v1",
		"http://127.0.0.1.nip.io/v1",
		"ftp://127.0.0.1/x",
	} {
		t.Setenv("DECISION_LOCAL_URL", remote)
		c := NewLocal()
		if c.BaseURL() != defaultSystemOneURL {
			t.Errorf("DECISION_LOCAL_URL=%q: base URL %q, want default local", remote, c.BaseURL())
		}
		if c.apiKey != "" {
			t.Errorf("local client must carry no API key")
		}
	}
	for _, local := range []string{"http://127.0.0.1:8080/v1/chat/completions", "http://localhost:1234/v1", "http://[::1]:11434/v1"} {
		t.Setenv("DECISION_LOCAL_URL", local)
		if got := NewLocal().BaseURL(); got != local {
			t.Errorf("loopback %q should be honoured, got %q", local, got)
		}
	}
}

func TestNewLocal_SendsNoAuthorizationHeader(t *testing.T) {
	t.Setenv("TOGETHER_API_KEY", "sk-should-never-be-sent")
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"A"}}]}`))
	}))
	defer srv.Close()
	t.Setenv("DECISION_LOCAL_URL", srv.URL) // httptest listens on 127.0.0.1
	c := NewLocal()
	if !strings.HasPrefix(c.BaseURL(), "http://127.0.0.1") {
		t.Fatalf("test server should be loopback, got %s", c.BaseURL())
	}
	_, _ = c.Decide(context.Background(), DecisionRequest{State: "s", Question: "q",
		Options: []Option{{Key: "a", Letter: "A", Label: "a"}, {Key: "b", Letter: "B", Label: "b"}}})
	if auth != "" {
		t.Fatalf("local client sent Authorization header %q", auth)
	}
}

// tev1 answers only on Ollama's /v1/systemone (decision capability).
func TestSystemOne_RequestShapeAndParse(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"model":"tev1-4b","answers":{"decision":{"type":"choice","choice":"denied","probabilities":{"approved":0.2,"denied":0.8},"confidence":0.27}}}`))
	}))
	defer srv.Close()
	t.Setenv("TOGETHER_API_KEY", "sk-should-never-be-sent")
	t.Setenv("DECISION_LOCAL_URL", srv.URL+"/v1/systemone")
	c := NewLocal()
	res, err := c.DecideWithReason(context.Background(), DecisionRequest{
		State: "Command: rm -rf ~", Question: "Approve?",
		Options: []Option{{Key: "approved", Letter: "A", Label: "Approve"}, {Key: "denied", Letter: "B", Label: "Deny"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectedKey != "denied" || res.SelectedLetter != "B" {
		t.Fatalf("got %+v, want denied/B", res)
	}
	if !strings.Contains(res.Reason, "p=0.80") {
		t.Errorf("reason should carry the probability, got %q", res.Reason)
	}
	if auth != "" {
		t.Fatalf("local client sent Authorization %q", auth)
	}
	q := got["questions"].(map[string]any)["decision"].(map[string]any)
	if q["type"] != "choice" || q["instructions"] != "Approve?" {
		t.Fatalf("bad question: %v", q)
	}
	if crit := q["criteria"].(map[string]any); crit["approved"] != "Approve" || crit["denied"] != "Deny" {
		t.Fatalf("criteria must be option keys to labels: %v", crit)
	}
}

func TestSystemOne_UnknownChoiceIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"decision":{"choice":"maybe"}}}`))
	}))
	defer srv.Close()
	t.Setenv("DECISION_LOCAL_URL", srv.URL+"/v1/systemone")
	_, err := NewLocal().Decide(context.Background(), DecisionRequest{Options: []Option{{Key: "approved", Letter: "A"}}})
	if err == nil {
		t.Fatal("an unknown choice must be an error, never a silent approve")
	}
}
