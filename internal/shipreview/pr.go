package shipreview

// STA-717: per-project merge modes. Besides today's direct merge, Approve can
// open (or update) a GitHub PR and either stop there ("open_pr") or wait for
// CI on the pinned head and merge through GitHub ("pr_merge"). All GitHub
// access goes through the gh CLI with the repo's own credentials.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/bridge"
	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
)

// Merge modes for ProjectDevConfig.MergeMode and Card.MergeMode.
const (
	MergeModeDirect  = "direct"
	MergeModeOpenPR  = "open_pr"
	MergeModePRMerge = "pr_merge"
)

// Checks summaries for Card.PRChecksSummary.
const (
	ChecksRunning = "running"
	ChecksPassed  = "passed"
	ChecksFailed  = "failed"
	ChecksNone    = "none" // GitHub reported no checks for the head
)

var (
	// ErrInvalidMergeMode is returned for a merge_mode outside the known set.
	ErrInvalidMergeMode = errors.New("merge_mode must be one of direct, open_pr, pr_merge")
	// ErrWorkRepoGHAuth is returned when a work repo has no gh identity of its
	// own: the personal gh login must never act on a work repo.
	ErrWorkRepoGHAuth = errors.New("work repo needs its own gh_config_dir (the work GitHub account's GH_CONFIG_DIR); refusing to use the default gh login")
	// ErrChecksNotGreen is returned by MergePR when CI is failing or still
	// running and the Board did not choose Merge anyway.
	ErrChecksNotGreen = errors.New("checks are not all green")
	// ErrGitHubRefused wraps gh's own error when GitHub refuses a merge.
	ErrGitHubRefused = errors.New("GitHub refused the merge")
	// ErrNoPR is returned for PR actions on a card that has no PR.
	ErrNoPR = errors.New("card has no pull request")
)

// ValidMergeMode reports whether m is a known merge mode or "" (unset).
func ValidMergeMode(m string) bool {
	switch m {
	case "", MergeModeDirect, MergeModeOpenPR, MergeModePRMerge:
		return true
	}
	return false
}

// EffectiveMergeMode is the mode Approve uses: the configured one, else
// open_pr for work repos and direct (today's behaviour) for everything else.
func EffectiveMergeMode(cfg *ProjectDevConfig, isWork bool) string {
	if cfg != nil && cfg.MergeMode != "" {
		return cfg.MergeMode
	}
	if isWork {
		return MergeModeOpenPR
	}
	return MergeModeDirect
}

// PRCheck is one CI check on a PR head, as reported by `gh pr checks`.
type PRCheck struct {
	Name        string `json:"name"`
	Workflow    string `json:"workflow,omitempty"`
	State       string `json:"state"`  // GitHub's raw state, e.g. SUCCESS, FAILURE, IN_PROGRESS
	Bucket      string `json:"bucket"` // pass, fail, pending, skipping, cancel
	Link        string `json:"link,omitempty"`
	StartedAt   string `json:"started_at,omitempty"`
	CompletedAt string `json:"completed_at,omitempty"`
	// DurationSec is completed-started, or so-far for a running check.
	DurationSec int `json:"duration_sec,omitempty"`
}

// green reports whether the check does not block a merge.
func (c PRCheck) green() bool { return c.Bucket == "pass" || c.Bucket == "skipping" }

// SummarizeChecks reduces a checks list to running / passed / failed / none.
// A failure wins over a still-running check so the Board sees it at once.
func SummarizeChecks(checks []PRCheck) string {
	if len(checks) == 0 {
		return ChecksNone
	}
	running := false
	for _, c := range checks {
		switch {
		case c.Bucket == "fail" || c.Bucket == "cancel":
			return ChecksFailed
		case !c.green():
			running = true
		}
	}
	if running {
		return ChecksRunning
	}
	return ChecksPassed
}

// NotGreen returns the checks that block a merge: failing, cancelled or
// still pending.
func NotGreen(checks []PRCheck) []PRCheck {
	var out []PRCheck
	for _, c := range checks {
		if !c.green() {
			out = append(out, c)
		}
	}
	return out
}

// ── gh runner ───────────────────────────────────────────────────────────────

