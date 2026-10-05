package osascript

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeOSAScript points binary at a shell script for the duration of the test.
func fakeOSAScript(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "osascript")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := binary
	binary = path
	t.Cleanup(func() { binary = old })
}

const denied = "execution error: No user interaction allowed. (-1713)"

func TestRun_FailureCarriesExitCodeAndStderr(t *testing.T) {
	fakeOSAScript(t, `echo "`+denied+`" >&2; exit 1`)

	err := Run(context.Background(), `display notification "x"`)
	if err == nil {
		t.Fatal("Run: want error from failing osascript, got nil")
	}
	var oe *Error
	if !errors.As(err, &oe) {
		t.Fatalf("Run: want *osascript.Error, got %T %v", err, err)
	}
	if oe.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", oe.ExitCode)
	}
	if oe.Stderr != denied {
		t.Errorf("Stderr = %q, want %q", oe.Stderr, denied)
	}
	if !strings.Contains(err.Error(), "exit 1") || !strings.Contains(err.Error(), denied) {
		t.Errorf("Error() = %q, want exit code and stderr", err.Error())
	}
}

func TestRun_Success(t *testing.T) {
	fakeOSAScript(t, `exit 0`)
	if err := Run(context.Background(), `display notification "x"`); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRun_MissingBinaryIsAnError(t *testing.T) {
	old := binary
	binary = filepath.Join(t.TempDir(), "no-such-osascript")
	t.Cleanup(func() { binary = old })

	if err := Run(context.Background(), `display notification "x"`); err == nil {
		t.Fatal("Run with missing osascript: want error, got nil")
	}
}

func TestStart_EarlyFailureIsReturned(t *testing.T) {
	fakeOSAScript(t, `echo "`+denied+`" >&2; exit 1`)

	err := Start(`display dialog "x"`, 5*time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), denied) {
		t.Fatalf("Start: want early failure with stderr, got %v", err)
	}
}

// A modal dialog keeps osascript running; Start must return once the grace
// period passes instead of blocking until the Board clicks OK.
func TestStart_StillRunningAfterGraceIsShown(t *testing.T) {
	fakeOSAScript(t, `sleep 3`)

	start := time.Now()
	if err := Start(`display dialog "x"`, 200*time.Millisecond, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Start blocked for %v; want it to return after the grace period", d)
	}
}

// A failure after the grace period is reported to the late callback, not dropped.
func TestStart_LateFailureGoesToCallback(t *testing.T) {
	fakeOSAScript(t, `sleep 0.5; echo "`+denied+`" >&2; exit 1`)

	late := make(chan error, 1)
	if err := Start(`display dialog "x"`, 100*time.Millisecond, func(err error) { late <- err }); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case err := <-late:
		if err == nil || !strings.Contains(err.Error(), denied) {
			t.Fatalf("late callback: want stderr, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late failure never reached the callback")
	}
}

func TestQuote(t *testing.T) {
	cases := map[string]string{
		`plain`:          `"plain"`,
		`say "hi"`:       `"say \"hi\""`,
		`back\slash`:     `"back\\slash"`,
		`"; do shell "x`: `"\"; do shell \"x"`,
	}
	for in, want := range cases {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}
