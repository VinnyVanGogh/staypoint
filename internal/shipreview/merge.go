package shipreview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
)

// Approve's merge never touches the repo root checkout (task-2bdcdc74).
//
// The root checkout is shared with people and other agents. Merging there
// failed on a stale index.lock and on a stray uncommitted edit, and a
// conflicting merge left it mid-merge with conflict markers for everything
// that built from it. The merge commit is now built with plumbing (git
// merge-tree --write-tree, then commit-tree) and pushed by SHA, so no working
// tree or index is involved. Git older than 2.38 has no --write-tree; it
// merges in a throwaway detached worktree instead, removed afterwards.

// ErrMergeConflict is wrapped by *MergeConflictError.
var ErrMergeConflict = errors.New("merge conflict")

// MergeConflictError is returned by ApproveAndMerge when the reviewed head
// conflicts with the target branch. Nothing was committed or pushed.
type MergeConflictError struct {
	Target string
	Files  []string
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("branch conflicts with %s in %s", e.Target, strings.Join(e.Files, ", "))
}

func (e *MergeConflictError) Unwrap() error { return ErrMergeConflict }

// approveLocks serializes Approve merges per repo (keyed by git common dir):
// two cards merging into the same target at once would otherwise both build
// on the same base, and the second push would be rejected.
var approveLocks sync.Map

func lockApprove(ctx context.Context, repoDir string) (func(), error) {
	key := gitexec.CommonDir(repoDir)
	if key == "" {
		key = filepath.Clean(repoDir)
	}
	v, _ := approveLocks.LoadOrStore(key, make(chan struct{}, 1))
	ch := v.(chan struct{})
	select {
	case ch <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-ch }) }, nil
	case <-ctx.Done():
		return func() {}, fmt.Errorf("waiting for another Approve in %s: %w", repoDir, ctx.Err())
	}
}

// mergeTreeSupported reports whether git has merge-tree --write-tree. A var
// so tests can force the worktree fallback.
var mergeTreeSupported = func(ctx context.Context, repoDir string) bool {
	out, err := gitOutput(ctx, repoDir, "version")
	if err != nil {
		return false
	}
	return gitAtLeast(out, 2, 38)
}

var gitVersionRe = regexp.MustCompile(`(\d+)\.(\d+)`)

func gitAtLeast(versionOut string, major, minor int) bool {
	m := gitVersionRe.FindStringSubmatch(versionOut)
	if m == nil {
		return false
	}
	maj, _ := strconv.Atoi(m[1])
	mnr, _ := strconv.Atoi(m[2])
	return maj > major || (maj == major && mnr >= minor)
}

// mergeTarget resolves the commit to merge into: what the old checkout plus
// `pull --ff-only` would have produced. Whichever of the local target branch
// and origin's (just fetched) contains the other; diverged is an error, as
// the ff-only pull was. origin's is used even without upstream config: the
// local branch is no longer advanced while it is checked out, so it lags.
// localTip is the local branch tip, or "".
func mergeTarget(ctx context.Context, repoDir, target string) (base, localTip string, err error) {
	localTip, _ = gitOutput(ctx, repoDir, "rev-parse", "--verify", "-q", "refs/heads/"+target+"^{commit}")
	upstream, _ := gitOutput(ctx, repoDir, "rev-parse", "--verify", "-q", "refs/remotes/origin/"+target+"^{commit}")
	switch {
	case localTip == "" && upstream == "":
		return "", "", fmt.Errorf("target branch %s not found locally or on origin", target)
	case localTip == "":
		return upstream, "", nil
	case upstream == "" || upstream == localTip:
		return localTip, localTip, nil
	case verifyAncestor(ctx, repoDir, localTip, upstream) == nil:
		return upstream, localTip, nil
	case verifyAncestor(ctx, repoDir, upstream, localTip) == nil:
		return localTip, localTip, nil
	}
	return "", "", fmt.Errorf("pull %s: local %s and its upstream %s have diverged", target, localTip, upstream)
}

