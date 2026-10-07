package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/config"
	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/trackgate"
)

// trackingEnv re-homes HOME (so router.IsWorkRepo sees a temp
// ~/Documents/dev/mansol-* tree) and points cfg at a temp DB.
func trackingEnv(t *testing.T) (workRepo, personalRepo string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workRepo = filepath.Join(home, "Documents", "dev", "mansol-portal")
	personalRepo = filepath.Join(home, "Documents", "dev", "hobby")
	for _, d := range []string{workRepo, personalRepo} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldCfg := cfg
	t.Cleanup(func() { cfg = oldCfg })
	cfg = config.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.DBPath = filepath.Join(cfg.DataDir, "staypoint.db")
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	return workRepo, personalRepo
}

func noEnv(string) string { return "" }

func claudePayload(tool string, input map[string]string, cwd, session string) []byte {
	in, _ := json.Marshal(input)
	b, _ := json.Marshal(map[string]any{"tool_name": tool, "tool_input": json.RawMessage(in), "cwd": cwd, "session_id": session})
	return b
}

func TestPreToolHook_ClaudeWorkRepo(t *testing.T) {
	work, personal := trackingEnv(t)
	gate := productionTrackingGate()

	// Unattached: Edit and git commit blocked with the Claude deny shape.
	for _, raw := range [][]byte{
		claudePayload("Edit", map[string]string{"file_path": filepath.Join(work, "a.go")}, work, "sess-A"),
		claudePayload("Bash", map[string]string{"command": "git commit -m x"}, work, "sess-A"),
	} {
		client, out, blocked := runTrackingGate(raw, "auto", noEnv, gate)
		if !blocked || client != trackgate.ClientClaude {
			t.Fatalf("want Claude block, got blocked=%v client=%s", blocked, client)
		}
		var resp struct {
			Decision string `json:"decision"`
			HSO      struct {
				PermissionDecision string `json:"permissionDecision"`
				Reason             string `json:"permissionDecisionReason"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Decision != "block" || resp.HSO.PermissionDecision != "deny" ||
			!strings.Contains(resp.HSO.Reason, "staypoint task attach <task-id> --session sess-A") {
			t.Errorf("bad block output: %s", out)
		}
	}

	// Read-only and personal-repo writes pass.
	for _, raw := range [][]byte{
		claudePayload("Read", map[string]string{"file_path": filepath.Join(work, "a.go")}, work, "sess-A"),
		claudePayload("Bash", map[string]string{"command": "git status"}, work, "sess-A"),
		claudePayload("Edit", map[string]string{"file_path": filepath.Join(personal, "a.go")}, personal, "sess-A"),
	} {
		if _, out, blocked := runTrackingGate(raw, "auto", noEnv, gate); blocked {
			t.Errorf("unexpected block: %s", out)
		}
	}

	// Daemon run.
	env := func(k string) string {
		if k == "STAYPOINT_TASK_ID" {
			return "task-daemon"
		}
		return ""
	}
	if _, out, blocked := runTrackingGate(claudePayload("Write", map[string]string{"file_path": filepath.Join(work, "b.go")}, work, "s"), "auto", env, gate); blocked {
		t.Errorf("daemon run blocked: %s", out)
	}
}

func TestPreToolHook_FailClosedWhenDBMissing(t *testing.T) {
	work, _ := trackingEnv(t)
	cfg.DBPath = filepath.Join(t.TempDir(), "absent", "staypoint.db")
	_, out, blocked := runTrackingGate(claudePayload("Edit", map[string]string{"file_path": filepath.Join(work, "a.go")}, work, "s"), "auto", noEnv, productionTrackingGate())
	if !blocked || !strings.Contains(out, "failing closed") {
		t.Fatalf("want fail-closed block, got blocked=%v %s", blocked, out)
	}
}

func TestPreToolHook_AgyPayload(t *testing.T) {
	work, _ := trackingEnv(t)
	raw, _ := json.Marshal(map[string]any{
		"toolCall":       map[string]any{"name": "write_to_file", "args": map[string]any{"TargetFile": filepath.Join(work, "main.go"), "CodeContent": "x"}},
		"conversationId": "conv-9",
		"workspacePaths": []string{work},
	})
	client, out, blocked := runTrackingGate(raw, "auto", noEnv, productionTrackingGate())
	if !blocked || client != trackgate.ClientGemini {
		t.Fatalf("agy write not blocked (client %s)", client)
	}
	var resp map[string]string
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["decision"] != "deny" || !strings.Contains(resp["reason"], "claude --work") {
		t.Errorf("bad agy deny output: %s", out)
	}
	// agy shell command via CommandLine/Cwd.
	raw, _ = json.Marshal(map[string]any{
		"toolCall":       map[string]any{"name": "run_command", "args": map[string]any{"CommandLine": "git push", "Cwd": work}},
		"conversationId": "conv-9",
	})
	if _, _, blocked := runTrackingGate(raw, "gemini", noEnv, productionTrackingGate()); !blocked {
		t.Error("agy git push in work repo not blocked")
	}
}

func TestTaskAttachAndCreateOrg(t *testing.T) {
	work, _ := trackingEnv(t)
	store, err := db.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conn := store.DB()

	var out bytes.Buffer
	// No session anywhere -> instructive error.
	if err := attachSession(&out, conn, "task-x", "", trackgate.ClientClaude, work); err == nil || !strings.Contains(err.Error(), "--session") {
		t.Errorf("want missing-session error, got %v", err)
	}

	if err := createLocalOrgTask(&out, conn, "Managed Solution", "", "Fix login", work, "sess-C", trackgate.ClientClaude, 0, 0); err != nil {
		t.Fatalf("create --org: %v", err)
	}
	if !strings.Contains(out.String(), "Attached claude session sess-C") {
		t.Errorf("create output: %s", out.String())
	}
	var org string
	if err := conn.QueryRow(`SELECT t.organization FROM task_session_attachments a JOIN tasks t ON t.id = a.task_id WHERE a.session_id = 'sess-C'`).Scan(&org); err != nil || org != "Managed Solution" {
		t.Fatalf("attachment/org = %q, %v", org, err)
	}

	// The hook now lets sess-C write.
	if _, o, blocked := runTrackingGate(claudePayload("Edit", map[string]string{"file_path": filepath.Join(work, "a.go")}, work, "sess-C"), "auto", noEnv, productionTrackingGate()); blocked {
		t.Errorf("attached session blocked: %s", o)
	}
}

func TestResolveSessionID(t *testing.T) {
	env := map[string]string{"CLAUDE_CODE_SESSION_ID": "from-env"}
	get := func(k string) string { return env[k] }
	if got := resolveSessionID("", get); got != "from-env" {
		t.Errorf("env fallback = %q", got)
	}
	if got := resolveSessionID("flag", get); got != "flag" {
		t.Errorf("flag = %q", got)
	}
}

func TestGateOverride_RefusedInAgentContext(t *testing.T) {
	for _, k := range agentContextEnv {
		get := func(key string) string {
			if key == k {
				return "1"
			}
			return ""
		}
		if err := refuseOverrideInAgentContext(get, true); err == nil {
			t.Errorf("%s set: override not refused", k)
		}
	}
	if err := refuseOverrideInAgentContext(noEnv, false); err == nil {
		t.Error("non-TTY: override not refused")
	}
	if err := refuseOverrideInAgentContext(noEnv, true); err != nil {
		t.Errorf("human terminal refused: %v", err)
	}
}

func TestGateOverride_FilesRequestAndWaits(t *testing.T) {
	var created atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/security/gate-requests":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			created.Store(body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"gr-1"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/security/gate-requests/gr-1":
			_, _ = w.Write([]byte(`{"status":"approved"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	if err := requestTrackingOverride(&bytes.Buffer{}, srv.URL, "tok", "Managed Solution", 0, "", true); err == nil {
		t.Error("0 minutes accepted")
	}
	if err := requestTrackingOverride(&bytes.Buffer{}, srv.URL, "tok", "Managed Solution", trackgate.MaxOverrideMinutes+1, "", true); err == nil {
		t.Error("too many minutes accepted")
	}
	var out bytes.Buffer
	if err := requestTrackingOverride(&out, srv.URL, "tok", "Managed Solution", 20, "hotfix", true); err != nil {
		t.Fatalf("override: %v", err)
	}
	body, _ := created.Load().(map[string]any)
	if body["run_id"] != trackgate.OverrideRunID || body["cmdline"] != trackgate.OverrideCmdline("Managed Solution", 20) {
		t.Errorf("request body = %v", body)
	}
	if !strings.Contains(out.String(), "Approved") {
		t.Errorf("output: %s", out.String())
	}
}

func TestAgyWorkRepoRefusal(t *testing.T) {
	work, personal := trackingEnv(t)
	err := agyWorkRepoRefusal(work, routerIsWorkRepo)
	if err == nil || !strings.Contains(err.Error(), "claude --work") {
		t.Fatalf("agy in work repo: %v", err)
	}
	if err := agyWorkRepoRefusal(personal, routerIsWorkRepo); err != nil {
		t.Errorf("agy in personal repo refused: %v", err)
	}
}

func TestShellWrapperGuardsAgy(t *testing.T) {
	// The wrapper text is printed with fmt.Print; check the source of truth.
	src, err := os.ReadFile("init_cmd.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "agy() {")
	if i < 0 || !strings.Contains(s[i:i+200], `staypoint agy-guard "$PWD" || return 1`) {
		t.Error("agy() wrapper must call agy-guard before the --force branch")
	}
	// ai() never launches agy at all (router.GeminiCodeForbidden): it runs
	// Claude whatever the route says, so it has no agy branch to guard.
	j := strings.Index(s, "ai() {")
	k := strings.Index(s, "# claude [--work|--personal]")
	if j < 0 || k < j {
		t.Fatal("ai() wrapper not found")
	}
	if body := s[j:k]; strings.Contains(body, "command agy") || strings.Contains(body, ":-agy}") {
		t.Errorf("ai() must never launch or default to agy:\n%s", body)
	}
}

func TestInstallClaudePreToolHook(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(p, []byte(`{"model":"opus","hooks":{"Stop":[{"hooks":[{"type":"command","command":"x"}]}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := installClaudePreToolHook(p, "/bin/staypoint")
	if err != nil || !changed {
		t.Fatalf("install: changed=%v err=%v", changed, err)
	}
	data, _ := os.ReadFile(p)
	s := string(data)
	for _, want := range []string{`"model": "opus"`, `"Stop"`, `/bin/staypoint hook pre-tool --tracking-only`, claudePreToolMatcher} {
		if !strings.Contains(s, want) {
			t.Errorf("settings missing %q:\n%s", want, s)
		}
	}
	if changed, _ := installClaudePreToolHook(p, "/bin/staypoint"); changed {
		t.Error("second install changed the file")
	}
}

// agyHookOutput is what `staypoint hook pre-tool --format gemini` prints for
// raw: the deny JSON when the tracking gate blocks, else the agy allow JSON.
func agyHookOutput(t *testing.T, raw []byte, getenv func(string) string) map[string]string {
	t.Helper()
	client, out, blocked := runTrackingGate(raw, "gemini", getenv, productionTrackingGate())
	if client != trackgate.ClientGemini {
		t.Fatalf("client = %s, want gemini", client)
	}
	if !blocked {
		out = preToolAllowJSON(client)
	}
	var resp map[string]string
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("hook output %q: %v", out, err)
	}
	return resp
}

func agyCall(tool string, args map[string]any, workspace string) []byte {
	b, _ := json.Marshal(map[string]any{
		"toolCall":       map[string]any{"name": tool, "args": args},
		"conversationId": "conv-7c9",
		"workspacePaths": []string{workspace},
	})
	return b
}

// task-7c9df5b6: every agy tool call in a daemon run was denied with an empty
// reason because the hook printed {} and agy requires "decision". Reads and
// doc writes must come back {"decision":"allow"}; code writes deny with a
// reason naming the path; no deny ever has an empty reason.
func TestPreToolHook_AgyWorkRepoAllowsReadsAndDocs(t *testing.T) {
	work, _ := trackingEnv(t)
	if err := os.Mkdir(filepath.Join(work, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	daemon := func(k string) string {
		if k == "STAYPOINT_TASK_ID" {
			return "task-d8338ab9"
		}
		return ""
	}
	for name, raw := range map[string][]byte{
		"view_file":           agyCall("view_file", map[string]any{"AbsolutePath": filepath.Join(work, "app.py")}, work),
		"list_dir":            agyCall("list_dir", map[string]any{"DirectoryPath": work}, work),
		"run_command echo":    agyCall("run_command", map[string]any{"CommandLine": "echo hi", "Cwd": work}, work),
		"write README.md":     agyCall("write_to_file", map[string]any{"TargetFile": filepath.Join(work, "README.md"), "CodeContent": "# hi"}, work),
		"write docs/guide.md": agyCall("write_to_file", map[string]any{"TargetFile": filepath.Join(work, "docs", "guide.md"), "CodeContent": "x"}, work),
	} {
		if resp := agyHookOutput(t, raw, daemon); resp["decision"] != "allow" {
			t.Errorf("%s: got %v, want decision allow", name, resp)
		}
	}

	app := filepath.Join(work, "app.py")
	resp := agyHookOutput(t, agyCall("write_to_file", map[string]any{"TargetFile": app, "CodeContent": "x"}, work), daemon)
	if resp["decision"] != "deny" || !strings.HasPrefix(resp["reason"], "Gemini may not write code in work repos: "+app) {
		t.Errorf("write app.py: got %v", resp)
	}

	// Interactive (no task id, not attached): reads still pass; a doc write is
	// held by the tracking gate, with a reason.
	if resp := agyHookOutput(t, agyCall("view_file", map[string]any{"AbsolutePath": app}, work), noEnv); resp["decision"] != "allow" {
		t.Errorf("interactive view_file: got %v", resp)
	}
	resp = agyHookOutput(t, agyCall("write_to_file", map[string]any{"TargetFile": filepath.Join(work, "README.md")}, work), noEnv)
	if resp["decision"] != "deny" || strings.TrimSpace(resp["reason"]) == "" {
		t.Errorf("interactive README write: got %v", resp)
	}
}

func TestPreToolDenyNeverEmptyReason(t *testing.T) {
	for _, c := range []trackgate.Client{trackgate.ClientGemini, trackgate.ClientClaude} {
		out := preToolDeny(c, "  ")
		if !strings.Contains(out, "recorded no reason") {
			t.Errorf("%s deny with empty reason: %s", c, out)
		}
	}
	if got := preToolAllowJSON(trackgate.ClientClaude); got != "{}" {
		t.Errorf("claude allow = %s", got)
	}
}
