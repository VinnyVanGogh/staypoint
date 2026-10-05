package repoaccess_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/db"
	"github.com/VinnyVanGogh/staypoint/internal/repoaccess"
	"github.com/VinnyVanGogh/staypoint/internal/shipreview"
)

// mkfifo creates a named pipe. Opening it for reading blocks in open() until a
// writer appears, which is the same kernel-level hang a pending macOS privacy
// prompt causes for the daemon's children.
func mkfifo(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "blocked")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	return p
}

// catProbe opens the path in a child process: instant for a regular file,
// blocked in open() for a FIFO.
func catProbe(path string) *exec.Cmd { return exec.Command("/bin/cat", path) }

func TestProbe_AccessibleDirIsOK(t *testing.T) {
	r := repoaccess.Probe(context.Background(), t.TempDir(), 3*time.Second, nil)
	if r.Status != repoaccess.StatusOK {
		t.Fatalf("status = %q (%s), want ok", r.Status, r.Detail)
	}
	if r.PermissionNeeded() {
		t.Fatal("an accessible dir must not need permission")
	}
}

func TestProbe_MissingDirIsMissingNotPermission(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	start := time.Now()
	r := repoaccess.Probe(context.Background(), missing, 3*time.Second, nil)
	if r.Status != repoaccess.StatusMissing {
		t.Fatalf("status = %q (%s), want missing", r.Status, r.Detail)
	}
	if r.PermissionNeeded() {
		t.Fatal("a deleted repo dir is not a macOS permission problem")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("missing dir took %v; it should fail fast", time.Since(start))
	}
}

func TestProbe_UnreadableDirIsDenied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	r := repoaccess.Probe(context.Background(), dir, 3*time.Second, nil)
	if r.Status != repoaccess.StatusDenied {
		t.Fatalf("status = %q (%s), want denied", r.Status, r.Detail)
	}
	if !r.PermissionNeeded() {
		t.Fatal("a denied dir needs permission")
	}
}

// The regression: a child blocked in open() must not hang the caller. The
// probe returns at its deadline, reports a timeout, and kills the child.
func TestProbe_BlockedOpenTimesOutAndKillsChild(t *testing.T) {
	fifo := mkfifo(t)
	var child *exec.Cmd
	cmdFn := func(path string) *exec.Cmd {
		child = catProbe(path)
		return child
	}

	start := time.Now()
	r := repoaccess.Probe(context.Background(), fifo, 300*time.Millisecond, cmdFn)
	elapsed := time.Since(start)

	if r.Status != repoaccess.StatusTimeout {
		t.Fatalf("status = %q (%s), want timeout", r.Status, r.Detail)
	}
	if !r.PermissionNeeded() {
		t.Fatal("a hung open() must be reported as needing permission")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Probe took %v with a 300ms deadline; it hung with the child", elapsed)
	}
	if child == nil || child.Process == nil {
		t.Fatal("probe never started a child process")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		// Signal fails once the child has been killed and reaped. A child that
		// was killed but never waited on stays a zombie and keeps this loop going.
		if err := child.Process.Signal(syscall.Signal(0)); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child pid %d still alive after timeout", child.Process.Pid)
		}
		time.Sleep(20 * time.Millisecond)
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
	return append([]string(nil), r.notices...), append([]string(nil), r.published...)
}

func TestChecker_NotifiesBoardOncePerLostPath(t *testing.T) {
	okFile := filepath.Join(t.TempDir(), "ok")
	if err := os.WriteFile(okFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fifo := mkfifo(t)
	missing := filepath.Join(t.TempDir(), "gone")

	rec := &recorder{}
	c := &repoaccess.Checker{
		Timeout: 300 * time.Millisecond,
		Command: catProbe,
		Notify:  rec.notify,
		Publish: rec.publish,
	}

	if snap := c.Snapshot(); snap.Checked {
		t.Fatal("Snapshot reports checked before any check ran")
	}

	paths := []string{okFile, fifo, missing}
	c.Check(context.Background(), paths)

	notices, published := rec.snapshot()
	want := "staypointd can't access " + fifo + ": macOS permission needed"
	if len(notices) != 1 || notices[0] != want {
		t.Fatalf("notices = %q, want exactly [%q]", notices, want)
	}
	if len(published) != 1 || published[0] != "repo_access_lost "+fifo {
		t.Fatalf("published = %q, want [repo_access_lost %s]", published, fifo)
	}

	snap := c.Snapshot()
	if !snap.Checked {
		t.Fatal("Snapshot.Checked = false after Check")
	}
	byPath := map[string]repoaccess.Result{}
	for _, r := range snap.Inaccessible {
		byPath[r.Path] = r
	}
	if len(byPath) != 2 {
		t.Fatalf("inaccessible = %+v, want the FIFO and the missing path", snap.Inaccessible)
	}
	if byPath[fifo].Status != repoaccess.StatusTimeout {
		t.Fatalf("fifo status = %q, want timeout", byPath[fifo].Status)
	}
	if byPath[missing].Status != repoaccess.StatusMissing {
		t.Fatalf("missing status = %q, want missing", byPath[missing].Status)
	}

	// A periodic recheck while the path is still blocked must not re-notify.
	c.Check(context.Background(), paths)
	notices, published = rec.snapshot()
	if len(notices) != 1 || len(published) != 1 {
		t.Fatalf("recheck re-notified: notices=%q published=%q", notices, published)
	}

	// Access comes back: the Board hears it was restored and health clears.
	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fifo, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c.Check(context.Background(), paths)
	notices, published = rec.snapshot()
	if len(notices) != 1 {
		t.Fatalf("restore sent a permission notice: %q", notices)
	}
	if len(published) != 2 || published[1] != "repo_access_restored "+fifo {
		t.Fatalf("published = %q, want repo_access_restored for %s", published, fifo)
	}
	for _, r := range c.Snapshot().Inaccessible {
		if r.Path == fifo {
			t.Fatalf("restored path still listed as inaccessible: %+v", r)
		}
	}
}

func TestChecker_ManyBlockedPathsCheckInParallel(t *testing.T) {
	var paths []string
	for i := 0; i < 6; i++ {
		paths = append(paths, mkfifo(t))
	}
	c := &repoaccess.Checker{Timeout: 300 * time.Millisecond, Command: catProbe}
	start := time.Now()
	c.Check(context.Background(), paths)
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("6 blocked paths took %v; probes must run in parallel", elapsed)
	}
	if n := len(c.Snapshot().Inaccessible); n != 6 {
		t.Fatalf("inaccessible = %d, want 6", n)
	}
}

func TestRepoPaths_DevConfigsPlusActiveTasks(t *testing.T) {
	store, err := db.Open(filepath.Join(t.TempDir(), "sp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	d := store.DB()

	if err := shipreview.UpsertProjectDevConfig(d, &shipreview.ProjectDevConfig{RepoPath: "/repos/dev"}); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, repo, status string }{
		{"t1", "/repos/active", "active"},
		{"t2", "/repos/active", "active"},
		{"t3", "/repos/dev", "active"},
		{"t4", "/repos/done", "done"},
		{"t5", "/repos/deleted", "soft_deleted"},
		{"t6", "", "active"},
	} {
		if _, err := d.Exec(`INSERT INTO tasks (id, name, repo_path, status) VALUES (?, ?, ?, ?)`,
			row.id, row.id, row.repo, row.status); err != nil {
			t.Fatal(err)
		}
	}

	got, err := repoaccess.RepoPaths(d, "/repos/harness", "", "/repos/active")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/repos/active", "/repos/dev", "/repos/harness"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("RepoPaths = %q, want %q", got, want)
	}
}
