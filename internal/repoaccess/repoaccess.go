// Package repoaccess checks that the daemon can still use the repos it works
// in, and says exactly what is wrong when it can't.
//
// Each repo is probed step by step: stat the path, open and read it, then
// `git rev-parse --show-toplevel` and `git status --porcelain`. Every step has
// its own deadline, judged from outside the process doing the work, because a
// blocked open() never returns. The result names the first step that failed,
// how it failed and the raw error (errno or git's stderr), plus the daemon's
// uid, executable and the time of the check. There is no catch-all
// diagnosis: a timeout is reported as blocked with possible causes, not as a
// specific one.
package repoaccess

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

// Step is a probe step.
type Step string

const (
	StepStat        Step = "stat"
	StepOpen        Step = "open"
	StepGitRevParse Step = "git rev-parse"
	StepGitStatus   Step = "git status"
	StepProbe       Step = "probe" // the probe itself, not the repo
)

// Cause is what the failing step found.
type Cause string

const (
	CauseOK               Cause = "ok"
	CauseMissing          Cause = "missing"           // ENOENT: path missing or moved
	CauseNotDirectory     Cause = "not_directory"     // ENOTDIR, or not a directory
	CausePrivacy          Cause = "macos_privacy"     // EPERM: a macOS privacy (TCC) setting
	CauseUnixPermissions  Cause = "unix_permissions"  // EACCES: owner/mode vs the daemon's uid
	CauseBlocked          Cause = "blocked"           // the step never returned
	CauseNotGitRepo       Cause = "not_git_repo"      // git: not a git repository
	CauseDubiousOwnership Cause = "dubious_ownership" // git: safe.directory
	CauseIndexLock        Cause = "index_lock"        // git: index.lock exists
	CauseGitError         Cause = "git_error"         // other git failure; stderr shown verbatim
	CauseFSError          Cause = "fs_error"          // other errno; shown raw
	CauseProbeFailed      Cause = "probe_failed"      // the probe could not run; says nothing about the repo
)

// DefaultTimeout bounds each step. A reachable repo answers in milliseconds.
const DefaultTimeout = 3 * time.Second

// maxParallel caps concurrent probes.
const maxParallel = 8

// startupAllowance bounds how long the probe child may take to start and
// report its first step. Starting a process is not a repo step: macOS can
// take hundreds of milliseconds to launch a freshly built binary.
const startupAllowance = 10 * time.Second

var blockedCauses = []string{
	"macOS privacy prompt pending",
	"file provider (e.g. iCloud Drive) not responding",
	"network mount not responding",
}

// Target is a repo path to check, with a short name for messages.
type Target struct {
	Path  string `json:"path"`
	Label string `json:"label,omitempty"`
}

// Owner is the owner and mode of a path that returned EACCES.
type Owner struct {
	UID  int
	Mode fs.FileMode
}

// Result is the outcome of probing one repo.
type Result struct {
	Path           string    `json:"path"`
	Label          string    `json:"label,omitempty"`
	OK             bool      `json:"ok"`
	Step           Step      `json:"step,omitempty"`
	Cause          Cause     `json:"cause"`
	Errno          string    `json:"errno,omitempty"`
	ExitCode       int       `json:"exit_code,omitempty"`
	RawError       string    `json:"raw_error,omitempty"`
	OwnerUID       *int      `json:"owner_uid,omitempty"`
	Mode           string    `json:"mode,omitempty"`
	PossibleCauses []string  `json:"possible_causes,omitempty"`
	Message        string    `json:"message"`
	DaemonUID      int       `json:"daemon_uid"`
	Executable     string    `json:"executable"`
	CheckedAt      time.Time `json:"checked_at"`

	explanation string
	blockedFor  time.Duration
}

// Notifiable reports whether the Board should get a desktop alert for this
// result. Missing paths are reported in health and on the SSE stream but do
// not alert: active tasks keep stale repo paths, and every restart would
// re-alert for each one. A probe that could not run is not a repo verdict.
func (r Result) Notifiable() bool {
	return !r.OK && r.Cause != CauseMissing && r.Cause != CauseProbeFailed
}

// Options configures a probe.
type Options struct {
	// Timeout bounds each step. Zero means DefaultTimeout.
	Timeout time.Duration
	// Command builds the filesystem probe child. Nil means DefaultCommand.
	Command CommandFunc
	// Git is the git binary. Empty means "git" from PATH.
	Git string
}

