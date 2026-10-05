package gitexec_test

// STA-685: a git child that never returns (e.g. blocked in open() waiting on a
// macOS privacy prompt) must not hang the daemon. Every call is bounded and the
// timeout comes back as a recognisable error.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
)

// fakeGit puts a `git` on PATH that runs script instead of real git.
func fakeGit(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// within fails the test if fn takes longer than limit.
func within(t *testing.T, limit time.Duration, fn func()) time.Duration {
	t.Helper()
	done := make(chan struct{})
	start := time.Now()
	go func() { defer close(done); fn() }()
	select {
	case <-done:
		return time.Since(start)
	case <-time.After(limit):
		t.Fatalf("git call still running after %s", limit)
		return 0
	}
}

func TestOutput_DefaultTimeoutWhenContextHasNoDeadline(t *testing.T) {
	// No `exec`, so sh is killed but its sleep child keeps stdout open. The call
	// must still return instead of waiting on the orphan's pipe.
	fakeGit(t, "sleep 30")
	t.Setenv(gitexec.TimeoutEnv, "300ms")
	repo := t.TempDir()

	var err error
	within(t, 5*time.Second, func() {
		cmd := gitexec.Command(context.Background(), "rev-parse", "--show-toplevel")
		cmd.Dir = repo
		_, err = cmd.Output()
	})
	if !errors.Is(err, gitexec.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if !gitexec.IsTimeout(err) {
		t.Errorf("IsTimeout(%v) = false", err)
	}
	msg := err.Error()
	for _, want := range []string{"rev-parse --show-toplevel", repo, "macOS"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

func TestRun_CallerDeadlineWins(t *testing.T) {
	fakeGit(t, "exec sleep 30")
	t.Setenv(gitexec.TimeoutEnv, "1m")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	var err error
	within(t, 5*time.Second, func() { err = gitexec.Command(ctx, "status").Run() })
	if !errors.Is(err, gitexec.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
}

func TestRun_CancelledContextIsNotTimeout(t *testing.T) {
	fakeGit(t, "exec sleep 30")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	var err error
	within(t, 5*time.Second, func() { err = gitexec.Command(ctx, "status").Run() })
	if err == nil || gitexec.IsTimeout(err) {
		t.Fatalf("err = %v, want non-timeout error", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestCombinedOutput_Success(t *testing.T) {
	fakeGit(t, `echo "out $*"; echo err >&2`)
	out, err := gitexec.Command(context.Background(), "log", "-1").CombinedOutput()
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := string(out); !strings.Contains(got, "out log -1") || !strings.Contains(got, "err") {
		t.Errorf("output = %q", got)
	}
}

func TestOutput_ExitErrorPassesThrough(t *testing.T) {
	fakeGit(t, "exit 3")
	_, err := gitexec.Command(context.Background(), "rev-parse", "HEAD").Output()
	var exitErr interface{ ExitCode() int }
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 {
		t.Fatalf("err = %v, want exit status 3", err)
	}
	if gitexec.IsTimeout(err) {
		t.Errorf("exit error reported as timeout")
	}
}

func TestTimeoutEnv_InvalidFallsBackToDefault(t *testing.T) {
	t.Setenv(gitexec.TimeoutEnv, "banana")
	if got := gitexec.Timeout(); got != gitexec.DefaultTimeout {
		t.Errorf("Timeout() = %s, want %s", got, gitexec.DefaultTimeout)
	}
	t.Setenv(gitexec.TimeoutEnv, "2s")
	if got := gitexec.Timeout(); got != 2*time.Second {
		t.Errorf("Timeout() = %s, want 2s", got)
	}
}

// STA-710: one short timeout for every git call made `worktree remove` of a
// node_modules-sized tree time out. Reads stay quick; calls that hit the
// network or a whole tree get the slow timeout.
func TestTimeoutFor_PerOperation(t *testing.T) {
	t.Setenv(gitexec.TimeoutEnv, "")
	t.Setenv(gitexec.SlowTimeoutEnv, "")
	fast, slow := gitexec.DefaultTimeout, gitexec.DefaultSlowTimeout
	for _, tc := range []struct {
		args []string
		want time.Duration
	}{
		{[]string{"rev-parse", "HEAD"}, fast},
		{[]string{"-C", "/repo", "rev-parse", "--show-toplevel"}, fast},
		{[]string{"worktree", "prune"}, fast},
		{[]string{"worktree", "list", "--porcelain"}, fast},
		{[]string{"merge-base", "--is-ancestor", "a", "b"}, fast},
		{[]string{"worktree", "add", "--detach", "/wt", "abc"}, slow},
		{[]string{"worktree", "remove", "--force", "/wt"}, slow},
		{[]string{"-C", "/repo", "worktree", "add", "/wt"}, slow},
		{[]string{"fetch", "--all", "--prune"}, slow},
		{[]string{"push", "origin", "main"}, slow},
		{[]string{"-c", "user.name=x", "merge", "--no-ff", "b"}, slow},
		{[]string{"ls-remote", "--heads", "origin"}, slow},
		{nil, fast},
	} {
		if got := gitexec.TimeoutFor(tc.args...); got != tc.want {
			t.Errorf("TimeoutFor(%q) = %s, want %s", tc.args, got, tc.want)
		}
	}
	if slow <= fast {
		t.Fatalf("DefaultSlowTimeout %s should exceed DefaultTimeout %s", slow, fast)
	}
}

func TestSlowTimeoutEnv(t *testing.T) {
	t.Setenv(gitexec.SlowTimeoutEnv, "banana")
	if got := gitexec.SlowTimeout(); got != gitexec.DefaultSlowTimeout {
		t.Errorf("SlowTimeout() = %s, want %s", got, gitexec.DefaultSlowTimeout)
	}
	t.Setenv(gitexec.SlowTimeoutEnv, "3s")
	t.Setenv(gitexec.TimeoutEnv, "1s")
	if got := gitexec.TimeoutFor("push"); got != 3*time.Second {
		t.Errorf("TimeoutFor(push) = %s, want 3s", got)
	}
	if got := gitexec.TimeoutFor("status"); got != time.Second {
		t.Errorf("TimeoutFor(status) = %s, want 1s", got)
	}
}

func TestCommand_SlowOperationGetsSlowTimeout(t *testing.T) {
	// The fake git outlives the quick timeout but finishes well inside the
	// slow one, so only a per-operation deadline lets it succeed.
	fakeGit(t, "exec sleep 0.5")
	t.Setenv(gitexec.TimeoutEnv, "100ms")
	t.Setenv(gitexec.SlowTimeoutEnv, "10s")

	if err := gitexec.Command(context.Background(), "worktree", "add", "/wt", "HEAD").Run(); err != nil {
		t.Errorf("worktree add: %v, want success under the slow timeout", err)
	}
	if err := gitexec.Command(context.Background(), "rev-parse", "HEAD").Run(); !gitexec.IsTimeout(err) {
		t.Errorf("rev-parse: err = %v, want ErrTimeout under the quick timeout", err)
	}
}
