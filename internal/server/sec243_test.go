package server_test

// #243-2: in trust mode tev1 decides alone, so a malformed advisor answer
// must fail closed: an out-of-range probability never clears the threshold.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/decision"
	"github.com/VinnyVanGogh/staypoint/internal/gates"
	"github.com/VinnyVanGogh/staypoint/internal/security"
)

func TestSec243_Tev1MalformedAnswerNeverApproves(t *testing.T) {
	answer := func(body string) gates.RequestAdvisor {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		t.Cleanup(srv.Close)
		return gates.TogetherAdvisor{Client: decision.NewWithEndpoint(srv.URL+"/v1/systemone", "", "tev1-4b", nil)}
	}
	cases := []struct {
		name string
		adv  gates.RequestAdvisor
	}{
		{"systemone p=7", answer(`{"answers":{"decision":{"choice":"approved","probabilities":{"approved":7,"denied":0}}}}`)},
		{"systemone missing probabilities", answer(`{"answers":{"decision":{"choice":"approved"}}}`)},
		{"systemone chosen is not argmax", answer(`{"answers":{"decision":{"choice":"approved","probabilities":{"approved":0.1,"denied":0.9}}}}`)},
		// An advisor that skips the client's checks is still bounded here.
		{"advisor p=7", &tev1Fake{rec: gates.RecApprove, p: 7}},
		{"advisor p<0", &tev1Fake{rec: gates.RecApprove, p: -1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := startGateServer(t, c.adv, nil)
			dir := runningTask(t, e, "T1")
			e.trust(t, "T1", `{"preset":"overnight","tev1":true,"tev1_ack":true}`)
			got := e.createIn(t, "sudo ls /var/log", "T1", dir)
			gr := waitDecided(t, e, got["id"].(string))
			if gr.Status != security.GateRequestDenied {
				t.Fatalf("tev1 decision %s, want denied (fail closed)", gr.Status)
			}
			if blocked, _ := blockedReason(t, e, "T1"); !blocked {
				t.Fatal("task not parked after a malformed tev1 answer")
			}
		})
	}
}
