package opstools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// devBases are the PR bases whose merge is a dev write; every other base,
// main and prod included, is a prod write (fail closed).
var devBases = map[string]bool{"dev-server": true, "dev": true}

// MergeEffect is the effect of merging a PR into base.
func MergeEffect(base string) Effect {
	if devBases[base] {
		return DevWrite
	}
	return ProdWrite
}

var (
	refRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	shaRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
)

// PRMergeRequest is a pr_merge call.
type PRMergeRequest struct {
	Repo   string `json:"repo"`
	PR     int    `json:"pr"`
	Base   string `json:"base"`
	Method string `json:"method"`
}

// PRMergePlan is a validated pr_merge: the PR as GitHub reports it.
type PRMergePlan struct {
	Call
	Repo string
	// GHRepo is the GitHub "owner/name" the PR lives in, from the PR's own
	// URL: the merge targets it explicitly, never the checkout's remotes.
	GHRepo  string
	PR      int
	Method  string
	Base    string
	HeadSHA string
}

// PRView is the part of `gh pr view --json` the PR tools check.
type PRView struct {
	URL         string `json:"url"`
	BaseRefName string `json:"baseRefName"`
	HeadRefName string `json:"headRefName"`
	HeadRefOid  string `json:"headRefOid"`
	State       string `json:"state"`
}

// PRViewArgs are the gh arguments that fetch a PRView; ghRepo pins the
// GitHub repo ("" lets gh resolve it from the checkout's remotes).
func PRViewArgs(pr int, ghRepo string) []string {
	args := []string{"pr", "view", strconv.Itoa(pr), "--json", "url,baseRefName,headRefName,headRefOid,state"}
	if ghRepo != "" {
		args = append(args, "--repo", ghRepo)
	}
	return args
}

