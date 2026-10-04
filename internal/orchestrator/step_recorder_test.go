package orchestrator

import (
	"sync"
	"testing"
	"time"
)

func collectPublished(t *testing.T) (PublishFunc, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var types []string
	return func(eventType string, _ any) {
		mu.Lock()
		types = append(types, eventType)
		mu.Unlock()
	}, &types
}

func TestStepRecorder_ThinkingStep(t *testing.T) {
	pub, types := collectPublished(t)
	r := NewStepRecorder(nil, pub, "run1", "task1")

	r.Feed(StepDelta{Kind: StepDeltaThinking, Text: "step 1"})
	r.Feed(StepDelta{Kind: StepDeltaThinking, Text: " more"})
	// Close thinking by sending a tool_use
	r.Feed(StepDelta{Kind: StepDeltaToolUse, ToolName: "Bash", ToolID: "t1"})
	r.Close()

	found := false
	for _, tp := range *types {
		if tp == "run.step" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected run.step event to be published")
	}
}

func TestStepRecorder_GroupsToolPair(t *testing.T) {
	var published []RunStep
	var mu sync.Mutex
	pub := func(eventType string, data any) {
		if eventType == "run.step" {
			if s, ok := data.(RunStep); ok {
				mu.Lock()
				published = append(published, s)
				mu.Unlock()
			}
		}
	}
	r := NewStepRecorder(nil, pub, "run1", "task1")

	r.Feed(StepDelta{Kind: StepDeltaToolUse, ToolName: "Read", ToolID: "t1"})
	r.Feed(StepDelta{Kind: StepDeltaToolResult, ToolID: "t1"})

	mu.Lock()
	defer mu.Unlock()
	// tool_use emits a live "running" row; tool_result emits the final "done" row with the same ID.
	if len(published) != 2 {
		t.Fatalf("expected 2 run.step events (live+final), got %d", len(published))
	}
	if published[0].Status != "running" {
		t.Errorf("first event: expected status 'running', got %q", published[0].Status)
	}
	if published[1].Status != "done" {
		t.Errorf("second event: expected status 'done', got %q", published[1].Status)
	}
	if published[0].ID == "" {
		t.Error("live event must have a non-empty ID")
	}
	if published[0].ID != published[1].ID {
		t.Errorf("live and final events must share the same ID; got %q vs %q", published[0].ID, published[1].ID)
	}
}

func TestStepRecorder_UsagePublishesStats(t *testing.T) {
	pub, types := collectPublished(t)
	r := NewStepRecorder(nil, pub, "run1", "task1")

	r.Feed(StepDelta{Kind: StepDeltaUsage, Usage: &StepUsage{
		InputTokens:  100,
		OutputTokens: 50,
	}})

	found := false
	for _, tp := range *types {
		if tp == "run.stats" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected run.stats event from StepDeltaUsage")
	}
}

func TestStepRecorder_WakeEmitsDone(t *testing.T) {
	pub, types := collectPublished(t)
	r := NewStepRecorder(nil, pub, "run1", "task1")
	r.EmitWake("issue_assigned")

	if len(*types) == 0 || (*types)[0] != "run.step" {
		t.Errorf("expected run.step from EmitWake, got %v", *types)
	}
}

func TestStepRecorder_StateEmitsBoth(t *testing.T) {
	pub, types := collectPublished(t)
	r := NewStepRecorder(nil, pub, "run1", "task1")
	r.EmitState("done")

	stepFound, stateFound := false, false
	for _, tp := range *types {
		switch tp {
		case "run.step":
			stepFound = true
		case "run.state":
			stateFound = true
		}
	}
	if !stepFound || !stateFound {
		t.Errorf("expected run.step and run.state, got %v", *types)
	}
}

