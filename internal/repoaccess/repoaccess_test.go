package repoaccess_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/repoaccess"
	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

// The Board's rule (STA-685 comment 55b893a9): no hardcoded diagnosis. Every
// failure names the probe step that failed and carries the raw error.
const retiredMessage = "macOS permission needed"

// fastTimeout is the step deadline for tests that expect a step to hang, so
// they finish quickly. Tests that expect real steps to finish use the
// production deadline: a real git process can take longer than 300ms on a
// loaded machine.
const fastTimeout = 300 * time.Millisecond

func opts() repoaccess.Options { return repoaccess.Options{Timeout: repoaccess.DefaultTimeout} }

func hangOpts() repoaccess.Options { return repoaccess.Options{Timeout: fastTimeout} }

func target(p string) repoaccess.Target { return repoaccess.Target{Path: p, Label: "Proj"} }

// gitRepo returns a fresh git repository.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return dir
}

// nonRepoDir returns a directory git will not find a repository above.
func nonRepoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	return dir
}

// mkfifo creates a named pipe. Opening it for reading blocks in open() until a
// writer appears: a call that never returns, like the hang seen under launchd.
func mkfifo(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "blocked")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	return p
}

// assertCause checks the parts every failure must carry: the cause, the step,
// the path, the raw evidence, and no retired catch-all wording.
func assertCause(t *testing.T, r repoaccess.Result, cause repoaccess.Cause, step repoaccess.Step, wants ...string) {
	t.Helper()
	if r.Cause != cause || r.Step != step {
		t.Fatalf("cause/step = %q/%q, want %q/%q; message: %s", r.Cause, r.Step, cause, step, r.Message)
	}
	if r.OK {
		t.Fatalf("a %s failure reported OK", cause)
	}
	for _, w := range append([]string{r.Path, "Proj: "}, wants...) {
		if !strings.Contains(r.Message, w) {
			t.Errorf("message lacks %q:\n%s", w, r.Message)
		}
	}
	if strings.Contains(r.Message, retiredMessage) {
		t.Errorf("message uses the retired catch-all %q:\n%s", retiredMessage, r.Message)
	}
}

func TestProbe_GitRepoIsOK(t *testing.T) {
	r := repoaccess.Probe(context.Background(), target(gitRepo(t)), opts())
	if !r.OK || r.Cause != repoaccess.CauseOK {
		t.Fatalf("cause = %q, want ok; message: %s", r.Cause, r.Message)
	}
}

func TestProbe_MissingPath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gone")
	r := repoaccess.Probe(context.Background(), target(p), opts())
	assertCause(t, r, repoaccess.CauseMissing, repoaccess.StepStat,
		"stat(", "ENOENT", "no such file or directory", "missing or moved")
	if r.Errno != "ENOENT" {
		t.Errorf("errno = %q, want ENOENT", r.Errno)
	}
}

func TestProbe_PathThroughAFileIsNotADirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := repoaccess.Probe(context.Background(), target(filepath.Join(file, "sub")), opts())
	assertCause(t, r, repoaccess.CauseNotDirectory, repoaccess.StepStat, "ENOTDIR", "not a directory")
}

func TestProbe_RegularFileIsNotADirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := repoaccess.Probe(context.Background(), target(file), opts())
	if r.Cause != repoaccess.CauseNotDirectory {
		t.Fatalf("cause = %q, want not_directory; message: %s", r.Cause, r.Message)
	}
	if !strings.Contains(r.Message, "not a directory") {
		t.Errorf("message lacks the cause: %s", r.Message)
	}
}

// chmod 000 is Unix permissions (EACCES), not a macOS privacy block (EPERM).
func TestProbe_Chmod000IsUnixPermissions(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	r := repoaccess.Probe(context.Background(), target(dir), opts())
	assertCause(t, r, repoaccess.CauseUnixPermissions, repoaccess.StepOpen,
		"open(", "EACCES", "permission denied", "Unix file permissions",
		fmt.Sprintf("owner uid %d", os.Getuid()), "mode d---------",
		fmt.Sprintf("staypointd runs as uid %d", os.Getuid()))
	if strings.Contains(r.Message, "privacy") {
		t.Errorf("EACCES was described as a privacy block: %s", r.Message)
	}
}