func (o Options) withDefaults() Options {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Command == nil {
		o.Command = DefaultCommand
	}
	if o.Git == "" {
		o.Git = "git"
	}
	return o
}

// CommandFunc builds the child process that runs the filesystem steps.
type CommandFunc func(path string) *exec.Cmd

// probeEnv carries the path to a probe child; probeFlag must be its first
// argument. See RunProbeChild.
const (
	probeEnv  = "STAYPOINT_REPO_PROBE_PATH"
	probeFlag = "-staypoint-repo-probe"
)

// DefaultCommand re-runs the current binary as a probe child (RunProbeChild).
// The filesystem steps run after exec, so a hang is inside the child and the
// parent's deadline still applies. (Setting cmd.Dir instead would chdir
// before exec, where a hang would block Start.) A binary without the
// RunProbeChild hook (e.g. a test binary) rejects the flag and exits.
func DefaultCommand(path string) *exec.Cmd {
	cmd := exec.Command(executable(), probeFlag)
	cmd.Env = append(os.Environ(), probeEnv+"="+path, "LC_ALL=C")
	return cmd
}

// RunProbeChild turns this process into a probe child when DefaultCommand
// started it, and exits. Call it first thing in main (and in TestMain for
// packages whose tests probe). Otherwise it returns at once.
func RunProbeChild() {
	path, ok := os.LookupEnv(probeEnv)
	if !ok || len(os.Args) < 2 || os.Args[1] != probeFlag {
		return
	}
	probeChild(path, os.Stdout)
	os.Exit(0)
}

// childEvent is one line of the probe child's report: a step beginning, or
// its result.
type childEvent struct {
	Step     Step   `json:"step"`
	Event    string `json:"event"` // begin | ok | fail
	Errno    int    `json:"errno,omitempty"`
	Error    string `json:"error,omitempty"`
	OwnerUID *int   `json:"owner_uid,omitempty"`
	Mode     uint32 `json:"mode,omitempty"`
}

// probeChild stats the path, then opens it and reads one entry, reporting
// each step as a JSON line before and after it runs. If a call never
// returns, the last line the parent saw is that step's "begin".
func probeChild(path string, out io.Writer) {
	enc := json.NewEncoder(out)
	emit := func(e childEvent) { _ = enc.Encode(e) }
	fail := func(step Step, err error, owner *Owner) {
		e := childEvent{Step: step, Event: "fail", Error: err.Error()}
		var errno syscall.Errno
		if errors.As(err, &errno) {
			e.Errno = int(errno)
		}
		if owner != nil {
			uid := owner.UID
			e.OwnerUID = &uid
			e.Mode = uint32(owner.Mode)
		}
		emit(e)
	}

	emit(childEvent{Step: StepStat, Event: "begin"})
	fi, err := os.Stat(path)
	if err != nil {
		fail(StepStat, err, nil)
		return
	}
	if fi.Mode().IsRegular() {
		fail(StepStat, syscall.ENOTDIR, nil)
		return
	}
	emit(childEvent{Step: StepStat, Event: "ok"})

	owner := ownerOf(fi)
	emit(childEvent{Step: StepOpen, Event: "begin"})
	f, err := os.Open(path)
	if err == nil {
		_, err = f.ReadDir(1)
		if errors.Is(err, io.EOF) {
			err = nil
		}
		_ = f.Close()
	}
	if err != nil {
		fail(StepOpen, err, owner)
		return
	}
	emit(childEvent{Step: StepOpen, Event: "ok"})
}

// Probe checks one repo step by step and returns the first failure, or OK.
// It returns within about one step deadline per step even if a step hangs.
func Probe(ctx context.Context, t Target, o Options) Result {
	o = o.withDefaults()
	if r, ok := probeFS(ctx, t, o); !ok {
		return r
	}
	for _, g := range []struct {
		step Step
		args []string
	}{
		{StepGitRevParse, []string{"rev-parse", "--show-toplevel"}},
		{StepGitStatus, []string{"status", "--porcelain"}},
	} {
		if r, ok := runGit(ctx, t, o, g.step, g.args); !ok {
			return r
		}
	}
	return finish(Result{Path: t.Path, Label: t.Label, OK: true, Cause: CauseOK})
}

// childMsg is one message from the goroutine reading a probe child.
type childMsg struct {
	ev      *childEvent
	waitErr error
	exited  bool
}

