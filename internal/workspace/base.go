package workspace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// baseRefPrefix namespaces the pinned base commit of each task (STA-774).
const baseRefPrefix = "refs/staypoint/base/"

// BaseRef is the ref that pins the commit a task's worktree was branched from.
func BaseRef(taskID string) string { return baseRefPrefix + taskID }

// ErrNoTaskBase is returned when the daemon has no recorded base for a task:
// a task whose branch predates STA-774, one created without a DB, or one
// whose record was lost. There is no fallback: every git ref is writable by
// the agent, so a base guessed from git state is not a base review can trust.
var ErrNoTaskBase = errors.New("no recorded base commit for task")

// ErrTaskBaseTampered is returned when the git pin of a task's base no longer
// matches the base the daemon recorded at worktree creation. Callers must fail
// closed: no card, no Approve, no diff that could hide the task's changes.
var ErrTaskBaseTampered = errors.New("task base does not match the recorded base")

// ErrNoDefaultBranch is returned when a repo has no origin/HEAD, origin/main,
// origin/master, main or master to branch a task from. The user's checkout
// HEAD is never used instead.
var ErrNoDefaultBranch = errors.New("no default branch to base the task on")

// DefaultBranchRef returns the ref new task worktrees branch from, without
// fetching: origin/HEAD, then origin/main, then origin/master. The local main
// or master is used only when origin has none of those (no remote, or a
// remote that was never fetched or pushed to). It returns ErrNoDefaultBranch
// when there is none of them: the checkout's HEAD is never a task base.
func DefaultBranchRef(ctx context.Context, repo string) (string, error) {
	if hasRemote(ctx, repo, "origin") {
		if sym, err := runGit(ctx, repo, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil && sym != "" && commitOf(ctx, repo, sym) != "" {
			return sym, nil
		}
		for _, ref := range []string{"refs/remotes/origin/main", "refs/remotes/origin/master"} {
			if commitOf(ctx, repo, ref) != "" {
				return ref, nil
			}
		}
	}
	for _, ref := range []string{"refs/heads/main", "refs/heads/master"} {
		if commitOf(ctx, repo, ref) != "" {
			return ref, nil
		}
	}
	return "", fmt.Errorf("%w in %s", ErrNoDefaultBranch, repo)
}

// ErrNoTargetBranch is returned when a project's configured target branch
// exists neither on origin nor locally. The default branch is never used
// instead: work meant for the target would be cut from, and merged into, the
// wrong branch.
var ErrNoTargetBranch = errors.New("target branch not found")

// DefaultBranchName is DefaultBranchRef as a branch name ("main").
func DefaultBranchName(ctx context.Context, repo string) (string, error) {
	ref, err := DefaultBranchRef(ctx, repo)
	if err != nil {
		return "", err
	}
	return ShortBranch(ref), nil
}

// ShortBranch strips refs/heads/ or refs/remotes/origin/ from ref.
func ShortBranch(ref string) string {
	if b, ok := strings.CutPrefix(ref, "refs/remotes/origin/"); ok {
		return b
	}
	return strings.TrimPrefix(ref, "refs/heads/")
}

// TargetBranchRef returns the ref a task targeting branch is cut from:
// origin/<branch>, else the local branch. branch "" is the default branch.
func TargetBranchRef(ctx context.Context, repo, branch string) (string, error) {
	if branch == "" {
		return DefaultBranchRef(ctx, repo)
	}
	if strings.HasPrefix(branch, "-") || strings.HasPrefix(branch, "refs/") {
		return "", fmt.Errorf("%w: invalid branch name %q", ErrNoTargetBranch, branch)
	}
	for _, ref := range []string{"refs/remotes/origin/" + branch, "refs/heads/" + branch} {
		if commitOf(ctx, repo, ref) != "" {
			return ref, nil
		}
	}
	return "", fmt.Errorf("%w: %q in %s", ErrNoTargetBranch, branch, repo)
}

// fetchOrigin fetches origin, best effort: an offline machine still branches
// from the last fetched origin/<target>.
func fetchOrigin(ctx context.Context, repo string) {
	if hasRemote(ctx, repo, "origin") {
		_, _ = runGit(ctx, repo, "fetch", "origin")
	}
}

// resolveNewTaskBase returns the commit SHA of the target branch ("" for the
// default branch) and that branch's name. The caller fetches first.
func resolveNewTaskBase(ctx context.Context, repo, target string) (sha, branch string, err error) {
	ref, err := TargetBranchRef(ctx, repo, target)
	if err != nil {
		return "", "", err
	}
	sha = commitOf(ctx, repo, ref)
	if sha == "" {
		return "", "", fmt.Errorf("resolve task base %s: no commit", ref)
	}
	return sha, ShortBranch(ref), nil
}

// TaskBase returns the commit a task's changes are measured from. It is the
// single source of truth for the ship review "Files changed" list, the Approve
// gate and the Diff tab's whole-run diff, so they can never disagree (STA-774).
//
// Only the base recorded in the daemon DB at worktree creation is trusted.
// Every git ref is writable by the agent working in the task worktree
// (worktrees share the repo's refs): an agent that moved refs/staypoint/base/
// <task>, its pre-run checkpoint or origin/main onto its own head would
// otherwise empty the card and Diff tab and slip its changes past review.
// So TaskBase fails closed on every path that is not the recorded base:
//   - no DB, no table or no row: ErrNoTaskBase (never a merge-base or
//     checkpoint guess);
//   - pin ref moved or deleted: ErrTaskBaseTampered;
//   - DB or git errors: returned as-is.
//
// The recorded base is never moved from git state, not even after the
// harness preflight fast-forwards the task branch: that fetch follows remote
// config and refs the agent can rewrite. Commits merged or fast-forwarded
// into the task branch are listed as its changes: over-reporting is safe,
// hiding is not.
func TaskBase(ctx context.Context, db *sql.DB, repo, taskID, head string) (string, error) {
	if err := ValidateTaskID(taskID); err != nil {
		return "", err
	}
	if _, err := runGit(ctx, repo, "rev-parse", "--verify", "--quiet", head+"^{commit}"); err != nil {
		// Carries git's own error (timeout, EPERM) so callers can report it.
		return "", fmt.Errorf("resolve task head %q: %w", head, err)
	}
	return VerifiedBase(ctx, db, repo, taskID)
}

// VerifiedBase returns taskID's recorded base after checking its pin, with
// TaskBase's fail-closed errors, for callers that have no head to check
// (the Diff tab before the task branch exists).
func VerifiedBase(ctx context.Context, db *sql.DB, repo, taskID string) (string, error) {
	if err := ValidateTaskID(taskID); err != nil {
		return "", err
	}
	recorded, err := RecordedTaskBase(ctx, db, taskID)
	if err != nil {
		return "", err
	}
	if err := verifyPin(ctx, repo, taskID, recorded); err != nil {
		return "", err
	}
	return recorded, nil
}

// verifyPin checks that BaseRef(taskID) still names the recorded base.
func verifyPin(ctx context.Context, repo, taskID, recorded string) error {
	pin, err := runGit(ctx, repo, "rev-parse", "--verify", "--quiet", BaseRef(taskID)+"^{commit}")
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || ctx.Err() != nil {
			// git could not answer (timeout, EPERM): that is not a
			// verified base either, but say what actually failed.
			return fmt.Errorf("verify task base pin: %w", err)
		}
		pin = "missing"
	}
	if pin == recorded {
		return nil
	}
	return fmt.Errorf("%w: task %s recorded base %s, %s is %s",
		ErrTaskBaseTampered, taskID, recorded, BaseRef(taskID), pin)
}

