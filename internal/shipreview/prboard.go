package shipreview

// Pull Requests page (task-9fb380ef): list a repo's open PRs and let the Board
// merge one, merge several in order, or combine several into one integration
// PR. Every GitHub call goes through GHAuth (the repo's own gh login or the
// project's gh_config_dir), the same identity ship review's PR mode uses.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// PlaywrightCheckName is the CI check main is known to fail; the page counts
// it apart so its failures do not hide the rest of the checks.
const PlaywrightCheckName = "Playwright UI Specs"

var (
	// ErrPRNotOpen is returned when a PR to merge is closed or already merged.
	ErrPRNotOpen = errors.New("pull request is not open")
	// ErrPRNotMergeable is returned when GitHub reports merge conflicts.
	ErrPRNotMergeable = errors.New("pull request has merge conflicts")
	// ErrCombineConflict is returned when a PR head does not merge cleanly into
	// the integration branch. Nothing is pushed.
	ErrCombineConflict = errors.New("merge conflict while combining pull requests")
)

// PRCheckCounts summarises a PR's checks.
type PRCheckCounts struct {
	Pass    int `json:"pass"`
	Fail    int `json:"fail"`
	Pending int `json:"pending"`
	// Playwright is the state of the Playwright UI Specs check: pass, fail,
	// pending or "" when the PR has no such check. It is not in the counts.
	Playwright string `json:"playwright"`
}

// OpenPR is one open pull request as the Pull Requests page shows it.
type OpenPR struct {
	Number           int           `json:"number"`
	Title            string        `json:"title"`
	URL              string        `json:"url"`
	HeadRefName      string        `json:"head_ref"`
	HeadRefOid       string        `json:"head_sha"`
	BaseRefName      string        `json:"base_ref"`
	Author           string        `json:"author"`
	CreatedAt        string        `json:"created_at"`
	IsDraft          bool          `json:"is_draft"`
	IsCrossRepo      bool          `json:"is_cross_repository"`
	Mergeable        string        `json:"mergeable"`          // MERGEABLE, CONFLICTING, UNKNOWN
	MergeStateStatus string        `json:"merge_state_status"` // CLEAN, BLOCKED, DIRTY, ...
	ChangedFiles     int           `json:"changed_files"`
	Checks           PRCheckCounts `json:"checks"`
}

