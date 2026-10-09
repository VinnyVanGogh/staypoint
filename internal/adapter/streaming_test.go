package adapter

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/router"
)

// streamWithCommit is streamWithCommitWatch with no watcher.
func streamWithCommit(r io.Reader, dst io.Writer, isCommit func(line []byte) bool) (bool, []byte) {
	return streamWithCommitWatch(r, dst, isCommit, nil)
}

// TestStreamWithCommit_CommitsOnAssistantEvent verifies that output is forwarded
// to dst once a commit event arrives, and pre-commit lines are flushed first.
func TestStreamWithCommit_CommitsOnAssistantEvent(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s1","model":"test"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`,
		`{"type":"result","subtype":"success","session_id":"s1","is_error":false}`,
	}, "\n") + "\n"

	var dst bytes.Buffer
	committed, prebuf := streamWithCommit(
		strings.NewReader(input),
		&dst,
		func(line []byte) bool {
			return isAssistantEvent(line, ClaudeAdapter{}.ParseStreamDelta)
		},
	)

	if !committed {
		t.Fatal("expected committed=true")
	}
	if len(prebuf) != 0 {
		t.Errorf("expected no prebuf after commit, got %d bytes", len(prebuf))
	}
	out := dst.String()
	if !strings.Contains(out, `"system"`) {
		t.Errorf("pre-commit init line not flushed: %q", out)
	}
	if !strings.Contains(out, `"assistant"`) {
		t.Errorf("commit line not written: %q", out)
	}
	if !strings.Contains(out, `"result"`) {
		t.Errorf("post-commit result line not written: %q", out)
	}
}

// TestStreamWithCommit_BuffersBeforeCommit verifies that pre-commit lines are
// returned as prebuf when EOF arrives before any commit event.
func TestStreamWithCommit_BuffersBeforeCommit(t *testing.T) {
	input := `{"type":"result","subtype":"error","is_error":true}` + "\n"

	var dst bytes.Buffer
	committed, prebuf := streamWithCommit(
		strings.NewReader(input),
		&dst,
		func(line []byte) bool {
			return isAssistantEvent(line, ClaudeAdapter{}.ParseStreamDelta)
		},
	)

	if committed {
		t.Error("expected committed=false for error-only stream")
	}
	if dst.Len() != 0 {
		t.Errorf("nothing should be written to dst before commit, got %q", dst.String())
	}
	if !strings.Contains(string(prebuf), `"result"`) {
		t.Errorf("expected prebuf to contain error result, got %q", string(prebuf))
	}
}

// timestampWriter records the wall-clock time of each Write call.
type timestampWriter struct {
	mu       sync.Mutex
	firstAt  time.Time
	buf      bytes.Buffer
}

func (w *timestampWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.firstAt.IsZero() {
		w.firstAt = time.Now()
	}
	return w.buf.Write(p)
}

func (w *timestampWriter) FirstAt() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.firstAt
}

