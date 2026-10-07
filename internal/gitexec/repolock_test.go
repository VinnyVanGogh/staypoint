package gitexec

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLockLane(t *testing.T) {
	cases := map[string]string{
		"fetch origin":                 laneFetch,
		"-C /x fetch --all --prune":    laneFetch,
		"push origin main":             laneFetch,
		"remote update":                laneFetch,
		"worktree add /p b":            laneWorktree,
		"worktree prune":               laneWorktree,
		"worktree remove --force /p":   laneWorktree,
		"worktree list --porcelain":    "",
		"update-ref refs/x abc":        laneRefs,
		"branch -D x":                  laneRefs,
		"config --remove-section b.x":  laneRefs,
		"config user.name Me":          laneRefs,
		"config --get user.name":       "",
		"config user.name":             "",
		"remote get-url origin":        "",
		"remote add origin /o":         laneRefs,
		"rev-parse HEAD":               "",
		"status --porcelain":           "",
		"commit-tree abc -m x":         "",
		"-c core.x=1 update-ref r abc": laneRefs,
	}
	for in, want := range cases {
		if got := lockLane(strings.Fields(in)); got != want {
			t.Errorf("lockLane(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCommonDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	run := func(dir string, args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(repo, "init", "-b", "main")
	run(repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "--allow-empty", "-m", "init")
	wt := filepath.Join(root, "wt")
	run(repo, "worktree", "add", "--detach", wt)

	want, _ := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	for _, d := range []string{repo, filepath.Join(repo, "sub"), wt} {
		if got := CommonDir(d); got != want {
			t.Errorf("CommonDir(%s) = %q, want %q", d, got, want)
		}
	}
	if got := CommonDir(t.TempDir()); got != "" {
		t.Errorf("CommonDir(non-repo) = %q, want empty", got)
	}
}

// fakeGit puts a git on PATH that logs "<start|end> <subcommand> <dir>" and
// sleeps, so tests can see whether calls overlapped.
func fakeGit(t *testing.T) (logPath string) {
	t.Helper()
	bin := t.TempDir()
	logPath = filepath.Join(t.TempDir(), "git.log")
	script := "#!/bin/sh\n" +
		"echo \"start $1 $PWD\" >> " + logPath + "\n" +
		"sleep 0.5\n" +
		"echo \"end $1 $PWD\" >> " + logPath + "\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// fakeRepo makes a directory that CommonDir recognises as a repo.
func fakeRepo(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.Mkdir(filepath.Join(d, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

// maxOverlap returns the most calls running at once in a fake git log.
func maxOverlap(t *testing.T, logPath string) int {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cur, max := 0, 0
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		switch {
		case strings.HasPrefix(l, "start"):
			cur++
		case strings.HasPrefix(l, "end"):
			cur--
		}
		if cur > max {
			max = cur
		}
	}
	return max
}

func runConcurrently(t *testing.T, calls [][2]string) {
	t.Helper()
	var wg sync.WaitGroup
	for _, c := range calls {
		wg.Add(1)
		go func(dir, args string) {
			defer wg.Done()
			cmd := Command(context.Background(), strings.Fields(args)...)
			cmd.Dir = dir
			if err := cmd.Run(); err != nil {
				t.Errorf("git %s: %v", args, err)
			}
		}(c[0], c[1])
	}
	wg.Wait()
}

// Concurrent fetches in one repo run one at a time.
func TestRepoLock_SerializesSameRepoSameLane(t *testing.T) {
	logPath := fakeGit(t)
	repo := fakeRepo(t)
	before := RepoLockCount(repo, laneFetch)
	runConcurrently(t, [][2]string{{repo, "fetch origin"}, {repo, "fetch --all --prune"}, {repo, "pull --ff-only"}})
	if n := maxOverlap(t, logPath); n != 1 {
		t.Fatalf("%d fetches overlapped in one repo, want 1 at a time", n)
	}
	if got := RepoLockCount(repo, laneFetch) - before; got != 3 {
		t.Fatalf("fetch lock taken %d times, want 3", got)
	}
}

// Different repos, different lanes and reads never wait for each other.
func TestRepoLock_DoesNotSerializeOtherReposLanesOrReads(t *testing.T) {
	logPath := fakeGit(t)
	r1, r2 := fakeRepo(t), fakeRepo(t)
	runConcurrently(t, [][2]string{
		{r1, "fetch origin"}, {r2, "fetch origin"}, // other repo
		{r1, "update-ref refs/x abc"}, {r1, "worktree add /p"}, // other lanes
		{r1, "rev-parse HEAD"}, {r1, "status"}, // reads
	})
	if n := maxOverlap(t, logPath); n < 4 {
		t.Fatalf("only %d calls overlapped; unrelated calls must not wait", n)
	}
}

// A worktree and its main checkout share one lock.
func TestRepoLock_WorktreeSharesMainCheckoutLock(t *testing.T) {
	logPath := fakeGit(t)
	repo := fakeRepo(t)
	admin := filepath.Join(repo, ".git", "worktrees", "wt")
	if err := os.MkdirAll(admin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+admin+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if CommonDir(wt) != CommonDir(repo) {
		t.Fatalf("CommonDir(worktree) = %q, main = %q", CommonDir(wt), CommonDir(repo))
	}
	runConcurrently(t, [][2]string{{repo, "fetch origin"}, {wt, "fetch --all"}})
	if n := maxOverlap(t, logPath); n != 1 {
		t.Fatalf("worktree and main checkout fetched at once (%d)", n)
	}
}

// Waiting for the lock honours the caller's deadline.
func TestRepoLock_WaitHonoursDeadline(t *testing.T) {
	fakeGit(t)
	repo := fakeRepo(t)
	unlock, err := lockRepo(context.Background(), repo, []string{"fetch"})
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	cmd := Command(ctx, "fetch", "origin")
	cmd.Dir = repo
	err = cmd.Run()
	if !IsTimeout(err) || !strings.Contains(err.Error(), "fetch lock") {
		t.Fatalf("got %v, want a timeout naming the fetch lock", err)
	}
	// Output and CombinedOutput lock too.
	for _, f := range []func(*Cmd) error{
		func(c *Cmd) error { _, err := c.Output(); return err },
		func(c *Cmd) error { _, err := c.CombinedOutput(); return err },
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		c := Command(ctx, "fetch")
		c.Dir = repo
		if err := f(c); !IsTimeout(err) {
			t.Errorf("got %v, want lock timeout", err)
		}
		cancel()
	}
}