// A call that never returns is reported as blocked, with possible causes, and
// is not diagnosed as a privacy block.
func TestProbe_FIFOBlockedOpenIsBlockedNotDiagnosed(t *testing.T) {
	fifo := mkfifo(t)
	var child *exec.Cmd
	o := hangOpts()
	o.Command = func(p string) *exec.Cmd {
		child = repoaccess.DefaultCommand(p)
		return child
	}

	start := time.Now()
	r := repoaccess.Probe(context.Background(), target(fifo), o)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Probe took %v with a %v deadline; it hung with the child", elapsed, fastTimeout)
	}
	assertCause(t, r, repoaccess.CauseBlocked, repoaccess.StepOpen,
		"open("+fifo+") blocked for 300ms", "possible causes",
		"macOS privacy prompt pending", "file provider", "network mount")
	if strings.Contains(r.Message, "EPERM") || strings.Contains(r.Message, "is blocking staypointd") {
		t.Errorf("a timeout was given a single diagnosis: %s", r.Message)
	}
	if len(r.PossibleCauses) < 3 {
		t.Errorf("possible causes = %q, want at least 3", r.PossibleCauses)
	}

	if child == nil || child.Process == nil {
		t.Fatal("probe never started a child")
	}
	deadline := time.Now().Add(2 * time.Second)
	for child.Process.Signal(syscall.Signal(0)) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("child pid %d still alive after the timeout", child.Process.Pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestProbe_NonRepoDir(t *testing.T) {
	r := repoaccess.Probe(context.Background(), target(nonRepoDir(t)), opts())
	assertCause(t, r, repoaccess.CauseNotGitRepo, repoaccess.StepGitRevParse,
		"git rev-parse --show-toplevel", "fatal: not a git repository")
	if !strings.Contains(r.RawError, "fatal: not a git repository") {
		t.Errorf("raw error is not git's stderr verbatim: %q", r.RawError)
	}
}

func TestProbe_DubiousOwnershipRepo(t *testing.T) {
	repo := gitRepo(t)
	// Non-root tests cannot create a repo owned by another uid; git's own test
	// hook makes it treat the repo that way.
	t.Setenv("GIT_TEST_ASSUME_DIFFERENT_OWNER", "1")
	r := repoaccess.Probe(context.Background(), target(repo), opts())
	assertCause(t, r, repoaccess.CauseDubiousOwnership, repoaccess.StepGitRevParse,
		"detected dubious ownership", "safe.directory")
}

func TestProbe_HungGitIsBlocked(t *testing.T) {
	fakeGit := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	o := hangOpts()
	o.Git = fakeGit
	start := time.Now()
	r := repoaccess.Probe(context.Background(), target(t.TempDir()), o)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("hung git held the probe for %v", elapsed)
	}
	assertCause(t, r, repoaccess.CauseBlocked, repoaccess.StepGitRevParse,
		"git rev-parse --show-toplevel blocked for 300ms", "possible causes")
}

// Every message carries the evidence the Board needs to act without digging:
// the daemon's uid, its executable and the time of the check.
func TestProbe_MessageCarriesDaemonEvidence(t *testing.T) {
	r := repoaccess.Probe(context.Background(), target(filepath.Join(t.TempDir(), "gone")), opts())
	exe, _ := os.Executable()
	if r.DaemonUID != os.Getuid() || r.Executable != exe {
		t.Fatalf("daemon uid/exe = %d/%q, want %d/%q", r.DaemonUID, r.Executable, os.Getuid(), exe)
	}
	for _, w := range []string{
		fmt.Sprintf("uid %d", os.Getuid()),
		exe,
		r.CheckedAt.UTC().Format(time.RFC3339),
		"step stat",
	} {
		if !strings.Contains(r.Message, w) {
			t.Errorf("message lacks %q:\n%s", w, r.Message)
		}
	}
}

