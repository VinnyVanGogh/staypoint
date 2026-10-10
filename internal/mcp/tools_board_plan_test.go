package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/config"
)

// boardPlanServer is an MCP server whose data dir holds the daemon tokens and
// whose proposals go to daemon (a fake staypointd).
func boardPlanServer(t *testing.T, daemon *httptest.Server) *Server {
	t.Helper()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "auth_token"), []byte("auth-token-0123456789abcdef"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "board_token"), []byte("board-token-0123456789abcdef"), 0o600)
	cfg := config.DefaultConfig()
	cfg.DataDir = dir
	s := NewServer(WithConfig(cfg))
	if daemon != nil {
		s.boardPlanDaemonURL = daemon.URL
	}
	return s
}

func proposeArgs(t *testing.T) json.RawMessage {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"actions": `[{"task_id":"task-1","action":"run_now"}]`})
	return b
}

// Inside a daemon run the tool refuses before reading any token or calling
// the daemon.
func TestBoardPlanPropose_RefusedInAgentRun(t *testing.T) {
	called := false
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer daemon.Close()
	t.Setenv("STAYPOINT_TASK_ID", "task-agent-run")
	s := boardPlanServer(t, daemon)

	res := s.handleCallTool(context.Background(), CallToolParams{Name: "staypoint_board_plan_propose", Arguments: proposeArgs(t)})
	if !res.IsError || !strings.Contains(res.Content[0].Text, "agent run") {
		t.Fatalf("want agent-run refusal, got %+v", res)
	}
	if called {
		t.Fatal("daemon was called from an agent run")
	}
}

// From the Board's session the tool sends the board token and returns the
// review URL.
func TestBoardPlanPropose_SendsBoardTokenAndReturnsURL(t *testing.T) {
	var gotBoard, gotProposer string
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBoard = r.Header.Get("X-Board-Token")
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Proposer string `json:"proposer"`
		}
		_ = json.Unmarshal(raw, &body)
		gotProposer = body.Proposer
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"plan":{"id":"plan-1234","actions":[{"task_id":"task-1","action":"run_now"}]},"url":"/board/plans/plan-1234"}`))
	}))
	defer daemon.Close()
	t.Setenv("STAYPOINT_TASK_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-42")
	s := boardPlanServer(t, daemon)

	res := s.handleCallTool(context.Background(), CallToolParams{Name: "staypoint_board_plan_propose", Arguments: proposeArgs(t)})
	if res.IsError {
		t.Fatalf("propose failed: %s", res.Content[0].Text)
	}
	if gotBoard != "board-token-0123456789abcdef" {
		t.Fatalf("board token not sent: %q", gotBoard)
	}
	if gotProposer != "claude-session:sess-42" {
		t.Fatalf("proposer %q, want the Claude session id", gotProposer)
	}
	if !strings.Contains(res.Content[0].Text, "/board/plans/plan-1234") || !strings.Contains(res.Content[0].Text, "http://localhost:") {
		t.Fatalf("review URL missing: %s", res.Content[0].Text)
	}
}

func TestToolsListIncludesBoardPlanPropose(t *testing.T) {
	s := NewServer()
	defer s.Close()
	for _, tool := range s.getToolsList() {
		if tool.Name == "staypoint_board_plan_propose" {
			return
		}
	}
	t.Fatal("staypoint_board_plan_propose not listed")
}