// GHAuth is how gh and git push authenticate for one repo.
type GHAuth struct {
	RepoDir string
	// ConfigDir is exported as GH_CONFIG_DIR when set; "" uses gh's default.
	ConfigDir string
	// Work repos never inherit GH_TOKEN-style variables from the daemon: those
	// would override ConfigDir with whatever account the daemon runs as.
	Work bool
}

// ghTokenVars override gh's stored login and git's gh credential helper.
var ghTokenVars = []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_CONFIG_DIR", "GH_HOST"}

// defaultGHConfigDir is where gh keeps its login when GH_CONFIG_DIR is unset.
// Variable so tests can point it at a temp dir.
var defaultGHConfigDir = func() string {
	if d := os.Getenv("GH_CONFIG_DIR"); d != "" {
		return d
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "gh")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "gh")
}

// ResolveGHAuth picks the gh identity for repoDir. A work repo must name its
// own config dir, and it may not be the default (personal) one.
func ResolveGHAuth(cfg *ProjectDevConfig, repoDir string, isWork bool) (GHAuth, error) {
	auth := GHAuth{RepoDir: repoDir, Work: isWork}
	if cfg != nil {
		auth.ConfigDir = strings.TrimSpace(cfg.GHConfigDir)
	}
	if auth.ConfigDir != "" {
		if !filepath.IsAbs(auth.ConfigDir) {
			return auth, fmt.Errorf("gh_config_dir must be an absolute path, got %q", auth.ConfigDir)
		}
		if fi, err := os.Stat(auth.ConfigDir); err != nil || !fi.IsDir() {
			return auth, fmt.Errorf("gh_config_dir %q is not a directory", auth.ConfigDir)
		}
	}
	if isWork {
		if auth.ConfigDir == "" || sameDir(auth.ConfigDir, defaultGHConfigDir()) {
			return auth, ErrWorkRepoGHAuth
		}
	}
	return auth, nil
}

func sameDir(a, b string) bool {
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if ra, err := filepath.EvalSymlinks(ca); err == nil {
		ca = ra
	}
	if rb, err := filepath.EvalSymlinks(cb); err == nil {
		cb = rb
	}
	return ca == cb
}