type ghRollupItem struct {
	Typename   string `json:"__typename"`
	Name       string `json:"name"`
	Context    string `json:"context"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

type ghOpenPR struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	HeadRefName string `json:"headRefName"`
	HeadRefOid  string `json:"headRefOid"`
	BaseRefName string `json:"baseRefName"`
	Author      struct {
		Login string `json:"login"`
	} `json:"author"`
	CreatedAt         string         `json:"createdAt"`
	IsDraft           bool           `json:"isDraft"`
	IsCrossRepository bool           `json:"isCrossRepository"`
	Mergeable         string         `json:"mergeable"`
	MergeStateStatus  string         `json:"mergeStateStatus"`
	ChangedFiles      int            `json:"changedFiles"`
	StatusCheckRollup []ghRollupItem `json:"statusCheckRollup"`
}

const openPRFields = "number,title,url,headRefName,headRefOid,baseRefName,author,createdAt,isDraft,isCrossRepository,mergeable,mergeStateStatus,changedFiles,statusCheckRollup"

// rollupBucket maps one statusCheckRollup entry to pass, fail or pending.
func rollupBucket(it ghRollupItem) string {
	if it.Typename == "StatusContext" || (it.Status == "" && it.State != "") {
		switch strings.ToUpper(it.State) {
		case "SUCCESS":
			return "pass"
		case "FAILURE", "ERROR":
			return "fail"
		}
		return "pending"
	}
	if strings.ToUpper(it.Status) != "COMPLETED" {
		return "pending"
	}
	switch strings.ToUpper(it.Conclusion) {
	case "SUCCESS", "NEUTRAL", "SKIPPED":
		return "pass"
	}
	return "fail"
}

// countChecks reduces a statusCheckRollup to pass/fail/pending counts, with
// the Playwright UI Specs check reported on its own.
func countChecks(items []ghRollupItem) PRCheckCounts {
	var c PRCheckCounts
	for _, it := range items {
		name := it.Name
		if name == "" {
			name = it.Context
		}
		b := rollupBucket(it)
		if strings.EqualFold(strings.TrimSpace(name), PlaywrightCheckName) {
			// A re-run leaves several entries; a failure wins, then pending.
			if c.Playwright != "fail" && (c.Playwright != "pending" || b == "fail") {
				c.Playwright = b
			}
			continue
		}
		switch b {
		case "pass":
			c.Pass++
		case "fail":
			c.Fail++
		default:
			c.Pending++
		}
	}
	return c
}

func (p ghOpenPR) toOpenPR() OpenPR {
	return OpenPR{
		Number: p.Number, Title: p.Title, URL: p.URL,
		HeadRefName: p.HeadRefName, HeadRefOid: p.HeadRefOid, BaseRefName: p.BaseRefName,
		Author: p.Author.Login, CreatedAt: p.CreatedAt, IsDraft: p.IsDraft, IsCrossRepo: p.IsCrossRepository,
		Mergeable: p.Mergeable, MergeStateStatus: p.MergeStateStatus, ChangedFiles: p.ChangedFiles,
		Checks: countChecks(p.StatusCheckRollup),
	}
}

// ListOpenPRs returns the repo's open pull requests, newest first.
func (a GHAuth) ListOpenPRs(ctx context.Context) ([]OpenPR, error) {
	out, err := a.gh(ctx, "pr", "list", "--state", "open", "--json", openPRFields, "--limit", "100")
	if err != nil {
		return nil, err
	}
	var raw []ghOpenPR
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse gh pr list: %w", err)
	}
	prs := make([]OpenPR, 0, len(raw))
	for _, p := range raw {
		prs = append(prs, p.toOpenPR())
	}
	return prs, nil
}

type prMergeView struct {
	Number      int    `json:"number"`
	State       string `json:"state"`
	HeadRefOid  string `json:"headRefOid"`
	BaseRefName string `json:"baseRefName"`
	Mergeable   string `json:"mergeable"`
}

func (a GHAuth) viewForMerge(ctx context.Context, number int) (*prMergeView, error) {
	out, err := a.gh(ctx, "pr", "view", strconv.Itoa(number), "--json", "number,state,headRefOid,baseRefName,mergeable")
	if err != nil {
		return nil, err
	}
	var v prMergeView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, fmt.Errorf("parse gh pr view: %w", err)
	}
	return &v, nil
}

// MergePRAt merges PR number with a merge commit, pinned to headSHA: it
// re-reads the PR first and refuses (ErrHeadMoved) when the head is no longer
// the one the Board saw, and passes --match-head-commit so GitHub refuses a
// head that moves between the check and the merge.
func (a GHAuth) MergePRAt(ctx context.Context, number int, headSHA string) error {
	if number <= 0 || headSHA == "" {
		return errors.New("pr number and head_sha are required")
	}
	v, err := a.viewForMerge(ctx, number)
	if err != nil {
		return fmt.Errorf("read PR #%d: %w", number, err)
	}
	if v.State != "OPEN" {
		return fmt.Errorf("%w: PR #%d is %s", ErrPRNotOpen, number, strings.ToLower(v.State))
	}
	if v.HeadRefOid != headSHA {
		return ErrHeadMoved
	}
	if v.Mergeable == "CONFLICTING" {
		return fmt.Errorf("%w: PR #%d", ErrPRNotMergeable, number)
	}
	if _, err := a.gh(ctx, "pr", "merge", strconv.Itoa(number), "--merge", "--match-head-commit", headSHA); err != nil {
		return fmt.Errorf("%w: %s", ErrGitHubRefused, err.Error())
	}
	if after, err := a.viewForMerge(ctx, number); err == nil && after.State != "MERGED" {
		return fmt.Errorf("gh pr merge succeeded but PR #%d is %s (queued?)", number, strings.ToLower(after.State))
	}
	return nil
}

// CombineSource is one PR to fold into the integration branch, pinned to the
// head the Board saw.
type CombineSource struct {
	Number  int    `json:"number"`
	HeadSHA string `json:"head_sha"`
	Title   string `json:"title,omitempty"`
}

// CombineRequest asks for one integration PR built from Sources, in order.
type CombineRequest struct {
	Base    string
	Branch  string // integration branch to create; must not exist on origin
	Title   string
	Sources []CombineSource
}

// CombineResult is the integration PR that was opened.
type CombineResult struct {
	Branch string `json:"branch"`
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// CombineError reports which source PR stopped a combine.
type CombineError struct {
	Number int
	Err    error
}

func (e *CombineError) Error() string { return fmt.Sprintf("PR #%d: %v", e.Number, e.Err) }
func (e *CombineError) Unwrap() error { return e.Err }

// CombineBody is the integration PR's body: one line per source PR, so
// GitHub links them. The source PRs are left open.
func CombineBody(base string, sources []CombineSource) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Combines these pull requests into `%s`, merged in this order:\n\n", base)
	for _, s := range sources {
		t := strings.TrimSpace(s.Title)
		if t != "" {
			fmt.Fprintf(&b, "- #%d %s (`%s`)\n", s.Number, t, shortSHA(s.HeadSHA))
		} else {
			fmt.Fprintf(&b, "- #%d (`%s`)\n", s.Number, shortSHA(s.HeadSHA))
		}
	}
	b.WriteString("\nThe source pull requests were not closed. Close them once this merges.\n\nOpened by StayPoint from the Pull Requests page (Board action).\n")
	return b.String()
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// CombinePRs builds Branch from origin/Base by merging each source head in
// order in a throwaway worktree, then pushes it and opens one PR. The branch
// is pushed only after every merge succeeded, so a conflict leaves nothing on
// origin; if opening the PR fails after the push, the pushed branch is
// deleted again.
func (a GHAuth) CombinePRs(ctx context.Context, req CombineRequest) (*CombineResult, error) {
	if len(req.Sources) < 2 {
		return nil, errors.New("combine needs at least two pull requests")
	}
	if err := validateBranch(req.Base); err != nil {
		return nil, err
	}
	if err := validateBranch(req.Branch); err != nil {
		return nil, err
	}
	ref := "refs/heads/" + req.Branch
	if out, err := a.git(ctx, "ls-remote", "--heads", "origin", ref); err != nil {
		return nil, err
	} else if strings.TrimSpace(out) != "" {
		return nil, fmt.Errorf("branch %s already exists on origin", req.Branch)
	}
	if _, err := a.git(ctx, "fetch", "origin", "refs/heads/"+req.Base); err != nil {
		return nil, err
	}
	baseSHA, err := a.git(ctx, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return nil, err
	}
	for _, s := range req.Sources {
		if _, err := a.git(ctx, "fetch", "origin", "refs/pull/"+strconv.Itoa(s.Number)+"/head"); err != nil {
			return nil, &CombineError{Number: s.Number, Err: err}
		}
		got, err := a.git(ctx, "rev-parse", "FETCH_HEAD")
		if err != nil {
			return nil, &CombineError{Number: s.Number, Err: err}
		}
		if got != s.HeadSHA {
			return nil, &CombineError{Number: s.Number, Err: ErrHeadMoved}
		}
	}

	dir, err := os.MkdirTemp("", "staypoint-combine-")
	if err != nil {
		return nil, err
	}
	_ = os.Remove(dir) // git worktree add wants to create it
	if _, err := a.git(ctx, "worktree", "add", "--detach", dir, baseSHA); err != nil {
		return nil, err
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = a.git(cctx, "worktree", "remove", "--force", dir)
		_ = os.RemoveAll(dir)
		_, _ = a.git(cctx, "worktree", "prune")
	}()
	wt := GHAuth{RepoDir: dir, ConfigDir: a.ConfigDir}
	for _, s := range req.Sources {
		msg := fmt.Sprintf("Merge pull request #%d into %s", s.Number, req.Branch)
		if _, err := wt.git(ctx, "merge", "--no-ff", "--no-edit", "-m", msg, s.HeadSHA); err != nil {
			_, _ = wt.git(ctx, "merge", "--abort")
			return nil, &CombineError{Number: s.Number, Err: fmt.Errorf("%w: %v", ErrCombineConflict, err)}
		}
	}
	if _, err := wt.git(ctx, "push", "origin", "HEAD:"+ref); err != nil {
		return nil, err
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		nums := make([]string, len(req.Sources))
		for i, s := range req.Sources {
			nums[i] = "#" + strconv.Itoa(s.Number)
		}
		title = "Combine " + strings.Join(nums, ", ")
	}
	out, err := a.gh(ctx, "pr", "create", "--base", req.Base, "--head", req.Branch, "--title", title, "--body", CombineBody(req.Base, req.Sources))
	if err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitNetTimeout)
		defer cancel()
		if _, delErr := a.git(cctx, "push", "origin", "--delete", req.Branch); delErr != nil {
			return nil, fmt.Errorf("open PR: %v (and deleting the pushed branch %s failed: %v)", err, req.Branch, delErr)
		}
		return nil, fmt.Errorf("open PR: %w", err)
	}
	res := &CombineResult{Branch: req.Branch, URL: strings.TrimSpace(out)}
	if m := prURLNumberRe.FindStringSubmatch(res.URL); m != nil {
		res.Number, _ = strconv.Atoi(m[1])
	}
	return res, nil
}
