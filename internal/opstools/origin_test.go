package opstools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Board review #2 H1: ops tools run only for the task whose harness-issued
// token checks out against the ops key.
func TestRunToken(t *testing.T) {
	dir := t.TempDir()
	if err := VerifyRunToken(dir, "task-a", "anything"); err == nil {
		t.Fatal("verified with no key on disk")
	}
	tok, err := RunToken(dir, "task-a")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := RunToken(dir, "task-a"); again != tok {
		t.Fatal("second run got a different token: key was rewritten")
	}
	if err := VerifyRunToken(dir, "task-a", tok); err != nil {
		t.Fatalf("own token refused: %v", err)
	}
	for name, c := range map[string]struct{ task, tok string }{
		"other task's id":    {"task-b", tok},
		"no task id":         {"", tok},
		"no token":           {"task-a", ""},
		"tampered token":     {"task-a", tok[:len(tok)-1] + "0"},
		"token for prefix":   {"task-a-evil", tok},
		"whitespace task id": {"  ", tok},
	} {
		if err := VerifyRunToken(dir, c.task, c.tok); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
	if tb, _ := RunToken(dir, "task-b"); tb == tok {
		t.Fatal("two tasks share a token")
	}

	// A key others can read, or a key that is a symlink, is refused.
	key := filepath.Join(dir, opsKeyFile)
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRunToken(dir, "task-a", tok); err == nil || !strings.Contains(err.Error(), "only by its owner") {
		t.Fatalf("group/world-readable key: %v", err)
	}
	other := t.TempDir()
	if _, err := RunToken(other, "x"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, opsKeyFile), key); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRunToken(dir, "task-a", tok); err == nil {
		t.Fatal("symlinked key accepted")
	}
	if _, err := RunToken("relative/dir", "x"); err == nil {
		t.Fatal("relative data dir accepted")
	}
}

func TestRunTokenConcurrentFirstUse(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	toks := make([]string, 8)
	for i := range toks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			toks[i], _ = RunToken(dir, "task-a")
		}(i)
	}
	wg.Wait()
	for _, tk := range toks {
		if tk == "" || tk != toks[0] {
			t.Fatalf("concurrent first use disagreed: %q", toks)
		}
	}
}

// Board review #2 H1: GH_HOST / GH_CONFIG_DIR (and friends) must not reach
// gh, git, ssh or bash, and PATH is pinned.
func TestChildEnvScrubs(t *testing.T) {
	in := []string{
		"HOME=/Users/x", "USER=x", "SSH_AUTH_SOCK=/tmp/agent", "LANG=en_US.UTF-8",
		"GH_HOST=evil.example", "GH_CONFIG_DIR=/tmp/fakegh", "GH_REPO=evil/r", "GITHUB_TOKEN=t",
		"GIT_SSH_COMMAND=/tmp/x", "GIT_CONFIG_GLOBAL=/tmp/gc", "XDG_CONFIG_HOME=/tmp/xdg",
		"BASH_ENV=/tmp/rc", "ENV=/tmp/rc", "HTTPS_PROXY=http://evil:8080", "https_proxy=http://evil:8080",
		"SSL_CERT_FILE=/tmp/ca.pem", "DYLD_INSERT_LIBRARIES=/tmp/x.dylib", "LD_PRELOAD=/tmp/x.so",
		"BASH_FUNC_git%%=() { evil; }", "PATH=/tmp/bin:/usr/bin",
	}
	out := ChildEnv(in)
	got := strings.Join(out, "\n")
	for _, keep := range []string{"HOME=/Users/x", "USER=x", "SSH_AUTH_SOCK=/tmp/agent", "LANG=en_US.UTF-8", "PATH=" + TrustedPath} {
		if !strings.Contains(got, keep) {
			t.Errorf("dropped %q", keep)
		}
	}
	kept := map[string]string{}
	for _, kv := range out {
		name, val, _ := strings.Cut(kv, "=")
		if _, dup := kept[name]; dup {
			t.Errorf("%s set twice", name)
		}
		kept[name] = val
	}
	if kept["PATH"] != TrustedPath {
		t.Errorf("PATH = %q", kept["PATH"])
	}
	for _, kv := range in {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "HOME", "USER", "SSH_AUTH_SOCK", "LANG", "PATH":
			continue
		}
		if _, ok := kept[name]; ok {
			t.Errorf("kept %s", kv)
		}
	}
}

// A PATH the caller set cannot swap in its own ssh: ExecRunner resolves on
// TrustedPath.
func TestExecRunnerIgnoresCallerPATH(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\necho fake-ssh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	res := ExecRunner(context.Background(), Cmd{Name: "ssh", Args: []string{"-V"}})
	if strings.Contains(res.Output, "fake-ssh") {
		t.Fatalf("ran the caller's ssh: %q", res.Output)
	}
	if res := ExecRunner(context.Background(), Cmd{Name: "no-such-tool-xyz"}); res.Err == nil || !strings.Contains(res.Err.Error(), "not found on") {
		t.Fatalf("missing tool: %+v", res)
	}
}

// Board review #2 L2: dev_host_run returned only "exit 0" because stdout
// never reached the interleaved output. Both streams must be there, and
// ordinary lines must survive redaction.
func TestExecRunnerCapturesStdout(t *testing.T) {
	res := ExecRunner(context.Background(), Cmd{Name: "/bin/sh", Args: []string{"-c", "echo active; echo warn >&2"}})
	if !strings.Contains(res.Output, "active") || !strings.Contains(res.Output, "warn") {
		t.Fatalf("Output = %q", res.Output)
	}
	if res.Stdout != "active\n" {
		t.Fatalf("Stdout = %q", res.Stdout)
	}
	if got := res.Format(); !strings.Contains(got, "active") || !strings.HasPrefix(got, "exit 0\n") {
		t.Fatalf("Format = %q", got)
	}
	for _, line := range []string{
		"active",
		"Already up to date.",
		"abc1234 fix(billing): totals",
		"● mansol-web.service - Mansol web",
		"     Active: active (running) since Fri 2026-10-09 10:00:00 UTC",
		"152 static files copied to '/var/www/static'.",
		"HTTP 200",
	} {
		if got := Redact(line); got != line {
			t.Errorf("Redact(%q) = %q", line, got)
		}
	}
}
