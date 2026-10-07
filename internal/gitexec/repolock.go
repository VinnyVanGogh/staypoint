package gitexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Per-repo git locks (STA-867).
//
// Several agent runs can now work in the same repo at once, each in its own
// task worktree. Worktrees share one git common dir: refs, packed-refs,
// config and the .git/worktrees admin area. Git guards each of those with a
// lock file and fails ("cannot lock ref", "Unable to create
// packed-refs.lock") instead of waiting when two writers meet, so two runs
// that fetch or write refs at the same moment break each other: a failed
// preflight fetch blocks the run.
//
// Command takes a per-repo lock around each daemon git call that writes that
// shared state, for the length of that one call. Calls are grouped into lanes
// so a slow fetch never holds up a quick ref write:
//
//   - laneFetch: fetch, pull, push and `remote update` (network, remote-tracking refs)
//   - laneWorktree: worktree add/remove/move/repair/prune (.git/worktrees)
//   - laneRefs: update-ref, branch, tag, pack-refs, gc, repack, prune,
//     config and remote writes (refs, packed-refs, .git/config)
//
// Everything else (reads, and commands that only touch one worktree's index
// and HEAD) runs unlocked. Agent CLIs run git themselves, outside this lock;
// git's own lock files still protect those.
const (
	laneFetch    = "fetch"
	laneWorktree = "worktree"
	laneRefs     = "refs"
)

// repoLocks maps "<common dir>\x00<lane>" to a one-slot channel used as a
// mutex whose wait can be cancelled.
var repoLocks sync.Map

// commonDirs caches worktree dir -> git common dir for dirs inside a repo.
var commonDirs sync.Map

// lockCounts counts lock acquisitions per key, for tests.
var lockCounts struct {
	sync.Mutex
	n map[string]int
}

func lockChan(key string) chan struct{} {
	ch, _ := repoLocks.LoadOrStore(key, make(chan struct{}, 1))
	return ch.(chan struct{})
}

// lockLane returns the lane git with args must hold, or "" for none.
func lockLane(args []string) string {
	sub, rest := subcommand(args)
	switch sub {
	case "fetch", "pull", "push":
		return laneFetch
	case "worktree":
		if len(rest) > 0 && rest[0] != "list" {
			return laneWorktree
		}
		return ""
	case "update-ref", "branch", "tag", "pack-refs", "gc", "repack", "prune":
		return laneRefs
	case "remote":
		if len(rest) == 0 {
			return ""
		}
		switch rest[0] {
		case "update":
			return laneFetch
		case "add", "remove", "rm", "rename", "set-url", "set-head", "set-branches", "prune":
			return laneRefs
		}
		return ""
	case "config":
		if configWrites(rest) {
			return laneRefs
		}
		return ""
	}
	return ""
}

// configWrites reports whether `git config <rest>` may write .git/config.
func configWrites(rest []string) bool {
	positional := 0
	for _, a := range rest {
		switch a {
		case "--unset", "--unset-all", "--add", "--replace-all", "--remove-section", "--rename-section", "set", "unset":
			return true
		case "--get", "--get-all", "--get-regexp", "--list", "-l", "get", "list":
			return false
		}
		if !strings.HasPrefix(a, "-") {
			positional++
		}
	}
	return positional >= 2 // `git config key value`
}

// gitDirArg returns the directory git with args runs in: -C (relative to dir)
// or dir, or the process cwd.
func gitDirArg(dir string, args []string) string {
	for i := 0; i < len(args)-1; i++ {
		a := args[i]
		if a == "-C" {
			c := args[i+1]
			if !filepath.IsAbs(c) && dir != "" {
				c = filepath.Join(dir, c)
			}
			dir = c
			i++
			continue
		}
		if a == "-c" || a == "--git-dir" || a == "--work-tree" || a == "--namespace" {
			i++
			continue
		}
		if !strings.HasPrefix(a, "-") {
			break
		}
	}
	if dir == "" {
		dir, _ = os.Getwd()
	}
	return dir
}

// CommonDir returns the git common dir of the repo containing dir (shared by
// all its worktrees), or "" when dir is not inside a git repo. It reads the
// .git file/dir directly instead of spawning git.
func CommonDir(dir string) string {
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	if v, ok := commonDirs.Load(abs); ok {
		return v.(string)
	}
	common := findCommonDir(abs)
	if common != "" {
		commonDirs.Store(abs, common)
	}
	return common
}

func findCommonDir(abs string) string {
	for d := abs; ; {
		dotGit := filepath.Join(d, ".git")
		if fi, err := os.Stat(dotGit); err == nil {
			gitDir := dotGit
			if !fi.IsDir() {
				raw, err := os.ReadFile(dotGit)
				if err != nil {
					return ""
				}
				line := strings.TrimSpace(string(raw))
				if !strings.HasPrefix(line, "gitdir:") {
					return ""
				}
				gitDir = strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
				if !filepath.IsAbs(gitDir) {
					gitDir = filepath.Join(d, gitDir)
				}
			}
			common := gitDir
			if raw, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
				c := strings.TrimSpace(string(raw))
				if !filepath.IsAbs(c) {
					c = filepath.Join(gitDir, c)
				}
				common = c
			}
			common = filepath.Clean(common)
			if r, err := filepath.EvalSymlinks(common); err == nil {
				common = r
			}
			return common
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// lockRepo takes the per-repo lane lock git with args needs, waiting no
// longer than ctx allows. The returned unlock is never nil.
func lockRepo(ctx context.Context, dir string, args []string) (func(), error) {
	lane := lockLane(args)
	if lane == "" {
		return func() {}, nil
	}
	common := CommonDir(gitDirArg(dir, args))
	if common == "" {
		return func() {}, nil
	}
	key := common + "\x00" + lane
	ch := lockChan(key)
	start := time.Now()
	select {
	case ch <- struct{}{}:
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return func() {}, fmt.Errorf("%w: git %s waited %s for the %s lock on %s",
				ErrTimeout, strings.Join(args, " "), time.Since(start).Round(time.Millisecond), lane, common)
		}
		return func() {}, fmt.Errorf("git %s: waiting for the %s lock on %s: %w", strings.Join(args, " "), lane, common, ctx.Err())
	}
	lockCounts.Lock()
	if lockCounts.n == nil {
		lockCounts.n = map[string]int{}
	}
	lockCounts.n[key]++
	lockCounts.Unlock()
	var once sync.Once
	return func() { once.Do(func() { <-ch }) }, nil
}

// RepoLockCount reports how many times the lock for lane ("fetch",
// "worktree" or "refs") of the repo containing dir has been taken. For tests.
func RepoLockCount(dir, lane string) int {
	key := CommonDir(dir) + "\x00" + lane
	lockCounts.Lock()
	defer lockCounts.Unlock()
	return lockCounts.n[key]
}