// env returns the environment for gh and git push under this auth.
func (a GHAuth) env() []string {
	var out []string
	for _, kv := range os.Environ() {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		drop := false
		for _, v := range ghTokenVars {
			// GH_CONFIG_DIR is always replaced below; the token vars only
			// for work repos, where they would be the wrong account.
			if name == v && (a.Work || v == "GH_CONFIG_DIR") {
				drop = true
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	if a.ConfigDir != "" {
		out = append(out, "GH_CONFIG_DIR="+a.ConfigDir)
	} else if d := os.Getenv("GH_CONFIG_DIR"); d != "" && !a.Work {
		out = append(out, "GH_CONFIG_DIR="+d)
	}
	return append(out, "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1", "GIT_TERMINAL_PROMPT=0")
}

type gitEnvKey struct{}

// WithAuth returns ctx carrying this auth's environment: every git call the
// package makes under it (fetch, push --delete in CleanupMergedBranch and
// DeleteBranch) then authenticates as the repo's own gh account.
func (a GHAuth) WithAuth(ctx context.Context) context.Context {
	return context.WithValue(ctx, gitEnvKey{}, a.env())
}

func gitEnvFrom(ctx context.Context) []string {
	env, _ := ctx.Value(gitEnvKey{}).([]string)
	return env
}

// ghTimeout bounds a single gh call; gitNetTimeout a push, fetch or ls-remote.
var (
	ghTimeout     = 60 * time.Second
	gitNetTimeout = 120 * time.Second
)

// IsWorkRepo decides whether a repo is a work (Managed Solution) repo. The
// server replaces it with a detector that also checks the git remote.
var IsWorkRepo = bridge.IsWorkRepo

// ghPath finds gh. launchd gives the daemon a short PATH, so fall back to the
// usual Homebrew locations.
func ghPath() string {
	if p, err := exec.LookPath("gh"); err == nil {
		return p
	}
	for _, p := range []string{"/opt/homebrew/bin/gh", "/usr/local/bin/gh"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return "gh"
}

// ghError carries gh's stderr verbatim so the Board sees GitHub's reason.
type ghError struct {
	args   []string
	stderr string
	err    error
}

func (e *ghError) Error() string {
	msg := strings.TrimSpace(e.stderr)
	if msg == "" {
		msg = e.err.Error()
	}
	return msg
}

func (e *ghError) Unwrap() error { return e.err }

// gh runs the gh CLI in the repo with this auth and returns stdout.
func (a GHAuth) gh(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ghPath(), args...) //nolint:gosec // fixed subcommands, values validated
	cmd.Dir = a.RepoDir
	cmd.Env = a.env()
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("gh %s timed out after %s: %w", strings.Join(args, " "), ghTimeout, err)
		}
		return stdout.String(), &ghError{args: args, stderr: stderr.String(), err: err}
	}
	return stdout.String(), nil
}

// git runs git in the repo with this auth (gh's credential helper reads
// GH_CONFIG_DIR, so a push uses the same account as gh).
func (a GHAuth) git(ctx context.Context, args ...string) (string, error) {
	// These talk to the remote; the 10s local-git default is too short.
	ctx, cancel := context.WithTimeout(ctx, gitNetTimeout)
	defer cancel()
	cmd := gitexec.Command(ctx, args...)
	cmd.Dir = a.RepoDir
	cmd.Env = a.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w (stderr: %s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// ── PR lifecycle ────────────────────────────────────────────────────────────

// PRInfo is the subset of `gh pr view` the review flow needs.
type PRInfo struct {
	Number      int    `json:"number"`
	URL         string `json:"url"`
	State       string `json:"state"` // OPEN, CLOSED, MERGED
	HeadRefOid  string `json:"headRefOid"`
	HeadRefName string `json:"headRefName"`
	BaseRefName string `json:"baseRefName"`
	// IsCrossRepository is true for a PR from a fork, which StayPoint never
	// treats as the task's PR even when the branch name matches.
	IsCrossRepository bool `json:"isCrossRepository"`
	MergeCommit       *struct {
		Oid string `json:"oid"`
	} `json:"mergeCommit"`
}

const prViewFields = "number,url,state,headRefOid,headRefName,baseRefName,isCrossRepository,mergeCommit"

// ViewPR reads one PR.
func (a GHAuth) ViewPR(ctx context.Context, number int) (*PRInfo, error) {
	out, err := a.gh(ctx, "pr", "view", strconv.Itoa(number), "--json", prViewFields)
	if err != nil {
		return nil, err
	}
	var pr PRInfo
	if err := json.Unmarshal([]byte(out), &pr); err != nil {
		return nil, fmt.Errorf("parse gh pr view: %w", err)
	}
	return &pr, nil
}

// findOpenPR returns the open same-repo PR whose head is branch, or nil.
// `--head` matches by branch name only, so a fork's PR from a branch with the
// same name is skipped.
func (a GHAuth) findOpenPR(ctx context.Context, branch string) (*PRInfo, error) {
	out, err := a.gh(ctx, "pr", "list", "--head", branch, "--state", "open", "--json", prViewFields, "--limit", "20")
	if err != nil {
		return nil, err
	}
	var prs []PRInfo
	if err := json.Unmarshal([]byte(out), &prs); err != nil {
		return nil, fmt.Errorf("parse gh pr list: %w", err)
	}
	for i := range prs {
		if !prs[i].IsCrossRepository && prs[i].HeadRefName == branch {
			return &prs[i], nil
		}
	}
	return nil, nil
}

var prURLNumberRe = regexp.MustCompile(`/pull/(\d+)\s*$`)

// defaultBranch is the repo's default branch on GitHub.
func (a GHAuth) defaultBranch(ctx context.Context) (string, error) {
	out, err := a.gh(ctx, "repo", "view", "--json", "defaultBranchRef", "--jq", ".defaultBranchRef.name")
	if err != nil {
		return "", err
	}
	b := strings.TrimSpace(out)
	if b == "" {
		return "", errors.New("gh repo view: empty default branch")
	}
	return b, nil
}

// pushHead pushes exactly sha to the task branch on origin. With force the
// push is leased to the remote tip it just read, so it never discards a push
// it has not seen; without force it only fast-forwards.
func (a GHAuth) pushHead(ctx context.Context, branch, sha string, force bool) error {
	if err := validateBranch(branch); err != nil {
		return err
	}
	ref := "refs/heads/" + branch
	args := []string{"push", "origin", sha + ":" + ref}
	if force {
		out, err := a.git(ctx, "ls-remote", "--heads", "origin", ref)
		if err != nil {
			return err
		}
		tip := ""
		if f := strings.Fields(out); len(f) > 0 {
			tip = f[0]
		}
		args = []string{"push", "--force-with-lease=" + ref + ":" + tip, "origin", sha + ":" + ref}
	}
	_, err := a.git(ctx, args...)
	return err
}

// prBody is the PR description StayPoint writes.
func prBody(card *Card) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Opened by StayPoint ship review for task `%s` at `%s`.\n", card.TaskID, card.HeadSHA)
	if len(card.TestSteps) > 0 {
		b.WriteString("\n## What to test\n")
		for i, s := range card.TestSteps {
			fmt.Fprintf(&b, "%d. %s\n", i+1, strings.TrimSpace(leadingNumber.ReplaceAllString(s, "")))
		}
	}
	return b.String()
}

var leadingNumber = regexp.MustCompile(`^\s*\d+[.)]\s*`)

// prTitle is the first line of the pinned commit's message.
func prTitle(ctx context.Context, a GHAuth, card *Card) string {
	if out, err := a.git(ctx, "log", "-1", "--format=%s", card.HeadSHA); err == nil && out != "" {
		return out
	}
	return "StayPoint: " + card.TaskID
}

// OpenOrUpdatePR pushes card.HeadSHA to the task branch and opens a PR
// against the default branch, or reuses the open one for that branch. The
// local branch must still be at the pinned SHA. force allows a leased
// non-fast-forward push (Board Approve); the agent-triggered re-push after a
// CI send-back passes false.
func OpenOrUpdatePR(ctx context.Context, a GHAuth, card *Card, force bool) (*PRInfo, error) {
	cur, err := gitOutput(ctx, a.RepoDir, "rev-parse", card.Branch)
	if err != nil {
		return nil, fmt.Errorf("resolve branch HEAD: %w", err)
	}
	if cur != card.HeadSHA {
		return nil, ErrHeadMoved
	}
	if err := a.pushHead(ctx, card.Branch, card.HeadSHA, force); err != nil {
		return nil, fmt.Errorf("push %s: %w", card.Branch, err)
	}
	pr, err := a.findOpenPR(ctx, card.Branch)
	if err != nil {
		return nil, fmt.Errorf("look up PR: %w", err)
	}
	if pr == nil {
		base, err := a.defaultBranch(ctx)
		if err != nil {
			return nil, fmt.Errorf("default branch: %w", err)
		}
		out, err := a.gh(ctx, "pr", "create", "--head", card.Branch, "--base", base,
			"--title", prTitle(ctx, a, card), "--body", prBody(card))
		if err != nil {
			return nil, fmt.Errorf("gh pr create: %w", err)
		}
		// Take the PR gh just created by its URL, not by a second branch lookup.
		m := prURLNumberRe.FindStringSubmatch(strings.TrimSpace(out))
		if m == nil {
			return nil, fmt.Errorf("gh pr create: no PR URL in output %q", strings.TrimSpace(out))
		}
		n, _ := strconv.Atoi(m[1])
		if pr, err = a.ViewPR(ctx, n); err != nil {
			return nil, fmt.Errorf("read created PR #%d: %w", n, err)
		}
	}
	if pr.IsCrossRepository || pr.HeadRefName != card.Branch || pr.HeadRefOid != card.HeadSHA {
		return nil, fmt.Errorf("PR #%d is not this task's branch at %s (head %s@%s, cross-repo %v)",
			pr.Number, card.HeadSHA, pr.HeadRefName, pr.HeadRefOid, pr.IsCrossRepository)
	}
	return pr, nil
}

// ── checks ──────────────────────────────────────────────────────────────────

type ghCheck struct {
	Name        string `json:"name"`
	State       string `json:"state"`
	Bucket      string `json:"bucket"`
	Link        string `json:"link"`
	StartedAt   string `json:"startedAt"`
	CompletedAt string `json:"completedAt"`
	Workflow    string `json:"workflow"`
}

// PRChecks returns the checks on the PR's current head. gh exits non-zero
// while checks are pending or failing, so its JSON is read regardless.
func (a GHAuth) PRChecks(ctx context.Context, number int, now time.Time) ([]PRCheck, error) {
	out, err := a.gh(ctx, "pr", "checks", strconv.Itoa(number), "--json", "name,state,bucket,link,startedAt,completedAt,workflow")
	var raw []ghCheck
	if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(out)), &raw); jsonErr != nil {
		var ge *ghError
		if errors.As(err, &ge) && strings.Contains(ge.stderr, "no checks reported") {
			return []PRCheck{}, nil
		}
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("parse gh pr checks: %w", jsonErr)
	}
	checks := make([]PRCheck, 0, len(raw))
	for _, r := range raw {
		c := PRCheck{Name: r.Name, Workflow: r.Workflow, State: r.State, Bucket: r.Bucket, Link: r.Link,
			StartedAt: r.StartedAt, CompletedAt: r.CompletedAt}
		if c.Bucket == "" {
			c.Bucket = "pending"
		}
		if st, err := time.Parse(time.RFC3339, r.StartedAt); err == nil && st.Year() > 1 {
			end := now
			if ct, err := time.Parse(time.RFC3339, r.CompletedAt); err == nil && ct.Year() > 1 {
				end = ct
			}
			if d := end.Sub(st); d > 0 {
				c.DurationSec = int(d.Seconds())
			}
		}
		checks = append(checks, c)
	}
	return checks, nil
}

