// Package repoaccess checks that the daemon can still reach the repos it works
// in. Under launchd, macOS can withhold folder access (e.g. ~/Documents) until
// someone answers a privacy prompt. Until then, any child the daemon starts in
// that repo blocks inside open() rather than failing, so requests hang with no
// error. This package probes each repo in a short-lived child with a deadline
// and tells the Board which ones are blocked.
package repoaccess

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

// Status is the outcome of probing one repo path.
type Status string

const (
	StatusOK      Status = "ok"
	StatusTimeout Status = "timeout" // the probe hung: typically a pending macOS privacy prompt
	StatusDenied  Status = "denied"  // the OS refused access
	StatusMissing Status = "missing" // the path no longer exists
	StatusError   Status = "error"   // the probe itself failed or was cancelled; says nothing about the repo
)

// DefaultTimeout bounds each probe. A reachable repo answers in milliseconds.
const DefaultTimeout = 3 * time.Second

// maxParallel caps concurrent probe children.
const maxParallel = 8

// Result is the outcome of probing one path.
type Result struct {
	Path      string    `json:"path"`
	Status    Status    `json:"status"`
	Detail    string    `json:"detail,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// PermissionNeeded reports whether the Board has to grant access before the
// daemon can use this path again.
func (r Result) PermissionNeeded() bool {
	return r.Status == StatusTimeout || r.Status == StatusDenied
}

// Snapshot is the latest check, as reported by /api/health.
type Snapshot struct {
	Checked      bool       `json:"checked"`
	CheckedAt    *time.Time `json:"checked_at,omitempty"`
	Inaccessible []Result   `json:"inaccessible"`
}

// CommandFunc builds the child process that probes path.
type CommandFunc func(path string) *exec.Cmd

// probeEnv carries the path to a probe child. See RunProbeChild.
const probeEnv = "STAYPOINT_REPO_PROBE_PATH"

// Probe child exit codes. Anything else means the probe itself failed.
const (
	exitMissing = 3
	exitDenied  = 4
)

// DefaultCommand re-runs the current binary as a probe child (RunProbeChild).
// The child classifies the failure by errno and reports it in its exit code,
// so the result does not depend on any shell's or locale's error wording.
// Everything happens after exec, so a hang is inside the child and the
// parent's deadline still applies. (Setting cmd.Dir instead would chdir
// before exec, where a hang would block Start.)
//
// The flag argument is only a guard: a binary without the RunProbeChild hook
// (e.g. a test binary) rejects the unknown flag and exits instead of running.
func DefaultCommand(path string) *exec.Cmd {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	cmd := exec.Command(exe, "-staypoint-repo-probe")
	cmd.Env = append(os.Environ(), probeEnv+"="+path, "LC_ALL=C")
	return cmd
}

// RunProbeChild turns this process into a probe child when DefaultCommand
// started it, and exits. Call it first thing in main (and in TestMain for
// packages whose tests use DefaultCommand). Otherwise it returns at once.
func RunProbeChild() {
	path, ok := os.LookupEnv(probeEnv)
	if !ok {
		return
	}
	os.Exit(probeChild(path))
}

// probeChild enters path and opens it, as git does on startup (chdir, then
// open "." to resolve the working directory).
func probeChild(path string) int {
	err := os.Chdir(path)
	if err == nil {
		_, err = os.ReadDir(".")
	}
	if err == nil {
		return 0
	}
	fmt.Fprintln(os.Stderr, err)
	switch {
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
		return exitMissing
	case errors.Is(err, fs.ErrPermission): // EACCES and EPERM; TCC denials are EPERM
		return exitDenied
	default:
		return 1
	}
}

// Message is the Board-facing text for a path that needs permission.
func Message(path string) string {
	return fmt.Sprintf("staypointd can't access %s: macOS permission needed", path)
}

// Probe checks path in a child process and returns within timeout even if the
// child is stuck in the kernel. A nil cmdFn uses DefaultCommand.
func Probe(ctx context.Context, path string, timeout time.Duration, cmdFn CommandFunc) Result {
	if cmdFn == nil {
		cmdFn = DefaultCommand
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	res := Result{Path: path, CheckedAt: time.Now().UTC()}

	cmd := cmdFn(path)
	var stderr bytes.Buffer
	cmd.Stdout = nil
	cmd.Stderr = &stderr
	// After a kill, stop waiting on the stderr pipe even if something still
	// holds it open.
	cmd.WaitDelay = time.Second
	ownProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		res.Status = StatusError
		res.Detail = "start probe: " + err.Error()
		return res
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err == nil {
			res.Status = StatusOK
			return res
		}
		res.Detail = strings.TrimSpace(stderr.String())
		if res.Detail == "" {
			res.Detail = err.Error()
		}
		var exitErr *exec.ExitError
		code := -1
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		}
		switch code {
		case exitMissing:
			res.Status = StatusMissing
		case exitDenied:
			res.Status = StatusDenied
		default:
			res.Status = StatusError
		}
		return res
	case <-ctx.Done():
		killProbe(cmd)
		res.Status = StatusError
		res.Detail = "check cancelled"
		return res
	case <-timer.C:
	}

	// Kill the stuck child. The Wait goroutine reaps it once the kernel lets
	// it die; the caller does not wait for that. If the child cannot die
	// (stuck uninterruptibly), that goroutine and the process leak until the
	// kernel releases it. TCC-blocked children observed so far die on SIGKILL.
	killProbe(cmd)
	res.Status = StatusTimeout
	res.Detail = fmt.Sprintf("no response within %s: %s", timeout, Message(path))
	return res
}

// Checker probes a set of repo paths and keeps the latest result per path.
// Board notices fire on transitions only, so periodic rechecks stay quiet
// while a path remains blocked.
type Checker struct {
	Timeout time.Duration
	Command CommandFunc
	// Notify raises a loud desktop notification (telemetry.SendNotification).
	Notify func(title, msg string)
	// Publish sends an SSE event to the Board UI (EventHub.Publish).
	Publish func(eventType string, data any)

	mu        sync.Mutex
	checked   bool
	checkedAt time.Time
	results   map[string]Result
}

// Check probes every path in parallel and returns the results in path order.
func (c *Checker) Check(ctx context.Context, paths []string) []Result {
	results := make([]Result, len(paths))
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = Probe(ctx, p, c.Timeout, c.Command)
		}(i, p)
	}
	wg.Wait()

	// A cancelled check (daemon shutdown) did not learn anything about the
	// repos; recording it would announce every in-flight probe as lost.
	if ctx.Err() != nil {
		return results
	}

	c.mu.Lock()
	prev := c.results
	next := make(map[string]Result, len(results))
	var lost, restored []Result
	for _, r := range results {
		next[r.Path] = r
		wasBlocked := prev[r.Path].PermissionNeeded()
		switch {
		case r.PermissionNeeded() && !wasBlocked:
			lost = append(lost, r)
		case !r.PermissionNeeded() && wasBlocked:
			restored = append(restored, r)
		}
	}
	c.results = next
	c.checked = true
	c.checkedAt = time.Now().UTC()
	c.mu.Unlock()

	for _, r := range lost {
		slog.Error("repo access lost", slog.String("path", r.Path), slog.String("status", string(r.Status)), slog.String("detail", r.Detail))
		if c.Notify != nil {
			c.Notify("[StayPoint] Repo access lost", Message(r.Path))
		}
		if c.Publish != nil {
			c.Publish("repo_access_lost", r)
		}
	}
	for _, r := range restored {
		slog.Info("repo access restored", slog.String("path", r.Path))
		if c.Publish != nil {
			c.Publish("repo_access_restored", r)
		}
	}
	return results
}

// Snapshot returns the paths that failed the latest check, sorted by path.
func (c *Checker) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := Snapshot{Checked: c.checked, Inaccessible: []Result{}}
	if c.checked {
		at := c.checkedAt
		snap.CheckedAt = &at
	}
	for _, r := range c.results {
		if r.Status != StatusOK {
			snap.Inaccessible = append(snap.Inaccessible, r)
		}
	}
	sort.Slice(snap.Inaccessible, func(i, j int) bool { return snap.Inaccessible[i].Path < snap.Inaccessible[j].Path })
	return snap
}

// Run checks once immediately, then every interval until ctx is done.
// paths is re-read before each check so new tasks and dev configs are covered.
func (c *Checker) Run(ctx context.Context, interval time.Duration, paths func() []string) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		c.Check(ctx, paths())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RepoPaths returns the distinct repo paths the daemon may run git or agents
// in: every project dev config, every active task's repo_path, and extra
// (e.g. the harness repo root). Empty values are dropped; output is sorted.
func RepoPaths(db *sql.DB, extra ...string) ([]string, error) {
	seen := map[string]bool{}
	add := func(p string) {
		if p = strings.TrimSpace(p); p != "" {
			seen[p] = true
		}
	}
	for _, p := range extra {
		add(p)
	}

	cfgs, err := shipreview.ListProjectDevConfigs(db)
	if err != nil {
		return nil, fmt.Errorf("list dev configs: %w", err)
	}
	for _, c := range cfgs {
		add(c.RepoPath)
	}

	rows, err := db.Query(`SELECT DISTINCT repo_path FROM tasks WHERE status = 'active'`)
	if err != nil {
		return nil, fmt.Errorf("list task repo paths: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		add(p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}
