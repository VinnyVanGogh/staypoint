package decision

// #243: the local tev1 client must stay on this machine even when the local
// endpoint answers with a redirect, and must not trust a malformed
// /v1/systemone answer (an out-of-range probability would clear any trust
// threshold).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var approveDeny = []Option{{Key: "approved", Letter: "A", Label: "Approve"}, {Key: "denied", Letter: "B", Label: "Deny"}}

func localSystemOne(t *testing.T, h http.HandlerFunc) *TogetherDecisionClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("DECISION_LOCAL_URL", srv.URL+"/v1/systemone")
	return NewLocal()
}

func TestNewLocal_DoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"answers":{"decision":{"choice":"approved","probabilities":{"approved":1,"denied":0}}}}`))
	}))
	defer other.Close()

	for _, loc := range []string{
		"http://192.0.2.1:9/v1/systemone", // TEST-NET-1: never loopback
		"https://api.together.xyz/v1/systemone",
		other.URL + "/v1/systemone", // even another loopback port
	} {
		c := localSystemOne(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, loc, http.StatusTemporaryRedirect)
		})
		start := time.Now()
		res, err := c.DecideWithReason(context.Background(), DecisionRequest{State: "s", Question: "q", Options: approveDeny})
		if err == nil {
			t.Fatalf("redirect to %s: decision %+v, want an error", loc, res)
		}
		if !strings.Contains(err.Error(), "307") {
			t.Errorf("redirect to %s: err %v, want the 307 itself reported (redirect not followed)", loc, err)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("redirect to %s took %s: it was followed", loc, d)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("redirect target was hit %d times", hits.Load())
	}
}

// The transport itself refuses to dial anything that is not loopback, so
// no code path (redirect, proxy, a future caller) can leave the machine.
func TestNewLocal_TransportRefusesNonLoopback(t *testing.T) {
	c := NewLocal()
	for _, u := range []string{
		"http://192.0.2.1:9/x",
		"http://10.0.0.5:11434/v1/systemone",
		"http://[2001:db8::1]:80/x",
		"http://127.0.0.1@192.0.2.1:9/x", // userinfo trick: the host is 192.0.2.1
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		_, err := c.httpClient.Do(req)
		cancel()
		if err == nil || !strings.Contains(err.Error(), "non-loopback") {
			t.Errorf("GET %s: err %v, want a non-loopback refusal", u, err)
		}
	}
}

func TestNewLocal_RefusesUserinfoAndHostnameTricks(t *testing.T) {
	for _, u := range []string{
		"http://127.0.0.1@evil.example/v1/systemone",
		"http://localhost@evil.example/v1/systemone",
		"http://evil.example#@127.0.0.1/v1/systemone",
		"http://evil.example\\@127.0.0.1/v1/systemone",
		"http://127.0.0.1.evil.example/v1/systemone",
		"http://localhost.evil.example/v1/systemone",
		"http://0x7f000001.evil.example/v1",
		"http://user:pass@127.0.0.1:11434/v1/systemone", // credentials are never sent
	} {
		t.Setenv("DECISION_LOCAL_URL", u)
		if got := NewLocal().BaseURL(); got != defaultSystemOneURL {
			t.Errorf("DECISION_LOCAL_URL=%q honoured as %q, want the default", u, got)
		}
	}
}

func TestSystemOne_RejectsMalformedProbabilities(t *testing.T) {
	cases := []struct{ name, body string }{
		{"probability above 1", `{"answers":{"decision":{"choice":"approved","probabilities":{"approved":7,"denied":0}}}}`},
		{"negative probability", `{"answers":{"decision":{"choice":"approved","probabilities":{"approved":1.2,"denied":-0.2}}}}`},
		{"missing probabilities", `{"answers":{"decision":{"choice":"approved"}}}`},
		{"chosen probability missing", `{"answers":{"decision":{"choice":"approved","probabilities":{"denied":1}}}}`},
		{"chosen is not argmax", `{"answers":{"decision":{"choice":"approved","probabilities":{"approved":0.2,"denied":0.8}}}}`},
		{"sum too low", `{"answers":{"decision":{"choice":"approved","probabilities":{"approved":0.5,"denied":0.1}}}}`},
		{"sum too high", `{"answers":{"decision":{"choice":"approved","probabilities":{"approved":0.9,"denied":0.6}}}}`},
		{"unknown key", `{"answers":{"decision":{"choice":"approved","probabilities":{"approved":0.9,"rm-rf":0.1}}}}`},
		{"choice not an option", `{"answers":{"decision":{"choice":"yolo","probabilities":{"yolo":1}}}}`},
		{"huge exponent", `{"answers":{"decision":{"choice":"approved","probabilities":{"approved":1e308,"denied":0}}}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cl := localSystemOne(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(c.body)) })
			res, err := cl.DecideWithReason(context.Background(), DecisionRequest{State: "s", Question: "q", Options: approveDeny})
			if err == nil {
				t.Fatalf("accepted %s: %+v", c.body, res)
			}
			if res.SelectedKey != "" {
				t.Fatalf("error result still carries a decision: %+v", res)
			}
		})
	}
}

func TestSystemOne_AcceptsWellFormedAnswer(t *testing.T) {
	cl := localSystemOne(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"decision":{"choice":"approved","probabilities":{"approved":0.71,"denied":0.28},"confidence":0.4}}}`))
	})
	res, err := cl.DecideWithReason(context.Background(), DecisionRequest{State: "s", Question: "q", Options: approveDeny})
	if err != nil || res.SelectedKey != "approved" || res.Probability != 0.71 {
		t.Fatalf("res %+v err %v", res, err)
	}
}