// buildMergeCommit creates the --no-ff merge of head into base without a
// working tree and returns its SHA. When base already contains head it
// returns base, as `git merge` would ("Already up to date").
func buildMergeCommit(ctx context.Context, repoDir, target, base, head, msg string) (string, error) {
	if verifyAncestor(ctx, repoDir, head, base) == nil {
		return base, nil
	}
	if !mergeTreeSupported(ctx, repoDir) {
		return mergeInScratchWorktree(ctx, repoDir, target, base, head, msg)
	}
	args := []string{"merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", base, head}
	cmd := gitexec.Command(ctx, args...) //nolint:gosec
	cmd.Dir = repoDir
	if env := gitEnvFrom(ctx); env != nil {
		cmd.Env = env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	fields := strings.Split(strings.TrimSuffix(stdout.String(), "\x00"), "\x00")
	// Exit 1 is a conflict: the tree OID, then each conflicted path.
	if code := cmd.ProcessState; runErr != nil && code != nil && code.ExitCode() == 1 && len(fields) > 1 {
		return "", &MergeConflictError{Target: target, Files: uniqueSorted(fields[1:])}
	}
	if runErr != nil {
		return "", fmt.Errorf("git %s: %w (stderr: %s)", strings.Join(args, " "), runErr, strings.TrimSpace(stderr.String()))
	}
	tree := fields[0]
	if tree == "" {
		return "", errors.New("git merge-tree printed no tree")
	}
	return gitOutput(ctx, repoDir, "commit-tree", tree, "-p", base, "-p", head, "-m", msg)
}

// mergeInScratchWorktree is the fallback for git without merge-tree
// --write-tree: merge in a detached worktree outside the repo, then remove
// it whatever happened.
func mergeInScratchWorktree(ctx context.Context, repoDir, target, base, head, msg string) (sha string, err error) {
	parent, err := os.MkdirTemp("", "staypoint-merge-")
	if err != nil {
		return "", err
	}
	wt := filepath.Join(parent, "wt")
	defer func() {
		// The request may have been cancelled; cleanup must still run.
		cctx := context.WithoutCancel(ctx)
		_, _ = gitOutput(cctx, repoDir, "worktree", "remove", "--force", wt)
		_ = os.RemoveAll(parent)
		_, _ = gitOutput(cctx, repoDir, "worktree", "prune")
	}()
	if _, err := gitOutput(ctx, repoDir, "worktree", "add", "--detach", wt, base); err != nil {
		return "", fmt.Errorf("create merge worktree: %w", err)
	}
	if _, mErr := gitOutput(ctx, wt, "merge", "--no-ff", "-m", msg, head); mErr != nil {
		out, _ := gitOutput(ctx, wt, "diff", "--name-only", "--diff-filter=U", "-z")
		if files := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00"); out != "" {
			return "", &MergeConflictError{Target: target, Files: uniqueSorted(files)}
		}
		return "", fmt.Errorf("merge: %w", mErr)
	}
	return gitOutput(ctx, wt, "rev-parse", "HEAD")
}

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// advanceLocalTarget moves the local target branch to merged after a
// successful push, but only when no worktree has it checked out (moving a
// checked-out branch would leave that checkout's index out of step with its
// HEAD) and only from the tip that was read before the merge. Best effort:
// origin already holds the merge.
func advanceLocalTarget(ctx context.Context, repoDir, target, localTip, merged string) {
	if localTip == "" || localTip == merged {
		return
	}
	if wt, err := worktreeWithBranch(ctx, repoDir, target); err != nil || wt != "" {
		return
	}
	if verifyAncestor(ctx, repoDir, localTip, merged) != nil {
		return
	}
	_, _ = gitOutput(ctx, repoDir, "update-ref", "--no-deref", "refs/heads/"+target, merged, localTip)
}
