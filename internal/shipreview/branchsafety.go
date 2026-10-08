package shipreview

// Branch safety for registered branches and post-merge cleanup (#245).
//
// On a case-insensitive filesystem (the default macOS APFS volume) a loose
// ref is a file, so refs/heads/Main opens refs/heads/main: `git rev-parse
// refs/heads/Main` returns main's tip and `git update-ref -d refs/heads/Main`
// deletes main. Branch names are therefore compared to protected names with
// names.Normalize (case and lookalike folding), and a name is only trusted when `git for-each-ref` (which
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

	"github.com/VinnyVanGogh/staypoint/internal/names"
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
	// anywhere, so the task created and pushed it first.
	ProvenanceTask = "task"
	// ProvenanceForeign: the branch already existed when it was registered.
	// It may be someone else's; Approve never deletes it.
	ProvenanceForeign = "foreign"
)

// alwaysProtected are branch names that are never shipped as a registered
// branch nor deleted, compared through names.Normalize.
var alwaysProtected = []string{"main", "master", WorkTargetBranch, "HEAD"}

// protectedName reports whether branch equals, once both are normalised
// (case, whitespace, invisible characters, accents and lookalike letters:
// names.Normalize), any of the always-protected names or extra (the default,
// target and project target branches; "" entries are ignored). APFS treats
// differently normalised Unicode spellings of a ref as the same file, so the
// folding is wider than case alone.
func protectedName(branch string, extra ...string) bool {
	nb := names.Normalize(branch)
	for _, n := range append(append([]string(nil), alwaysProtected...), extra...) {
		if n != "" && nb == names.Normalize(n) {
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

// ErrBranchSymref is returned for a branch that is a symbolic ref, or whose
// loose ref file is reached through a filesystem symlink: deleting it would
// delete the ref it points to (git update-ref -d follows symrefs, and a
// symlinked directory makes refs/heads/loop/main the file refs/heads/main).
var ErrBranchSymref = fmt.Errorf("%w: branch is a symbolic ref or reached through a symlink", ErrProtectedBranch)

// requireExactRefs refuses a branch name that git resolves, as a local or
// origin branch, only through a ref of another name; a branch that is a
// symbolic ref; and a loose ref reached through a filesystem symlink.
func requireExactRefs(ctx context.Context, repoDir, branch string) error {
	heads, remote := "refs/heads/"+branch, "refs/remotes/origin/"+branch
	out, err := gitOutput(ctx, repoDir, "for-each-ref", "--format=%(refname) %(symref)", heads, remote)
	if err != nil {
		return fmt.Errorf("list refs for %q: %w", branch, err)
	}
	found := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		name, target, _ := strings.Cut(strings.TrimSpace(line), " ")
		if name != heads && name != remote {
			continue
		}
		found[name] = true
		if strings.TrimSpace(target) != "" {
			return fmt.Errorf("%w: %s -> %s", ErrBranchSymref, name, strings.TrimSpace(target))
		}
	}
	for _, ref := range []string{heads, remote} {
		if found[ref] {
			continue
		}
		if sha, err := gitOutput(ctx, repoDir, "rev-parse", "--verify", "--quiet", ref); err == nil && sha != "" {
			return fmt.Errorf("%w: %q resolves through another ref (case-insensitive filesystem alias?)", ErrBranchRefMismatch, ref)
		}
	}
	common, err := gitOutput(ctx, repoDir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("locate git dir: %w", err)
	}
	for _, ref := range []string{heads, remote} {
		if p := symlinkInRefPath(common, ref); p != "" {
			return fmt.Errorf("%w: %s (symlink at %s)", ErrBranchSymref, ref, p)
		}
	}
	return nil
}

// symlinkInRefPath returns the first path component of the loose ref file
// for ref under gitDir that is a symlink, or "" when there is none (or the
// ref has no loose file).
func symlinkInRefPath(gitDir, ref string) string {
	p := gitDir
	for _, comp := range strings.Split(ref, "/") {
		p = filepath.Join(p, comp)
		fi, err := os.Lstat(p)
		if err != nil {
			return ""
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return p
		}
	}
	return ""
}

// lsRemoteSymref reports whether ls-remote --symref output shows
// refs/heads/<branch> as a symbolic ref on the remote. Deleting a remote
// symref deletes the ref it points to.
func lsRemoteSymref(out, branch string) bool {
	want := "refs/heads/" + branch
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[0] == "ref:" && f[2] == want {
			return true
		}
	}
	return false
}

// remoteBranchTip asks origin for refs/heads/<branch> and returns its tip,
// or "" when origin has no such branch. A branch that is a symbolic ref on
// the remote is refused.
func remoteBranchTip(ctx context.Context, repoDir, branch string) (string, error) {
	out, err := gitOutput(ctx, repoDir, "ls-remote", "--symref", "origin", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	if lsRemoteSymref(out, branch) {
		return "", fmt.Errorf("%w: origin refs/heads/%s", ErrBranchSymref, branch)
	}
	return lsRemoteTip(out, branch), nil
}

// protectedNames are the branches whose refs branch cleanup must leave
// exactly as they were: main, master, dev-server, the default branch and the
// card's target.
func protectedNames(ctx context.Context, repoDir, target string) []string {
	seen := map[string]bool{}
	var names []string
	for _, n := range append([]string{"main", "master", WorkTargetBranch, target}, defaultBranchNames(ctx, repoDir)...) {
		if n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	return names
}

// localRefSnapshot maps each existing refs/heads/<n> and
// refs/remotes/origin/<n> for names to its object.
func localRefSnapshot(ctx context.Context, repoDir string, names []string) (map[string]string, error) {
	args := []string{"for-each-ref", "--format=%(refname) %(objectname)"}
	want := map[string]bool{}
	for _, n := range names {
		for _, r := range []string{"refs/heads/" + n, "refs/remotes/origin/" + n} {
			args = append(args, r)
			want[r] = true
		}
	}
	out, err := gitOutput(ctx, repoDir, args...)
	if err != nil {
		return nil, err
	}
	snap := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && want[f[0]] {
			snap[f[0]] = f[1]
		}
	}
	return snap, nil
}

// remoteRefSnapshot maps each refs/heads/<n> on origin for names to its object.
func remoteRefSnapshot(ctx context.Context, repoDir string, names []string) (map[string]string, error) {
	out, err := gitOutput(ctx, repoDir, "ls-remote", "--heads", "origin")
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, n := range names {
		want["refs/heads/"+n] = true
	}
	snap := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && want[f[1]] {
			snap[f[1]] = f[0]
		}
	}
	return snap, nil
}

// compareSnapshots reports every ref in before that is gone or moved in
// after. restore, when set, is called for each vanished ref.
func compareSnapshots(where string, before, after map[string]string, restore func(ref, sha string) error) error {
	var errs []error
	for ref, sha := range before {
		now, ok := after[ref]
		switch {
		case !ok && restore != nil:
			if err := restore(ref, sha); err != nil {
				errs = append(errs, fmt.Errorf("PROTECTED REF DELETED: %s %s (was %s) vanished during branch cleanup; restore failed: %v", where, ref, sha, err))
			} else {
				errs = append(errs, fmt.Errorf("PROTECTED REF DELETED: %s %s (was %s) vanished during branch cleanup; restored", where, ref, sha))
			}
		case !ok:
			errs = append(errs, fmt.Errorf("PROTECTED REF DELETED: %s %s (was %s) vanished during branch cleanup; restore it from %s", where, ref, sha, sha))
		case now != sha:
			errs = append(errs, fmt.Errorf("PROTECTED REF MOVED: %s %s moved from %s to %s during branch cleanup", where, ref, sha, now))
		}
	}
	return errors.Join(errs...)
}

// checkLocalProtected compares the protected local refs to before and puts
// back any that vanished.
func checkLocalProtected(ctx context.Context, repoDir string, names []string, before map[string]string) error {
	after, err := localRefSnapshot(ctx, repoDir, names)
	if err != nil {
		return fmt.Errorf("re-check protected refs: %w", err)
	}
	return compareSnapshots("local", before, after, func(ref, sha string) error {
		// Create only: never overwrite a ref that came back meanwhile.
		_, err := gitOutput(ctx, repoDir, "update-ref", "--no-deref", ref, sha, "")
		return err
	})
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
// The branch is the task's own (ProvenanceTask) only when it exists neither
// on origin nor locally: the task registered it first and creates it after.
// Anything else is foreign.
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
	// A branch that already exists locally is someone's: even checked out
	// in the task's own worktree it may predate the task (re-review F3).
	if !found["refs/heads/"+branch] {
		return ProvenanceTask, nil
	}
	return ProvenanceForeign, nil
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

// ErrUnmergedBranch is returned when a Reject would delete a branch holding
// commits that are not in the card's target and the Board did not confirm it
// by naming the branch.
var ErrUnmergedBranch = errors.New("branch has commits not in the target")

// RejectBranchDelete is a checked plan to delete a rejected card's branch on
// origin, leased to the tip that was checked.
type RejectBranchDelete struct {
	Branch    string
	RemoteTip string // "" when origin has no such branch: nothing to delete
	Target    string
}

// PlanRejectBranchDelete checks that Reject may delete card.Branch on
// origin: the task owns it (BranchOwnedByTask), it passes the same guards as
// post-merge cleanup (no protected, default or target name in any case, no
// alias, no symref), and its remote tip is contained in the target, unless
// confirmUnmerged names the branch exactly (an explicit Board override for
// discarding unmerged work). Nothing is deleted here.
func PlanRejectBranchDelete(ctx context.Context, db *sql.DB, repoDir string, card *Card, confirmUnmerged string) (*RejectBranchDelete, error) {
	branch := card.Branch
	if !BranchOwnedByTask(ctx, db, card.TaskID, branch) {
		return nil, fmt.Errorf("branch %s is %w; not deleted", branch, ErrBranchNotOwned)
	}
	target := card.TargetBranch
	if target == "" {
		t, err := TaskTargetBranch(ctx, db, repoDir, card.TaskID)
		if err != nil {
			return nil, fmt.Errorf("resolve target branch: %w; not deleted", err)
		}
		target = t
	}
	if err := guardDeletableBranch(ctx, repoDir, branch); err != nil {
		return nil, err
	}
	if protectedName(branch, target) {
		return nil, fmt.Errorf("%w: %q is the merge target", ErrProtectedBranch, branch)
	}
	if _, err := gitOutput(ctx, repoDir, "remote", "get-url", "origin"); err != nil {
		return &RejectBranchDelete{Branch: branch, Target: target}, nil
	}
	if _, err := gitOutput(ctx, repoDir, "fetch", "--prune", "origin"); err != nil {
		return nil, fmt.Errorf("fetch --prune origin: %w; not deleted", err)
	}
	tip, err := remoteBranchTip(ctx, repoDir, branch)
	if err != nil {
		return nil, fmt.Errorf("read remote branch %s: %w; not deleted", branch, err)
	}
	plan := &RejectBranchDelete{Branch: branch, RemoteTip: tip, Target: target}
	if tip == "" {
		return plan, nil
	}
	merged := false
	for _, t := range refTips(ctx, repoDir, target) {
		if verifyAncestor(ctx, repoDir, tip, t) == nil {
			merged = true
			break
		}
	}
	if !merged && confirmUnmerged != branch {
		return nil, fmt.Errorf("%w: %s (tip %s) is not in %s; to discard that work, confirm by naming the branch exactly", ErrUnmergedBranch, branch, tip, target)
	}
	return plan, nil
}

// DeleteRejectedBranch carries out a plan from PlanRejectBranchDelete: the
// remote delete is leased to the checked tip, the branch is re-checked just
// before, and protected remote refs are compared before and after.
func DeleteRejectedBranch(ctx context.Context, repoDir string, plan *RejectBranchDelete) error {
	if plan == nil || plan.RemoteTip == "" {
		return nil
	}
	tip, err := remoteBranchTip(ctx, repoDir, plan.Branch)
	if err != nil {
		return fmt.Errorf("read remote branch %s: %w; not deleted", plan.Branch, err)
	}
	if tip != plan.RemoteTip {
		return fmt.Errorf("remote branch %s moved from %s to %s since it was checked; not deleted", plan.Branch, plan.RemoteTip, tip)
	}
	protected := protectedNames(ctx, repoDir, plan.Target)
	before, err := remoteRefSnapshot(ctx, repoDir, protected)
	if err != nil {
		return fmt.Errorf("snapshot protected remote refs: %w; not deleted", err)
	}
	if err := deleteRemoteBranchLeased(ctx, repoDir, plan.Branch, plan.RemoteTip); err != nil {
		return err
	}
	after, err := remoteRefSnapshot(ctx, repoDir, protected)
	if err != nil {
		return fmt.Errorf("re-check protected remote refs: %w", err)
	}
	return compareSnapshots("origin", before, after, nil)
}