func probeFS(ctx context.Context, t Target, o Options) (Result, bool) {
	cmd := o.Command(t.Path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second
	ownProcessGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return probeFailed(t, "probe setup: "+err.Error()), false
	}
	if err := cmd.Start(); err != nil {
		return probeFailed(t, "start probe: "+err.Error()), false
	}

	msgs := make(chan childMsg, 32)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var ev childEvent
			if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Step != "" {
				msgs <- childMsg{ev: &ev}
			}
		}
		msgs <- childMsg{exited: true, waitErr: cmd.Wait()}
	}()

	// The deadline covers one step at a time and restarts when the child
	// reports the next step. Before the first report it covers startup.
	deadline := time.NewTimer(max(o.Timeout, startupAllowance))
	defer deadline.Stop()
	var current Step
	for {
		select {
		case m := <-msgs:
			switch {
			case m.exited:
				detail := strings.TrimSpace(stderr.String())
				if detail == "" && m.waitErr != nil {
					detail = m.waitErr.Error()
				}
				return probeFailed(t, "probe exited without a result: "+detail), false
			case m.ev.Event == "begin":
				current = m.ev.Step
				deadline.Reset(o.Timeout)
			case m.ev.Event == "ok" && m.ev.Step == StepOpen:
				// The last filesystem step passed. The child exits on its
				// own and the reader goroutine reaps it.
				return Result{}, true
			case m.ev.Event == "fail":
				killProbe(cmd)
				var owner *Owner
				if m.ev.OwnerUID != nil {
					owner = &Owner{UID: *m.ev.OwnerUID, Mode: fs.FileMode(m.ev.Mode)}
				}
				var err error = errors.New(m.ev.Error)
				if m.ev.Errno != 0 {
					err = syscall.Errno(m.ev.Errno)
				}
				return FSFailure(t, m.ev.Step, err, owner), false
			}
		case <-ctx.Done():
			killProbe(cmd)
			return probeFailed(t, "check cancelled"), false
		case <-deadline.C:
			// Kill the stuck child. The reader goroutine reaps it once the
			// kernel lets it die; we do not wait for that. A child that
			// cannot die leaks until the kernel releases it.
			killProbe(cmd)
			if current == "" {
				return probeFailed(t, fmt.Sprintf("probe reported nothing within %s", max(o.Timeout, startupAllowance))), false
			}
			return blocked(t, current, fmt.Sprintf("%s(%s)", current, t.Path), o.Timeout), false
		}
	}
}

