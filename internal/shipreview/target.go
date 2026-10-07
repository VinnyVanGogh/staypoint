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
// StayPoint task branch.
func ValidTargetBranch(b string) error {
	if b == "" {
		return nil
	}
	if validateBranch(b) != nil || strings.HasPrefix(b, "refs/") || strings.HasPrefix(b, "origin/") ||
		strings.HasPrefix(b, taskBranchPrefix) || strings.Contains(b, "..") || strings.HasSuffix(b, "/") {
		return fmt.Errorf("%w: %q", ErrInvalidTargetBranch, b)
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
// also sets the target: Approve merges into the PR's base.
//
// The card is still measured against the task's recorded base, never
// against a base derived from the registered branch: an agent-chosen branch
// may over-report its changes but cannot hide any.
func workProductSource(ctx context.Context, db *sql.DB, repo, taskID string) (src cardSource, ok bool, err error) {
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
		src.branch, src.target = pr.HeadRefName, pr.BaseRefName
	}
	if err := checkWorkProductBranch(ctx, repo, taskID, src.branch); err != nil {
		return cardSource{}, false, err
	}
	if src.target != "" {
		if err := ValidTargetBranch(src.target); err != nil {
			return cardSource{}, false, err
		}
	}
	if src.head, err = CurrentBranchHEAD(ctx, repo, src.branch); err != nil {
		return cardSource{}, false, fmt.Errorf("registered branch %q: %w", src.branch, err)
	}
	return src, true, nil
}

// checkWorkProductBranch refuses registered branches a card must never ship
// and Approve must never delete: protected and default branches, the task's
// target, and other tasks' StayPoint branches.
func checkWorkProductBranch(ctx context.Context, repo, taskID, branch string) error {
	if err := ValidTargetBranch(branch); err != nil {
		return fmt.Errorf("registered branch %q cannot be reviewed: %w", branch, err)
	}
	if err := guardDeletableBranch(ctx, repo, branch); err != nil {
		return fmt.Errorf("registered branch %q cannot be reviewed: %w", branch, err)
	}
	if branch == WorkTargetBranch {
		return fmt.Errorf("registered branch %q cannot be reviewed: %w", branch, ErrProtectedBranch)
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