func (w *timestampWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestStreamingFallback_LiveThroughput asserts that stdout receives its first
// write before the provider process exits. A slow fake provider writes a
// DeltaToolUse event immediately, sleeps 300 ms, then exits. Without streaming
// the first write would arrive only after the full 300 ms; with streaming it
// arrives within a few milliseconds of the first content event.
//
// Note: DeltaInit is NOT a commit trigger (see isAssistantEvent). We use a
// step_update (DeltaToolUse) as the first event so the commit fires immediately.
func TestStreamingFallback_LiveThroughput(t *testing.T) {
	// Fake provider: writes a step_update with state RUNNING (→ DeltaToolUse) immediately —
	// this is the commit event — then sleeps 300 ms before writing the result and exiting.
	// DeltaInit is no longer a commit trigger (STA-479), so we use DeltaToolUse here.
	contentLine := `{"event":"step_update","step_update":{"conversation_id":"live-test","step_index":0,"state":"RUNNING","step_type":"tool","tool_name":"Read"}}`
	resultLine := `{"event":"result","result":{"conversation_id":"live-test","status":"success","response":"done"}}`

	script := "#!/bin/sh\n" +
		"echo '" + contentLine + "'\n" +
		"sleep 0.3\n" +
		"echo '" + resultLine + "'\n"

	f, err := os.CreateTemp("", "slow-provider-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString(script)
	f.Close()
	os.Chmod(f.Name(), 0755)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, testBinKey, f.Name())

	w := &timestampWriter{}
	pacerState := &router.PacerState{}
	start := time.Now()

	runErr := RunAdapter(ctx, ".", pacerState, "gemini", []string{"--model", "google/gemini-3.8-flash"}, nil, w, io.Discard)
	total := time.Since(start)

	if runErr != nil {
		t.Fatalf("RunAdapter error: %v", runErr)
	}

	firstAt := w.FirstAt()
	if firstAt.IsZero() {
		t.Fatal("no writes to stdout at all")
	}

	// Streaming assertion: first write must arrive before the process exits.
	// The script writes init immediately, sleeps 300 ms, then writes more and exits.
	// With streaming: first write ≈ subprocess startup latency (can be 200+ ms on macOS).
	// With buffering: first write == total (everything arrives at exit).
	// We use a relative check: first write must arrive at least 100 ms before process exit,
	// which proves streaming regardless of absolute startup latency.
	latency := firstAt.Sub(start)
	if latency+100*time.Millisecond >= total {
		t.Errorf("output was buffered: first write at +%v, total run %v "+
			"(expected first write at least 100ms before process exit to prove streaming)", latency, total)
	}

	out := w.String()
	if !strings.Contains(out, "live-test") {
		t.Errorf("expected output to contain session id, got %q", out)
	}
}

// TestStreamingFallback_FallbackOnPreCommitFailure verifies that a candidate
// that fails before producing any commit-class event does not poison stdout and
// the next candidate's output reaches the caller.
func TestStreamingFallback_FallbackOnPreCommitFailure(t *testing.T) {
	// First candidate: writes only an error result (no init/assistant) then exits 1.
	firstScript := "#!/bin/sh\n" +
		`echo '{"type":"result","subtype":"error","is_error":true,"result":"rate limited"}'` + "\n" +
		"exit 1\n"
	f1, err := os.CreateTemp("", "first-fail-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f1.Name())
	f1.WriteString(firstScript)
	f1.Close()
	os.Chmod(f1.Name(), 0755)

	// Second candidate: writes init + content + result.
	initLine2 := `{"type":"system","subtype":"init","session_id":"s2","model":"fake"}`
	contentLine2 := `{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}`
	resultLine2 := `{"type":"result","subtype":"success","session_id":"s2","is_error":false}`
	secondScript := "#!/bin/sh\n" +
		"echo '" + initLine2 + "'\n" +
		"echo '" + contentLine2 + "'\n" +
		"echo '" + resultLine2 + "'\n"

	f2, err := os.CreateTemp("", "second-ok-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f2.Name())
	f2.WriteString(secondScript)
	f2.Close()
	os.Chmod(f2.Name(), 0755)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	opts := ParsedOptions{Prompt: "test", OutputFormat: "stream-json"}
	var outBuf, errBuf bytes.Buffer

	// Exercise the streaming fallback loop directly via providerCandidate.
	candidates := []providerCandidate{
		{Name: "first-fail", Adapter: AgyAdapter{}, PoolID: router.PoolGeminiNative},
		{Name: "second-ok", Adapter: AgyAdapter{}, PoolID: router.PoolPersonalClaude},
	}
	bins := []string{f1.Name(), f2.Name()}

	stdinBytes := []byte{}
	var lastErr error
	committed := false
	for i, cand := range candidates {
		pr, pw := io.Pipe()
		execErrCh := make(chan error, 1)
		go func(c providerCandidate, b string) {
			e := c.Adapter.Execute(ctx, ExecRequest{
				Bin:      b,
				Dir:      ".",
				Opts:     opts,
				ExtraEnv: nil,
				Stdin:    bytes.NewReader(stdinBytes),
				Stdout:   pw,
				Stderr:   &errBuf,
			})
			execErrCh <- e
			if e != nil {
				pw.CloseWithError(e)
			} else {
				pw.Close()
			}
		}(cand, bins[i])

		parse := cand.Adapter.ParseStreamDelta
		c, prebuf := streamWithCommit(pr, &outBuf, func(line []byte) bool {
			return isAssistantEvent(line, parse)
		})
		execErr := <-execErrCh

		if c || execErr == nil {
			committed = true
			if !c && len(prebuf) > 0 {
				outBuf.Write(prebuf)
			}
			lastErr = execErr
			break
		}
		lastErr = execErr
		_ = lastErr
	}

	if !committed {
		t.Fatalf("fallback should have committed on second candidate")
	}

	out := outBuf.String()
	if strings.Contains(out, "rate limited") {
		t.Errorf("first candidate's error output must not reach stdout, got: %q", out)
	}
	if !strings.Contains(out, "s2") {
		t.Errorf("expected second candidate session id in stdout, got: %q", out)
	}
}

// TestStreamingFallback_InitThenQuotaError is the regression test for STA-479.
// It reproduces the exact failure mode: the first candidate (Gemini/agy) emits a
// DeltaInit event before calling the model, then fails with a quota error. Before
// the fix, the init event committed the candidate and suppressed fallback. After
// the fix, init must not count as a commit, so the fallback fires and the second
// candidate's output reaches stdout.
func TestStreamingFallback_InitThenQuotaError(t *testing.T) {
	// First candidate: emits an agy-format init event, then an error result, then exits 3
	// (the exit code the real agy CLI uses for quota exhaustion).
	initLine1 := `{"event":"init","conversation_id":"quota-test","init":{"model":"gemini-3.8-flash"}}`
	errLine1 := `{"event":"result","result":{"conversation_id":"quota-test","status":"error","error":"RESOURCE_EXHAUSTED (code 429): Individual quota reached"}}`
	firstScript := "#!/bin/sh\n" +
		"echo '" + initLine1 + "'\n" +
		"echo '" + errLine1 + "'\n" +
		"exit 3\n"

	f1, err := os.CreateTemp("", "quota-fail-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f1.Name())
	f1.WriteString(firstScript)
	f1.Close()
	os.Chmod(f1.Name(), 0755)

	// Second candidate: a working provider that emits content and exits 0.
	contentLine2 := `{"event":"step_update","step_update":{"conversation_id":"fallback-ok","step_index":0,"state":"DONE","step_type":"tool","tool_name":"Write"}}`
	resultLine2 := `{"event":"result","result":{"conversation_id":"fallback-ok","status":"success","response":"done"}}`
	secondScript := "#!/bin/sh\n" +
		"echo '" + contentLine2 + "'\n" +
		"echo '" + resultLine2 + "'\n"

	f2, err := os.CreateTemp("", "fallback-ok-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f2.Name())
	f2.WriteString(secondScript)
	f2.Close()
	os.Chmod(f2.Name(), 0755)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	opts := ParsedOptions{Prompt: "test", OutputFormat: "stream-json"}
	var outBuf, errBuf bytes.Buffer

	candidates := []providerCandidate{
		{Name: "gemini-quota", Adapter: AgyAdapter{}, PoolID: router.PoolGeminiNative},
		{Name: "personal-claude", Adapter: AgyAdapter{}, PoolID: router.PoolPersonalClaude},
	}
	bins := []string{f1.Name(), f2.Name()}

	stdinBytes := []byte{}
	committed := false
	for i, cand := range candidates {
		pr, pw := io.Pipe()
		execErrCh := make(chan error, 1)
		go func(c providerCandidate, b string) {
			e := c.Adapter.Execute(ctx, ExecRequest{
				Bin:      b,
				Dir:      ".",
				Opts:     opts,
				ExtraEnv: nil,
				Stdin:    bytes.NewReader(stdinBytes),
				Stdout:   pw,
				Stderr:   &errBuf,
			})
			execErrCh <- e
			if e != nil {
				pw.CloseWithError(e)
			} else {
				pw.Close()
			}
		}(cand, bins[i])

		parse := cand.Adapter.ParseStreamDelta
		c, prebuf := streamWithCommit(pr, &outBuf, func(line []byte) bool {
			return isAssistantEvent(line, parse)
		})
		execErr := <-execErrCh

		if c || execErr == nil {
			committed = true
			if !c && len(prebuf) > 0 {
				outBuf.Write(prebuf)
			}
			break
		}
	}

	if !committed {
		t.Fatalf("expected fallback to second candidate, but neither committed")
	}

	out := outBuf.String()
	if strings.Contains(out, "quota-test") {
		t.Errorf("first (quota-failed) candidate output must not reach stdout; got: %q", out)
	}
	if !strings.Contains(out, "fallback-ok") {
		t.Errorf("expected second candidate output in stdout; got: %q", out)
	}
}
