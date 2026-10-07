package shipreview

// Branch safety for registered branches and post-merge cleanup (#245).
//
// On a case-insensitive filesystem (the default macOS APFS volume) a loose
// ref is a file, so refs/heads/Main opens refs/heads/main: `git rev-parse
// refs/heads/Main` returns main's tip and `git update-ref -d refs/heads/Main`
// deletes main. Branch names are therefore compared to protected names with
// strings.EqualFold, and a name is only trusted when `git for-each-ref` (which
// lists refs by their real names) reports exactly that ref.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/workspace"
)

// ErrBranchRefMismatch is returned for a branch name that git resolves only
// through another ref (a case-insensitive filesystem alias such as "Main" for
// main). Such a name is never shipped, resolved or deleted.
var ErrBranchRefMismatch = fmt.Errorf("%w: name does not match its ref exactly", ErrProtectedBranch)

// ErrBranchNotOwned is returned by CleanupTaskBranch for a branch the task
// neither got from StayPoint nor registered before it existed.
var ErrBranchNotOwned = errors.New("not owned by this task")

// Provenance of a registered branch work product (task_work_products.provenance).
const (
	// ProvenanceStayPoint: StayPoint created the branch (staypoint/<task>).
	ProvenanceStayPoint = "staypoint"
	// ProvenanceTask: the task registered the branch before it existed
	// anywhere (or while it lived only in the task's own worktree), so the
	// task created and pushed it first.
	ProvenanceTask = "task"
	// ProvenanceForeign: the branch already existed when it was registered.
	// It may be someone else's; Approve never deletes it.
	ProvenanceForeign = "foreign"
)

// alwaysProtected are branch names that are never shipped as a registered
// branch nor deleted, compared case-insensitively.
var alwaysProtected = []string{"main", "master", WorkTargetBranch, "HEAD"}

// protectedName reports whether branch equals, ignoring case, any of the
// always-protected names or extra (the default, target and project target
// branches; "" entries are ignored).
func protectedName(branch string, extra ...string) bool {
	for _, n := range append(append([]string(nil), alwaysProtected...), extra...) {
		if n != "" && strings.EqualFold(branch, n) {
			return true
		}
	}
	return false
}

// defaultBranchNames returns the remote default branch (origin/HEAD) and the
// default branch tasks are cut from; either may be "".
func defaultBranchNames(ctx context.Context, repoDir string) []string {
	var names []string
	if remoteRef, err := gitOutput(ctx, repoDir, "rev-parse", "--abbrev-ref", "origin/HEAD"); err == nil {
		if b := strings.TrimPrefix(remoteRef, "origin/"); b != "" && b != "origin/HEAD" {
			names = append(names, b)
		}
	}
	if b, err := workspace.DefaultBranchName(ctx, repoDir); err == nil && b != "" {
		names = append(names, b)
	}
	return names
}

// exactRefs returns which of refs/heads/<branch> and refs/remotes/origin/<branch>
// exist under exactly that name.
func exactRefs(ctx context.Context, repoDir, branch string) (map[string]bool, error) {
	heads, remote := "refs/heads/"+branch, "refs/remotes/origin/"+branch
	out, err := gitOutput(ctx, repoDir, "for-each-ref", "--format=%(refname)", heads, remote)
	if err != nil {
		return nil, err
	}
	return matchExactRefs(out, heads, remote), nil
}

// matchExactRefs picks the wanted refs out of for-each-ref output by exact
// (case-sensitive) comparison. for-each-ref patterns also match children
// (refs/heads/a matches refs/heads/a/b), which are not the branch.
func matchExactRefs(out string, want ...string) map[string]bool {
	found := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		for _, w := range want {
			if line == w {
				found[w] = true
			}
		}
	}
	return found
}

// requireExactRefs refuses a branch name that git resolves, as a local or
// origin branch, only through a ref of another name.
func requireExactRefs(ctx context.Context, repoDir, branch string) error {
	found, err := exactRefs(ctx, repoDir, branch)
	if err != nil {
		return fmt.Errorf("list refs for %q: %w", branch, err)
	}
	for _, ref := range []string{"refs/heads/" + branch, "refs/remotes/origin/" + branch} {
		if found[ref] {
			continue
		}
		if sha, err := gitOutput(ctx, repoDir, "rev-parse", "--verify", "--quiet", ref); err == nil && sha != "" {
			return fmt.Errorf("%w: %q resolves through another ref (case-insensitive filesystem alias?)", ErrBranchRefMismatch, ref)
		}
	}
	return nil
}

