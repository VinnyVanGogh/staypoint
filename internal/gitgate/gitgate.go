// Package gitgate provides hard git-state guards that run before and after
// every autonomous agent execution. Guards shell out to git so they work in
// any checkout; they never require an external library.
package gitgate

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
)

// Result is returned by every gate function.
type Result struct {
	// OK is true only when the gate passes with no blocking conditions.
	OK bool
	// Ahead is how many local commits are not yet on upstream.
	Ahead int
	// Behind is how many upstream commits are not yet local.
	Behind int
	// MissingFromMain lists SHAs that are not in origin/main (used by PostFlight
	// and MainContains only).
	MissingFromMain []string
	// Conflicts lists the files a PreFlight merge of upstream conflicted on.
	// The merge is aborted, so the branch is left as it was.
	Conflicts []string
	// Details is a human-readable summary of all checked conditions.
	Details []string
	// Errors lists blocking error descriptions.
	Errors []string
}

func (r *Result) addErr(msg string) {
	r.Errors = append(r.Errors, msg)
	r.Details = append(r.Details, "FAIL: "+msg)
	r.OK = false
}

func (r *Result) addInfo(msg string) {
	r.Details = append(r.Details, "ok:   "+msg)
}

// git runs a git command in dir with a 30 s timeout and returns trimmed stdout.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := gitexec.Command(ctx, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w (stderr: %s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// gitNoFail runs git and returns empty string on error (for optional checks).
func gitNoFail(ctx context.Context, dir string, args ...string) string {
	out, _ := git(ctx, dir, args...)
	return out
}

// PreFlight runs before every agent wake/run:
//  1. git fetch --all --prune
//  2. Fails if the worktree is dirty (uncommitted tracked changes or staged files)
//  3. If the branch is behind upstream, fast-forwards it, or merges upstream
//     when the branch has commits of its own; fails (merge aborted) on conflict
//  4. Reports ahead/behind counts
//
// repo is the worktree directory. branch is the local branch name (e.g. "main").
func PreFlight(ctx context.Context, repo, branch string) (*Result, error) {
	r := &Result{OK: true}

	// 1. Fetch
	if _, err := git(ctx, repo, "fetch", "--all", "--prune"); err != nil {
		r.addErr(fmt.Sprintf("fetch failed: %v", err))
		return r, nil
	}
	r.addInfo("fetch --all --prune: ok")

	// 2. Dirty check (tracked files only; untracked files are ignored)
	porcelain, err := git(ctx, repo, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		r.addErr(fmt.Sprintf("status check failed: %v", err))
		return r, nil
	}
	if porcelain != "" {
		r.addErr(fmt.Sprintf("worktree is dirty (uncommitted changes):\n%s", porcelain))
		return r, nil
	}
	r.addInfo("worktree clean")

	// 3. Ahead/behind
	upstream := "origin/" + branch
	revList, err := git(ctx, repo, "rev-list", "--left-right", "--count", "HEAD..."+upstream)
	if err != nil {
		// Upstream may not exist (new branch); treat as 0 behind.
		r.addInfo(fmt.Sprintf("no upstream tracking for %s (new branch)", branch))
		return r, nil
	}
	parts := strings.Fields(revList)
	if len(parts) == 2 {
		fmt.Sscanf(parts[0], "%d", &r.Ahead)
		fmt.Sscanf(parts[1], "%d", &r.Behind)
	}
	r.addInfo(fmt.Sprintf("ahead=%d behind=%d vs %s", r.Ahead, r.Behind, upstream))

	// 4. Catch up if behind: fast-forward a branch with no commits of its own,
	// merge into one that has them (a resumed task branch kept from an
	// earlier run). Merge, not rebase: the branch may already be pushed, and
	// rewriting it would need a force-push.
	if r.Behind > 0 {
		if r.Ahead == 0 {
			if _, err := git(ctx, repo, "merge", "--ff-only", upstream); err != nil {
				r.addErr(fmt.Sprintf("fast-forward from %s failed: %v", upstream, err))
				return r, nil
			}
			r.addInfo(fmt.Sprintf("fast-forwarded %d commit(s) from %s", r.Behind, upstream))
			return r, nil
		}
		msg := fmt.Sprintf("Merge %s into task branch (git pre-flight)", upstream)
		if _, err := git(ctx, repo, "merge", "--no-edit", "-m", msg, upstream); err != nil {
			conflicts := gitNoFail(ctx, repo, "diff", "--name-only", "--diff-filter=U")
			_, _ = git(ctx, repo, "merge", "--abort")
			if conflicts != "" {
				r.Conflicts = strings.Split(conflicts, "\n")
				r.addErr(fmt.Sprintf("branch has %d commit(s) of its own and is %d behind %s; merging %s conflicts in: %s (merge aborted, branch unchanged)",
					r.Ahead, r.Behind, upstream, upstream, strings.Join(r.Conflicts, ", ")))
			} else {
				r.addErr(fmt.Sprintf("merging %s into the branch failed (merge aborted, branch unchanged): %v", upstream, err))
			}
			return r, nil
		}
		r.addInfo(fmt.Sprintf("merged %d commit(s) from %s into a branch %d ahead", r.Behind, upstream, r.Ahead))
	}

	return r, nil
}