func TestToolUseKind(t *testing.T) {
	tests := []struct {
		name string
		want StepKind
	}{
		{"Bash", StepRun},
		{"Read", StepRead},
		{"Edit", StepEdit},
		{"Write", StepEdit},
		{"Glob", StepRead},
		{"Grep", StepRead},
		{"unknown_tool", StepRun},
	}
	for _, tt := range tests {
		got := toolUseKind(tt.name)
		if got != tt.want {
			t.Errorf("toolUseKind(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestStepRecorder_MultipleThinkMerged(t *testing.T) {
	pub, types := collectPublished(t)
	r := NewStepRecorder(nil, pub, "run1", "task1")

	// Multiple thinking deltas should produce ONE think step
	r.Feed(StepDelta{Kind: StepDeltaThinking, Text: "part1"})
	r.Feed(StepDelta{Kind: StepDeltaThinking, Text: "part2"})
	r.Feed(StepDelta{Kind: StepDeltaThinking, Text: "part3"})
	r.Close()

	stepCount := 0
	for _, tp := range *types {
		if tp == "run.step" {
			stepCount++
		}
	}
	if stepCount != 1 {
		t.Errorf("expected 1 merged think step, got %d", stepCount)
	}
}

func TestStepRecorder_ErrorToolResult(t *testing.T) {
	var published []RunStep
	var mu sync.Mutex
	pub := func(eventType string, data any) {
		if eventType == "run.step" {
			if s, ok := data.(RunStep); ok {
				mu.Lock()
				published = append(published, s)
				mu.Unlock()
			}
		}
	}
	r := NewStepRecorder(nil, pub, "run1", "task1")

	r.Feed(StepDelta{Kind: StepDeltaToolUse, ToolName: "Bash", ToolID: "t2"})
	r.Feed(StepDelta{Kind: StepDeltaToolResult, ToolID: "t2", IsError: true})

	mu.Lock()
	defer mu.Unlock()
	// live "running" event + final "error" event with the same ID
	if len(published) != 2 {
		t.Fatalf("expected 2 run.step events for error result, got %d", len(published))
	}
	if published[0].Status != "running" {
		t.Errorf("first event: expected status 'running', got %q", published[0].Status)
	}
	if published[1].Status != "error" {
		t.Errorf("second event: expected status 'error', got %q", published[1].Status)
	}
	if published[0].ID != published[1].ID {
		t.Errorf("live and final events must share the same ID; got %q vs %q", published[0].ID, published[1].ID)
	}
}

func TestExtractToolTitle_Command(t *testing.T) {
	// With description: description is the title (plain-language summary wins).
	got := extractToolTitle("Bash", `{"command":"go test ./internal/server/...","description":"run tests"}`, "")
	want := "run tests"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractToolTitle_CommandNoDescription(t *testing.T) {
	// Without description: fall back to raw command.
	got := extractToolTitle("Bash", `{"command":"go test ./internal/server/..."}`, "")
	want := "go test ./internal/server/..."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractToolMeta_WithDescription(t *testing.T) {
	title, cmd := extractToolMeta("Bash", `{"command":"sleep 20","description":"Wait 20 seconds"}`, "")
	if title != "Wait 20 seconds" {
		t.Errorf("title: got %q, want %q", title, "Wait 20 seconds")
	}
	if cmd != "sleep 20" {
		t.Errorf("command: got %q, want %q", cmd, "sleep 20")
	}
}

func TestExtractToolMeta_WithoutDescription(t *testing.T) {
	title, cmd := extractToolMeta("Bash", `{"command":"echo hello"}`, "")
	if title != "echo hello" {
		t.Errorf("title: got %q, want %q", title, "echo hello")
	}
	if cmd != "echo hello" {
		t.Errorf("command: got %q, want %q", cmd, "echo hello")
	}
}

func TestExtractToolMeta_FileTool(t *testing.T) {
	title, cmd := extractToolMeta("Read", `{"file_path":"internal/server/events.go"}`, "")
	if title != "Read internal/server/events.go" {
		t.Errorf("title: got %q, want %q", title, "Read internal/server/events.go")
	}
	if cmd != "" {
		t.Errorf("command: got %q, want empty", cmd)
	}
}

func TestExtractToolTitle_FilePath(t *testing.T) {
	// No worktree root: path is returned as-is with verb prefix.
	got := extractToolTitle("Edit", `{"file_path":"internal/server/events.go","old_string":"x","new_string":"y"}`, "")
	want := "Edit internal/server/events.go"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractToolTitle_FilePathRelative(t *testing.T) {
	// With worktree root: absolute path is stripped to repo-relative.
	root := "/home/agent/.worktrees/task-abc"
	got := extractToolTitle("Read", `{"file_path":"/home/agent/.worktrees/task-abc/internal/server/events.go"}`, root)
	want := "Read internal/server/events.go"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractToolTitle_Fallback(t *testing.T) {
	got := extractToolTitle("Bash", "", "")
	want := "Run command"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractToolTitle_FallbackBadJSON(t *testing.T) {
	got := extractToolTitle("Read", "{not json}", "")
	want := "Read file"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStepRecorder_ToolInputInTitle(t *testing.T) {
	var published []RunStep
	var mu sync.Mutex
	pub := func(eventType string, data any) {
		if eventType == "run.step" {
			if s, ok := data.(RunStep); ok {
				mu.Lock()
				published = append(published, s)
				mu.Unlock()
			}
		}
	}
	r := NewStepRecorder(nil, pub, "run1", "task1")
	r.Feed(StepDelta{
		Kind:      StepDeltaToolUse,
		ToolName:  "Bash",
		ToolID:    "t1",
		ToolInput: `{"command":"go test ./internal/server/...","description":"run tests"}`,
	})
	r.Feed(StepDelta{Kind: StepDeltaToolResult, ToolID: "t1"})

	mu.Lock()
	defer mu.Unlock()
	// live "running" + final "done", both with the same ID and title
	if len(published) != 2 {
		t.Fatalf("expected 2 run.step events (live+final), got %d", len(published))
	}
	// When description is present, it becomes the title (plain-language wins).
	for i, s := range published {
		if s.Title != "run tests" {
			t.Errorf("published[%d]: expected description as title, got %q", i, s.Title)
		}
		// Command field always holds the verbatim command regardless of description.
		if s.Command != "go test ./internal/server/..." {
			t.Errorf("published[%d]: expected command field to hold raw command, got %q", i, s.Command)
		}
	}
	if published[0].ID != published[1].ID {
		t.Errorf("live and final events must share the same ID; got %q vs %q", published[0].ID, published[1].ID)
	}
	// Live event body holds the tool input JSON; final body is empty (no result text).
	if published[0].Body == "" {
		t.Error("live event: expected non-empty body with tool input JSON")
	}
}

func TestStepRecorder_ToolResultSetsBody(t *testing.T) {
	var published []RunStep
	var mu sync.Mutex
	pub := func(eventType string, data any) {
		if eventType == "run.step" {
			if s, ok := data.(RunStep); ok {
				mu.Lock()
				published = append(published, s)
				mu.Unlock()
			}
		}
	}
	r := NewStepRecorder(nil, pub, "run1", "task1")
	r.Feed(StepDelta{
		Kind:      StepDeltaToolUse,
		ToolName:  "Bash",
		ToolID:    "t2",
		ToolInput: `{"command":"cat /etc/hosts"}`,
	})
	r.Feed(StepDelta{
		Kind:    StepDeltaToolResult,
		ToolID:  "t2",
		Text:    "127.0.0.1 localhost\n",
		IsError: false,
	})

	mu.Lock()
	defer mu.Unlock()
	// live "running" event + final "done" event with the same ID
	if len(published) != 2 {
		t.Fatalf("expected 2 run.step events (live+final), got %d", len(published))
	}
	if published[0].Status != "running" {
		t.Errorf("live event: expected status 'running', got %q", published[0].Status)
	}
	final := published[1]
	if final.Status != "done" {
		t.Errorf("final event: expected status done, got %q", final.Status)
	}
	if final.Body != "127.0.0.1 localhost\n" {
		t.Errorf("final event: expected output in body, got %q", final.Body)
	}
	if published[0].ID != final.ID {
		t.Errorf("live and final events must share the same ID; got %q vs %q", published[0].ID, final.ID)
	}
}

func TestStepRecorder_ToolResultError(t *testing.T) {
	var published []RunStep
	var mu sync.Mutex
	pub := func(eventType string, data any) {
		if eventType == "run.step" {
			if s, ok := data.(RunStep); ok {
				mu.Lock()
				published = append(published, s)
				mu.Unlock()
			}
		}
	}
	r := NewStepRecorder(nil, pub, "run1", "task1")
	r.Feed(StepDelta{
		Kind:      StepDeltaToolUse,
		ToolName:  "Bash",
		ToolID:    "t3",
		ToolInput: `{"command":"cat does-not-exist.txt"}`,
	})
	r.Feed(StepDelta{
		Kind:    StepDeltaToolResult,
		ToolID:  "t3",
		Text:    "cat: does-not-exist.txt: No such file or directory\n",
		IsError: true,
	})

	mu.Lock()
	defer mu.Unlock()
	// live "running" event + final "error" event with the same ID
	if len(published) != 2 {
		t.Fatalf("expected 2 run.step events (live+final), got %d", len(published))
	}
	if published[0].Status != "running" {
		t.Errorf("live event: expected status 'running', got %q", published[0].Status)
	}
	final := published[1]
	if final.Status != "error" {
		t.Errorf("final event: expected status error, got %q", final.Status)
	}
	if final.Body != "cat: does-not-exist.txt: No such file or directory\n" {
		t.Errorf("final event: unexpected body: %q", final.Body)
	}
	if published[0].ID != final.ID {
		t.Errorf("live and final events must share the same ID; got %q vs %q", published[0].ID, final.ID)
	}
}

func TestStepRecorder_TextBetweenToolsOpensThink(t *testing.T) {
	var published []RunStep
	var mu sync.Mutex
	pub := func(eventType string, data any) {
		if eventType == "run.step" {
			if s, ok := data.(RunStep); ok {
				mu.Lock()
				published = append(published, s)
				mu.Unlock()
			}
		}
	}
	r := NewStepRecorder(nil, pub, "run1", "task1")
	// Complete one tool pair
	r.Feed(StepDelta{Kind: StepDeltaToolUse, ToolName: "Bash", ToolID: "t1"})
	r.Feed(StepDelta{Kind: StepDeltaToolResult, ToolID: "t1"})
	// Text arrives with no pending step — should open a think step
	r.Feed(StepDelta{Kind: StepDeltaText, Text: "Done. Checking output now."})
	r.Close()

	mu.Lock()
	defer mu.Unlock()
	thinkFound := false
	for _, s := range published {
		if s.Kind == StepThink {
			thinkFound = true
			if s.Body != "Done. Checking output now." {
				t.Errorf("think body = %q, want %q", s.Body, "Done. Checking output now.")
			}
		}
	}
	if !thinkFound {
		t.Error("expected a think step from text between tool calls")
	}
}

func TestStepRecorder_RouteEmitsStep(t *testing.T) {
	var published []RunStep
	var mu sync.Mutex
	pub := func(eventType string, data any) {
		if eventType == "run.step" {
			if s, ok := data.(RunStep); ok {
				mu.Lock()
				published = append(published, s)
				mu.Unlock()
			}
		}
	}
	r := NewStepRecorder(nil, pub, "run1", "task1")
	r.EmitRoute("Ran on Claude Opus", "Kind of work: coding")

	mu.Lock()
	defer mu.Unlock()
	if len(published) != 1 {
		t.Fatalf("expected 1 run.step, got %d", len(published))
	}
	s := published[0]
	if s.Kind != StepRoute {
		t.Errorf("expected kind %q, got %q", StepRoute, s.Kind)
	}
	if s.Title != "Ran on Claude Opus" {
		t.Errorf("unexpected title: %q", s.Title)
	}
	if s.Body != "Kind of work: coding" {
		t.Errorf("unexpected body: %q", s.Body)
	}
	if s.Status != "done" {
		t.Errorf("expected status done, got %q", s.Status)
	}
}

// TestStepRecorder_WakeStartedAtIsOwnTime verifies that EmitWake records its own
// start time, not r.startedAt (the run creation time). This ensures the wake step's
// StartedAt is not anchored to a moment before the step actually executed.
func TestStepRecorder_WakeStartedAtIsOwnTime(t *testing.T) {
	var mu sync.Mutex
	var steps []RunStep
	pub := func(eventType string, data any) {
		if eventType == "run.step" {
			if s, ok := data.(RunStep); ok {
				mu.Lock()
				steps = append(steps, s)
				mu.Unlock()
			}
		}
	}

	before := time.Now().UTC()
	r := NewStepRecorder(nil, pub, "run1", "task1")
	// Pause briefly so run creation time (r.startedAt) is measurably before the wake.
	time.Sleep(2 * time.Millisecond)
	r.EmitWake("issue_assigned")
	after := time.Now().UTC()

	mu.Lock()
	defer mu.Unlock()
	if len(steps) == 0 {
		t.Fatal("no run.step published")
	}
	s := steps[0]
	if s.StartedAt == "" {
		t.Fatal("StartedAt must not be empty")
	}
	ts, err := time.Parse(time.RFC3339Nano, s.StartedAt)
	if err != nil {
		t.Fatalf("StartedAt %q not parseable: %v", s.StartedAt, err)
	}
	// The step's StartedAt must be within [before, after], not at the recorder's creation.
	if ts.Before(before) || ts.After(after) {
		t.Errorf("StartedAt %v not between %v and %v (wake should use its own time, not r.startedAt)", ts, before, after)
	}
}

// TestStepRecorder_EmptyThinkBlockDropped verifies that a thinking delta with empty
// text does not produce a persisted step (STA-460).
func TestStepRecorder_EmptyThinkBlockDropped(t *testing.T) {
	var published []RunStep
	var mu sync.Mutex
	pub := func(eventType string, data any) {
		if eventType == "run.step" {
			if s, ok := data.(RunStep); ok {
				mu.Lock()
				published = append(published, s)
				mu.Unlock()
			}
		}
	}
	r := NewStepRecorder(nil, pub, "run1", "task1")
	// Empty thinking block — simulates a redacted/empty thinking block from Claude.
	r.Feed(StepDelta{Kind: StepDeltaThinking, Text: ""})
	// A subsequent tool_use should close the (non-existent) think step.
	r.Feed(StepDelta{Kind: StepDeltaToolUse, ToolName: "Bash", ToolID: "t1"})
	r.Feed(StepDelta{Kind: StepDeltaToolResult, ToolID: "t1"})
	r.Close()

	mu.Lock()
	defer mu.Unlock()
	for _, s := range published {
		if s.Kind == StepThink {
			t.Errorf("expected no think step, but got one with body=%q", s.Body)
		}
	}
}

// TestStepRecorder_EmptyThinkThenRealThink verifies that an empty thinking block
// followed by a real thinking block produces exactly one non-empty think step.
func TestStepRecorder_EmptyThinkThenRealThink(t *testing.T) {
	var published []RunStep
	var mu sync.Mutex
	pub := func(eventType string, data any) {
		if eventType == "run.step" {
			if s, ok := data.(RunStep); ok {
				mu.Lock()
				published = append(published, s)
				mu.Unlock()
			}
		}
	}
	r := NewStepRecorder(nil, pub, "run1", "task1")
	r.Feed(StepDelta{Kind: StepDeltaThinking, Text: ""})
	r.Feed(StepDelta{Kind: StepDeltaThinking, Text: "actual thinking"})
	r.Close()

	mu.Lock()
	defer mu.Unlock()
	thinkSteps := 0
	for _, s := range published {
		if s.Kind == StepThink {
			thinkSteps++
			if s.Body == "" {
				t.Error("think step has empty body")
			}
			if s.Body != "actual thinking" {
				t.Errorf("think body = %q, want %q", s.Body, "actual thinking")
			}
		}
	}
	if thinkSteps != 1 {
		t.Errorf("expected 1 think step, got %d", thinkSteps)
	}
}

// TestStepRecorder_CloseDropsEmptyThink verifies the defense-in-depth path:
// a think step that somehow ends up with an empty body is not persisted.
func TestStepRecorder_CloseDropsEmptyThink(t *testing.T) {
	var published []RunStep
	var mu sync.Mutex
	pub := func(eventType string, data any) {
		if eventType == "run.step" {
			if s, ok := data.(RunStep); ok {
				mu.Lock()
				published = append(published, s)
				mu.Unlock()
			}
		}
	}
	r := NewStepRecorder(nil, pub, "run1", "task1")
	// Directly inject an empty think step to hit the closePendingWithStatusLocked guard.
	r.mu.Lock()
	r.seq++
	r.pending = &openStep{
		seq:       r.seq,
		kind:      StepThink,
		title:     "Thinking",
		startedAt: time.Now().UTC(),
	}
	r.mu.Unlock()
	r.Close()

	mu.Lock()
	defer mu.Unlock()
	for _, s := range published {
		if s.Kind == StepThink {
			t.Errorf("expected empty think step to be dropped, but it was persisted with body=%q", s.Body)
		}
	}
}

// TestStepRecorder_RouteStartedAtIsOwnTime verifies the same property for EmitRoute.
func TestStepRecorder_RouteStartedAtIsOwnTime(t *testing.T) {
	var mu sync.Mutex
	var steps []RunStep
	pub := func(eventType string, data any) {
		if eventType == "run.step" {
			if s, ok := data.(RunStep); ok {
				mu.Lock()
				steps = append(steps, s)
				mu.Unlock()
			}
		}
	}

	before := time.Now().UTC()
	r := NewStepRecorder(nil, pub, "run1", "task1")
	time.Sleep(2 * time.Millisecond)
	r.EmitRoute("Ran on Claude Opus", "")
	after := time.Now().UTC()

	mu.Lock()
	defer mu.Unlock()
	if len(steps) == 0 {
		t.Fatal("no run.step published")
	}
	s := steps[0]
	if s.StartedAt == "" {
		t.Fatal("StartedAt must not be empty")
	}
	ts, err := time.Parse(time.RFC3339Nano, s.StartedAt)
	if err != nil {
		t.Fatalf("StartedAt %q not parseable: %v", s.StartedAt, err)
	}
	if ts.Before(before) || ts.After(after) {
		t.Errorf("StartedAt %v not between %v and %v", ts, before, after)
	}
}

// collectSteps returns a PublishFunc and a pointer to the slice of RunStep events.
func collectSteps(t *testing.T) (PublishFunc, *[]RunStep) {
	t.Helper()
	var mu sync.Mutex
	var steps []RunStep
	pub := func(eventType string, data any) {
		if eventType == "run.step" {
			if s, ok := data.(RunStep); ok {
				mu.Lock()
				steps = append(steps, s)
				mu.Unlock()
			}
		}
	}
	return pub, &steps
}

// TestStepRecorder_ParallelToolCallsInOrder sends two tool_use blocks followed by
// their tool_results in the same order and verifies each step gets its own real output.
func TestStepRecorder_ParallelToolCallsInOrder(t *testing.T) {
	pub, steps := collectSteps(t)
	r := NewStepRecorder(nil, pub, "run1", "task1")

	// Two parallel tool_use blocks (as Claude sends them in one assistant message).
	r.Feed(StepDelta{Kind: StepDeltaToolUse, ToolName: "Bash", ToolID: "t1",
		ToolInput: `{"command":"echo hello","description":"print hello"}`})
	r.Feed(StepDelta{Kind: StepDeltaToolUse, ToolName: "Bash", ToolID: "t2",
		ToolInput: `{"command":"sleep 20","description":"long sleep"}`})

	// Results arrive in the same order.
	r.Feed(StepDelta{Kind: StepDeltaToolResult, ToolID: "t1", Text: "hello\n"})
	r.Feed(StepDelta{Kind: StepDeltaToolResult, ToolID: "t2", Text: "slept-again\n"})
	r.Close()

	got := *steps
	// Each tool_use emits a live "running" event then a final "done"/"error" event:
	// 2 tools × 2 events = 4 total. Filter to finals for assertions.
	if len(got) != 4 {
		t.Fatalf("expected 4 run.step events (2 live + 2 final), got %d: %+v", len(got), got)
	}
	var finals []RunStep
	for _, s := range got {
		if s.Status != "running" {
			finals = append(finals, s)
		}
	}
	if len(finals) != 2 {
		t.Fatalf("expected 2 final (non-running) events, got %d", len(finals))
	}

	byTitle := map[string]RunStep{}
	for _, s := range finals {
		byTitle[s.Title] = s
	}

	s1 := finals[0]
	if s1.Body != "hello\n" {
		t.Errorf("step 1 body = %q, want %q", s1.Body, "hello\n")
	}
	if s1.Status != "done" {
		t.Errorf("step 1 status = %q, want done", s1.Status)
	}

	s2 := finals[1]
	if s2.Body != "slept-again\n" {
		t.Errorf("step 2 body = %q, want %q", s2.Body, "slept-again\n")
	}
	if s2.Status != "done" {
		t.Errorf("step 2 status = %q, want done", s2.Status)
	}
}

// TestStepRecorder_ParallelToolCallsReverseOrder sends results in reverse order
// (second result arrives before first) and verifies correct matching by ID.
func TestStepRecorder_ParallelToolCallsReverseOrder(t *testing.T) {
	pub, steps := collectSteps(t)
	r := NewStepRecorder(nil, pub, "run1", "task1")

	r.Feed(StepDelta{Kind: StepDeltaToolUse, ToolName: "Read", ToolID: "ta",
		ToolInput: `{"file_path":"a.txt"}`})
	r.Feed(StepDelta{Kind: StepDeltaToolUse, ToolName: "Read", ToolID: "tb",
		ToolInput: `{"file_path":"b.txt"}`})

	// Results arrive in reverse order.
	r.Feed(StepDelta{Kind: StepDeltaToolResult, ToolID: "tb", Text: "content-b"})
	r.Feed(StepDelta{Kind: StepDeltaToolResult, ToolID: "ta", Text: "content-a"})
	r.Close()

	got := *steps
	if len(got) != 4 {
		t.Fatalf("expected 4 run.step events (2 live + 2 final), got %d: %+v", len(got), got)
	}

	// Build title map from finals only — live events have tool-input JSON as body.
	byTitle := map[string]RunStep{}
	for _, s := range got {
		if s.Status != "running" {
			byTitle[s.Title] = s
		}
	}

	if sa, ok := byTitle["Read a.txt"]; ok {
		if sa.Body != "content-a" {
			t.Errorf("step a body = %q, want content-a", sa.Body)
		}
	} else {
		t.Errorf("no step with title %q; titles: %v", "Read a.txt", titlesOf(got))
	}

	if sb, ok := byTitle["Read b.txt"]; ok {
		if sb.Body != "content-b" {
			t.Errorf("step b body = %q, want content-b", sb.Body)
		}
	} else {
		t.Errorf("no step with title %q; titles: %v", "Read b.txt", titlesOf(got))
	}
}

// TestStepRecorder_ParallelToolCallDuration verifies that each parallel step's
// duration is measured from its own tool_use, not from the other tool's result.
// Specifically: a step whose result arrives after a real delay should show that delay,
// not a near-zero 294ms (the bug reported in STA-501).
func TestStepRecorder_ParallelToolCallDuration(t *testing.T) {
	var mu sync.Mutex
	type entry struct {
		step RunStep
		seen time.Time
	}
	var entries []entry
	pub := func(eventType string, data any) {
		if eventType == "run.step" {
			if s, ok := data.(RunStep); ok {
				mu.Lock()
				entries = append(entries, entry{step: s, seen: time.Now().UTC()})
				mu.Unlock()
			}
		}
	}
	r := NewStepRecorder(nil, pub, "run1", "task1")

	r.Feed(StepDelta{Kind: StepDeltaToolUse, ToolName: "Bash", ToolID: "fast",
		ToolInput: `{"command":"echo quick","description":"quick"}`})
	r.Feed(StepDelta{Kind: StepDeltaToolUse, ToolName: "Bash", ToolID: "slow",
		ToolInput: `{"command":"sleep 0.05","description":"slow"}`})

	r.Feed(StepDelta{Kind: StepDeltaToolResult, ToolID: "fast", Text: "quick\n"})
	time.Sleep(50 * time.Millisecond)
	r.Feed(StepDelta{Kind: StepDeltaToolResult, ToolID: "slow", Text: "slept\n"})
	r.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(entries) != 4 {
		t.Fatalf("expected 4 steps (2 live + 2 final), got %d", len(entries))
	}

	// Duration check only applies to final events (live events have no EndedAt).
	for _, e := range entries {
		if e.step.EndedAt == nil {
			continue
		}
		startedAt, err := time.Parse(time.RFC3339Nano, e.step.StartedAt)
		if err != nil {
			t.Fatalf("StartedAt parse: %v", err)
		}
		endedAt, err := time.Parse(time.RFC3339Nano, *e.step.EndedAt)
		if err != nil {
			t.Fatalf("EndedAt parse: %v", err)
		}
		dur := endedAt.Sub(startedAt)
		if e.step.Body == "slept\n" && dur < 40*time.Millisecond {
			t.Errorf("slow step duration = %v, want ≥ 40ms; was started prematurely", dur)
		}
	}
}

// TestStepRecorder_ParallelToolCallErrorStatus verifies is_error propagates correctly
// when one of two parallel tool results is an error.
func TestStepRecorder_ParallelToolCallErrorStatus(t *testing.T) {
	pub, steps := collectSteps(t)
	r := NewStepRecorder(nil, pub, "run1", "task1")

	r.Feed(StepDelta{Kind: StepDeltaToolUse, ToolName: "Bash", ToolID: "ok", ToolInput: `{"command":"echo ok"}`})
	r.Feed(StepDelta{Kind: StepDeltaToolUse, ToolName: "Bash", ToolID: "fail", ToolInput: `{"command":"false"}`})

	r.Feed(StepDelta{Kind: StepDeltaToolResult, ToolID: "ok", Text: "ok\n", IsError: false})
	r.Feed(StepDelta{Kind: StepDeltaToolResult, ToolID: "fail", Text: "exit 1\n", IsError: true})
	r.Close()

	got := *steps
	if len(got) != 4 {
		t.Fatalf("expected 4 steps (2 live + 2 final), got %d", len(got))
	}
	// Only check final events — live events carry tool-input JSON, not result text.
	statusByBody := map[string]string{}
	for _, s := range got {
		if s.Status != "running" {
			statusByBody[s.Body] = s.Status
		}
	}
	if statusByBody["ok\n"] != "done" {
		t.Errorf("ok step status = %q, want done", statusByBody["ok\n"])
	}
	if statusByBody["exit 1\n"] != "error" {
		t.Errorf("fail step status = %q, want error", statusByBody["exit 1\n"])
	}
}

func titlesOf(steps []RunStep) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Title
	}
	return out
}

// TestStepRecorder_UsageMonotonicAcrossTurns verifies two bugs fixed in STA-526:
//  1. Multiple intermediate DeltaUsage events per turn update SpentUSD (not just the result event).
//  2. SpentUSD never decreases across turns, even when turn 2 has large cheap cache reads that
//     would produce a lower cost than turn 1 when computed standalone.
func TestStepRecorder_UsageMonotonicAcrossTurns(t *testing.T) {
	var mu sync.Mutex
	var stats []RunStats
	pub := func(eventType string, data any) {
		if eventType == "run.stats" {
			if rs, ok := data.(RunStats); ok {
				mu.Lock()
				stats = append(stats, rs)
				mu.Unlock()
			}
		}
	}
	r := NewStepRecorder(nil, pub, "run1", "task1")

	// Turn 1: two intermediate events with the same values (same API call, deduplicated by MAX).
	// Then result event has authoritative totals (higher output due to thinking tokens).
	turn1Intermediate := &StepUsage{Model: "claude-sonnet-5-5", InputTokens: 10, OutputTokens: 4, CacheCreationTokens: 21093, CacheReadTokens: 17820}
	turn1Result := &StepUsage{Model: "claude-sonnet-5-5", InputTokens: 18, OutputTokens: 355, CacheCreationTokens: 23169, CacheReadTokens: 56733}

	r.Feed(StepDelta{Kind: StepDeltaUsage, Usage: turn1Intermediate})
	r.Feed(StepDelta{Kind: StepDeltaUsage, Usage: turn1Intermediate}) // duplicate, same API call
	r.Feed(StepDelta{Kind: StepDeltaUsage, Usage: turn1Result})        // result event, authoritative

	mu.Lock()
	if len(stats) == 0 {
		t.Fatal("expected at least one run.stats from intermediate usage events (Bug 1: stats bar never updated)")
	}
	afterTurn1 := stats[len(stats)-1]
	mu.Unlock()

	if afterTurn1.SpentUSD <= 0 {
		t.Fatalf("turn1 SpentUSD = %v, want > 0", afterTurn1.SpentUSD)
	}

	// Commit turn 1 via DeltaResult.
	r.Feed(StepDelta{Kind: StepDeltaResult})

	// Turn 2: large cache_read (cheap) for the prompt context; this would produce a
	// *lower* standalone cost than turn 1 if the old MAX logic were used cross-turn.
	turn2Intermediate := &StepUsage{Model: "claude-sonnet-5-5", InputTokens: 8, OutputTokens: 2, CacheCreationTokens: 2076, CacheReadTokens: 38913}
	turn2Result := &StepUsage{Model: "claude-sonnet-5-5", InputTokens: 8, OutputTokens: 39, CacheCreationTokens: 2076, CacheReadTokens: 38913}

	mu.Lock()
	statsBeforeTurn2 := len(stats)
	mu.Unlock()

	r.Feed(StepDelta{Kind: StepDeltaUsage, Usage: turn2Intermediate})
	r.Feed(StepDelta{Kind: StepDeltaUsage, Usage: turn2Result})
	r.Feed(StepDelta{Kind: StepDeltaResult})

	mu.Lock()
	defer mu.Unlock()

	if len(stats) <= statsBeforeTurn2 {
		t.Fatal("no run.stats published during turn 2")
	}

	// Verify monotonically non-decreasing SpentUSD across all events.
	for i := 1; i < len(stats); i++ {
		if stats[i].SpentUSD < stats[i-1].SpentUSD {
			t.Errorf("SpentUSD decreased: stats[%d]=%.6f > stats[%d]=%.6f (Bug 2: cost dropped across turns)",
				i-1, stats[i-1].SpentUSD, i, stats[i].SpentUSD)
		}
	}

	// Final value must exceed turn-1-only cost (accumulated, not replaced).
	finalSpent := stats[len(stats)-1].SpentUSD
	if finalSpent <= afterTurn1.SpentUSD {
		t.Errorf("final SpentUSD %v <= turn1 SpentUSD %v; turn 2 tokens not accumulated", finalSpent, afterTurn1.SpentUSD)
	}
}