// refTips resolves each name as origin/<name> and refs/heads/<name>, by exact
// name only, and returns the commits found.
func refTips(ctx context.Context, repoDir string, names ...string) []string {
	var tips []string
	for _, n := range names {
		if n == "" {
			continue
		}
		found, err := exactRefs(ctx, repoDir, n)
		if err != nil {
			continue
		}
		for ref := range found {
			if sha, err := gitOutput(ctx, repoDir, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil && sha != "" {
				tips = append(tips, sha)
			}
		}
	}
	return tips
}

// checkRegisteredBranchName applies the name rules every registered branch
// must pass: a plain branch name (git check-ref-format --branch), not a
// StayPoint task branch, and not, in any letter case, a protected, default,
// target or project-target branch.
func checkRegisteredBranchName(ctx context.Context, db *sql.DB, repo, branch, target string) error {
	if err := ValidTargetBranch(branch); err != nil {
		return fmt.Errorf("registered branch %q cannot be reviewed: %w", branch, err)
	}
	if err := validateBranch(branch); err != nil {
		return fmt.Errorf("registered branch %q cannot be reviewed: %w", branch, err)
	}
	projectTarget, err := ProjectTargetBranch(ctx, db, repo)
	if err != nil {
		return err
	}
	names := append([]string{target, projectTarget}, defaultBranchNames(ctx, repo)...)
	if protectedName(branch, names...) {
		return fmt.Errorf("registered branch %q is a protected or merge-target branch: %w", branch, ErrProtectedBranch)
	}
	return nil
}

// ClassifyRegisteredBranch validates a branch an agent registers as a task's
// work product and returns its provenance. It refuses case variants of
// protected, default and target branches, names that resolve only through
// another ref, and malformed names (wrapping ErrInvalidBranch,
// ErrInvalidTargetBranch or ErrProtectedBranch).
//
// The branch is the task's own (ProvenanceTask) only when it does not exist
// on origin and either does not exist locally or is checked out only in the
// task's own worktree: the task created it. Anything else is foreign.
func ClassifyRegisteredBranch(ctx context.Context, db *sql.DB, repo, taskID, ref string) (string, error) {
	branch := strings.TrimPrefix(strings.TrimSpace(ref), "origin/")
	if branch == taskBranchPrefix+taskID {
		return ProvenanceStayPoint, nil
	}
	if _, err := gitOutput(ctx, repo, "rev-parse", "--git-dir"); repo == "" || err != nil {
		// No repo to check against (none recorded, or not a git checkout
		// here): the name rules still apply, and the branch is never the
		// task's to delete. A card cannot be built from it anyway until
		// the repo is readable, and then checkWorkProductBranch runs.
		if err := ValidTargetBranch(branch); err != nil {
			return "", err
		}
		if protectedName(branch) {
			return "", fmt.Errorf("%w: %q", ErrProtectedBranch, branch)
		}
		return ProvenanceForeign, nil
	}
	target, err := TaskTargetBranch(ctx, db, repo, taskID)
	if err != nil {
		target = ""
	}
	if err := checkRegisteredBranchName(ctx, db, repo, branch, target); err != nil {
		return "", err
	}
	if err := requireExactRefs(ctx, repo, branch); err != nil {
		return "", err
	}
	found, err := exactRefs(ctx, repo, branch)
	if err != nil {
		return ProvenanceForeign, nil
	}
	if found["refs/remotes/origin/"+branch] {
		return ProvenanceForeign, nil
	}
	if _, err := gitOutput(ctx, repo, "remote", "get-url", "origin"); err == nil {
		// Never prompt for credentials (the CLI registers from a terminal).
		lsCtx := ctx
		if gitEnvFrom(ctx) == nil {
			lsCtx = context.WithValue(ctx, gitEnvKey{}, append(os.Environ(), "GIT_TERMINAL_PROMPT=0"))
		}
		out, err := gitOutput(lsCtx, repo, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
		if err != nil {
			// Cannot tell: never claim ownership.
			return ProvenanceForeign, nil
		}
		if lsRemoteTip(out, branch) != "" {
			return ProvenanceForeign, nil
		}
	}
	if !found["refs/heads/"+branch] {
		return ProvenanceTask, nil
	}
	wt, err := worktreeWithBranch(ctx, repo, branch)
	if err == nil && wt != "" && samePath(wt, filepath.Join(repo, ".worktrees", taskID)) {
		return ProvenanceTask, nil
	}
	return ProvenanceForeign, nil
}

func samePath(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return false
	}
	ia, errA := os.Stat(ra)
	ib, errB := os.Stat(rb)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}

// lsRemoteTip returns the tip of exactly refs/heads/<branch> in ls-remote
// output, or "". ls-remote patterns match ref name tails, so other refs
// ending in the same path may be listed too.
func lsRemoteTip(out, branch string) string {
	want := "refs/heads/" + branch
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == want {
			return f[0]
		}
	}
	return ""
}

// BranchOwnedByTask reports whether Approve may delete branch for taskID:
// it is the task's own staypoint/<task> branch, or the first registration of
// that exact branch name by any task is this task's, with StayPoint or task
// provenance. Unknown (legacy) and foreign registrations are not owned.
func BranchOwnedByTask(ctx context.Context, db *sql.DB, taskID, branch string) bool {
	if taskID != "" && branch == taskBranchPrefix+taskID {
		return true
	}
	var owner, provenance string
	err := db.QueryRowContext(ctx, `
		SELECT task_id, provenance FROM task_work_products
		WHERE product_type = 'branch' AND reference IN (?, ?)
		ORDER BY created_at ASC, id ASC LIMIT 1`, branch, "origin/"+branch).Scan(&owner, &provenance)
	if err != nil {
		return false
	}
	return owner == taskID && (provenance == ProvenanceStayPoint || provenance == ProvenanceTask)
}

// CleanupTaskBranch is CleanupMergedBranch for a branch the task owns
// (BranchOwnedByTask). Any other branch is left alone: the merge stands and
// the returned error, shown on the card, says why nothing was deleted.
func CleanupTaskBranch(ctx context.Context, db *sql.DB, repoDir string, card *Card, mainSHA string) error {
	if !BranchOwnedByTask(ctx, db, card.TaskID, card.Branch) {
		return fmt.Errorf("branch %s is %w; not deleted", card.Branch, ErrBranchNotOwned)
	}
	return CleanupMergedBranch(ctx, repoDir, card, mainSHA)
}

// hexObjectRe matches an abbreviated or full object name.
var hexObjectRe = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
