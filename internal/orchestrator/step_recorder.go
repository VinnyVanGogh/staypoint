package orchestrator

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// StepKind classifies a run timeline step for the board UI.
type StepKind string

const (
	StepWake       StepKind = "wake"
	StepRoute      StepKind = "route"
	StepThink      StepKind = "think"
	StepRead       StepKind = "read"
	StepRun        StepKind = "run"
	StepEdit       StepKind = "edit"
	StepCheckpoint StepKind = "checkpoint"
	StepMessage    StepKind = "message"
	StepState      StepKind = "state"
	StepStats      StepKind = "stats"
)

// StepDeltaKind mirrors adapter.DeltaKind without the import cycle.
// The harness converts adapter.StreamDelta → StepDelta before feeding the recorder.
type StepDeltaKind string

const (
	StepDeltaThinking   StepDeltaKind = "thinking"
	StepDeltaToolUse    StepDeltaKind = "tool_use"
	StepDeltaToolResult StepDeltaKind = "tool_result"
	StepDeltaText       StepDeltaKind = "text"
	StepDeltaUsage      StepDeltaKind = "usage"
	StepDeltaResult     StepDeltaKind = "result"
	StepDeltaOther      StepDeltaKind = "other"
)

// StepUsage carries token accounting from a stream event.
type StepUsage struct {
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	Model               string
}

// StepDelta is the recorder's internal representation of one stream event.
// It mirrors adapter.StreamDelta but lives in this package to avoid the import cycle.
type StepDelta struct {
	Kind      StepDeltaKind
	Text      string
	ToolName  string
	ToolID    string
	ToolInput string // raw JSON input for tool_use deltas
	IsError   bool
	Usage     *StepUsage
}

// RunStep is one timeline entry, stored in run_steps and broadcast as run.step SSE.
type RunStep struct {
	ID        string   `json:"id"`
	RunID     string   `json:"run_id"`
	TaskID    string   `json:"task_id"`
	Seq       int      `json:"seq"`
	ParentSeq *int     `json:"parent_seq,omitempty"`
	Kind      StepKind `json:"kind"`
	Title     string   `json:"title"`
	Body      string   `json:"body,omitempty"`
	// Command holds the raw shell command for run/Bash steps. Title may be a
	// plain-language description (from the agent's tool-call description field);
	// Command is always the verbatim command shown in the expanded body.
	Command   string   `json:"command,omitempty"`
	Status    string   `json:"status"` // running | done | error
	StartedAt string   `json:"started_at"`
	EndedAt   *string  `json:"ended_at,omitempty"`
}

