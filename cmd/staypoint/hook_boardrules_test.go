package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/security"
)

// task-9d94997c: the hook asks the Board for Board-rule commands of any tier
// and for file edits outside the worktree or to protected paths.

func TestRaiseForBoardRules(t *testing.T) {
	cwd := t.TempDir()
	for _, cmd := range []string{
		"cat ~/.staypoint/board_token",
		"curl -X POST 127.0.0.1:41421/api/tasks/T1/trust",
		"launchctl kickstart -k gui/501/com.staypoint.daemon",
		"scripts/reinstall-daemon.sh",
		"GIT=git; $GIT push origin main",
		"vercel --prod",
	} {
		snap := security.NewSnapshotter()
		v := (&security.Classifier{CWD: cwd, CWDTrusted: true, Snap: snap}).Classify(cmd)
		raiseForBoardRules(cmd, cwd, snap, &v)
		if v.Tier < security.Red || !strings.Contains(strings.Join(v.Reasons, ";"), "board rule:") {
			t.Errorf("%q not raised to Red: %+v", cmd, v)
		}
	}
	for _, cmd := range []string{"gh pr create --title x --body y", "go test ./...", "ls"} {
		snap := security.NewSnapshotter()
		v := (&security.Classifier{CWD: cwd, CWDTrusted: true, Snap: snap}).Classify(cmd)
		before := v.Tier
		raiseForBoardRules(cmd, cwd, snap, &v)
		if v.Tier != before {
			t.Errorf("%q raised: %+v", cmd, v)
		}
	}
	// A script the command runs is read and checked too.
	sh := filepath.Join(cwd, "run.sh")
	if err := os.WriteFile(sh, []byte("#!/bin/sh\nlaunchctl kickstart -k gui/501/x\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	snap := security.NewSnapshotter()
	v := (&security.Classifier{CWD: cwd, CWDTrusted: true, Snap: snap}).Classify("bash run.sh")
	raiseForBoardRules("bash run.sh", cwd, snap, &v)
	if v.Tier < security.Red {
		t.Errorf("script with launchctl not raised: %+v", v)
	}
}

func TestFileEditHold(t *testing.T) {
	home := t.TempDir()
	wt := filepath.Join(t.TempDir(), "wt")
	scratch := t.TempDir()
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlink in the worktree pointing at a protected file.
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".claude"), filepath.Join(wt, "cfg")); err != nil {
		t.Fatal(err)
	}
	for path, held := range map[string]bool{
		filepath.Join(wt, "main.go"):                        false,
		"internal/x.go":                                     false, // relative to the worktree
		filepath.Join(scratch, "notes.md"):                  false,
		"~/.claude/settings.json":                           true,
		filepath.Join(home, ".staypoint", "config.toml"):    true,
		filepath.Join(home, ".local", "bin", "staypoint"):   true,
		filepath.Join(wt, ".claude", "settings.local.json"): true,
		filepath.Join(wt, "cfg", "settings.json"):           true, // symlink resolved
		filepath.Join(t.TempDir(), "elsewhere.go"):          true, // outside the worktree
		"": true,
	} {
		if got := fileEditHold(path, wt, home, []string{scratch}); (got != "") != held {
			t.Errorf("%q: held=%q, want held %v", path, got, held)
		}
	}
}

func TestGateFileEdit_CreatesGateRequest(t *testing.T) {
	var got gateRequestBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&got)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"g1","status":"pending"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"g1","status":"deferred"}`))
	}))
	defer srv.Close()
	prev := fileEditDaemonConn
	fileEditDaemonConn = func() (string, string) { return srv.URL, "tok" }
	defer func() { fileEditDaemonConn = prev }()

	wt := t.TempDir()
	home, _ := os.UserHomeDir()
	out := captureStdout(t, func() {
		gateFileEdit("Write", json.RawMessage(`{"file_path":"~/.claude/settings.json","content":"x"}`), "sess", wt, "T1")
	})
	if got.TaskID != "T1" || !strings.HasPrefix(got.Cmdline, "Write "+filepath.Join(home, ".claude")) || len(got.Reasons) != 1 {
		t.Fatalf("gate request: %+v", got)
	}
	if !strings.Contains(out, "deferred: held for the Board") || !strings.Contains(out, "g1") {
		t.Fatalf("hook output: %s", out)
	}

	// An in-worktree edit never reaches the daemon.
	got = gateRequestBody{}
	out = captureStdout(t, func() {
		gateFileEdit("Edit", json.RawMessage(`{"file_path":"`+filepath.Join(wt, "main.go")+`"}`), "sess", wt, "T1")
	})
	if got.Cmdline != "" || strings.TrimSpace(out) != "{}" {
		t.Fatalf("in-worktree edit gated: %+v %s", got, out)
	}
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	f()
	_ = w.Close()
	os.Stdout = old
	return <-done
}