// TaskFilesChanged lists the files that differ between the task base and
// head, one entry per path (renames show as delete + add, matching the Diff
// tab's --no-renames numstat).
func TaskFilesChanged(ctx context.Context, db *sql.DB, repo, taskID, head string) (files []string, base string, err error) {
	base, err = TaskBase(ctx, db, repo, taskID, head)
	if err != nil {
		return nil, "", err
	}
	out, err := runGit(ctx, repo, "diff", "--name-only", "-z", "--no-renames", base, head)
	if err != nil {
		return nil, base, err
	}
	files = []string{}
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, base, nil
}

// RecordTaskBase stores sha as taskID's base in the daemon DB and pins it at
// BaseRef. The DB row is what TaskBase trusts; the ref keeps the commit
// reachable and lets TaskBase notice tampering. Only the daemon calls this,
// at worktree creation, before any agent has run in the worktree.
func RecordTaskBase(ctx context.Context, db *sql.DB, repo, taskID, sha string) error {
	if err := ValidateTaskID(taskID); err != nil {
		return err
	}
	if db == nil {
		return errors.New("record task base: no database")
	}
	if sha == "" || commitOf(ctx, repo, sha) != sha {
		return fmt.Errorf("record task base: %q is not a full commit SHA in %s", sha, repo)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO task_worktree_bases (task_id, repo_path, base_sha)
		VALUES (?, ?, ?)
		ON CONFLICT(task_id) DO UPDATE SET
			repo_path  = excluded.repo_path,
			base_sha   = excluded.base_sha,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`,
		taskID, repo, sha); err != nil {
		return fmt.Errorf("record task base: %w", err)
	}
	if _, err := runGit(ctx, repo, "update-ref", BaseRef(taskID), sha); err != nil {
		return fmt.Errorf("pin task base: %w", err)
	}
	return nil
}

// RecordedTaskBase returns the base the daemon recorded for taskID. It
// returns ErrNoTaskBase when there is none, including when there is no DB or
// the DB never ran migration 32, and any other DB error as-is.
func RecordedTaskBase(ctx context.Context, db *sql.DB, taskID string) (string, error) {
	if db == nil {
		return "", fmt.Errorf("%w %s: no database", ErrNoTaskBase, taskID)
	}
	var sha string
	err := db.QueryRowContext(ctx, `SELECT base_sha FROM task_worktree_bases WHERE task_id = ?`, taskID).Scan(&sha)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && sha == "") {
		return "", fmt.Errorf("%w %s", ErrNoTaskBase, taskID)
	}
	if err != nil {
		return "", fmt.Errorf("read recorded task base: %w", err)
	}
	return sha, nil
}

// RecordTaskTarget stores the branch taskID was cut from and its Approve
// merges into, next to its recorded base. Only the daemon calls this, at
// worktree creation, right after RecordTaskBase.
func RecordTaskTarget(ctx context.Context, db *sql.DB, taskID, branch string) error {
	if db == nil {
		return errors.New("record task target: no database")
	}
	res, err := db.ExecContext(ctx, `UPDATE task_worktree_bases SET target_branch = ? WHERE task_id = ?`, branch, taskID)
	if err != nil {
		return fmt.Errorf("record task target: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("record task target: %w %s", ErrNoTaskBase, taskID)
	}
	return nil
}

// RecordedTaskTarget returns the target branch recorded for taskID, or ""
// when none was (tasks cut before targets were recorded, or no DB).
func RecordedTaskTarget(ctx context.Context, db *sql.DB, taskID string) (string, error) {
	if db == nil {
		return "", nil
	}
	var branch string
	err := db.QueryRowContext(ctx, `SELECT COALESCE(target_branch,'') FROM task_worktree_bases WHERE task_id = ?`, taskID).Scan(&branch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read recorded task target: %w", err)
	}
	return branch, nil
}

// DeleteTaskBase forgets taskID's recorded base and its pin, for when the
// task branch itself is deleted.
func DeleteTaskBase(ctx context.Context, db *sql.DB, repo, taskID string) {
	if db != nil {
		_, _ = db.ExecContext(ctx, `DELETE FROM task_worktree_bases WHERE task_id = ?`, taskID)
	}
	_, _ = runGit(ctx, repo, "update-ref", "-d", BaseRef(taskID))
}

func hasRemote(ctx context.Context, repo, name string) bool {
	_, err := runGit(ctx, repo, "remote", "get-url", name)
	return err == nil
}

// commitOf resolves ref to a commit SHA, or "" when it does not name one.
func commitOf(ctx context.Context, repo, ref string) string {
	out, err := runGit(ctx, repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return ""
	}
	return out
}