func runGit(ctx context.Context, t Target, o Options, step Step, args []string) (Result, bool) {
	cmd := exec.Command(o.Git, append([]string{"-C", t.Path}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second
	ownProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return probeFailed(t, "start git: "+err.Error()), false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(o.Timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err == nil {
			return Result{}, true
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return probeFailed(t, "git: "+err.Error()), false
		}
		return GitFailure(t, step, exitErr.ExitCode(), stderr.String()), false
	case <-ctx.Done():
		killProbe(cmd)
		return probeFailed(t, "check cancelled"), false
	case <-timer.C:
		killProbe(cmd)
		return blocked(t, step, gitCommand(step), o.Timeout), false
	}
}

// FSFailure classifies a failed filesystem step from its error. owner, when
// known, is reported for EACCES.
func FSFailure(t Target, step Step, err error, owner *Owner) Result {
	r := Result{Path: t.Path, Label: t.Label, Step: step, RawError: err.Error()}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		r.Cause = CauseFSError
		return finish(r)
	}
	r.Errno = errnoName(errno)
	r.RawError = errno.Error()
	switch errno {
	case syscall.ENOENT:
		r.Cause, r.explanation = CauseMissing, "path missing or moved"
	case syscall.ENOTDIR:
		r.Cause, r.explanation = CauseNotDirectory, "the configured repo path is not a directory"
	case syscall.EPERM:
		r.Cause, r.explanation = CausePrivacy, "macOS privacy setting is blocking staypointd"
	case syscall.EACCES:
		r.Cause = CauseUnixPermissions
		uid := os.Getuid()
		if owner != nil {
			ownerUID := owner.UID
			r.OwnerUID = &ownerUID
			r.Mode = owner.Mode.String()
			r.explanation = fmt.Sprintf("Unix file permissions: owner uid %d%s, mode %s; staypointd runs as uid %d%s",
				owner.UID, userName(owner.UID), r.Mode, uid, userName(uid))
		} else {
			r.explanation = fmt.Sprintf("Unix file permissions; staypointd runs as uid %d%s", uid, userName(uid))
		}
	default:
		r.Cause = CauseFSError
	}
	return finish(r)
}

// GitFailure classifies a failed git step from its exit code and stderr.
// The stderr is kept verbatim; known messages add an explanation.
func GitFailure(t Target, step Step, exitCode int, stderr string) Result {
	r := Result{Path: t.Path, Label: t.Label, Step: step, ExitCode: exitCode, RawError: strings.TrimSpace(stderr)}
	switch {
	case strings.Contains(stderr, "detected dubious ownership"):
		r.Cause, r.explanation = CauseDubiousOwnership, "git safe.directory: the repo is owned by a different user than staypointd"
	case strings.Contains(stderr, "index.lock"):
		r.Cause, r.explanation = CauseIndexLock, "index.lock exists: another git process is running, or one crashed and left the lock"
	case strings.Contains(stderr, "not a git repository"):
		r.Cause, r.explanation = CauseNotGitRepo, "the configured repo path is not inside a git repo"
	default:
		r.Cause = CauseGitError
	}
	return finish(r)
}

func blocked(t Target, step Step, call string, d time.Duration) Result {
	return finish(Result{
		Path: t.Path, Label: t.Label, Step: step, Cause: CauseBlocked,
		RawError:       call + " did not return",
		PossibleCauses: append([]string(nil), blockedCauses...),
		blockedFor:     d,
	})
}

func probeFailed(t Target, detail string) Result {
	return finish(Result{Path: t.Path, Label: t.Label, Step: StepProbe, Cause: CauseProbeFailed, RawError: detail})
}

func gitCommand(step Step) string {
	switch step {
	case StepGitRevParse:
		return "git rev-parse --show-toplevel"
	case StepGitStatus:
		return "git status --porcelain"
	}
	return string(step)
}

// finish stamps the daemon evidence and builds the Board-facing message from
// the step and the raw error.
func finish(r Result) Result {
	r.DaemonUID = os.Getuid()
	r.Executable = executable()
	r.CheckedAt = time.Now().UTC()

	var b strings.Builder
	if r.Label != "" {
		b.WriteString(r.Label + ": ")
	}
	switch {
	case r.OK:
		fmt.Fprintf(&b, "%s is readable and a working git repo", r.Path)
	case r.Step == StepProbe:
		fmt.Fprintf(&b, "couldn't check %s: %s", r.Path, r.RawError)
	case r.Step == StepGitRevParse || r.Step == StepGitStatus:
		fmt.Fprintf(&b, "git can't use %s: ", r.Path)
		if r.Cause == CauseBlocked {
			fmt.Fprintf(&b, "%s blocked for %s (possible causes: %s)", gitCommand(r.Step), r.blockedFor, strings.Join(r.PossibleCauses, ", "))
		} else {
			fmt.Fprintf(&b, "%s failed (exit %d): %s", gitCommand(r.Step), r.ExitCode, r.RawError)
		}
	default:
		fmt.Fprintf(&b, "can't read %s: ", r.Path)
		if r.Cause == CauseBlocked {
			fmt.Fprintf(&b, "%s(%s) blocked for %s (possible causes: %s)", r.Step, r.Path, r.blockedFor, strings.Join(r.PossibleCauses, ", "))
		} else if r.Errno != "" {
			fmt.Fprintf(&b, "%s(%s) failed: %s: %s", r.Step, r.Path, r.Errno, r.RawError)
		} else {
			fmt.Fprintf(&b, "%s(%s) failed: %s", r.Step, r.Path, r.RawError)
		}
	}
	if r.explanation != "" {
		fmt.Fprintf(&b, " (%s)", r.explanation)
	}
	if !r.OK {
		fmt.Fprintf(&b, " [step %s; staypointd uid %d %s; %s]", r.Step, r.DaemonUID, r.Executable, r.CheckedAt.Format(time.RFC3339))
	}
	r.Message = b.String()
	return r
}

func executable() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return os.Args[0]
}

func userName(uid int) string {
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil && u.Username != "" {
		return " (" + u.Username + ")"
	}
	return ""
}

// Snapshot is the latest check, as served by /api/health and /api/health/repos.
type Snapshot struct {
	Checked    bool       `json:"checked"`
	CheckedAt  *time.Time `json:"checked_at,omitempty"`
	DaemonUID  int        `json:"daemon_uid"`
	Executable string     `json:"executable"`
	Repos      []Result   `json:"repos"`
}

// Failing returns the repos whose latest result is not OK.
func (s Snapshot) Failing() []Result {
	out := []Result{}
	for _, r := range s.Repos {
		if !r.OK {
			out = append(out, r)
		}
	}
	return out
}

