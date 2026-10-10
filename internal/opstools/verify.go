package opstools

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// VerifyRequest is a dev_deploy_verify call. Repo is a GitHub "owner/name".
type VerifyRequest struct {
	Repo       string   `json:"repo"`
	SHA        string   `json:"sha"`
	PageChecks []string `json:"page_checks"`
}

const (
	// verifyScript is the MAN-255 gate script; it runs from dev-server.
	verifyScript  = "scripts/verify_dev_deploy.sh"
	verifyTimeout = 5 * time.Minute
	maxPageChecks = 20
)

var (
	pageCheckRe = regexp.MustCompile(`^/[^\s=]*=[^\x00\r\n]+$`)
	ghRepoRe    = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	blobRe      = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// ValidateVerify checks r: the repo must be one of allowed ([gates.ops]
// verify_repos).
func ValidateVerify(r *VerifyRequest, allowed []string) error {
	if !ghRepoRe.MatchString(r.Repo) || strings.Contains(r.Repo, "..") {
		return fmt.Errorf("repo %q is not a GitHub owner/name", r.Repo)
	}
	if !slices.ContainsFunc(allowed, func(a string) bool { return strings.EqualFold(a, r.Repo) }) {
		return fmt.Errorf("repo %s is not in [gates.ops] verify_repos", r.Repo)
	}
	if !shaRe.MatchString(r.SHA) {
		return fmt.Errorf("sha %q is not a 7-40 character hex commit", r.SHA)
	}
	if len(r.PageChecks) == 0 || len(r.PageChecks) > maxPageChecks {
		return fmt.Errorf("page_checks needs 1-%d entries of the form /path=marker", maxPageChecks)
	}
	for _, c := range r.PageChecks {
		if !pageCheckRe.MatchString(c) {
			return fmt.Errorf("page check %q is not /path=marker", c)
		}
	}
	return nil
}

// verifyLineRe picks the lines dev_deploy_verify reports.
var verifyLineRe = regexp.MustCompile(`PASS|FAIL|DEV DEPLOY VERIFIED|NOT ON DEV`)

// ghContent is the part of GitHub's contents API answer the verify flow reads.
type ghContent struct {
	SHA      string `json:"sha"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

// commitRe is a full commit id.
var commitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// fetchScript reads the verify script at the head of branch on GitHub and
// checks that the bytes hash to the blob id GitHub reported. The branch is
// resolved through refs/heads to a commit first, so a tag or other ref an
// agent pushed under the same name cannot stand in for it.
func fetchScript(ctx context.Context, run Runner, repo, branch string) (blob string, body []byte, err error) {
	ref := run(ctx, Cmd{Name: "gh", Args: []string{"api", "repos/" + repo + "/git/ref/heads/" + branch, "--jq", ".object.sha"}})
	commit := strings.TrimSpace(ref.Stdout)
	if ref.ExitCode != 0 || ref.Err != nil || !commitRe.MatchString(commit) {
		return "", nil, fmt.Errorf("branch %s not found on GitHub", branch)
	}
	res := run(ctx, Cmd{Name: "gh", Args: []string{"api", "repos/" + repo + "/contents/" + verifyScript + "?ref=" + commit}})
	if res.ExitCode != 0 || res.Err != nil {
		return "", nil, fmt.Errorf("%s has no %s on GitHub", branch, verifyScript)
	}
	var c ghContent
	if err := json.Unmarshal([]byte(res.Stdout), &c); err != nil || c.Encoding != "base64" || !blobRe.MatchString(c.SHA) {
		return "", nil, fmt.Errorf("unexpected GitHub contents answer for %s", branch)
	}
	body, err = base64.StdEncoding.DecodeString(strings.ReplaceAll(c.Content, "\n", ""))
	if err != nil {
		return "", nil, fmt.Errorf("decode %s: %w", branch, err)
	}
	if gitBlobID(body) != c.SHA {
		return "", nil, fmt.Errorf("%s content does not hash to blob %s", branch, c.SHA)
	}
	return c.SHA, body, nil
}

// gitBlobID is git's SHA-1 object id for a blob holding b.
func gitBlobID(b []byte) string {
	h := sha1.New()
	h.Write([]byte("blob " + strconv.Itoa(len(b)) + "\x00"))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// RunVerify runs the dev-server copy of r.Repo's verify script with bash in
// dir and returns its PASS/FAIL lines and verdict line.
//
// Agents can push dev-server, so the script there is agent-writable, and an
// agent controls its local refs and remotes. The trust anchor is GitHub: the
// script runs only when its blob there equals the blob on the repo's main
// branch on GitHub (Board-merged) or one the Board listed in trusted, and
// the bytes run are the ones hashed.
func RunVerify(ctx context.Context, run Runner, r VerifyRequest, dir string, trusted []string) string {
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	blob, script, err := fetchScript(ctx, run, r.Repo, "dev-server")
	if err != nil {
		return "NOT ON DEV: " + err.Error()
	}
	mainBlob, _, err := fetchScript(ctx, run, r.Repo, "main")
	if err != nil {
		mainBlob = ""
	}
	if blob != mainBlob && !slices.Contains(trusted, blob) {
		return fmt.Sprintf("NOT ON DEV: %s on dev-server (blob %s) differs from main on GitHub and is not in [gates.ops] verify_script_blobs; the Board must review it before it runs", verifyScript, blob)
	}
	// The script checks the local origin/dev-server; refresh it as the
	// manual form did. A failed fetch is left for the script to report.
	_ = run(ctx, Cmd{Name: "git", Args: []string{"fetch", "origin", "dev-server"}, Dir: dir})
	args := append([]string{"-s", "--", r.SHA}, r.PageChecks...)
	res := run(ctx, Cmd{Name: "bash", Args: args, Dir: dir, Stdin: script})
	return summarizeVerify(res)
}

// summarizeVerify keeps the verdict lines; with none, the output's tail. A
// run that ends without a verdict line gets a NOT ON DEV one.
func summarizeVerify(res Result) string {
	all := strings.Split(strings.TrimRight(res.Output, "\n"), "\n")
	var keep []string
	for _, l := range all {
		if verifyLineRe.MatchString(l) {
			keep = append(keep, l)
		}
	}
	if len(keep) == 0 {
		if len(all) > 40 {
			all = all[len(all)-40:]
		}
		keep = all
	}
	last := strings.TrimSpace(all[len(all)-1])
	if !strings.HasPrefix(last, "DEV DEPLOY VERIFIED") && !strings.HasPrefix(last, "NOT ON DEV") {
		keep = append(keep, fmt.Sprintf("NOT ON DEV: verify script ended without a verdict (exit %d)", res.ExitCode))
	}
	out := Result{Output: strings.Join(keep, "\n"), ExitCode: res.ExitCode, Err: res.Err}
	return out.Format()
}
