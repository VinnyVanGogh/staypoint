package opstools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	Repo    string
	PR      int
	Method  string
	HeadSHA string
}

// PRView is the part of `gh pr view --json` pr_merge checks.
type PRView struct {
	BaseRefName string `json:"baseRefName"`
	HeadRefOid  string `json:"headRefOid"`
	State       string `json:"state"`
}

// PRViewArgs are the gh arguments that fetch a PRView.
func PRViewArgs(pr int) []string {
	return []string{"pr", "view", strconv.Itoa(pr), "--json", "baseRefName,headRefOid,state"}
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
	var v PRView
	if err := json.Unmarshal(viewJSON, &v); err != nil {
		return nil, fmt.Errorf("read gh pr view: %w", err)
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
	p := &PRMergePlan{Repo: r.Repo, PR: r.PR, Method: r.Method, HeadSHA: v.HeadRefOid}
	p.Tool = "pr_merge"
	p.Effect = MergeEffect(r.Base)
	p.Summary = fmt.Sprintf("repo=%s pr=%d base=%s method=%s head=%s", r.Repo, r.PR, r.Base, r.Method, v.HeadRefOid)
	return p, nil
}

// MergeArgs are the gh arguments that merge p, pinned to its head commit.
func (p *PRMergePlan) MergeArgs() []string {
	return []string{"pr", "merge", strconv.Itoa(p.PR), "--" + p.Method, "--match-head-commit", p.HeadSHA}
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
