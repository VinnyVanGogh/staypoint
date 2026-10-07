package shipreview

// Target branches: a project's work can land on a branch other than main.
// Work repos ship to dev-server; StayPoint cuts their task branches from it,
// measures cards against it and merges Approve into it, never into main
// implicitly.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/workspace"
)

// WorkTargetBranch is the default target of a work repo whose origin has it.
const WorkTargetBranch = "dev-server"

// taskBranchPrefix names the branches StayPoint makes for its own tasks.
const taskBranchPrefix = "staypoint/"

// ErrInvalidTargetBranch is returned for a target_branch that is not a plain
// branch name.
var ErrInvalidTargetBranch = errors.New("target_branch must be a plain branch name, e.g. dev-server")

// ValidTargetBranch accepts "" (unset) or a plain branch name that is not a
// StayPoint task branch. A plain name follows `git check-ref-format --branch`
// (and so cannot be HEAD, "@", a revision expression such as "a^" or "a@{1}",
// or a glob), carries no refs/ or origin/ prefix, and is safe to pass to git
// as an argument.
func ValidTargetBranch(b string) error {
	if b == "" {
		return nil
	}
	if validateBranch(b) != nil || checkRefFormatBranch(b) != nil || strings.HasPrefix(b, "refs/") ||
		strings.HasPrefix(b, "origin/") || strings.HasPrefix(b, taskBranchPrefix) {
		return fmt.Errorf("%w: %q", ErrInvalidTargetBranch, b)
	}
	return nil
}

// checkRefFormatBranch mirrors `git check-ref-format --branch <b>` for a
// plain branch name: the rules of git-check-ref-format(1), plus --branch's
// refusal of names starting with '-' and of "HEAD".
func checkRefFormatBranch(b string) error {
	bad := func(why string) error { return fmt.Errorf("%w: %q %s", ErrInvalidTargetBranch, b, why) }
	switch {
	case b == "" || b == "@" || b == "HEAD":
		return bad("is reserved")
	case strings.HasPrefix(b, "-"):
		return bad("starts with '-'")
	case strings.HasPrefix(b, "/") || strings.HasSuffix(b, "/") || strings.Contains(b, "//"):
		return bad("has an empty path component")
	case strings.HasSuffix(b, "."):
		return bad("ends with '.'")
	case strings.Contains(b, ".."):
		return bad("contains '..'")
	case strings.Contains(b, "@{"):
		return bad("contains '@{'")
	}
	for _, r := range b {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return bad(fmt.Sprintf("contains %q", r))
		}
	}
	for _, comp := range strings.Split(b, "/") {
		if strings.HasPrefix(comp, ".") || strings.HasSuffix(comp, ".lock") {
			return bad("has a component starting with '.' or ending with '.lock'")
		}
	}
	return nil
}

// ProjectTargetBranch is the branch new tasks in repo are cut from: the
// project's configured target_branch, else dev-server for a work repo whose
// origin has one, else "" (the default branch).
func ProjectTargetBranch(ctx context.Context, db *sql.DB, repo string) (string, error) {
	cfg, err := GetProjectDevConfig(db, repo)
	if err != nil {
		return "", err
	}
	if cfg.TargetBranch != "" {
		return cfg.TargetBranch, nil
	}
	if IsWorkRepo(repo) {
		if _, err := gitOutput(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+WorkTargetBranch+"^{commit}"); err == nil {
			return WorkTargetBranch, nil
		}
	}
	return "", nil
}

// TaskTargetBranch is the branch taskID's Approve merges into: the target
// recorded when its worktree was cut, else the project's current target,
// else the default branch. It is always a branch name, never "".
func TaskTargetBranch(ctx context.Context, db *sql.DB, repo, taskID string) (string, error) {
	target, err := workspace.RecordedTaskTarget(ctx, db, taskID)
	if err != nil {
		return "", err
	}
	if target == "" {
		if target, err = ProjectTargetBranch(ctx, db, repo); err != nil {
			return "", err
		}
	}
	if target == "" {
		return workspace.DefaultBranchName(ctx, repo)
	}
	return target, nil
}

// cardSource is the branch a card ships and the branch it merges into.
type cardSource struct {
	branch string
	head   string
	target string
}

