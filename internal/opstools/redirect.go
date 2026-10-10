package opstools

import (
	"regexp"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/config"
)

// sshValueFlags are the ssh options that take a separate value.
var sshValueFlags = map[string]bool{
	"-b": true, "-B": true, "-c": true, "-D": true, "-E": true, "-e": true, "-F": true,
	"-I": true, "-i": true, "-J": true, "-L": true, "-l": true, "-m": true, "-O": true,
	"-o": true, "-p": true, "-P": true, "-Q": true, "-R": true, "-S": true, "-W": true, "-w": true,
}

var (
	// stateDirRe is StayPoint's data dir spelled the ways agents spell it.
	stateDirRe = regexp.MustCompile(`(~|\$HOME|\$\{HOME\}|/Users/[^/\s'"]+|/home/[^/\s'"]+|/var/root)/\.staypoint(/|\b)`)
	// stateReaderRe is a command that only reads what it is given.
	stateReaderRe = regexp.MustCompile(`^(cat|ls|head|tail|less|more|sqlite3|grep|rg|find|jq|wc|stat|file)$`)
	// heredocToTmpRe is `cat > /tmp/x.md <<EOF` and `cat <<EOF > /tmp/x.md`.
	heredocToTmpRe = regexp.MustCompile(`(?s)\bcat\s+(?:>\s*` + tmpTextFile + `\s*<<|<<-?\s*['"]?\w+['"]?\s*>\s*` + tmpTextFile + `)`)
	// ghMergeRe is `gh [global flags] pr merge`.
	ghMergeRe = regexp.MustCompile(`\bgh\s+(?:-\S+\s+(?:[^-\s]\S*\s+)?)*pr\s+merge\b`)
)

const tmpTextFile = `['"]?(?:/tmp|/private/tmp|/var/folders|\$TMPDIR|\$\{TMPDIR\})/\S*\.(?:md|txt)['"]?`

// BashRedirect says which ops tool replaces a Bash command the Board kept
// approving, or "" when none does. The hook denies such a command with this
// pointer instead of holding it for the Board.
func BashRedirect(cmd string, hosts config.HostClasses) string {
	if host := sshDevHost(cmd, hosts); host != "" {
		return "use mcp__staypoint__dev_host_run (host=" + host + ", action=git_status|git_log|ls|cat_file|journal_tail|http_get_local|systemctl_status|git_ff_pull|collectstatic|restart|pip_sync) instead of ssh to dev host " + host + ": it declares its effect, so reads and dev deploys run without waiting for the Board, and its output is redacted"
	}
	if strings.Contains(cmd, "verify_dev_deploy.sh") {
		return "use mcp__staypoint__dev_deploy_verify (repo, sha, page_checks) instead of running verify_dev_deploy.sh from a shell: it returns the PASS/FAIL lines and the DEV DEPLOY VERIFIED / NOT ON DEV verdict"
	}
	if stateDirRe.MatchString(cmd) && stateReaderRe.MatchString(firstWord(cmd)) {
		return "use mcp__staypoint__staypoint_query (query=task|comments|documents|document|handoffs|handoff|run_steps|run_errors|gate_requests) instead of reading ~/.staypoint from a shell"
	}
	if heredocToTmpRe.MatchString(cmd) {
		return "pass the text directly instead of staging it in a temp file: mcp__staypoint__task_comment (text), mcp__staypoint__pr_body (repo, pr, text) or mcp__staypoint__task_doc (key, text)"
	}
	if ghMergeRe.MatchString(cmd) {
		return "use mcp__staypoint__pr_merge (repo, pr, base) instead of gh pr merge: a dev-server/dev base merges under the unattended policy, main/prod goes to the Board"
	}
	return ""
}

// firstWord is the program a command line starts with, past env
// assignments and a leading path.
func firstWord(cmd string) string {
	for _, f := range strings.Fields(cmd) {
		if strings.Contains(f, "=") && !strings.HasPrefix(f, "-") && !strings.HasPrefix(f, "/") {
			continue
		}
		if i := strings.LastIndexByte(f, '/'); i >= 0 {
			f = f[i+1:]
		}
		return f
	}
	return ""
}

// sshDevHost returns the dev host an ssh in cmd connects to, or "". Only a
// dev destination is redirected; a prod or unknown one is left to the
// classifier, which holds it.
func sshDevHost(cmd string, hosts config.HostClasses) string {
	fields := strings.Fields(cmd)
	for i, f := range fields {
		if f != "ssh" && !strings.HasSuffix(f, "/ssh") {
			continue
		}
		for j := i + 1; j < len(fields); j++ {
			a := strings.Trim(fields[j], `'"`)
			if a == "--" {
				continue
			}
			if strings.HasPrefix(a, "-") {
				if sshValueFlags[a] {
					j++
				}
				continue
			}
			dest := strings.TrimPrefix(a, "ssh://")
			if k := strings.LastIndexByte(dest, '@'); k >= 0 {
				dest = dest[k+1:]
			}
			if k := strings.IndexByte(dest, ':'); k >= 0 {
				dest = dest[:k]
			}
			if hosts.IsDevHost(dest) {
				return dest
			}
			break
		}
	}
	return ""
}
