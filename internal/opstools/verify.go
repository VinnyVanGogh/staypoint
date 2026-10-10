package opstools

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// VerifyRequest is a dev_deploy_verify call.
type VerifyRequest struct {
	Repo       string   `json:"repo"`
	SHA        string   `json:"sha"`
	PageChecks []string `json:"page_checks"`
}

const (
	// verifyScript is the MAN-255 gate script; it runs from origin/dev-server.
	verifyScript  = "scripts/verify_dev_deploy.sh"
	verifyTimeout = 5 * time.Minute
	maxPageChecks = 20
)

var (
	pageCheckRe = regexp.MustCompile(`^/[^\s=]*=[^\x00\r\n]+$`)
	blobRe      = regexp.MustCompile(`^[0-9a-f]{40,64}$`)
)

// ValidateVerify checks r and resolves its repo.
func ValidateVerify(r *VerifyRequest, defaultRepo string) error {
	repo, err := RepoDir(r.Repo, defaultRepo)
	if err != nil {
		return err
	}
	r.Repo = repo
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

// RunVerify runs the dev-server copy of the verify script with bash and
// returns its PASS/FAIL lines and verdict line.
//
// Agents can push dev-server, so the script there is agent-writable: it runs
// only when its blob is the one on origin/main (Board-merged) or one the
// Board listed in trusted. The blob is read by id, so the bytes checked are
// the bytes run.
func RunVerify(ctx context.Context, run Runner, r VerifyRequest, trusted []string) string {
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	git := func(args ...string) Result {
		return run(ctx, Cmd{Name: "git", Args: append([]string{"-C", r.Repo}, args...)})
	}
	if res := git("fetch", "origin", "dev-server", "main"); res.ExitCode != 0 || res.Err != nil {
		return "NOT ON DEV: could not fetch origin dev-server and main\n" + res.Format()
	}
	dev := git("rev-parse", "--verify", "-q", "origin/dev-server:"+verifyScript)
	blob := strings.TrimSpace(dev.Stdout)
	if dev.ExitCode != 0 || !blobRe.MatchString(blob) {
		return fmt.Sprintf("NOT ON DEV: origin/dev-server has no %s", verifyScript)
	}
	mainBlob := strings.TrimSpace(git("rev-parse", "--verify", "-q", "origin/main:"+verifyScript).Stdout)
	if blob != mainBlob && !slices.Contains(trusted, blob) {
		return fmt.Sprintf("NOT ON DEV: %s on origin/dev-server (blob %s) differs from origin/main and is not in [gates.ops] verify_script_blobs; the Board must review it before it runs", verifyScript, blob)
	}
	script := git("cat-file", "blob", blob)
	if script.ExitCode != 0 || script.Err != nil || script.Stdout == "" {
		return "NOT ON DEV: could not read the verify script\n" + script.Format()
	}
	args := append([]string{"-s", "--", r.SHA}, r.PageChecks...)
	res := run(ctx, Cmd{Name: "bash", Args: args, Dir: r.Repo, Stdin: []byte(script.Stdout)})
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