// ChecksResult is one poll of a PR-mode card.
type ChecksResult struct {
	PR      *PRInfo   `json:"pr"`
	Checks  []PRCheck `json:"checks"`
	Summary string    `json:"summary"`
	// HeadMoved is set when the PR head is no longer the card's pinned SHA;
	// the checks then belong to another commit and are not recorded.
	HeadMoved bool `json:"head_moved"`
}

// PollChecks reads the PR and its checks and records them on the card when
// they are for the pinned head.
func PollChecks(ctx context.Context, db *sql.DB, a GHAuth, card *Card) (*ChecksResult, error) {
	if card.PRNumber <= 0 {
		return nil, ErrNoPR
	}
	pr, err := a.ViewPR(ctx, card.PRNumber)
	if err != nil {
		return nil, err
	}
	res := &ChecksResult{PR: pr, Checks: []PRCheck{}}
	if pr.HeadRefOid != card.HeadSHA {
		res.HeadMoved = true
		return res, nil
	}
	checks, err := a.PRChecks(ctx, card.PRNumber, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	res.Checks = checks
	res.Summary = SummarizeChecks(checks)
	if err := SetPRChecks(db, card.ID, card.HeadSHA, checks); err != nil {
		return nil, err
	}
	return res, nil
}

// ── card persistence ────────────────────────────────────────────────────────

// SetPROpened records the PR an Approve opened or reused, and resets the
// checks to "running" for head.
func SetPROpened(db *sql.DB, cardID, mode string, pr *PRInfo, head string) error {
	_, err := db.Exec(`
		UPDATE ship_review_cards
		SET merge_mode = ?, pr_number = ?, pr_url = ?, pr_checks_json = '[]', pr_checks_sha = ?,
		    pr_checks_at = '', pr_merge_error = '', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?`, mode, pr.Number, pr.URL, head, cardID)
	return err
}

// SetPRChecks stores a checks snapshot for head.
func SetPRChecks(db *sql.DB, cardID, head string, checks []PRCheck) error {
	b, err := json.Marshal(checks)
	if err != nil {
		return err
	}
	_, err = db.Exec(`
		UPDATE ship_review_cards
		SET pr_checks_json = ?, pr_checks_sha = ?, pr_checks_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
		    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?`, string(b), head, cardID)
	return err
}

// SetPRMergeError records GitHub's refusal of the last merge attempt.
func SetPRMergeError(db *sql.DB, cardID, msg string) error {
	_, err := db.Exec(`UPDATE ship_review_cards SET pr_merge_error = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, msg, cardID)
	return err
}

// MarkPRApproved closes an open_pr card: the Board approved and the PR is
// handed to GitHub. main_sha stays empty; StayPoint never merged.
func MarkPRApproved(db *sql.DB, cardID, head string) error {
	_, err := db.Exec(`
		UPDATE ship_review_cards
		SET status = 'approved', approved_sha = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?`, head, cardID)
	return err
}

// SetCIFixRequested flags a sent-back card whose comment carries CI failures.
func SetCIFixRequested(db *sql.DB, cardID string) error {
	_, err := db.Exec(`UPDATE ship_review_cards SET ci_fix_requested = 1, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, cardID)
	return err
}

// inheritedPR is what a resubmitted card carries over from the card it
// replaces: the PR stays the same across the agent's fix runs.
type inheritedPR struct {
	mode     string
	number   int
	url      string
	ciFixReq bool
}

func previousOpenPR(db *sql.DB, taskID string) inheritedPR {
	var p inheritedPR
	_ = db.QueryRow(`
		SELECT COALESCE(merge_mode,''), COALESCE(pr_number,0), COALESCE(pr_url,''), COALESCE(ci_fix_requested,0)
		FROM ship_review_cards
		WHERE task_id = ? AND status IN ('pending','sent_back') AND COALESCE(pr_number,0) > 0
		ORDER BY created_at DESC LIMIT 1`, taskID).Scan(&p.mode, &p.number, &p.url, &p.ciFixReq)
	return p
}

// ── merge ───────────────────────────────────────────────────────────────────

// MergeResult is the outcome of MergePR.
type MergeResult struct {
	MainSHA string    `json:"main_sha"`
	Checks  []PRCheck `json:"checks"`
	// Overridden lists the checks that were not green when the Board merged
	// anyway.
	Overridden []PRCheck `json:"overridden,omitempty"`
}

// MergePR merges the card's PR through GitHub, pinned to card.HeadSHA:
//  1. the local branch and the PR head must both still be the pinned SHA;
//  2. checks are re-read now, and anything not green blocks unless override;
//  3. `gh pr merge --match-head-commit <sha>` makes GitHub refuse if the head
//     moves in between, and branch protection / required reviews still apply.
//
// onOverride runs before an override merge with the checks being overridden;
// if it fails (e.g. the audit write) nothing is merged. On a GitHub refusal
// the error wraps ErrGitHubRefused and carries gh's message verbatim.
func MergePR(ctx context.Context, db *sql.DB, a GHAuth, card *Card, override bool, onOverride func([]PRCheck) error) (*MergeResult, error) {
	if card.PRNumber <= 0 {
		return nil, ErrNoPR
	}
	cur, err := gitOutput(ctx, a.RepoDir, "rev-parse", card.Branch)
	if err != nil {
		return nil, fmt.Errorf("resolve branch HEAD: %w", err)
	}
	if cur != card.HeadSHA {
		return nil, ErrHeadMoved
	}
	pr, err := a.ViewPR(ctx, card.PRNumber)
	if err != nil {
		return nil, fmt.Errorf("read PR #%d: %w", card.PRNumber, err)
	}
	if pr.HeadRefOid != card.HeadSHA {
		return nil, ErrHeadMoved
	}
	checks, err := a.PRChecks(ctx, card.PRNumber, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("read checks: %w", err)
	}
	_ = SetPRChecks(db, card.ID, card.HeadSHA, checks)
	res := &MergeResult{Checks: checks}
	if SummarizeChecks(checks) != ChecksPassed {
		if !override {
			return res, ErrChecksNotGreen
		}
		res.Overridden = NotGreen(checks)
		if res.Overridden == nil {
			res.Overridden = []PRCheck{}
		}
		if onOverride != nil {
			if err := onOverride(res.Overridden); err != nil {
				return res, fmt.Errorf("record override: %w", err)
			}
		}
	}
	if _, err := a.gh(ctx, "pr", "merge", strconv.Itoa(card.PRNumber), "--merge", "--match-head-commit", card.HeadSHA); err != nil {
		msg := err.Error()
		_ = SetPRMergeError(db, card.ID, msg)
		return res, fmt.Errorf("%w: %s", ErrGitHubRefused, msg)
	}
	merged, err := a.ViewPR(ctx, card.PRNumber)
	if err != nil || merged.MergeCommit == nil || merged.MergeCommit.Oid == "" {
		return res, fmt.Errorf("merged, but could not read the merge commit: %v", err)
	}
	res.MainSHA = merged.MergeCommit.Oid
	_, err = db.Exec(`
		UPDATE ship_review_cards
		SET status = 'approved', approved_sha = ?, main_sha = ?, pr_merge_error = '',
		    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?`, card.HeadSHA, res.MainSHA, card.ID)
	return res, err
}

// FetchOrigin brings origin's refs (and the merge commit) into the repo so the
// post-merge branch cleanup can check ancestry against it.
func FetchOrigin(ctx context.Context, a GHAuth) error {
	_, err := a.git(ctx, "fetch", "--prune", "origin")
	return err
}

// ── CI failures for Send back ───────────────────────────────────────────────

// CheckFailure is one failing check with the log lines that explain it.
type CheckFailure struct {
	Name  string   `json:"name"`
	Link  string   `json:"link,omitempty"`
	Lines []string `json:"lines"`
}

var (
	jobURLRe = regexp.MustCompile(`/actions/runs/(\d+)/job(?:s)?/(\d+)`)
	// failLineRe picks the lines that name what failed in a CI log.
	failLineRe = regexp.MustCompile(`(?i)(--- FAIL|^\s*FAIL\b|\bFAILED\b|✘|✗|panic:|\berror\b[:\[]|##\[error\]|AssertionError|Error: )`)
	// logPrefixRe strips gh's "<job>\t<step>\t<timestamp> " prefix.
	logPrefixRe = regexp.MustCompile(`^[^\t]*\t[^\t]*\t(\d{4}-\d\d-\d\dT[0-9:.]+Z )?`)
)

// maxFailLines caps the log lines kept per failing check.
const maxFailLines = 15

// CheckFailures collects failing checks with their failing log lines. Checks
// that are not GitHub Actions jobs (no job URL) are listed with their link.
func CheckFailures(ctx context.Context, a GHAuth, checks []PRCheck) []CheckFailure {
	var out []CheckFailure
	for _, c := range checks {
		if c.Bucket != "fail" && c.Bucket != "cancel" {
			continue
		}
		f := CheckFailure{Name: c.Name, Link: c.Link, Lines: []string{}}
		if m := jobURLRe.FindStringSubmatch(c.Link); m != nil {
			if log, err := a.gh(ctx, "run", "view", m[1], "--job", m[2], "--log-failed"); err == nil {
				f.Lines = failingLines(log)
			}
		}
		out = append(out, f)
	}
	return out
}

func failingLines(log string) []string {
	var lines []string
	seen := map[string]bool{}
	for _, raw := range strings.Split(log, "\n") {
		line := strings.TrimSpace(logPrefixRe.ReplaceAllString(raw, ""))
		if line == "" || !failLineRe.MatchString(line) || seen[line] {
			continue
		}
		seen[line] = true
		if len(line) > 300 {
			line = line[:300] + "…"
		}
		lines = append(lines, line)
		if len(lines) == maxFailLines {
			break
		}
	}
	return lines
}

// FailuresComment is the pre-filled Send back text for CI failures.
func FailuresComment(card *Card, failures []CheckFailure, pending []PRCheck) string {
	var b strings.Builder
	sha := card.HeadSHA
	if len(sha) > 12 {
		sha = sha[:12]
	}
	fmt.Fprintf(&b, "CI is not green on PR #%d at `%s`. Fix these, push to `%s`, and resubmit the card; checks re-run on the new head.\n", card.PRNumber, sha, card.Branch)
	for _, f := range failures {
		fmt.Fprintf(&b, "\n### ✗ %s\n", f.Name)
		if f.Link != "" {
			fmt.Fprintf(&b, "Log: %s\n", f.Link)
		}
		if len(f.Lines) > 0 {
			b.WriteString("```\n")
			for _, l := range f.Lines {
				b.WriteString(l + "\n")
			}
			b.WriteString("```\n")
		}
	}
	if len(pending) > 0 {
		b.WriteString("\nStill running when sent back:\n")
		for _, p := range pending {
			fmt.Fprintf(&b, "- %s", p.Name)
			if p.Link != "" {
				fmt.Fprintf(&b, " (%s)", p.Link)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}