// PostFlight runs after every agent run or step:
//  1. Fails on uncommitted changes
//  2. Fails on unpushed commits (HEAD..@{u})
//  3. Reports whether the branch tip is merged into origin/main using both
//     ancestry (regular merges) and squash-equivalence (cherry + subject match).
func PostFlight(ctx context.Context, repo, branch string) (*Result, error) {
	r := &Result{OK: true}

	// 1. Dirty check
	porcelain, err := git(ctx, repo, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		r.addErr(fmt.Sprintf("status check failed: %v", err))
		return r, nil
	}
	if porcelain != "" {
		r.addErr(fmt.Sprintf("uncommitted changes at post-flight:\n%s", porcelain))
	} else {
		r.addInfo("no uncommitted changes")
	}

	// 2. Unpushed commits
	unpushed, err := git(ctx, repo, "log", "--oneline", "@{u}..HEAD")
	if err != nil {
		// No upstream tracking set. Check if the current branch has a same-named
		// remote ref (e.g. pushed via `git push origin HEAD` without -u).
		branchName := gitNoFail(ctx, repo, "rev-parse", "--abbrev-ref", "HEAD")
		remoteRef := "origin/" + branchName
		remoteExists := gitNoFail(ctx, repo, "rev-parse", "--verify", remoteRef) != ""
		if remoteExists {
			// Remote ref exists: compare against it to detect truly unpushed commits.
			unpushed = gitNoFail(ctx, repo, "log", "--oneline", remoteRef+"..HEAD")
			if unpushed != "" {
				r.addErr(fmt.Sprintf("unpushed commits (no upstream tracking, checked vs %s):\n%s", remoteRef, unpushed))
			} else {
				r.addInfo(fmt.Sprintf("no unpushed commits (no upstream tracking, checked vs %s)", remoteRef))
			}
		} else {
			// No remote ref at all — fall back to origin/main comparison.
			unpushed = gitNoFail(ctx, repo, "log", "--oneline", "origin/main..HEAD")
			if unpushed != "" {
				r.addErr(fmt.Sprintf("unpushed commits (no upstream, checked vs origin/main):\n%s", unpushed))
			} else {
				r.addInfo("no upstream tracking; branch tip is in origin/main")
			}
		}
	} else if unpushed != "" {
		r.addErr(fmt.Sprintf("unpushed commits:\n%s", unpushed))
	} else {
		r.addInfo("no unpushed commits")
	}

	// 3. Merged-into-main check
	headSHA, _ := git(ctx, repo, "rev-parse", "HEAD")
	missing := mainContains(ctx, repo, headSHA)
	if len(missing) == 0 {
		r.addInfo(fmt.Sprintf("branch tip %s is merged into origin/main", headSHA[:min(len(headSHA), 8)]))
	} else {
		r.MissingFromMain = missing
		r.addInfo(fmt.Sprintf("branch tip %s is NOT merged into origin/main (normal for in-flight work)", headSHA[:min(len(headSHA), 8)]))
	}

	return r, nil
}

// MainContains checks whether every SHA in shas is reachable from origin/main
// (regular ancestry) or squash-equivalent (subject match via git cherry).
// Returns the subset that is not in origin/main.
func MainContains(ctx context.Context, repo string, shas ...string) ([]string, error) {
	return mainContains(ctx, repo, shas...), nil
}

// mainContains is the internal implementation (no error return).
func mainContains(ctx context.Context, repo string, shas ...string) []string {
	var missing []string
	for _, sha := range shas {
		if sha == "" {
			continue
		}
		if isAncestor(ctx, repo, sha) {
			continue
		}
		if isSquashEquivalent(ctx, repo, sha) {
			continue
		}
		missing = append(missing, sha)
	}
	return missing
}

// isAncestor returns true when sha is a reachable ancestor of origin/main.
func isAncestor(ctx context.Context, repo, sha string) bool {
	// git merge-base --is-ancestor exits 0 if ancestor, 1 if not
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := gitexec.Command(ctx2, "merge-base", "--is-ancestor", sha, "origin/main")
	cmd.Dir = repo
	return cmd.Run() == nil
}

// isSquashEquivalent returns true when sha's commit subject matches a commit
// already in origin/main — the pattern used by GitHub's squash-merge.
//
// git cherry origin/main <branch-tip> lists commits in <branch-tip> not yet
// in origin/main. A "- " prefix means the commit IS equivalent (applied).
// We compare the one-line subject as a second gate when ancestry fails.
func isSquashEquivalent(ctx context.Context, repo, sha string) bool {
	// Get the commit subject we are looking for
	subject := gitNoFail(ctx, repo, "log", "--format=%s", "-1", sha)
	if subject == "" {
		return false
	}

	// Check if any commit in origin/main has the same subject
	// git log --format="%s" origin/main lists all subjects reachable from main
	// Limit to recent commits with --max-count to avoid huge log traversal
	mainSubjects := gitNoFail(ctx, repo, "log", "--format=%s", "--max-count=2000", "origin/main")
	for _, line := range strings.Split(mainSubjects, "\n") {
		if strings.TrimSpace(line) == subject {
			return true
		}
	}

	// Also check via git cherry: lines prefixed with "- " are equivalent
	cherryOut := gitNoFail(ctx, repo, "cherry", "origin/main", sha)
	for _, line := range strings.Split(cherryOut, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- ") {
			return true
		}
	}

	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
