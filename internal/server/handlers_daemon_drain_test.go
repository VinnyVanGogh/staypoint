package server_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/orchestrator"
)

func drainReq(t *testing.T, method, url, token, board, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if board != "" {
		req.AddCookie(&http.Cookie{Name: "staypoint_board", Value: board})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// An agent holds the API token but not the Board cookie: it must not be able
// to put the daemon in drain (every new run would stop) or cancel one.
func TestDrainEndpoints_BoardOnly(t *testing.T) {
	prev := orchestrator.GlobalRunSlots
	slots := orchestrator.NewRunSlots(3)
	slots.Wake = func(string, string) {}
	orchestrator.GlobalRunSlots = slots
	t.Cleanup(func() { orchestrator.GlobalRunSlots = prev })

	srv, token := startTestServer(t, setupTestDB(t))
	url := srv.URL() + "/api/daemon/drain"

	if code, _ := drainReq(t, http.MethodPost, url, token, "", `{"mode":"finish"}`); code != http.StatusForbidden {
		t.Fatalf("POST without Board session = %d, want 403", code)
	}
	if code, _ := drainReq(t, http.MethodPost, url, token, "not-the-board-token", `{"mode":"now"}`); code != http.StatusForbidden {
		t.Fatalf("POST with a wrong Board cookie = %d, want 403", code)
	}
	if slots.Drain() != orchestrator.DrainOff {
		t.Fatalf("a refused request started a drain: %v", slots.Drain())
	}

	code, body := drainReq(t, http.MethodGet, url, token, "", "")
	if code != http.StatusOK || !strings.Contains(body, `"draining":false`) {
		t.Fatalf("GET = %d %s, want 200 not draining", code, body)
	}

	if code, body := drainReq(t, http.MethodPost, url, token, srv.BoardToken(), `{"mode":"bogus"}`); code != http.StatusBadRequest {
		t.Fatalf("bad mode = %d %s, want 400", code, body)
	}
	code, body = drainReq(t, http.MethodPost, url, token, srv.BoardToken(), `{"mode":"finish"}`)
	if code != http.StatusOK || !strings.Contains(body, `"draining":true`) || !strings.Contains(body, "Draining for deploy: 0 runs left, 0 queued") {
		t.Fatalf("Board POST = %d %s, want 200 draining with label", code, body)
	}

	if code, _ := drainReq(t, http.MethodDelete, url, token, "", ""); code != http.StatusForbidden {
		t.Fatalf("DELETE without Board session = %d, want 403", code)
	}
	if slots.Drain() == orchestrator.DrainOff {
		t.Fatal("a refused DELETE cancelled the drain")
	}
	if code, body := drainReq(t, http.MethodDelete, url, token, srv.BoardToken(), ""); code != http.StatusOK || !strings.Contains(body, `"draining":false`) {
		t.Fatalf("Board DELETE = %d %s, want 200 not draining", code, body)
	}
}
