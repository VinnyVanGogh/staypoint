package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata/*.golden.json from current parser output")

// Fixtures are sanitized captures of real CLI output:
//   - claude_stream.ndjson: claude 2.1.285 `--print --output-format stream-json --verbose`, one Bash tool call.
//   - agy_stream_success.ndjson: agy 1.2.x stream-json from a StayPoint run (trimmed, paths scrubbed).
//   - agy_stream_quota_error.ndjson: agy 1.2.13 stream-json when the Gemini quota was exhausted (HTTP 429).
var goldenCases = []struct {
	fixture string
	adapter ProviderAdapter
}{
	{"claude_stream", ClaudeAdapter{}},
	{"agy_stream_success", AgyAdapter{}},
	{"agy_stream_quota_error", AgyAdapter{}},
	{"codex_stream", CodexAdapter{}},
}

func parseFixture(t *testing.T, a ProviderAdapter, name string) []StreamDelta {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name+".ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var all []StreamDelta
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for n := 1; sc.Scan(); n++ {
		deltas, err := a.ParseStreamDelta(sc.Bytes())
		if err != nil {
			t.Fatalf("%s line %d: %v", name, n, err)
		}
		all = append(all, deltas...)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return all
}

func TestStreamGoldenFixtures(t *testing.T) {
	for _, tc := range goldenCases {
		t.Run(tc.fixture, func(t *testing.T) {
			got, err := json.MarshalIndent(parseFixture(t, tc.adapter, tc.fixture), "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			goldenPath := filepath.Join("testdata", tc.fixture+".golden.json")
			if *updateGolden {
				if err := os.WriteFile(goldenPath, got, 0644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("missing golden file (run go test -run TestStreamGoldenFixtures -update): %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("parsed stream differs from %s; rerun with -update if the change is intended.\ngot:\n%s", goldenPath, got)
			}
		})
	}
}

func findKind(deltas []StreamDelta, kind DeltaKind) []StreamDelta {
	var out []StreamDelta
	for _, d := range deltas {
		if d.Kind == kind {
			out = append(out, d)
		}
	}
	return out
}

// Semantic checks so the golden files cannot silently encode a broken parse.
func TestClaudeStreamSemantics(t *testing.T) {
	deltas := parseFixture(t, ClaudeAdapter{}, "claude_stream")
	const session = "00000000-0000-4000-8000-00000000c1a0"

	inits := findKind(deltas, DeltaInit)
	if len(inits) != 1 || inits[0].SessionID != session || inits[0].Model == "" {
		t.Errorf("init = %+v", inits)
	}
	tools := findKind(deltas, DeltaToolUse)
	if len(tools) != 1 || tools[0].ToolName != "Bash" || tools[0].ToolID == "" {
		t.Fatalf("tool_use = %+v", tools)
	}
	results := findKind(deltas, DeltaToolResult)
	if len(results) != 1 || results[0].ToolID != tools[0].ToolID || results[0].IsError {
		t.Errorf("tool_result = %+v", results)
	}
	texts := findKind(deltas, DeltaText)
	if len(texts) != 1 || texts[0].Text != "done" {
		t.Errorf("text = %+v", texts)
	}
	final := findKind(deltas, DeltaResult)
	if len(final) != 1 || final[0].Status != "success" || final[0].IsError || final[0].Text != "done" {
		t.Fatalf("result = %+v", final)
	}
	if u := final[0].Usage; u == nil || u.OutputTokens == 0 || u.CacheReadTokens == 0 {
		t.Errorf("result usage = %+v", u)
	}
}

func TestAgyStreamSemantics(t *testing.T) {
	ok := parseFixture(t, AgyAdapter{}, "agy_stream_success")
	if inits := findKind(ok, DeltaInit); len(inits) != 1 || inits[0].Model != "gemini-3.8-flash" {
		t.Errorf("init = %+v", inits)
	}
	if uses := findKind(ok, DeltaToolUse); len(uses) != 2 || uses[0].ToolName != "view_file" {
		t.Errorf("tool_use = %+v", uses)
	}
	if len(findKind(ok, DeltaUsage)) != 3 {
		t.Errorf("expected 3 usage deltas, got %+v", findKind(ok, DeltaUsage))
	}
	final := findKind(ok, DeltaResult)
	if len(final) != 1 || final[0].IsError || final[0].Status != "SUCCESS" || final[0].Text == "" || final[0].Usage == nil {
		t.Errorf("result = %+v", final)
	}

	bad := parseFixture(t, AgyAdapter{}, "agy_stream_quota_error")
	if len(findKind(bad, DeltaError)) != 1 {
		t.Errorf("expected one error step, got %+v", bad)
	}
	final = findKind(bad, DeltaResult)
	if len(final) != 1 || !final[0].IsError || !strings.Contains(final[0].Error, "quota") {
		t.Errorf("quota result = %+v", final)
	}
}

func TestCodexStreamSemantics(t *testing.T) {
	deltas := parseFixture(t, CodexAdapter{}, "codex_stream")

	thinks := findKind(deltas, DeltaThinking)
	if len(thinks) != 1 || thinks[0].SessionID == "" {
		t.Errorf("expected 1 thinking delta, got %+v", thinks)
	}

	tools := findKind(deltas, DeltaToolUse)
	if len(tools) != 1 || tools[0].ToolName != "shell" || tools[0].ToolID != "call_Jkn3kXFFwFABIlHAN83qAQYC" {
		t.Fatalf("expected tool_use for shell, got %+v", tools)
	}

	results := findKind(deltas, DeltaToolResult)
	if len(results) != 1 || results[0].ToolID != tools[0].ToolID || results[0].IsError || !strings.Contains(results[0].Text, "README.md") {
		t.Errorf("expected successful tool_result with README.md, got %+v", results)
	}

	texts := findKind(deltas, DeltaText)
	if len(texts) != 1 || !strings.Contains(texts[0].Text, "inspected the repository") {
		t.Errorf("expected assistant text delta, got %+v", texts)
	}

	final := findKind(deltas, DeltaResult)
	if len(final) != 1 || final[0].Status != "completed" || final[0].Usage == nil || final[0].Usage.InputTokens != 120 {
		t.Fatalf("expected completed result with usage, got %+v", final)
	}
}

func TestCodexBuildArgs(t *testing.T) {
	opts := ParsedOptions{
		Prompt:  "fix bug in main.go",
		Model:   "o4-mini",
		AddDirs: []string{"/path/to/dir1", "/path/to/dir2"},
	}
	args := (CodexAdapter{}).BuildArgs(opts)

	expected := []string{
		"-q",
		"--dangerously-auto-approve-everything",
		"-m", "o4-mini",
		"-w", "/path/to/dir1",
		"-w", "/path/to/dir2",
		"fix bug in main.go",
	}
	if !reflect.DeepEqual(args, expected) {
		t.Errorf("codex BuildArgs = %v, want %v", args, expected)
	}
}

func TestLocalOpenAI_MockSSE(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		var req openAIChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if !req.Stream {
			t.Errorf("expected stream: true")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected http.Flusher")
		}

		events := []string{
			`data: {"id":"chatcmpl-test","model":"llama-3","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello "},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-test","model":"llama-3","choices":[{"index":0,"delta":{"content":"from local SSE!"},"finish_reason":null}]}`,
			`data: {"id":"chatcmpl-test","model":"llama-3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":8}}`,
			`data: [DONE]`,
		}

		for _, ev := range events {
			fmt.Fprintf(w, "%s\n\n", ev)
			flusher.Flush()
		}
	}))
	defer server.Close()

	adapter := LocalOpenAIAdapter{BaseURL: server.URL}
	var stdout bytes.Buffer
	req := ExecRequest{
		Opts: ParsedOptions{
			Prompt: "Say hello",
			Model:  "llama-3",
		},
		Stdout: &stdout,
	}

	if err := adapter.Execute(context.Background(), req); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	// Parse lines and verify deltas
	var allDeltas []StreamDelta
	sc := bufio.NewScanner(&stdout)
	for sc.Scan() {
		line := sc.Bytes()
		deltas, err := adapter.ParseStreamDelta(line)
		if err != nil {
			t.Fatalf("ParseStreamDelta(%q): %v", string(line), err)
		}
		allDeltas = append(allDeltas, deltas...)
	}

	texts := findKind(allDeltas, DeltaText)
	if len(texts) != 2 || texts[0].Text != "Hello " || texts[1].Text != "from local SSE!" {
		t.Errorf("text deltas = %+v", texts)
	}

	results := findKind(allDeltas, DeltaResult)
	if len(results) != 2 || results[0].Status != "stop" || results[1].Status != "completed" {
		t.Errorf("result deltas = %+v", results)
	}

	usages := findKind(allDeltas, DeltaUsage)
	if len(usages) != 1 || usages[0].Usage.InputTokens != 12 || usages[0].Usage.OutputTokens != 8 {
		t.Errorf("usage deltas = %+v", usages)
	}
}

func TestOllama_MockNDJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/api/chat") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected http.Flusher")
		}

		lines := []string{
			`{"model":"llama3","created_at":"2026-09-29T00:00:00Z","message":{"role":"assistant","content":"Hello "},"done":false}`,
			`{"model":"llama3","created_at":"2026-09-29T00:00:01Z","message":{"role":"assistant","content":"from Ollama!"},"done":false}`,
			`{"model":"llama3","created_at":"2026-09-29T00:00:02Z","message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":15,"eval_count":9}`,
		}

		for _, line := range lines {
			fmt.Fprintf(w, "%s\n", line)
			flusher.Flush()
		}
	}))
	defer server.Close()

	adapter := OllamaAdapter{BaseURL: server.URL}
	var stdout bytes.Buffer
	req := ExecRequest{
		Opts: ParsedOptions{
			Prompt: "Say hello",
			Model:  "llama3",
		},
		Stdout: &stdout,
	}

	if err := adapter.Execute(context.Background(), req); err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	var allDeltas []StreamDelta
	sc := bufio.NewScanner(&stdout)
	for sc.Scan() {
		line := sc.Bytes()
		deltas, err := adapter.ParseStreamDelta(line)
		if err != nil {
			t.Fatalf("ParseStreamDelta(%q): %v", string(line), err)
		}
		allDeltas = append(allDeltas, deltas...)
	}

	texts := findKind(allDeltas, DeltaText)
	if len(texts) != 2 || texts[0].Text != "Hello " || texts[1].Text != "from Ollama!" {
		t.Errorf("text deltas = %+v", texts)
	}

	results := findKind(allDeltas, DeltaResult)
	if len(results) != 1 || results[0].Status != "completed" || results[0].Usage == nil || results[0].Usage.InputTokens != 15 || results[0].Usage.OutputTokens != 9 {
		t.Errorf("result deltas = %+v", results)
	}
}

func TestParseStreamDeltaKeepaliveAndGarbage(t *testing.T) {
	for _, a := range Adapters() {
		for _, blank := range []string{"", "   ", "\n"} {
			d, err := a.ParseStreamDelta([]byte(blank))
			if err != nil || d != nil {
				t.Errorf("%s: keepalive %q -> %v, %v", a.Provider(), blank, d, err)
			}
		}
		if _, err := a.ParseStreamDelta([]byte("not json")); err == nil {
			t.Errorf("%s: expected error for non-JSON line", a.Provider())
		}
	}
}

func TestBuildArgsMatchesLegacyBuilders(t *testing.T) {
	opts := parseRawArgs([]string{"--model", "google/gemini-3.8-flash-high", "--resume", "s1", "--add-dir", "/tmp/x", "--prompt", "hi"})
	if got := (ClaudeAdapter{}).BuildArgs(opts); !reflect.DeepEqual(got, buildClaudeArgs(opts)) {
		t.Errorf("claude BuildArgs = %v", got)
	}
	if got := (AgyAdapter{}).BuildArgs(opts); !reflect.DeepEqual(got, buildAgyArgs(opts)) {
		t.Errorf("agy BuildArgs = %v", got)
	}
}

func writeExec(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	return p
}

func envFunc(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolverSkipsShimsAndSelf(t *testing.T) {
	shimDir, selfDir, realDir := t.TempDir(), t.TempDir(), t.TempDir()
	writeExec(t, shimDir, "claude", "#!/usr/bin/env bash\n# Auto-generated by Staypoint\nexec staypoint adapter claude \"$@\"\n")
	self := writeExec(t, selfDir, "claude", "#!/bin/sh\necho self\n")
	real := writeExec(t, realDir, "claude", "#!/bin/sh\necho real\n")

	r := &Resolver{
		Getenv: envFunc(map[string]string{"PATH": strings.Join([]string{shimDir, selfDir, realDir}, string(os.PathListSeparator))}),
		Self:   canonicalPath(self),
	}
	got, err := r.Resolve(ClaudeAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	if canonicalPath(got) != canonicalPath(real) {
		t.Errorf("resolved %s, want %s", got, real)
	}
}

func TestResolverExtraDirsAndNotFound(t *testing.T) {
	extra := t.TempDir()
	agy := writeExec(t, extra, "agy", "#!/bin/sh\n")
	r := &Resolver{Getenv: envFunc(map[string]string{"PATH": t.TempDir()}), ExtraDirs: []string{extra}}
	if got, err := r.Resolve(AgyAdapter{}); err != nil || canonicalPath(got) != canonicalPath(agy) {
		t.Errorf("extra dir resolve = %q, %v", got, err)
	}

	r = &Resolver{Getenv: envFunc(map[string]string{"PATH": t.TempDir()})}
	_, err := r.Resolve(ClaudeAdapter{})
	if err == nil || !strings.Contains(err.Error(), "STAYPOINT_CLAUDE_BIN") {
		t.Errorf("expected not-found error naming the env override, got %v", err)
	}
}

func TestResolverOverrides(t *testing.T) {
	dir := t.TempDir()
	envBin := writeExec(t, dir, "claude-env", "#!/bin/sh\n")
	cfgBin := writeExec(t, dir, "claude-cfg", "#!/bin/sh\n")
	shim := writeExec(t, dir, "claude-shim", "#!/bin/sh\n# Auto-generated by Staypoint\n")

	r := &Resolver{
		Overrides: map[string]string{"claude": cfgBin},
		Getenv:    envFunc(map[string]string{"STAYPOINT_CLAUDE_BIN": envBin, "PATH": ""}),
	}
	if got, err := r.Resolve(ClaudeAdapter{}); err != nil || got != envBin {
		t.Errorf("env override = %q, %v", got, err)
	}

	r.Getenv = envFunc(map[string]string{"PATH": ""})
	if got, err := r.Resolve(ClaudeAdapter{}); err != nil || got != cfgBin {
		t.Errorf("config override = %q, %v", got, err)
	}

	r.Overrides["claude"] = shim
	if _, err := r.Resolve(ClaudeAdapter{}); err == nil || !strings.Contains(err.Error(), "recurse") {
		t.Errorf("override pointing at a staypoint shim must be rejected, got %v", err)
	}

	r.Overrides["claude"] = filepath.Join(dir, "missing")
	if _, err := r.Resolve(ClaudeAdapter{}); err == nil {
		t.Errorf("missing override must error, not fall back silently")
	}
}

func TestProbeVersion(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name, output string
		a            ProviderAdapter
		major        int
		known        bool
	}{
		{"claude-current", "2.1.285 (Claude Code)", ClaudeAdapter{}, 2, true},
		{"claude-future", "3.0.0 (Claude Code)", ClaudeAdapter{}, 3, false},
		{"agy-current", "1.2.13", AgyAdapter{}, 1, true},
		{"agy-garbage", "agy dev build", AgyAdapter{}, 0, false},
		{"codex-current", "0.1.2504172351", CodexAdapter{}, 0, true},
		{"codex-future", "1.0.0", CodexAdapter{}, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := writeExec(t, dir, tc.name, "#!/bin/sh\necho '"+tc.output+"'\n")
			info, err := ProbeVersion(context.Background(), tc.a, bin)
			if err != nil {
				t.Fatal(err)
			}
			if info.Major != tc.major || info.Known != tc.known || (info.Warning == "") != tc.known {
				t.Errorf("info = %+v", info)
			}
		})
	}

	if _, err := ProbeVersion(context.Background(), ClaudeAdapter{}, filepath.Join(dir, "absent")); err == nil {
		t.Error("expected error for missing binary")
	}
}

// An explicit Gemini run whose agy binary is missing falls over to Claude
// (Claude is always a permitted fallback; Gemini never is).
func TestFailoverWhenPrimaryBinaryMissing(t *testing.T) {
	bin := writeExec(t, t.TempDir(), "fake-claude", "#!/bin/sh\necho ran-fallback\n")
	resolve := func(a ProviderAdapter) (string, error) {
		if a.Provider() != "claude" {
			return "", errors.New("agy CLI not found")
		}
		return bin, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var stdout, stderr bytes.Buffer
	if err := runWithFailover(ctx, "", nil, "gemini", []string{"--prompt", "x"}, nil, &stdout, &stderr, resolve); err != nil {
		t.Fatalf("expected fallback to succeed: %v", err)
	}
	if !strings.Contains(stdout.String(), "ran-fallback") || !strings.Contains(stderr.String(), "trying next provider...") {
		t.Errorf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// Guards acceptance criterion "works on a machine without /Users/vincevasile":
// adapter code must not embed any user home path.
func TestNoHardcodedUserPaths(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"/Users/", "/home/", `C:\Users`} {
			if bytes.Contains(src, []byte(bad)) {
				t.Errorf("%s contains hardcoded path prefix %q", f, bad)
			}
		}
	}
}