// workProductSource returns the branch the agent registered as the task's
// work (a branch, or a same-repo pull request) for a task whose own
// staypoint/<task> branch has no changes, e.g. work done on a branch cut from
// dev-server. ok is false when the task registered none. A registered PR
// must be based on the task's target (the branch Approve merges into); an
// agent cannot pick another target by opening its PR against it.
//
// The card is still measured against the task's recorded base, never
// against a base derived from the registered branch: an agent-chosen branch
// may over-report its changes but cannot hide any.
func workProductSource(ctx context.Context, db *sql.DB, repo, taskID, target string) (src cardSource, ok bool, err error) {
	rows, err := db.QueryContext(ctx, `
		SELECT product_type, reference FROM task_work_products
		WHERE task_id = ? AND product_type IN ('branch', 'pull_request')
		ORDER BY created_at DESC, id DESC`, taskID)
	if err != nil {
		// No work products table (a DB-only test schema) is no work product.
		return cardSource{}, false, nil
	}
	type product struct{ typ, ref string }
	var products []product
	for rows.Next() {
		var p product
		if rows.Scan(&p.typ, &p.ref) == nil {
			products = append(products, p)
		}
	}
	rows.Close()
	if len(products) == 0 {
		return cardSource{}, false, nil
	}

	p := products[0]
	switch p.typ {
	case "branch":
		src.branch = strings.TrimPrefix(strings.TrimSpace(p.ref), "origin/")
	case "pull_request":
		n, nErr := prNumberFromRef(p.ref)
		if nErr != nil {
			return cardSource{}, false, nErr
		}
		cfg, _ := GetProjectDevConfig(db, repo)
		auth, aErr := ResolveGHAuth(cfg, repo)
		if aErr != nil {
			return cardSource{}, false, fmt.Errorf("read registered PR %s: %w", p.ref, aErr)
		}
		pr, vErr := auth.ViewPR(ctx, n)
		if vErr != nil {
			return cardSource{}, false, fmt.Errorf("read registered PR %s: %w", p.ref, vErr)
		}
		if pr.IsCrossRepository {
			return cardSource{}, false, fmt.Errorf("registered PR %s is from a fork", p.ref)
		}
		if pr.State != "OPEN" {
			return cardSource{}, false, fmt.Errorf("registered PR %s is %s", p.ref, strings.ToLower(pr.State))
		}
		if pr.BaseRefName != target {
			return cardSource{}, false, fmt.Errorf("registered PR %s targets %s, but this task merges into %s; retarget the PR to %s",
				p.ref, pr.BaseRefName, target, target)
		}
		src.branch = pr.HeadRefName
	}
	src.target = target
	if err := checkWorkProductBranch(ctx, db, repo, src.branch, target); err != nil {
		return cardSource{}, false, err
	}
	if src.head, err = CurrentBranchHEAD(ctx, repo, src.branch); err != nil {
		return cardSource{}, false, fmt.Errorf("registered branch %q: %w", src.branch, err)
	}
	// #245: a registered branch at the tip of the target or default branch
	// is that branch under another name (or an alias the name checks
	// missed): there is nothing to merge, and cleanup would delete a ref
	// equal to the target.
	for _, tip := range refTips(ctx, repo, append([]string{target}, defaultBranchNames(ctx, repo)...)...) {
		if tip == src.head {
			return cardSource{}, false, fmt.Errorf("registered branch %q is at the tip of the target or default branch (%s): %w",
				src.branch, tip, ErrProtectedBranch)
		}
	}
	return src, true, nil
}

// checkWorkProductBranch refuses registered branches a card must never ship
// and Approve must never delete: protected and default branches, the task's
// target, the project's configured target (all compared ignoring case),
// StayPoint task branches, and names git resolves only through another ref
// (a case-insensitive filesystem alias).
func checkWorkProductBranch(ctx context.Context, db *sql.DB, repo, branch, target string) error {
	if err := checkRegisteredBranchName(ctx, db, repo, branch, target); err != nil {
		return err
	}
	if err := guardDeletableBranch(ctx, repo, branch); err != nil {
		return fmt.Errorf("registered branch %q cannot be reviewed: %w", branch, err)
	}
	return nil
}

var prRefNumberRe = regexp.MustCompile(`(?:/pull/|^#?)(\d+)/?$`)

// prNumberFromRef reads the PR number from a registered PR reference: a PR
// URL, "#123" or "123".
func prNumberFromRef(ref string) (int, error) {
	m := prRefNumberRe.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil {
		return 0, fmt.Errorf("registered PR %q: no PR number", ref)
	}
	return strconv.Atoi(m[1])
}