func TestProbe_StartFailureIsProbeFailedNotARepoVerdict(t *testing.T) {
	o := opts()
	o.Command = func(p string) *exec.Cmd { return exec.Command(filepath.Join(t.TempDir(), "no-such-binary")) }
	r := repoaccess.Probe(context.Background(), target(t.TempDir()), o)
	if r.Cause != repoaccess.CauseProbeFailed || r.Step != repoaccess.StepProbe {
		t.Fatalf("cause/step = %q/%q, want probe_failed/probe", r.Cause, r.Step)
	}
	if r.Notifiable() {
		t.Fatal("a probe that never ran must not alert the Board about the repo")
	}
	if !strings.Contains(r.Message, "no-such-binary") {
		t.Errorf("message lacks the raw start error: %s", r.Message)
	}
}

type recorder struct {
	mu        sync.Mutex
	notices   []string
	published []string
}

func (r *recorder) notify(title, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notices = append(r.notices, msg)
}

func (r *recorder) publish(eventType string, data any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	res, _ := data.(repoaccess.Result)
	r.published = append(r.published, eventType+" "+res.Path)
}

func (r *recorder) snapshot() ([]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := append([]string(nil), r.notices...)
	p := append([]string(nil), r.published...)
	sort.Strings(p)
	return n, p
}

func failingByPath(s repoaccess.Snapshot) map[string]repoaccess.Result {
	m := map[string]repoaccess.Result{}
	for _, r := range s.Failing() {
		m[r.Path] = r
	}
	return m
}

func TestChecker_AlertsOncePerProblemWithTheRealMessage(t *testing.T) {
	ok := gitRepo(t)
	fifo := mkfifo(t)
	missing := filepath.Join(t.TempDir(), "gone")
	targets := []repoaccess.Target{target(ok), target(fifo), target(missing)}

	rec := &recorder{}
	c := &repoaccess.Checker{Options: repoaccess.Options{Timeout: time.Second}, Notify: rec.notify, Publish: rec.publish}
	if c.Snapshot().Checked {
		t.Fatal("Snapshot reports checked before any check ran")
	}
	c.Check(context.Background(), targets)

	snap := c.Snapshot()
	if !snap.Checked || snap.CheckedAt == nil {
		t.Fatalf("snapshot after check = %+v", snap)
	}
	if len(snap.Repos) != 3 {
		t.Fatalf("snapshot has %d repos, want all 3 (ok ones included)", len(snap.Repos))
	}
	failing := failingByPath(snap)
	if len(failing) != 2 || failing[fifo].Cause != repoaccess.CauseBlocked || failing[missing].Cause != repoaccess.CauseMissing {
		t.Fatalf("failing = %+v", failing)
	}

	// Desktop alert: the blocked repo, with its real message. A missing path
	// (stale task rows) is reported but does not raise a desktop alert.
	notices, published := rec.snapshot()
	if len(notices) != 1 || notices[0] != failing[fifo].Message {
		t.Fatalf("notices = %q, want exactly the blocked repo's message %q", notices, failing[fifo].Message)
	}
	wantPub := []string{"repo_access_lost " + missing, "repo_access_lost " + fifo}
	sort.Strings(wantPub)
	if strings.Join(published, "|") != strings.Join(wantPub, "|") {
		t.Fatalf("published = %q, want %q", published, wantPub)
	}

	// Recheck with nothing changed: quiet.
	c.Check(context.Background(), targets)
	if n, p := rec.snapshot(); len(n) != 1 || len(p) != 2 {
		t.Fatalf("unchanged recheck re-alerted: notices=%q published=%q", n, p)
	}

	// The blocked path becomes a working repo: restored, and no longer failing.
	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", fifo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	c.Check(context.Background(), targets)
	notices, published = rec.snapshot()
	if len(notices) != 1 {
		t.Fatalf("restore raised an alert: %q", notices)
	}
	if !contains(published, "repo_access_restored "+fifo) {
		t.Fatalf("published = %q, want repo_access_restored for %s", published, fifo)
	}
	if _, still := failingByPath(c.Snapshot())[fifo]; still {
		t.Fatal("restored path still listed as failing")
	}
}

