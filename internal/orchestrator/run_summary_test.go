package orchestrator

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// extractFinalResponse
// ---------------------------------------------------------------------------

func TestExtractFinalResponse_Claude(t *testing.T) {
	// Minimal Claude stream-json: one assistant event with two text blocks
	// (the second includes the task-complete marker which must be stripped).
	data := []byte(`{"type":"system","subtype":"init","session_id":"s1","model":"claude-opus-5"}
{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"text","text":"Thinking..."},{"type":"thinking","thinking":"internal"}]}}
{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"text","text":"Done. All changes applied."},{"type":"text","text":" [[TASK_COMPLETE]]"}]}}
{"type":"result","subtype":"success","result":"ok"}
`)

	got := extractFinalResponse(data)
	if !strings.Contains(got, "Done. All changes applied.") {
		t.Errorf("expected final assistant text; got: %q", got)
	}
	if strings.Contains(got, taskCompleteMarker) {
		t.Errorf("task-complete marker must be stripped; got: %q", got)
	}
}

func TestExtractFinalResponse_LastAssistantWins(t *testing.T) {
	// Multiple assistant events: only the last one should be returned.
	data := []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"first response"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"second response [[TASK_COMPLETE]]"}]}}
`)
	got := extractFinalResponse(data)
	if !strings.Contains(got, "second response") {
		t.Errorf("expected last assistant response; got: %q", got)
	}
	if strings.Contains(got, "first response") {
		t.Errorf("earlier response must not be included; got: %q", got)
	}
}

func TestExtractFinalResponse_Agy(t *testing.T) {
	// Agy stream-json format: event="result" with result.response
	data := []byte(`{"event":"init","conversation_id":"c1","init":{"model":"gemini-3.1-pro"}}
{"event":"step_update","step_update":{"step_type":"tool","state":"DONE","tool_name":"Bash"}}
{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"Work complete. Branch pushed."}}
`)
	got := extractFinalResponse(data)
	if !strings.Contains(got, "Work complete.") {
		t.Errorf("expected agy result response; got: %q", got)
	}
}

func TestExtractFinalResponse_Empty(t *testing.T) {
	got := extractFinalResponse(nil)
	if got != "" {
		t.Errorf("expected empty string for nil input; got: %q", got)
	}
}

func TestExtractFinalResponse_NoTextBlocks(t *testing.T) {
	// Only thinking blocks — no text to surface.
	data := []byte(`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"internal deliberation"}]}}`)
	got := extractFinalResponse(data)
	if got != "" {
		t.Errorf("expected empty string when no text blocks; got: %q", got)
	}
}

// ---------------------------------------------------------------------------
// detectNeedsAttention
// ---------------------------------------------------------------------------

func TestDetectNeedsAttention_Migrations(t *testing.T) {
	diffStat := " internal/db/migrations/0042_users.sql | 5 +++++"
	flags := detectNeedsAttention(diffStat)
	found := false
	for _, f := range flags {
		if strings.Contains(f, "migration") || strings.Contains(f, "DB") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected migration flag; flags: %v", flags)
	}
}

func TestDetectNeedsAttention_DepsGoMod(t *testing.T) {
	diffStat := " go.mod | 2 +-\n go.sum | 4 ++++"
	flags := detectNeedsAttention(diffStat)
	found := false
	for _, f := range flags {
		if strings.Contains(f, "dependency") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected dependency flag; flags: %v", flags)
	}
}

func TestDetectNeedsAttention_EnvFile(t *testing.T) {
	diffStat := " .env.production | 1 +"
	flags := detectNeedsAttention(diffStat)
	found := false
	for _, f := range flags {
		if strings.Contains(f, ".env") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected .env flag; flags: %v", flags)
	}
}

func TestDetectNeedsAttention_Clean(t *testing.T) {
	diffStat := " internal/foo.go | 10 ++++------\n internal/bar.go |  5 +++--\n 2 files changed"
	flags := detectNeedsAttention(diffStat)
	if len(flags) != 0 {
		t.Errorf("expected no flags for clean diff; got: %v", flags)
	}
}

func TestDetectNeedsAttention_Empty(t *testing.T) {
	flags := detectNeedsAttention("")
	if len(flags) != 0 {
		t.Errorf("expected nil/empty for empty diffStat; got: %v", flags)
	}
}

// ---------------------------------------------------------------------------
// buildRunFooter
// ---------------------------------------------------------------------------

func TestBuildRunFooter_ContainsDisposition(t *testing.T) {
	result := &RunResult{
		TaskID:      "task-xyz",
		RunID:       "run-1",
		Disposition: "in_review",
		Turns:       3,
		DiffStat:    " internal/foo.go | 2 +-\n 1 file changed",
	}
	footer := buildRunFooter(result, "")
	if !strings.Contains(footer, "in_review") {
		t.Errorf("footer should contain disposition; got:\n%s", footer)
	}
	if !strings.Contains(footer, "3") {
		t.Errorf("footer should contain turn count; got:\n%s", footer)
	}
	if !strings.Contains(footer, "1 file changed") {
		t.Errorf("footer should contain diffstat; got:\n%s", footer)
	}
}

func TestBuildRunFooter_CappedNote(t *testing.T) {
	result := &RunResult{Disposition: "capped", Turns: 50}
	footer := buildRunFooter(result, "")
	if !strings.Contains(footer, "stopped early") && !strings.Contains(footer, "cap") {
		t.Errorf("capped footer should mention early stop; got:\n%s", footer)
	}
}

func TestBuildRunFooter_NeedsAttentionIncluded(t *testing.T) {
	result := &RunResult{
		Disposition: "in_review",
		Turns:       2,
		DiffStat:    " go.mod | 1 +\n .env.local | 1 +",
	}
	footer := buildRunFooter(result, "")
	if !strings.Contains(footer, "Needs attention") {
		t.Errorf("footer should have Needs attention section; got:\n%s", footer)
	}
}

// ---------------------------------------------------------------------------
// Integration: harness posts agent comment at end of run (STA-552)
// ---------------------------------------------------------------------------

// TestRunPostsAgentSummaryComment verifies that after a run the harness inserts
// a task comment authored by the agent (not 'harness') whose body includes:
//   - the agent's last assistant text (from Claude stream-json)
//   - the run footer with disposition and turn count
func TestRunPostsAgentSummaryComment(t *testing.T) {
	useSlots(t, 1)

	db := openTestDB(t)
	insertTask(t, db, "summary-task", "/tmp")

	// Pre-register work product so the interceptor passes.
	_, _ = db.Exec(
		`INSERT INTO task_work_products (task_id, product_type, reference) VALUES ('summary-task', 'workspace_file', '.worktrees/summary-task')`,
	)

	h := &Harness{
		DB:          db,
		RepoRoot:    "/tmp",
		WM:          &noopWorktreeManager{},
		Interceptor: NewInterceptor(db),
	}
	h.Interceptor.Guards = []GuardFunc{h.Interceptor.checkWorkProducts}

	// Mock adapter emits Claude-format stream-json with a final assistant text block.
	claudeOutput := `{"type":"system","subtype":"init","session_id":"s1","model":"claude-opus-5"}
{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"text","text":"All done. PR is ready for review."}]}}
{"type":"result","subtype":"success","result":"ok"}
` + taskCompleteMarker + "\n"

	result, err := h.Run(context.Background(), "summary-task", RunConfig{
		MaxTurns:         2,
		AgentID:          "agent-test",
		MaxWallclock:     10 * time.Second,
		SkipGitPreflight: true,
		RunAdapter: func(_ context.Context, _, _ string, _, _ []string, stdout, _ io.Writer) error {
			_, _ = stdout.Write([]byte(claudeOutput))
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != "in_review" {
		t.Fatalf("expected in_review, got %q (diagnostic: %s)", result.Disposition, result.DiagnosticMsg)
	}

	// Must have a comment authored by 'agent-summary' (not 'harness' and not the
	// agent ID — agent-summary is excluded from fetchUserComments to prevent re-injection).
	var msg string
	_ = db.QueryRow(
		`SELECT message FROM task_comments WHERE task_id='summary-task' AND author='agent-summary' LIMIT 1`,
	).Scan(&msg)
	if msg == "" {
		t.Fatal("expected agent-authored run summary comment in task_comments")
	}

	// Comment must contain the agent's final text.
	if !strings.Contains(msg, "All done. PR is ready for review.") {
		t.Errorf("summary comment should contain agent text; got:\n%s", msg)
	}

	// Comment must contain the run footer.
	if !strings.Contains(msg, "Run summary") {
		t.Errorf("summary comment should contain run footer; got:\n%s", msg)
	}

	// The [[TASK_COMPLETE]] marker must not appear in the posted comment.
	if strings.Contains(msg, taskCompleteMarker) {
		t.Errorf("task-complete marker must be stripped from summary comment; got:\n%s", msg)
	}
}
