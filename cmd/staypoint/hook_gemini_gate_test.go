package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGateDaemon records created requests and answers polls with pollStatus.
type fakeGateDaemon struct {
	mu         sync.Mutex
	created    []map[string]any
	createStat string // status returned on create ("pending" or "approved")
	pollStatus string // status every poll returns
	pollDelay  time.Duration
}

func (f *fakeGateDaemon) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/security/gate-requests":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.created = append(f.created, body)
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"gr1","status":"` + f.createStat + `"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/security/gate-requests/"):
			if f.pollDelay > 0 {
				select {
				case <-time.After(f.pollDelay):
				case <-r.Context().Done():
					return
				}
			}
			_, _ = w.Write([]byte(`{"status":"` + f.pollStatus + `","decided_by":"board"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func geminiPayload(t *testing.T, cmd, cwd string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"conversationId": "conv-1",
		"toolCall": map[string]any{
			"name": "run_command",
			"args": map[string]string{"CommandLine": cmd, "Cwd": cwd},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func withGeminiDaemon(t *testing.T, url string, budget time.Duration) {
	t.Helper()
	oldD, oldB := geminiGateDaemon, geminiGateBudget
	geminiGateDaemon = func() (string, string) { return url, "tok" }
	geminiGateBudget = budget
	t.Cleanup(func() { geminiGateDaemon, geminiGateBudget = oldD, oldB })
	t.Setenv("STAYPOINT_TASK_ID", "")
}

func decision(t *testing.T, out string) (string, string) {
	t.Helper()
	var d struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("hook output %q is not agy JSON: %v", out, err)
	}
	return d.Decision, d.Reason
}

// A command any Claude run would have to get past the Board.
const redCmd = "git push --force origin main"

func TestGeminiGate_RedCommandIsHeldNotAllowed(t *testing.T) {
	cases := []struct {
		name, poll, want, reasonHas string
	}{
		{"board denies", "denied", "deny", "Board denied"},
		{"daemon skips it", "deferred", "deny", "NOT run"},
		{"board approves", "approved", "allow", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGateDaemon{createStat: "pending", pollStatus: tc.poll}
			withGeminiDaemon(t, f.server(t).URL, 5*time.Second)
			got, reason := decision(t, gateGeminiPreTool(geminiPayload(t, redCmd, t.TempDir())))
			if got != tc.want || !strings.Contains(reason, tc.reasonHas) {
				t.Fatalf("decision %q (%q), want %q containing %q", got, reason, tc.want, tc.reasonHas)
			}
			if len(f.created) != 1 {
				t.Fatalf("gate requests created: %d, want 1", len(f.created))
			}
			if mw, _ := f.created[0]["max_wait_seconds"].(float64); mw <= 0 || mw >= 30 {
				t.Fatalf("max_wait_seconds = %v, want inside agy's 30s hook timeout", f.created[0]["max_wait_seconds"])
			}
		})
	}
}

// The unverified case: if the Board never answers, the hook must still deny,
// itself, before agy's 30s timeout could decide for it.
func TestGeminiGate_NoAnswerDeniesInsideBudget(t *testing.T) {
	f := &fakeGateDaemon{createStat: "pending", pollStatus: "pending", pollDelay: 50 * time.Millisecond}
	withGeminiDaemon(t, f.server(t).URL, 300*time.Millisecond)
	start := time.Now()
	got, reason := decision(t, gateGeminiPreTool(geminiPayload(t, redCmd, t.TempDir())))
	if got != "deny" || !strings.Contains(reason, "NOT run") {
		t.Fatalf("decision %q (%q), want deny NOT run", got, reason)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("took %s with a 300ms budget", el)
	}
}

func TestGeminiGate_SlowPollIsCutAtBudget(t *testing.T) {
	// One long-poll that would hold for 10s must not outlast a 300ms budget.
	f := &fakeGateDaemon{createStat: "pending", pollStatus: "approved", pollDelay: 10 * time.Second}
	withGeminiDaemon(t, f.server(t).URL, 300*time.Millisecond)
	start := time.Now()
	got, _ := decision(t, gateGeminiPreTool(geminiPayload(t, redCmd, t.TempDir())))
	if got != "deny" {
		t.Fatalf("decision %q, want deny when the budget runs out", got)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("a slow poll held the hook for %s", el)
	}
}

func TestGeminiGate_DaemonUnreachableDenies(t *testing.T) {
	withGeminiDaemon(t, "", time.Second)
	if got, _ := decision(t, gateGeminiPreTool(geminiPayload(t, redCmd, t.TempDir()))); got != "deny" {
		t.Fatalf("decision %q, want deny with no daemon", got)
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // refuses connections
	withGeminiDaemon(t, srv.URL, time.Second)
	if got, _ := decision(t, gateGeminiPreTool(geminiPayload(t, redCmd, t.TempDir()))); got != "deny" {
		t.Fatalf("decision %q, want deny when the request cannot be filed", got)
	}
}

func TestGeminiGate_AllowRuleApprovesWithoutWaiting(t *testing.T) {
	f := &fakeGateDaemon{createStat: "approved", pollStatus: "pending"}
	withGeminiDaemon(t, f.server(t).URL, 5*time.Second)
	if got, _ := decision(t, gateGeminiPreTool(geminiPayload(t, redCmd, t.TempDir()))); got != "allow" {
		t.Fatalf("decision %q, want allow from a matching rule", got)
	}
}

func rawCall(t *testing.T, name string, args map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"conversationId": "c", "toolCall": map[string]any{"name": name, "args": args}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Parser differential (security review of cd91bea): every way agy could carry
// a command the gate did not read must end held or denied, never allowed.
func TestGeminiGate_UnreadOrHiddenCommandsNeverAllowed(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		raw  []byte
	}{
		{"red command in an unexpected key", rawCall(t, "run_command", map[string]any{"Cmd": redCmd, "Cwd": dir})},
		{"safe CommandLine, red Command", rawCall(t, "run_command", map[string]any{"CommandLine": "ls", "Command": redCmd, "Cwd": dir})},
		{"red input to a running shell", rawCall(t, "send_command_input", map[string]any{"Input": redCmd})},
		{"step-type tool name", rawCall(t, "CORTEX_STEP_TYPE_RUN_COMMAND", map[string]any{"CommandLine": redCmd, "Cwd": dir})},
		{"bypass sandbox on a safe command", rawCall(t, "run_command", map[string]any{"CommandLine": "ls", "Cwd": dir, "BypassSandbox": true})},
		{"bypass sandbox not a bool", rawCall(t, "run_command", map[string]any{"CommandLine": "ls", "Cwd": dir, "BypassSandbox": "yes"})},
		{"argv array", rawCall(t, "run_command", map[string]any{"Argv": []string{"git", "push", "--force", "origin", "main"}, "Cwd": dir})},
		{"shell call with no command", rawCall(t, "run_command", map[string]any{"Cwd": dir})},
		{"unreadable payload", []byte(`{"toolCall":`)},
		{"no tool call", []byte(`{"conversationId":"c"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGateDaemon{createStat: "pending", pollStatus: "denied"}
			withGeminiDaemon(t, f.server(t).URL, 5*time.Second)
			if got, reason := decision(t, gateGeminiPreTool(tc.raw)); got != "deny" {
				t.Fatalf("decision %q (%q), want deny", got, reason)
			}
		})
	}
}

func TestGeminiGate_SafeCommandsAndOtherToolsPass(t *testing.T) {
	f := &fakeGateDaemon{createStat: "pending", pollStatus: "denied"}
	withGeminiDaemon(t, f.server(t).URL, 5*time.Second)
	if got, _ := decision(t, gateGeminiPreTool(geminiPayload(t, "ls -la", t.TempDir()))); got != "allow" {
		t.Fatalf("ls: %q, want allow", got)
	}
	view, _ := json.Marshal(map[string]any{"conversationId": "c", "toolCall": map[string]any{
		"name": "view_file", "args": map[string]string{"AbsolutePath": "/etc/hosts"}}})
	if got, _ := decision(t, gateGeminiPreTool(view)); got != "allow" {
		t.Fatalf("view_file: %q, want allow", got)
	}
	if len(f.created) != 0 {
		t.Fatalf("safe calls filed %d gate requests", len(f.created))
	}
}
