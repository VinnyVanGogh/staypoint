package security

import (
	"net"
	"net/url"
	"regexp"
	"strings"
)

// Board rules for unattended runs (task-33692ffb). Under an organization
// trust these always wait for the Board, on top of the trust exclusions in
// AnalyzeForTrust (protected merges and pushes, deletes outside the
// worktree):
//   - prod writes, deploys and renders (prod reads are fine);
//   - external API writes (POST/PUT/PATCH/DELETE to a non-local host);
//   - destructive deletes of real data (SQL DELETE/DROP/TRUNCATE, API and
//     cloud deletes);
//   - sending PII (mail, uploads to other hosts, payloads carrying an email
//     address or SSN-like number);
//   - self-protection: rebuilding, reinstalling, restarting or replacing
//     StayPoint, touching ~/.staypoint (tokens, DB, config), writing to the
//     local StayPoint API, or editing agent guard config (hooks, settings,
//     LaunchAgents).
//
// The check reads the command text and its scripts' snapshots, not the
// parse tree: it is a deliberately broad backstop, so a false match only
// means the Board is asked. Opening a pull request is not held; merging is
// held by AnalyzeForTrust.

var (
	prodWordRe  = regexp.MustCompile(`(?i)(\bprod\b|\bproduction\b|--prod\b|\bprd\b|[-_.]prod\b|\bprod[-_.])`)
	prodWriteRe = regexp.MustCompile(`(?i)\b(deploy|render|apply|publish|release|rollout|migrate|upgrade|install|restart|scale|push|sync|upload|put|post|patch|delete|drop|truncate|insert|update|create|set|write|exec|rm|cp|mv|kill|destroy|promote)\b`)
	// deployRe holds deploys whatever their target names: a deploy that
	// does not say prod may still be prod.
	deployRe = regexp.MustCompile(`(?i)(\b(wrangler|vercel|netlify|fly|flyctl|firebase|heroku|railway|render|serverless|sls|cdk|sam|amplify|eb|gcloud\s+app|supabase\s+functions)\b[\s\S]*\bdeploy\b|\bterraform\s+(apply|destroy|import)\b|\bpulumi\s+(up|destroy)\b|\bkubectl\s+(apply|create|delete|replace|patch|scale|rollout|set|edit)\b|\bhelm\s+(install|upgrade|uninstall|rollback)\b|\bsupabase\s+db\s+push\b|\bansible-playbook\b|\bdeploy[\w.-]*\.sh\b|\bdocker\s+push\b)`)

	httpClientRe = regexp.MustCompile(`(?i)(^|[\s;&|(` + "`" + `])(curl|wget|http|https|xh|httpie)\b`)
	httpWriteRe  = regexp.MustCompile(`(?i)(-X\s*['"]?(POST|PUT|PATCH|DELETE)\b|--request\s*[= ]\s*['"]?(POST|PUT|PATCH|DELETE)\b|--method\s*[= ]\s*['"]?(POST|PUT|PATCH|DELETE)\b|(^|\s)(-d|--data[\w-]*|-F|--form[\w-]*|--json|-T|--upload-file|--post-data|--post-file|--body-data|--body-file)(\s|=|$)|(^|\s)(POST|PUT|PATCH|DELETE)\s+\S|\s[\w.-]+:?=\S)`)
	codeWriteRe  = regexp.MustCompile(`(?i)(requests\.(post|put|patch|delete)\(|httpx\.(post|put|patch|delete)\(|urllib\.request\.urlopen\([^)]*data|method\s*[:=]\s*['"](POST|PUT|PATCH|DELETE)['"]|axios\.(post|put|patch|delete)\(|http\.(Post|PostForm|NewRequest)\(|Net::HTTP::(Post|Put|Patch|Delete)|\.(post|put|patch|delete)\(\s*['"]https?://)`)
	urlRe        = regexp.MustCompile(`(?i)\bhttps?://[^\s'"<>` + "`" + `]+`)

	ghAPIWriteRe = regexp.MustCompile(`(?i)\bgh\s+api\b[\s\S]*(-X\s*['"]?(POST|PUT|PATCH|DELETE)\b|--method\s*[= ]?\s*['"]?(POST|PUT|PATCH|DELETE)\b|\s(-f|-F|--field|--raw-field|--input)(\s|=))`)
	ghWriteRe    = regexp.MustCompile(`(?i)\bgh\s+(repo\s+(delete|archive|rename|edit|create)|release\s+(create|delete|upload|edit)|secret\s+(set|delete|remove)|variable\s+(set|delete)|issue\s+(create|delete|close|comment|edit|transfer)|gist\s+(create|delete|edit)|workflow\s+(run|enable|disable)|run\s+(rerun|cancel|delete)|ruleset|label\s+(create|delete|edit)|pr\s+(close|comment|review))\b`)

	sqlDeleteRe   = regexp.MustCompile(`(?i)(\bdelete\s+from\b|\bdrop\s+(table|database|schema|index|view|collection)\b|\btruncate\s+(table\s+)?\w|\bdeleteMany\b|\bdropDatabase\b|\bflushall\b|\bflushdb\b)`)
	cloudDeleteRe = regexp.MustCompile(`(?i)(\baws\s+\S+\s+(rm|delete[\w-]*|terminate[\w-]*)\b|\bgsutil\s+(rm|rb)\b|\bgcloud\b[\s\S]*\bdelete\b|\baz\b[\s\S]*\bdelete\b|\bdocker\s+(volume\s+rm|system\s+prune|volume\s+prune)\b|\bdropdb\b|\bwrangler\b[\s\S]*\bdelete\b|\bsupabase\s+db\s+reset\b|\bgit\s+push\b[\s\S]*(--delete|\s:\S))`)

	mailRe   = regexp.MustCompile(`(?i)(^|[\s;&|(/])(sendmail|mail|mailx|mutt|msmtp|swaks|agentmail)\b|\bsmtplib\b|\bnodemailer\b|\bsmtp://|\bsmtps://`)
	uploadRe = regexp.MustCompile(`(?i)(^|[\s;&|(])(scp|sftp|nc|ncat|netcat|socat|ftp|rclone\s+(copy|sync|move))\b|\brsync\b[^|;&]*\s[\w.@-]+:`)
	emailRe  = regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`)
	ssnRe    = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)

	// selfCmdRe: commands that rebuild, reinstall, restart or replace
	// StayPoint or the agent's own guards.
	selfCmdRe = regexp.MustCompile(`(?i)(reinstall-daemon|\blaunchctl\b|\bgo\s+install\b[^;&|\n]*staypoint|\bgo\s+(install|build)\b[^;&|\n]*\.local/bin|\bgo\s+build\b[^;&|\n]*-o[\s=]*\S*staypoint|\b(cp|mv|ln|install|rsync|ditto)\b[^;&|\n]*\.local/bin|\b(kill|pkill|killall)\b[^;&|\n]*staypoint|\bbrew\s+(re)?install\b[^;&|\n]*staypoint)`)
	// selfPathRe: paths StayPoint and the agent guards depend on. Any
	// reference holds, reads included: they hold tokens and the guards.
	selfPathRe = regexp.MustCompile(`(?i)((~|\$HOME|\$\{HOME\}|/Users/[^/\s'"]+|/home/[^/\s'"]+|/var/root)/\.staypoint\b|\.claude/settings[\w.-]*\.json|\.claude/hooks\b|\.gemini/settings|\.gemini/hooks\b|Library/LaunchAgents\b)`)
)

// progAt is a regexp prefix for "at a program position": a line or segment
// start, then any env assignments and wrappers (env, command, exec, nohup,
// sudo, npx, ...), then an optional backslash and directory, so `\ssh`,
// `/usr/bin/ssh` and `command ssh` read as ssh.
const progAt = `(^|[;&|(\n{` + "`" + `]|\$\(|\bthen\b|\bdo\b|\belse\b)\s*` +
	`(([A-Za-z_]\w*=\S*|env|command|builtin|exec|nohup|sudo|doas|time|nice|ionice|caffeinate|timeout\s+\S+|xargs|npx|bunx|pnpm\s+dlx|yarn\s+dlx|npm\s+exec|uvx|pipx\s+run)(\s+-\S+)*\s+)*` +
	`\\?([^\s;&|]*/)?`

var (
	// nestedAgentRe: a daemon-run agent starting another agent CLI escapes
	// its own hook and task (task-9d94997c).
	nestedAgentRe = regexp.MustCompile(`(?i)` + progAt + `(claude|claude-code|gemini|codex|agy|cursor-agent|aider|@anthropic-ai/claude-code|@google/gemini-cli|@openai/codex)(@\S*)?(\s|$|[;&|)` + "`" + `])`)
	// stayEnvRe: unsetting or overriding the StayPoint env the hook relies on.
	stayEnvRe = regexp.MustCompile(`(?i)(\bunset\b[^;&|\n]*STAYPOINT_|\benv\b[^;&|\n]*(-u\s*STAYPOINT_|--unset[= ]STAYPOINT_|\s-i\b|\s--ignore-environment\b|\s-(\s|$))|(^|[\s;&|(])(export\s+)?STAYPOINT_\w*=|\bexport\s+-n\s+STAYPOINT_)`)
	// remoteShellRe: a shell on another machine may write prod or delete
	// data, and a tunnel exposes local services. Held whatever it runs.
	remoteShellRe = regexp.MustCompile(`(?i)(` + progAt + `(ssh|mosh|autossh)(\s|$)|\bgcloud\s+compute\s+(ssh|scp)\b|\baws\s+ssm\s+(start-session|send-command)\b|\bkubectl\s+(exec|attach|port-forward)\b|\bdocker\s+(-H|--host|context)\b)`)
	// indirectRe: a program word that is a variable, a substitution or eval
	// cannot be checked (` + "`" + `GIT=git; $GIT push origin main` + "`" + `).
	indirectRe = regexp.MustCompile(`(^|[;&|(\n{]|\$\(|\bthen\b|\bdo\b|\belse\b)\s*([A-Za-z_]\w*=\S*\s+)*(\$|` + "`" + `|eval\b|exec\s+\$)`)
	// dataFileRe: SQL fed from a file may delete anything.
	dataFileRe = regexp.MustCompile(`(?i)(\b(sqlite3|psql|mysql|mariadb|duckdb|mongosh|mongo|redis-cli|clickhouse-client)\b[^;&|\n]*(<|\s-f\s|\s--file|\.read\b|\s-e\s+source\b))`)
	// methodRe: an HTTP method chosen at run time cannot be checked.
	methodRe     = regexp.MustCompile(`(?i)(requests\.request\(|httpx\.request\(|urllib\.request\.Request\([^)]*data\s*=|method\s*[:=]\s*[A-Za-z_$]|fetch\([^)]*\{[^}]*method\s*:|axios\(\s*\{|axios\.request\()`)
	vercelProdRe = regexp.MustCompile(`(?i)\b(vercel|netlify)\b[^;&|\n]*--prod\b`)
)

// StayPointPort is the local StayPoint API port; writes to it are
// self-protection, not a local call.
const StayPointPort = "41421"

// bareStayPointRe is the StayPoint API written without a scheme.
var bareStayPointRe = regexp.MustCompile(`(?i)(127\.0\.0\.1|localhost|\[::1\]|0\.0\.0\.0):41421\b`)

// SelfProtectedPath reports why editing path (absolute, cleaned) breaks the
// self-protection rule, or "".
func SelfProtectedPath(path string) string {
	switch {
	case selfPathRe.MatchString(path):
		return "edits StayPoint state or agent guard config (Board rule: self-protection)"
	case strings.Contains(path, "/.local/bin/") || strings.HasSuffix(path, "/.local/bin"):
		return "replaces a binary in ~/.local/bin (Board rule: self-protection)"
	}
	return ""
}

// AnalyzeBoardRules returns why line (or a script it runs) breaks a Board
// rule for unattended runs, or "" when it does not.
func AnalyzeBoardRules(line string, scripts []ScriptHash) string {
	// Source text inside a pure-edit Python heredoc is data being written,
	// not commands being run (task-31dea40b).
	if why := boardRuleText(stripPureEditBodies(line), "command"); why != "" {
		return why
	}
	for _, s := range scripts {
		if why := boardRuleText(s.Content, "script "+s.Path); why != "" {
			return why
		}
	}
	return ""
}

func boardRuleText(code, what string) string {
	if strings.TrimSpace(code) == "" {
		return ""
	}
	switch {
	case nestedAgentRe.MatchString(code) || stayEnvRe.MatchString(code):
		return what + " starts a nested agent or changes the StayPoint env (Board rule: self-protection: nested agent or StayPoint env change)"
	case indirectRe.MatchString(code):
		return what + " runs an indirect command ($VAR, $(...), eval) that cannot be checked"
	case remoteShellRe.MatchString(code):
		return what + " opens a remote shell or tunnel, which may write prod or delete data (Board rule: no prod writes or deletes)"
	case selfCmdRe.MatchString(code):
		return what + " rebuilds, restarts or replaces StayPoint or its guards (Board rule: self-protection)"
	case selfPathRe.MatchString(code):
		return what + " touches StayPoint state or agent guard config (Board rule: self-protection)"
	case localAPIWrite(code):
		return what + " writes to the local StayPoint API (Board rule: self-protection)"
	case deployRe.MatchString(code) || vercelProdRe.MatchString(code):
		return what + " deploys (Board rule: no prod writes or deploys)"
	case prodWordRe.MatchString(code) && prodWriteRe.MatchString(code):
		return what + " may write to prod (Board rule: prod reads only)"
	case sqlDeleteRe.MatchString(code) || cloudDeleteRe.MatchString(code) || dataFileRe.MatchString(code):
		return what + " may delete real data (Board rule: no destructive deletes)"
	case ghAPIWriteRe.MatchString(code) || ghWriteRe.MatchString(code):
		return what + " writes to the GitHub API (Board rule: no external API writes)"
	case methodRe.MatchString(code):
		return what + " makes an HTTP call whose method cannot be checked (Board rule: external reads only)"
	case externalHTTPWrite(code):
		return what + " writes to an external API (Board rule: external reads only)"
	case mailRe.MatchString(code):
		return what + " sends mail (Board rule: no sending PII)"
	case uploadRe.MatchString(code):
		return what + " sends data to another host (Board rule: no sending PII)"
	case (httpClientRe.MatchString(code) || urlRe.MatchString(code)) && (emailRe.MatchString(code) || ssnRe.MatchString(code)):
		return what + " sends what looks like PII (Board rule: no sending PII)"
	}
	return ""
}

// externalHTTPWrite reports an HTTP write (curl -d, -X POST, requests.post,
// ...) to a non-local host. A write with no URL it can read counts as
// external (fail closed).
func externalHTTPWrite(code string) bool {
	if !httpWrite(code) {
		return false
	}
	urls := urlRe.FindAllString(code, -1)
	if len(urls) == 0 {
		return true
	}
	for _, u := range urls {
		if !isLocalURL(u) {
			return true
		}
	}
	return false
}

// httpWrite reports an HTTP write in code (curl -d, -X POST, requests.post).
func httpWrite(code string) bool {
	return (httpClientRe.MatchString(code) && httpWriteRe.MatchString(code)) || codeWriteRe.MatchString(code)
}

// localAPIWrite reports an HTTP write to the StayPoint API on this machine.
func localAPIWrite(code string) bool {
	if !httpWrite(code) {
		return false
	}
	if bareStayPointRe.MatchString(code) {
		return true
	}
	for _, u := range urlRe.FindAllString(code, -1) {
		if isStayPointURL(u) {
			return true
		}
	}
	return false
}

func isStayPointURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Port() == StayPointPort && isLocalURL(raw)
}

func isLocalURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