// Checker probes a set of repos and keeps the latest result per path.
// Board alerts fire when a repo's cause changes, so periodic rechecks stay
// quiet while a problem persists.
type Checker struct {
	Options
	// Notify raises a desktop notification (telemetry.SendNotification).
	Notify func(title, msg string)
	// Publish sends an SSE event to the Board UI (EventHub.Publish).
	Publish func(eventType string, data any)

	mu        sync.Mutex
	checked   bool
	checkedAt time.Time
	results   map[string]Result
}

// Check probes every target in parallel and returns the results in order.
func (c *Checker) Check(ctx context.Context, targets []Target) []Result {
	results := make([]Result, len(targets))
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t Target) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = Probe(ctx, t, c.Options)
		}(i, t)
	}
	wg.Wait()

	// A cancelled check (daemon shutdown) learned nothing about the repos.
	if ctx.Err() != nil {
		return results
	}

	c.mu.Lock()
	prev := c.results
	next := make(map[string]Result, len(results))
	var lost, restored []Result
	for _, r := range results {
		old, had := prev[r.Path]
		if r.Cause == CauseProbeFailed {
			// The probe could not run, so it says nothing about the repo.
			// Keep the previous verdict so this is neither a loss nor a
			// recovery (STA-692).
			slog.Warn("repo check: probe failed", slog.String("path", r.Path), slog.String("detail", r.RawError))
			if had {
				next[r.Path] = old
			} else {
				next[r.Path] = r
			}
			continue
		}
		next[r.Path] = r
		known := had && old.Cause != CauseProbeFailed
		switch {
		case !r.OK && (!known || old.Cause != r.Cause):
			lost = append(lost, r)
		case r.OK && known && !old.OK:
			restored = append(restored, r)
		}
	}
	c.results = next
	c.checked = true
	c.checkedAt = time.Now().UTC()
	c.mu.Unlock()

	for _, r := range lost {
		slog.Error("repo check failed", slog.String("path", r.Path), slog.String("cause", string(r.Cause)), slog.String("message", r.Message))
		if c.Notify != nil && r.Notifiable() {
			name := r.Label
			if name == "" {
				name = r.Path
			}
			c.Notify("[StayPoint] Repo check: "+name, r.Message)
		}
		if c.Publish != nil {
			c.Publish("repo_access_lost", r)
		}
	}
	for _, r := range restored {
		slog.Info("repo check recovered", slog.String("path", r.Path))
		if c.Publish != nil {
			c.Publish("repo_access_restored", r)
		}
	}
	return results
}

// Snapshot returns every repo's latest result, sorted by path.
func (c *Checker) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := Snapshot{Checked: c.checked, DaemonUID: os.Getuid(), Executable: executable(), Repos: []Result{}}
	if c.checked {
		at := c.checkedAt
		snap.CheckedAt = &at
	}
	for _, r := range c.results {
		snap.Repos = append(snap.Repos, r)
	}
	sort.Slice(snap.Repos, func(i, j int) bool { return snap.Repos[i].Path < snap.Repos[j].Path })
	return snap
}

// Run checks once immediately, then every interval until ctx is done.
// targets is re-read before each check so new tasks and dev configs are covered.
func (c *Checker) Run(ctx context.Context, interval time.Duration, targets func() []Target) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		c.Check(ctx, targets())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RepoTargets returns the distinct repos the daemon may run git or agents in:
// every project dev config, every active task's repo_path, and extra (e.g.
// the harness repo root). A repo's label is its project when its active tasks
// name exactly one project, otherwise the directory name. Sorted by path.
func RepoTargets(db *sql.DB, extra ...string) ([]Target, error) {
	projects := map[string]map[string]bool{}
	add := func(p string) {
		if p = strings.TrimSpace(p); p != "" && projects[p] == nil {
			projects[p] = map[string]bool{}
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

	rows, err := db.Query(`SELECT repo_path, COALESCE(project, '') FROM tasks WHERE status = 'active'`)
	if err != nil {
		return nil, fmt.Errorf("list task repo paths: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p, project string
		if err := rows.Scan(&p, &project); err != nil {
			return nil, err
		}
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		add(p)
		if project = strings.TrimSpace(project); project != "" {
			projects[p][project] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]Target, 0, len(projects))
	for p, names := range projects {
		label := filepath.Base(p)
		if len(names) == 1 {
			for n := range names {
				label = n
			}
		}
		out = append(out, Target{Path: p, Label: label})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