var prURLRe = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/pull/([0-9]+)$`)

// ParsePRView reads gh's answer for PR pr and returns it with the GitHub
// "owner/name" its URL names. A URL that is not github.com, or names
// another PR, is refused.
func ParsePRView(pr int, viewJSON []byte) (*PRView, string, error) {
	var v PRView
	if err := json.Unmarshal(viewJSON, &v); err != nil {
		return nil, "", fmt.Errorf("read gh pr view: %w", err)
	}
	m := prURLRe.FindStringSubmatch(v.URL)
	if m == nil || strings.Contains(m[1], "..") {
		return nil, "", fmt.Errorf("PR #%d has no github.com URL (%q)", pr, v.URL)
	}
	if m[2] != strconv.Itoa(pr) {
		return nil, "", fmt.Errorf("gh answered for PR #%s, not #%d", m[2], pr)
	}
	return &v, m[1], nil
}

// ValidatePRMerge checks r's own fields and resolves its repo.
func ValidatePRMerge(r *PRMergeRequest, defaultRepo string) error {
	repo, err := RepoDir(r.Repo, defaultRepo)
	if err != nil {
		return err
	}
	r.Repo = repo
	if r.PR <= 0 {
		return fmt.Errorf("pr must be a positive PR number")
	}
	if !refRe.MatchString(r.Base) || strings.Contains(r.Base, "..") {
		return fmt.Errorf("base %q is not a branch name", r.Base)
	}
	switch r.Method {
	case "":
		r.Method = "merge"
	case "merge", "squash", "rebase":
	default:
		return fmt.Errorf("method %q is not merge, squash or rebase", r.Method)
	}
	return nil
}

// PlanPRMerge binds r to the PR GitHub reported: the declared base must be
// the real base (so the effect is decided from the truth), the PR must be
// open, and the merge is pinned to the head commit the Board saw.
func PlanPRMerge(r PRMergeRequest, viewJSON []byte) (*PRMergePlan, error) {
	v, ghRepo, err := ParsePRView(r.PR, viewJSON)
	if err != nil {
		return nil, err
	}
	if v.State != "OPEN" {
		return nil, fmt.Errorf("PR #%d is %s, not OPEN", r.PR, v.State)
	}
	if v.BaseRefName != r.Base {
		return nil, fmt.Errorf("PR #%d targets %q, not the declared base %q", r.PR, v.BaseRefName, r.Base)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(v.HeadRefOid) {
		return nil, fmt.Errorf("PR #%d has no head commit", r.PR)
	}
	p := &PRMergePlan{Repo: r.Repo, GHRepo: ghRepo, PR: r.PR, Method: r.Method, Base: r.Base, HeadSHA: v.HeadRefOid}
	p.Tool = "pr_merge"
	p.Effect = MergeEffect(r.Base)
	p.Summary = fmt.Sprintf("gh_repo=%s pr=%d base=%s method=%s head=%s", ghRepo, r.PR, r.Base, r.Method, v.HeadRefOid)
	return p, nil
}

// MergeArgs are the gh arguments that merge p: in the GitHub repo the Board
// saw, pinned to its head commit.
func (p *PRMergePlan) MergeArgs() []string {
	return []string{"pr", "merge", strconv.Itoa(p.PR), "--repo", p.GHRepo, "--" + p.Method, "--match-head-commit", p.HeadSHA}
}

// LiveArgs are the gh arguments that read p's PR straight from the REST API
// as "base head state", for the check right before the merge.
func (p *PRMergePlan) LiveArgs() []string {
	return []string{"api", "repos/" + p.GHRepo + "/pulls/" + strconv.Itoa(p.PR), "--jq", `[.base.ref, .head.sha, .state] | join(" ")`}
}

// CheckLive reads LiveArgs' answer: the PR must still be open on the base
// the gate decided on, at the head the Board saw. It runs immediately
// before the merge so a `gh pr edit --base main` during the hold is caught
// before, not only after (Board review #2 M1).
func (p *PRMergePlan) CheckLive(out string) error {
	f := strings.Fields(out)
	if len(f) != 3 {
		return fmt.Errorf("PR #%d: unreadable API answer %q", p.PR, strings.TrimSpace(out))
	}
	switch {
	case f[0] != p.Base:
		return fmt.Errorf("PR #%d base is now %q, not %q (the gate decided on %q); not merged", p.PR, f[0], p.Base, p.Base)
	case f[1] != p.HeadSHA:
		return fmt.Errorf("PR #%d head is now %s, not %s; not merged", p.PR, f[1], p.HeadSHA)
	case f[2] != "open":
		return fmt.Errorf("PR #%d is %s; not merged", p.PR, f[2])
	}
	return nil
}

// CheckMerged reads gh's answer after the merge: the PR must be merged into
// the base the gate decided on. The base cannot be checked atomically with
// the merge (GitHub's merge call takes a head sha, not a base), so a base
// changed in that window is reported, never hidden.
func (p *PRMergePlan) CheckMerged(viewJSON []byte) error {
	v, ghRepo, err := ParsePRView(p.PR, viewJSON)
	if err != nil {
		return err
	}
	if ghRepo != p.GHRepo {
		return fmt.Errorf("PR #%d now reports repo %s, not %s", p.PR, ghRepo, p.GHRepo)
	}
	if v.BaseRefName != p.Base {
		return fmt.Errorf("PR #%d base changed to %q during the merge (gate decided on %q); tell the Board", p.PR, v.BaseRefName, p.Base)
	}
	if v.State != "MERGED" {
		return fmt.Errorf("PR #%d is %s after the merge call", p.PR, v.State)
	}
	return nil
}

var remoteSlugRe = regexp.MustCompile(`^(?:https://github\.com/|ssh://git@github\.com/|git@github\.com:)([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

// GitHubSlug is the "owner/name" of a github.com remote URL, or "".
func GitHubSlug(remoteURL string) string {
	m := remoteSlugRe.FindStringSubmatch(strings.TrimSpace(remoteURL))
	if m == nil || strings.Contains(m[1], "..") {
		return ""
	}
	return m[1]
}

// OriginSlug is the GitHub "owner/name" dir's origin points at, as git
// resolves it (insteadOf rewrites applied), and that URL; "" when it is not
// a github.com remote.
func OriginSlug(ctx context.Context, run Runner, dir string) (slug, url string) {
	res := run(ctx, Cmd{Name: "git", Args: []string{"remote", "get-url", "origin"}, Dir: dir})
	if res.ExitCode != 0 || res.Err != nil {
		return "", ""
	}
	url = strings.TrimSpace(res.Stdout)
	if slug = GitHubSlug(url); slug == "" {
		return "", ""
	}
	return slug, url
}

// TaskBranch is the branch the harness gives taskID's worktree.
func TaskBranch(taskID string) string { return "staypoint/" + taskID }

// PRBodyEffect is the effect of replacing PR body text: a dev write only
// for the task's own PR, one whose head is the task's branch
// (TaskBranch(taskID)) in the task's own GitHub repo (taskSlug), when that
// repo is also one the Board listed (ownRepos, [gates.ops] own_repos: an
// agent can repoint a checkout's origin but not edit the config); an
// external write (the Board) for every other PR (Board review #2 #7/L1).
func PRBodyEffect(ghRepo, headRef, taskSlug, taskID string, ownRepos []string) Effect {
	listed := slices.ContainsFunc(ownRepos, func(r string) bool { return strings.EqualFold(r, ghRepo) })
	if listed && taskSlug != "" && taskID != "" && strings.EqualFold(ghRepo, taskSlug) && headRef == TaskBranch(taskID) {
		return DevWrite
	}
	return ExternalWrite
}

// RepoDir resolves a repo parameter: empty means def; it must be an
// absolute directory.
func RepoDir(repo, def string) (string, error) {
	if repo == "" {
		repo = def
	}
	if repo == "" || !filepath.IsAbs(repo) {
		return "", fmt.Errorf("repo must be an absolute path")
	}
	repo = filepath.Clean(repo)
	if fi, err := os.Stat(repo); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("repo %s is not a directory", repo)
	}
	return repo, nil
}

// maxTextBytes bounds comment, PR body and document text.
const maxTextBytes = 256 << 10

// ValidateText checks text a tool posts.
func ValidateText(text string) error {
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("text is required")
	}
	if len(text) > maxTextBytes {
		return fmt.Errorf("text is %d bytes; the limit is %d", len(text), maxTextBytes)
	}
	return nil
}