// A probe that could not run says nothing about the repo, so it must not read
// as "restored" and must not re-arm the alert (STA-692).
func TestChecker_ProbeFailureKeepsThePreviousVerdict(t *testing.T) {
	fifo := mkfifo(t)
	rec := &recorder{}
	c := &repoaccess.Checker{Options: hangOpts(), Notify: rec.notify, Publish: rec.publish}
	targets := []repoaccess.Target{target(fifo)}

	c.Check(context.Background(), targets)
	c.Command = func(p string) *exec.Cmd { return exec.Command(filepath.Join(t.TempDir(), "no-such-binary")) }
	c.Check(context.Background(), targets)
	c.Command = nil
	c.Check(context.Background(), targets)

	notices, published := rec.snapshot()
	if len(notices) != 1 {
		t.Fatalf("notices = %q, want the single original alert", notices)
	}
	if len(published) != 1 || published[0] != "repo_access_lost "+fifo {
		t.Fatalf("published = %q, want only the original repo_access_lost", published)
	}
	if got := failingByPath(c.Snapshot())[fifo].Cause; got != repoaccess.CauseBlocked {
		t.Fatalf("after a failed probe the verdict became %q, want blocked kept", got)
	}
}

// Daemon shutdown cancels ctx mid-check. Abandoned probes learned nothing.
func TestChecker_CancelledCheckRecordsNothing(t *testing.T) {
	rec := &recorder{}
	o := repoaccess.Options{Timeout: 3 * time.Second, Command: func(string) *exec.Cmd { return exec.Command("sleep", "5") }}
	c := &repoaccess.Checker{Options: o, Notify: rec.notify, Publish: rec.publish}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	start := time.Now()
	c.Check(ctx, []repoaccess.Target{target(t.TempDir()), target(t.TempDir())})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Check took %v after cancel", elapsed)
	}
	if n, p := rec.snapshot(); len(n) != 0 || len(p) != 0 {
		t.Fatalf("cancelled check alerted: notices=%q published=%q", n, p)
	}
	if s := c.Snapshot(); s.Checked || len(s.Repos) != 0 {
		t.Fatalf("cancelled check recorded results: %+v", s)
	}
}

func TestChecker_ManyBlockedPathsCheckInParallel(t *testing.T) {
	var targets []repoaccess.Target
	for i := 0; i < 6; i++ {
		targets = append(targets, target(mkfifo(t)))
	}
	c := &repoaccess.Checker{Options: hangOpts()}
	start := time.Now()
	c.Check(context.Background(), targets)
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("6 blocked paths took %v; probes must run in parallel", elapsed)
	}
	if n := len(c.Snapshot().Failing()); n != 6 {
		t.Fatalf("failing = %d, want 6", n)
	}
}

func TestRepoTargets_SourcesAndLabels(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "sp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	d := store.DB()

	if err := shipreview.UpsertProjectDevConfig(d, &shipreview.ProjectDevConfig{RepoPath: "/repos/dev"}); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, repo, status, project string }{
		{"t1", "/repos/rhizome_site", "active", "rhizome-site"},
		{"t2", "/repos/rhizome_site", "active", "rhizome-site"},
		{"t3", "/repos/shared", "active", "Alpha"},
		{"t4", "/repos/shared", "active", "Beta"},
		{"t5", "/repos/dev", "active", ""},
		{"t6", "/repos/done", "done", "Gamma"},
		{"t7", "/repos/deleted", "soft_deleted", "Delta"},
		{"t8", "", "active", "Empty"},
	} {
		if _, err := d.Exec(`INSERT INTO tasks (id, name, repo_path, status, project) VALUES (?, ?, ?, ?, ?)`,
			row.id, row.id, row.repo, row.status, row.project); err != nil {
			t.Fatal(err)
		}
	}

	got, err := repoaccess.RepoTargets(d, "/repos/harness", "", "/repos/shared")
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for _, tg := range got {
		parts = append(parts, tg.Path+"="+tg.Label)
	}
	want := []string{
		"/repos/dev=dev",                   // dev config, no single project
		"/repos/harness=harness",           // extra
		"/repos/rhizome_site=rhizome-site", // one project
		"/repos/shared=shared",             // several projects: fall back to the dir name
	}
	if strings.Join(parts, ",") != strings.Join(want, ",") {
		t.Fatalf("RepoTargets = %q, want %q", parts, want)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
