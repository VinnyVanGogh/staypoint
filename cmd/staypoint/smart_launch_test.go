package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A bare `staypoint` must answer fast. It used to parse every Claude
// transcript on disk to find a "last used tool" the router never reads: ~8s
// of CPU before the first byte on a real HOME. The fixture HOME holds a FIFO
// named like a transcript, so any code that opens transcripts blocks forever
// and the timeout below fails the test.
func TestSmartLaunchDryRun_FastAndOneRoute(t *testing.T) {
	repoDir, homeDir := setupE2ETestRepo(t)

	projDir := filepath.Join(homeDir, ".claude", "projects", "-fixture")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(projDir, "never-read.jsonl"), 0644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, testBinaryPath, "--dry-run", "--no-ssh")
	cmd.Dir = repoDir
	cmd.Env = append(isolatedEnviron(os.Environ()), "HOME="+homeDir)
	cmd.Stdin = nil
	start := time.Now()
	raw, err := cmd.CombinedOutput()
	elapsed := time.Since(start)
	out := string(raw)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("no-arg dry run hung (opened a transcript?) after %v:\n%s", elapsed, out)
	}
	if err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, out)
	}
	t.Logf("no-arg dry run: %v", elapsed)
	if elapsed > 2*time.Second {
		t.Errorf("no-arg dry run took %v, want well under 2s", elapsed)
	}

	for _, want := range []string{
		"staypoint: routing...",
		"staypoint board",
		"claude-opus-5 (claude)", // banner plan line
		"Tool:        claude",
		"Model:       claude-opus-5",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "(agy)") {
		t.Errorf("banner routes to agy while the launch is claude:\n%s", out)
	}
	if strings.Index(out, "staypoint: routing...") > strings.Index(out, "[Staypoint :: Dry Run]") {
		t.Errorf("routing notice must print before the route result:\n%s", out)
	}
}

func TestRootHelp_NamesBoardTUI(t *testing.T) {
	if !strings.Contains(rootCmd.Long, "staypoint board") || !strings.Contains(rootCmd.Long, "smart launcher") {
		t.Fatalf("root --help must say no-args is the smart launcher and the TUI is `staypoint board`:\n%s", rootCmd.Long)
	}
}