// RunStats is broadcast as run.stats SSE.
type RunStats struct {
	RunID        string  `json:"run_id"`
	TaskID       string  `json:"task_id"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	SpentTokens  int64   `json:"spent_tokens,omitempty"`
	SpentUSD     float64 `json:"spent_usd,omitempty"`
	ElapsedSec   float64 `json:"elapsed_sec"`
}

// PublishFunc is the EventHub.Publish signature subset used by StepRecorder.
type PublishFunc func(eventType string, data any)

// StepRecorder groups StepDeltas into steps, persists them, and publishes SSE events.
// It is safe for concurrent use.
type StepRecorder struct {
	db        *sql.DB
	publish   PublishFunc
	runID     string
	taskID    string
	startedAt time.Time

	mu           sync.Mutex
	seq          int
	pending      *openStep            // non-tool step being assembled (think, text)
	pendingTools map[string]*openStep // parallel tool steps keyed by tool_use_id
	worktreeRoot string               // set by harness after worktree creation; relativizes file paths

	// Two-tier token accounting: pending (MAX within current API call) +
	// committed (SUM across completed API calls). Published SpentUSD always
	// reflects committed+pending, so values are monotonically non-decreasing
	// even when cache_read tokens jump between turns.
	model            string
	pendingInput     int64
	pendingOutput    int64
	pendingCacheRead int64
	pendingCacheCreate int64
	committedInput     int64
	committedOutput    int64
	committedCacheRead int64
	committedCacheCreate int64
	// contentSeen is set once the agent produced any thinking, text or tool
	// call, so the harness can tell a silent run from a quiet one (STA-775).
	contentSeen bool
}

// openStep is a step that has been started but not yet closed.
type openStep struct {
	seq       int
	id        string // pre-assigned for tool_use steps so the live row can be updated on result
	kind      StepKind
	title     string
	command   string // raw shell command for run steps; empty for non-Bash tools
	body      strings.Builder
	startedAt time.Time
	toolID    string // for matching tool_use → tool_result
}

// NewStepRecorder creates a StepRecorder for the given run.
func NewStepRecorder(db *sql.DB, publish PublishFunc, runID, taskID string) *StepRecorder {
	return &StepRecorder{
		db:        db,
		publish:   publish,
		runID:     runID,
		taskID:    taskID,
		startedAt: time.Now().UTC(),
	}
}

// SetWorktreeRoot sets the absolute path of the task's git worktree.
// Called by the harness after the worktree is created so that file paths
// in tool titles are emitted repo-relative instead of absolute.
func (r *StepRecorder) SetWorktreeRoot(path string) {
	r.mu.Lock()
	r.worktreeRoot = path
	r.mu.Unlock()
}

// EmitWake emits a wake step describing why the run started.
func (r *StepRecorder) EmitWake(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drainAllLocked()
	r.seq++
	now := time.Now().UTC().Format(time.RFC3339Nano)
	step := RunStep{
		RunID:     r.runID,
		TaskID:    r.taskID,
		Seq:       r.seq,
		Kind:      StepWake,
		Title:     "Woke up",
		Body:      reason,
		Status:    "done",
		StartedAt: now,
		EndedAt:   &now,
	}
	r.persist(step)
	r.publish("run.step", step)
}

// EmitRoute emits a route step as the first substantive row of each run.
// title is the plain-language label ("Ran on Claude Opus") and body is the
// optional reason detail ("Gemini quota locked" etc.).
func (r *StepRecorder) EmitRoute(title, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drainAllLocked()
	r.seq++
	now := time.Now().UTC().Format(time.RFC3339Nano)
	step := RunStep{
		RunID:     r.runID,
		TaskID:    r.taskID,
		Seq:       r.seq,
		Kind:      StepRoute,
		Title:     title,
		Body:      body,
		Status:    "done",
		StartedAt: now,
		EndedAt:   &now,
	}
	r.persist(step)
	r.publish("run.step", step)
}

// EmitCheckpoint emits a checkpoint step.
func (r *StepRecorder) EmitCheckpoint(sha, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drainAllLocked()
	r.seq++
	now := time.Now().UTC().Format(time.RFC3339Nano)
	step := RunStep{
		RunID:     r.runID,
		TaskID:    r.taskID,
		Seq:       r.seq,
		Kind:      StepCheckpoint,
		Title:     "Checkpoint",
		Body:      "sha=" + sha + " " + msg,
		Status:    "done",
		StartedAt: now,
		EndedAt:   &now,
	}
	r.persist(step)
	r.publish("run.step", step)
}

// EmitMessage records a message row in the timeline (status "done" or
// "error"), e.g. why a run that printed nothing ended (STA-775).
func (r *StepRecorder) EmitMessage(title, body, status string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drainAllLocked()
	r.seq++
	now := time.Now().UTC().Format(time.RFC3339Nano)
	step := RunStep{
		RunID:     r.runID,
		TaskID:    r.taskID,
		Seq:       r.seq,
		Kind:      StepMessage,
		Title:     title,
		Body:      body,
		Status:    status,
		StartedAt: now,
		EndedAt:   &now,
	}
	r.persist(step)
	r.publish("run.step", step)
}

// SawContent reports whether the agent produced any thinking, text or tool
// call during this run.
func (r *StepRecorder) SawContent() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.contentSeen
}

// EmitRunState broadcasts a run.state SSE event without persisting a step row.
// Use for transient states like "paused" / "in_progress" that don't mark the run terminal.
func (r *StepRecorder) EmitRunState(disposition string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	elapsed := time.Since(r.startedAt).Seconds()
	r.publish("run.state", map[string]any{
		"run_id":      r.runID,
		"task_id":     r.taskID,
		"disposition": disposition,
		"elapsed_sec": elapsed,
	})
}

// EmitState emits a state step (done / in_review / blocked / stopped).
func (r *StepRecorder) EmitState(disposition string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drainAllLocked()
	r.seq++
	now := time.Now().UTC().Format(time.RFC3339Nano)
	elapsed := time.Since(r.startedAt).Seconds()
	step := RunStep{
		RunID:     r.runID,
		TaskID:    r.taskID,
		Seq:       r.seq,
		Kind:      StepState,
		Title:     "Finished: " + disposition,
		Status:    "done",
		StartedAt: now,
		EndedAt:   &now,
	}
	r.persist(step)
	r.publish("run.step", step)
	r.publish("run.state", map[string]any{
		"run_id":      r.runID,
		"task_id":     r.taskID,
		"disposition": disposition,
		"elapsed_sec": elapsed,
	})
}

// Feed processes one StepDelta, updating the current open step.
func (r *StepRecorder) Feed(d StepDelta) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch d.Kind {
	case StepDeltaThinking, StepDeltaText:
		if strings.TrimSpace(d.Text) != "" {
			r.contentSeen = true
		}
	case StepDeltaToolUse:
		r.contentSeen = true
	}

	switch d.Kind {
	case StepDeltaThinking:
		r.openOrReuseThinkLocked(d)

	case StepDeltaToolUse:
		// Close only the non-tool pending (e.g. a think step); do not disturb
		// other in-flight tool steps so parallel tool_use blocks each get their own row.
		r.closeCurrentLocked()
		r.seq++
		liveID := uuid.New().String()
		title, cmd := extractToolMeta(d.ToolName, d.ToolInput, r.worktreeRoot)
		p := &openStep{
			seq:       r.seq,
			id:        liveID,
			kind:      toolUseKind(d.ToolName),
			title:     title,
			command:   cmd,
			startedAt: time.Now().UTC(),
			toolID:    d.ToolID,
		}
		if d.ToolInput != "" {
			p.body.WriteString(d.ToolInput)
		}
		if d.ToolID != "" {
			if r.pendingTools == nil {
				r.pendingTools = make(map[string]*openStep)
			}
			r.pendingTools[d.ToolID] = p
		} else {
			// No ID: fall back to single-pending slot (legacy / non-parallel path).
			r.pending = p
		}
		// Publish a live row immediately so the timeline shows the running command.
		liveStep := RunStep{
			ID:        liveID,
			RunID:     r.runID,
			TaskID:    r.taskID,
			Seq:       p.seq,
			Kind:      p.kind,
			Title:     p.title,
			Command:   p.command,
			Body:      p.body.String(),
			Status:    "running",
			StartedAt: p.startedAt.Format(time.RFC3339Nano),
		}
		r.persist(liveStep)
		r.publish("run.step", liveStep)

	case StepDeltaToolResult:
		// Fast path: match by tool_use_id in the parallel map.
		if d.ToolID != "" {
			if p, ok := r.pendingTools[d.ToolID]; ok {
				p.body.Reset()
				if d.Text != "" {
					out := d.Text
					const maxBody = 2000
					if len(out) > maxBody {
						out = out[:maxBody] + "\n…(truncated)"
					}
					p.body.WriteString(out)
				}
				status := "done"
				if d.IsError {
					status = "error"
				}
				r.closeToolStepLocked(p, status)
				delete(r.pendingTools, d.ToolID)
				return
			}
		}
		// Fallback: legacy single-pending slot (empty tool ID or pre-map code path).
		if r.pending != nil && (r.pending.toolID == d.ToolID || d.ToolID == "") {
			r.pending.body.Reset()
			if d.Text != "" {
				out := d.Text
				const maxBody = 2000
				if len(out) > maxBody {
					out = out[:maxBody] + "\n…(truncated)"
				}
				r.pending.body.WriteString(out)
			}
			status := "done"
			if d.IsError {
				status = "error"
			}
			r.closePendingWithStatusLocked(r.pending, status)
			return
		}
		r.closeCurrentLocked()

	case StepDeltaText:
		r.accumulateTextLocked(d.Text)

	case StepDeltaUsage:
		if d.Usage != nil {
			// MAX into pending: multiple events per API call carry identical values;
			// taking max deduplicates without double-counting.
			if d.Usage.InputTokens > r.pendingInput {
				r.pendingInput = d.Usage.InputTokens
			}
			if d.Usage.OutputTokens > r.pendingOutput {
				r.pendingOutput = d.Usage.OutputTokens
			}
			if d.Usage.CacheReadTokens > r.pendingCacheRead {
				r.pendingCacheRead = d.Usage.CacheReadTokens
			}
			if d.Usage.CacheCreationTokens > r.pendingCacheCreate {
				r.pendingCacheCreate = d.Usage.CacheCreationTokens
			}
			if d.Usage.Model != "" {
				r.model = d.Usage.Model
			}
			totalInput := r.committedInput + r.pendingInput
			totalOutput := r.committedOutput + r.pendingOutput
			totalCacheRead := r.committedCacheRead + r.pendingCacheRead
			totalCacheCreate := r.committedCacheCreate + r.pendingCacheCreate
			spentTokens := totalInput + totalOutput + totalCacheRead + totalCacheCreate
			spentUSD := estimateRunCost(r.model, totalInput, totalOutput, totalCacheRead, totalCacheCreate)
			r.publish("run.stats", RunStats{
				RunID:        r.runID,
				TaskID:       r.taskID,
				InputTokens:  totalInput,
				OutputTokens: totalOutput,
				SpentTokens:  spentTokens,
				SpentUSD:     spentUSD,
				ElapsedSec:   time.Since(r.startedAt).Seconds(),
			})
		}

	case StepDeltaResult:
		// Commit pending into accumulated-across-turns totals, then reset pending.
		r.committedInput += r.pendingInput
		r.committedOutput += r.pendingOutput
		r.committedCacheRead += r.pendingCacheRead
		r.committedCacheCreate += r.pendingCacheCreate
		r.pendingInput = 0
		r.pendingOutput = 0
		r.pendingCacheRead = 0
		r.pendingCacheCreate = 0
		r.drainAllLocked()
	}
}

// FeedRawLine feeds a raw provider output line. parse is injected to avoid import cycle.
// parse should be adapter.ProviderAdapter.ParseStreamDelta converted by the caller.
func (r *StepRecorder) FeedRawLine(line []byte, parse func([]byte) ([]StepDelta, error)) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	deltas, err := parse(line)
	if err != nil {
		slog.Debug("step_recorder: parse error", slog.Any("err", err))
		return
	}
	for _, d := range deltas {
		r.Feed(d)
	}
}

// Close flushes all pending open steps as "done".
func (r *StepRecorder) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drainAllLocked()
}

// --- locked helpers (must be called with r.mu held) ---

func (r *StepRecorder) openOrReuseThinkLocked(d StepDelta) {
	if r.pending != nil && r.pending.kind == StepThink {
		if d.Text != "" {
			r.pending.body.WriteString(d.Text)
		}
		return
	}
	// Skip opening a think step for an empty text block (redacted/empty thinking).
	if d.Text == "" {
		return
	}
	r.closeCurrentLocked()
	r.seq++
	r.pending = &openStep{
		seq:       r.seq,
		kind:      StepThink,
		title:     "Thinking",
		startedAt: time.Now().UTC(),
	}
	r.pending.body.WriteString(d.Text)
}

func (r *StepRecorder) accumulateTextLocked(text string) {
	if r.pending == nil {
		r.openOrReuseThinkLocked(StepDelta{Kind: StepDeltaThinking, Text: text})
		return
	}
	r.pending.body.WriteString(text)
}

func (r *StepRecorder) closePendingWithStatusLocked(p *openStep, status string) {
	if p == nil {
		return
	}
	r.pending = nil
	// Drop think steps with no body — they produce empty Thinking rows in the UI.
	if p.kind == StepThink && p.body.Len() == 0 {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	step := RunStep{
		ID:        p.id, // reuse pre-assigned ID so INSERT OR REPLACE updates the live row
		RunID:     r.runID,
		TaskID:    r.taskID,
		Seq:       p.seq,
		Kind:      p.kind,
		Title:     p.title,
		Command:   p.command,
		Body:      p.body.String(),
		Status:    status,
		StartedAt: p.startedAt.Format(time.RFC3339Nano),
		EndedAt:   &now,
	}
	r.persist(step)
	r.publish("run.step", step)
}

func (r *StepRecorder) closeCurrentLocked() {
	r.closePendingWithStatusLocked(r.pending, "done")
}

// closeToolStepLocked closes a single parallel tool step without touching r.pending or the map.
// The caller must remove the entry from r.pendingTools after calling this.
func (r *StepRecorder) closeToolStepLocked(p *openStep, status string) {
	if p == nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	step := RunStep{
		ID:        p.id, // reuse pre-assigned ID so INSERT OR REPLACE updates the live row
		RunID:     r.runID,
		TaskID:    r.taskID,
		Seq:       p.seq,
		Kind:      p.kind,
		Title:     p.title,
		Command:   p.command,
		Body:      p.body.String(),
		Status:    status,
		StartedAt: p.startedAt.Format(time.RFC3339Nano),
		EndedAt:   &now,
	}
	r.persist(step)
	r.publish("run.step", step)
}

// closeAllPendingToolsLocked drains every in-flight parallel tool step as "done".
func (r *StepRecorder) closeAllPendingToolsLocked() {
	for id, p := range r.pendingTools {
		r.closeToolStepLocked(p, "done")
		delete(r.pendingTools, id)
	}
}

// drainAllLocked closes r.pending and all parallel tool steps.
// Use at run boundaries (EmitWake, EmitState, Close, StepDeltaResult).
func (r *StepRecorder) drainAllLocked() {
	r.closeCurrentLocked()
	r.closeAllPendingToolsLocked()
}

func (r *StepRecorder) persist(step RunStep) {
	if r.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if step.ID == "" {
		step.ID = uuid.New().String()
	}

	var parentSeqNull sql.NullInt64
	if step.ParentSeq != nil {
		parentSeqNull = sql.NullInt64{Int64: int64(*step.ParentSeq), Valid: true}
	}
	var endedAtNull sql.NullString
	if step.EndedAt != nil {
		endedAtNull = sql.NullString{String: *step.EndedAt, Valid: true}
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO run_steps (id, run_id, task_id, seq, parent_seq, kind, title, body, status, started_at, ended_at, command)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		step.ID, step.RunID, step.TaskID, step.Seq, parentSeqNull,
		string(step.Kind), step.Title, step.Body, step.Status,
		step.StartedAt, endedAtNull, step.Command,
	)
	if err != nil {
		slog.Warn("run_steps upsert failed", slog.Any("err", err))
	}
}

// toolUseKind maps a tool name to a StepKind.
func toolUseKind(name string) StepKind {
	name = strings.ToLower(name)
	switch {
	case name == "bash" || name == "computer" || strings.Contains(name, "execute"):
		return StepRun
	case strings.Contains(name, "edit") || strings.Contains(name, "write") || strings.Contains(name, "create"):
		return StepEdit
	case strings.Contains(name, "read") || strings.Contains(name, "view") || strings.Contains(name, "glob") || strings.Contains(name, "grep") || strings.Contains(name, "list") || strings.Contains(name, "search"):
		return StepRead
	case strings.Contains(name, "checkpoint"):
		return StepCheckpoint
	default:
		return StepRun
	}
}

// extractToolMeta extracts a plain-language title and the raw command from tool input JSON.
// For Bash: uses "description" when present (plain-language summary the agent provided),
// falls back to "command". For Read/Edit/Write: uses a verb + repo-relative path.
// Returns (title, command) where command is the verbatim shell command (non-empty only for
// Bash/run steps). root is the absolute task worktree path used to relativize file paths.
func extractToolMeta(name, inputJSON, root string) (title, command string) {
	if inputJSON != "" {
		var m map[string]json.RawMessage
		if json.Unmarshal([]byte(inputJSON), &m) == nil {
			if v, ok := m["command"]; ok {
				var cmd string
				if json.Unmarshal(v, &cmd) == nil && cmd != "" {
					command = cmd
					// Use the agent's plain-language description when available.
					if d, ok2 := m["description"]; ok2 {
						var desc string
						if json.Unmarshal(d, &desc) == nil && desc != "" {
							return desc, command
						}
					}
					return command, command
				}
			}
			if v, ok := m["file_path"]; ok {
				var s string
				if json.Unmarshal(v, &s) == nil && s != "" {
					rel := relativizePath(s, root)
					verb := fileToolVerb(name)
					return verb + " " + rel, ""
				}
			}
		}
	}
	return toolUseTitle(name), ""
}

// extractToolTitle is a compatibility shim used by tests and callers that only need the title.
func extractToolTitle(name, inputJSON, root string) string {
	title, _ := extractToolMeta(name, inputJSON, root)
	return title
}

// relativizePath strips root from an absolute path, returning a repo-relative path.
// If root is empty or path is not under root, the path is returned unchanged.
func relativizePath(path, root string) string {
	if root == "" || path == "" {
		return path
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}

// fileToolVerb returns the capitalized action word for a file tool name.
func fileToolVerb(name string) string {
	switch strings.ToLower(name) {
	case "read", "readfile", "view":
		return "Read"
	case "edit", "multiedit":
		return "Edit"
	case "write", "writefile", "create":
		return "Write"
	default:
		return "Access"
	}
}

// toolUseTitle returns a human-readable title for a tool_use delta.
func toolUseTitle(name string) string {
	switch strings.ToLower(name) {
	case "bash":
		return "Run command"
	case "read", "readfile":
		return "Read file"
	case "edit", "multiedit":
		return "Edit file"
	case "write", "writefile":
		return "Write file"
	case "glob":
		return "Glob files"
	case "grep":
		return "Search codebase"
	default:
		if name == "" {
			return "Tool call"
		}
		return name
	}
}

// MarshalJSON for RunStep — used by SSE publish and tests.
func (s RunStep) MarshalJSON() ([]byte, error) {
	type Alias RunStep
	return json.Marshal(Alias(s))
}

// estimateRunCost returns an estimated USD cost using published API list prices.
// Mirrors telemetry.EstimateModelCost without importing the telemetry package
// (which would create an import cycle through telemetry → context → orchestrator).
func estimateRunCost(model string, inputTokens, outputTokens, cacheRead, cacheCreation int64) float64 {
	lower := strings.ToLower(model)
	var inPerM, outPerM, cacheReadPerM, cacheCreatePerM float64
	switch {
	case strings.Contains(lower, "opus-5-5") || strings.Contains(lower, "opus-5.5"):
		inPerM, outPerM, cacheReadPerM, cacheCreatePerM = 4.00, 20.00, 0.20, 5.00
	case strings.Contains(lower, "opus-5"):
		inPerM, outPerM, cacheReadPerM, cacheCreatePerM = 5.00, 25.00, 0.50, 6.25
	case strings.Contains(lower, "opus"):
		inPerM, outPerM, cacheReadPerM, cacheCreatePerM = 15.00, 75.00, 1.50, 18.75
	case strings.Contains(lower, "haiku"):
		inPerM, outPerM, cacheReadPerM, cacheCreatePerM = 0.80, 4.00, 0.08, 1.00
	case strings.Contains(lower, "gemini-3.1-flash-lite") || strings.Contains(lower, "flash-lite"):
		inPerM, outPerM, cacheReadPerM, cacheCreatePerM = 0.25, 1.50, 0.025, 0.25
	case strings.Contains(lower, "gemini") && strings.Contains(lower, "flash"):
		inPerM, outPerM, cacheReadPerM, cacheCreatePerM = 0.75, 3.75, 0.075, 0.75
	case strings.Contains(lower, "gemini") && strings.Contains(lower, "pro"):
		inPerM, outPerM, cacheReadPerM, cacheCreatePerM = 2.00, 12.00, 0.20, 2.00
	default: // sonnet-class default
		inPerM, outPerM, cacheReadPerM, cacheCreatePerM = 3.00, 15.00, 0.30, 3.75
	}
	return (float64(inputTokens)*inPerM +
		float64(outputTokens)*outPerM +
		float64(cacheRead)*cacheReadPerM +
		float64(cacheCreation)*cacheCreatePerM) / 1_000_000.0
}
