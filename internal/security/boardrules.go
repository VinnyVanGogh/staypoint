package security

import (
	"net"
	"net/url"
	"os"
	"path"
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

	// selfCmdRe: commands that reinstall, restart or replace the installed
	// StayPoint or the agent's own guards. Building a test binary under /tmp
	// or the worktree is not one, and neither is killing a test server
	// (staypoint-apitest-server): only staypoint and staypointd themselves.
	// Running the reinstall script is reinstallExec, launchctl runsLaunchctl.
	// A cd (or -C) into ~/.local makes a relative `go build -o staypointd`
	// or `cp x staypoint` replace the installed binary.
	selfCmdRe = regexp.MustCompile(`(?i)(\b(cd|pushd)\b[^;&|\n]*(\.local\b|[*?\[{])|\s-C\s*['"]?[^\s;&|]*\.local\b|\bgo\s+install\b[^;&|\n]*staypoint|\bgo\s+(install|build)\b[^;&|\n]*\.local/bin|\b(cp|mv|ln|install|rsync|ditto)\b[^;&|\n]*\.local/bin|\b(kill|pkill|killall)\b[^;&|\n]*\bstaypointd?([^\w-]|$)|\bbrew\s+(re)?install\b[^;&|\n]*staypoint)`)
	// selfPathRe: paths StayPoint and the agent guards depend on. Any
	// reference holds, reads included: they hold tokens and the guards. The
	// one exception is reading the run's own handoff files (ownHandoffRead).
	selfPathRe = regexp.MustCompile(`(?i)((~|\$HOME|\$\{HOME\}|/Users/[^/\s'"]+|/home/[^/\s'"]+|/var/root)/\.staypoint\b|\.claude/settings[\w.-]*\.json|\.claude/hooks\b|\.gemini/settings|\.gemini/hooks\b|\.gemini/config/hooks\b|Library/LaunchAgents\b)`)
)

// progAt is a regexp prefix for "at a program position": a line or segment
// start, then any env assignments and wrappers (env, command, exec, nohup,
// sudo, npx, ...), then an optional backslash and directory, so `\ssh`,
// `/usr/bin/ssh` and `command ssh` read as ssh.
const gateVarPat = `STAYPOINT_(TASK_ID|SESSION_ID|RUN_ID|HOOK\w*|SKIP\w*|SCRATCH_ROOT|REPO_ROOT|CLI_BIN|CLAUDE_BIN|CODEX_BIN|AGY_BIN|GEMINI_BIN|LOCAL_URL|GATE\w*|TRUST\w*|SECURITY\w*)`

const progAt = `(^|[;&|(\n{` + "`" + `]|\$\(|\bthen\b|\bdo\b|\belse\b)\s*` +
	`(([A-Za-z_]\w*=\S*|env|command|builtin|exec|nohup|sudo|doas|time|nice|ionice|caffeinate|timeout\s+\S+|xargs|npx|bunx|pnpm\s+dlx|yarn\s+dlx|npm\s+exec|uvx|pipx\s+run)(\s+-\S+)*\s+)*` +
	`\\?([^\s;&|]*/)?`

var (
	// nestedAgentRe: a daemon-run agent starting another agent CLI escapes
	// its own hook and task (task-9d94997c).
	nestedAgentRe = regexp.MustCompile(`(?i)` + progAt + `(claude|claude-code|gemini|codex|agy|cursor-agent|aider|@anthropic-ai/claude-code|@google/gemini-cli|@openai/codex)(@\S*)?(\s|$|[;&|)` + "`" + `])`)
	// stayEnvRe: unsetting or overriding the StayPoint env the gate relies
	// on (gateVarPat). The fallback when stayEnvChange cannot parse the code.
	stayEnvRe = regexp.MustCompile(`(\bunset\b[^;&|\n]*\b` + gateVarPat + `|\benv\b[^;&|\n]*(-u\s*` + gateVarPat + `|--unset[= ]` + gateVarPat + `|\s-i\b|\s--ignore-environment\b|\s-(\s|$))|(^|[\s;&|('"])(export\s+)?` + gateVarPat + `=|\b(export|declare|typeset|local|readonly)\b[^;&|\n]*\b` + gateVarPat + `)`)
	// gateVarRe: a StayPoint variable whose value changes what the gate sees
	// or who it thinks is asking: the task and session, the hook and agent
	// binaries, scratch and repo roots. Others (STAYPOINT_UI_*, a test
	// server's tokens, an audit's output dir) only configure the command.
	gateVarRe = regexp.MustCompile(`^` + gateVarPat + `$`)
	// remoteShellRe: a shell on another machine may write prod or delete
	// data, and a tunnel exposes local services. Held whatever it runs.
	remoteShellRe = regexp.MustCompile(`(?i)(` + progAt + `(ssh|mosh|autossh)(\s|$)|\bgcloud\s+compute\s+(ssh|scp)\b|\baws\s+ssm\s+(start-session|send-command)\b|\bkubectl\s+(exec|attach|port-forward)\b|\bdocker\s+(-H|--host|context)\b)`)
	// indirectRe: a program word that is a variable, a substitution or eval
	// cannot be checked (` + "`" + `GIT=git; $GIT push origin main` + "`" + `).
	indirectRe = regexp.MustCompile(`(^|[;&|(\n{]|\$\(|\bthen\b|\bdo\b|\belse\b)\s*([A-Za-z_]\w*=\S*\s+)*(\$|` + "`" + `|eval\b|exec\s+\$)`)
	// gitShellAliasRe: git -c alias.x='!cmd' runs cmd in a shell.
	gitShellAliasRe = regexp.MustCompile(`(?i)\bgit\b[^;&|\n]*\s-c\s*['"]?alias\.[^=\s]*=\s*['"]?!`)
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
	return AnalyzeBoardRulesForTask("", line, scripts)
}

// AnalyzeBoardRulesForTask is AnalyzeBoardRules for a run of taskID, which
// may read its own handoff files (~/.staypoint/handoffs/<taskID>/).
func AnalyzeBoardRulesForTask(taskID, line string, scripts []ScriptHash) string {
	if why := boardRuleText(line, "command", taskID); why != "" {
		return why
	}
	for _, s := range scripts {
		// Comments are read too: a script can run its own comments
		// (bash -c "$(sed 's/^#//' "$0")").
		if why := boardRuleText(s.Content, "script "+s.Path, ""); why != "" {
			return why
		}
	}
	return ""
}

func boardRuleText(code, what, taskID string) string {
	if strings.TrimSpace(code) == "" {
		return ""
	}
	switch {
	case runsAgent(code, 0) || stayEnvChange(code, 0):
		return what + " starts a nested agent or changes the StayPoint env (Board rule: self-protection: nested agent or StayPoint env change)"
	case indirectRe.MatchString(strings.ReplaceAll(code, "$((", "$_A_")) || gitShellAliasRe.MatchString(code):
		// $(( is arithmetic, not a subshell whose first word is $...
		return what + " runs an indirect command ($VAR, $(...), eval) that cannot be checked"
	case inputShell(code, 0):
		return what + " runs shell commands it reads from input or a file, which cannot be checked"
	case remoteShellRe.MatchString(code):
		return what + " opens a remote shell or tunnel, which may write prod or delete data (Board rule: no prod writes or deletes)"
	case selfCmdRe.MatchString(code) || runsLaunchctl(code, 0) || reinstallExec(code, 0) || killsSelf(code, 0) || runsDaemon(code, 0):
		return what + " rebuilds, restarts or replaces StayPoint or its guards (Board rule: self-protection)"
	case selfPathRe.MatchString(code) && !ownHandoffRead(code, taskID):
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

// The self-protection checks below read the parsed command, not its text,
// so a protected name that is only an argument (git add, sed -n, bash -n)
// does not hold (task-6e2bcd75). Each one fails closed where it cannot
// parse.

// boardWrappers are wrappers whose real command follows, on top of the
// classifier's wrappers.
var boardWrappers = map[string]bool{"sudo": true, "doas": true, "caffeinate": true, "parallel": true,
	"npx": true, "bunx": true, "uvx": true}

// shellKeywords start a command without being one: `do unset X` runs unset.
var shellKeywords = map[string]bool{"if": true, "while": true, "until": true, "do": true, "then": true,
	"else": true, "elif": true, "!": true, "{": true, "time": true}

func dropKeywords(argv []string) []string {
	for len(argv) > 0 && shellKeywords[argv[0]] {
		argv = argv[1:]
	}
	return argv
}

// unwrapArgv drops leading assignments and wrapper commands (env, nohup,
// timeout, xargs, sudo, ...). viaXargs reports xargs or parallel: the
// command's arguments then come from its input.
func unwrapArgv(argv []string) (out []string, viaXargs bool) {
	argv = stripPrefixes(dropKeywords(argv))
	for d := 0; len(argv) > 0 && d < maxDepth; d++ {
		name := baseCmd(argv)
		if !wrappers[name] && !boardWrappers[name] {
			break
		}
		if name == "xargs" || name == "parallel" {
			viaXargs = true
		}
		argv = stripPrefixes(skipWrapper(name, argv[1:]))
		// Package runners: pnpm dlx, yarn dlx, npm exec, pipx run.
		for len(argv) > 1 && pkgRunners[baseCmd(argv)+" "+argv[1]] {
			argv = stripPrefixes(skipWrapper("npx", argv[2:]))
		}
	}
	return argv, viaXargs
}

var agentWordRe = regexp.MustCompile(`(?i)\b(claude|claude-code|gemini|codex|agy|cursor-agent|aider)\b`)

var exportLike = map[string]bool{"export": true, "declare": true, "typeset": true, "local": true, "readonly": true}

var pkgRunners = map[string]bool{"pnpm dlx": true, "yarn dlx": true, "npm exec": true, "pipx run": true}

// runsAgent reports a nested agent CLI that may run (task-9d94997c):
// nestedAgentRe finds the name at a program position in the text, and the
// parse confirms it is not only data, such as a grep pattern 'a|claude' or
// a commit message.
func runsAgent(code string, depth int) bool {
	if !agentWordRe.MatchString(code) {
		return false
	}
	if depth > maxDepth {
		return true
	}
	segs, subs, err := parseShell(code)
	if err != nil {
		return true
	}
	for _, s := range subs {
		if runsAgent(s, depth+1) {
			return true
		}
	}
	for _, s := range segs {
		argv, viaXargs := unwrapArgv(s.argv)
		// GIT_PAGER=claude, EDITOR='sh -c codex': a variable naming an
		// agent may run it, set for one command, the line or exported.
		if assignRunsAgent(s.argv[:len(s.argv)-len(argv)], depth) ||
			(len(argv) > 0 && exportLike[baseCmd(argv)] && assignRunsAgent(argv[1:], depth)) {
			return true
		}
		if len(argv) == 0 {
			continue
		}
		if nestedAgentRe.MatchString(argv[0]) {
			return true
		}
		if shells[baseCmd(argv)] {
			if ci, ok := shellCommandArg(baseCmd(argv), argv[1:]); ok && runsAgent(argv[1+ci], depth+1) {
				return true
			}
			for _, r := range s.redirects {
				if r.heredoc && runsAgent(r.body, depth+1) {
					return true
				}
			}
			continue
		}
		// Written to a file, the name may run later in this same line,
		// before the hook could read the file.
		if writesFile(s) && anyNames(nestedAgentRe, s.argv) {
			return true
		}
		// git grep -O'claude -p x #': the pager value runs, glued to its flag.
		if baseCmd(argv) == "git" && gitRunsProgram(gitSub(argv[1:])) {
			return true
		}
		if !viaXargs && !writesFile(s) && dataArgs(argv) {
			continue
		}
		// Anything else that carries the name (watch 'x | claude', a
		// heredoc to python) is held, as the text check did.
		if nestedAgentRe.MatchString(strings.Join(s.argv, " ")) {
			return true
		}
		for _, r := range s.redirects {
			if r.heredoc && nestedAgentRe.MatchString(r.body) {
				return true
			}
		}
	}
	return false
}

func assignRunsAgent(words []string, depth int) bool {
	for _, a := range words {
		if isAssign(a) && runsAgent(a[strings.IndexByte(a, '=')+1:], depth+1) {
			return true
		}
	}
	return false
}

// dataArgs reports a program whose arguments are only data (patterns,
// messages, paths), never commands it runs.
func dataArgs(argv []string) bool {
	if argv[0] != baseCmd(argv) {
		return false
	}
	name, args := argv[0], argv[1:]
	switch {
	case nameReaders[name] || name == "staypoint" || name == "jq":
		return true
	case name == "rg":
		for _, a := range args {
			if strings.HasPrefix(a, "--pre") {
				return false
			}
		}
		return true
	case name == "sed":
		return sedNamesOnly(nestedAgentRe, args)
	case name == "git":
		return gitNamesOnly(nestedAgentRe, args)
	case name == "go":
		return len(args) > 0 && greenGoNames[args[0]] && goTestOrVet(append([]string{"go", "test"}, args[1:]...))
	case name == "gh":
		for _, a := range args {
			if a == "alias" || a == "extension" {
				return false
			}
		}
		return true
	}
	return false
}

// shellCommandArg returns the index of shell's -c command string in its
// args.
func shellCommandArg(shell string, args []string) (int, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		// Flags whose value is always the next word. -O takes one in bash
		// (sh -O -c x fails on the option name) but is a plain option in zsh.
		case a == "-o" || a == "+o" || a == "--rcfile" || a == "--init-file",
			(a == "-O" || a == "+O") && shell != "zsh":
			i++
		case a == "--" || (!strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+")):
			return 0, false
		case !strings.HasPrefix(a, "--") && strings.Contains(a, "c") && i+1 < len(args):
			return i + 1, true
		}
	}
	return 0, false
}

// writesFile reports an output redirect on s to anything but /dev/null or
// another descriptor.
func writesFile(s segment) bool {
	for _, r := range s.redirects {
		if r.heredoc || !strings.Contains(r.op, ">") {
			continue
		}
		// >&1, >&2 go to the terminal; >&3 may be a file opened by exec.
		if r.target == "/dev/null" || r.target == "-" || (strings.HasSuffix(r.op, "&") && (r.target == "1" || r.target == "2")) {
			continue
		}
		return true
	}
	return false
}

// readsFile reports a shell's stdin redirected from a file (not a heredoc).
func readsFile(s segment) bool {
	for _, r := range s.redirects {
		if !r.heredoc && (r.op == "<" || r.op == "<>") && (r.fd == "" || r.fd == "0") {
			return true
		}
	}
	return false
}

// inputShell reports a shell that runs commands read from input: piped in,
// redirected from a file or handed over by xargs (`xargs bash < list`).
// Those commands are not in the text, so no rule can check them.
func inputShell(code string, depth int) bool {
	if depth > maxDepth {
		return true
	}
	segs, subs, err := parseShell(code)
	if err != nil {
		return false
	}
	for _, s := range subs {
		if inputShell(s, depth+1) {
			return true
		}
	}
	piped := false
	for _, s := range segs {
		argv, viaXargs := unwrapArgv(s.argv)
		if shells[baseCmd(argv)] && !shellNoExec(argv) {
			args := argv[1:]
			if viaXargs {
				return true
			}
			if ci, ok := shellCommandArg(baseCmd(argv), args); ok {
				if inputShell(args[ci], depth+1) {
					return true
				}
			} else if _, script := shellScriptArg(args); !script && (piped || readsFile(s)) {
				return true
			}
			for _, r := range s.redirects {
				if r.heredoc && inputShell(r.body, depth+1) {
					return true
				}
			}
		}
		piped = s.piped
	}
	return false
}

// envWordRe: words that may change the env without naming STAYPOINT_ in
// full (env -i, unset STAY{POINT,X}_TASK_ID).
var envWordRe = regexp.MustCompile(`\b(env|unset|export|declare|typeset|local|readonly)\b`)

// stayEnvChange reports a command that unsets, clears or overrides the
// StayPoint env the gate relies on (gateVarRe) for anything but go test or
// go vet: tests clear STAYPOINT_TASK_ID to isolate themselves.
func stayEnvChange(code string, depth int) bool {
	if !strings.Contains(code, "STAYPOINT_") && !envWordRe.MatchString(code) {
		return false
	}
	if depth > maxDepth {
		return true
	}
	segs, subs, err := parseShell(code)
	if err != nil {
		return stayEnvRe.MatchString(code)
	}
	for _, s := range subs {
		if stayEnvChange(s, depth+1) {
			return true
		}
	}
	for _, s := range segs {
		if envChange(s.argv, &envState{other: goShadowRe.MatchString(code)}, depth) {
			return true
		}
		if argv, _ := unwrapArgv(s.argv); shells[baseCmd(argv)] {
			for _, r := range s.redirects {
				if r.heredoc && stayEnvChange(r.body, depth+1) {
					return true
				}
			}
		}
	}
	return false
}

// gateVarName reports a name that is, or may expand to, a gate variable:
// STAYPOINT_{TASK,SESSION}_ID is two of them once the shell expands it, and
// ST*_TASK_ID may glob to one. Any name the shell may still rewrite counts.
func gateVarName(n string) bool {
	return gateVarRe.MatchString(n) || strings.ContainsAny(n, "{}*?[]\\'\"")
}

// envState tracks one command's env changes. other is any change besides
// clearing a gate variable (PATH=, GOFLAGS=, STAYPOINT_TASK_ID=other): with
// it, `go test` may not be the go test it looks like, so the exemption is
// off.
type envState struct{ changed, other bool }

func (st *envState) assign(a string) {
	if !gateVarRe.MatchString(assignName(a)) {
		st.other = true
		return
	}
	st.changed = true
	if v := a[strings.IndexByte(a, '=')+1:]; v != "" {
		st.other = true
	}
}

func envChange(argv []string, st *envState, depth int) bool {
	argv = dropKeywords(argv)
	for len(argv) > 0 && isAssign(argv[0]) {
		st.assign(argv[0])
		argv = argv[1:]
	}
	if len(argv) == 0 {
		return st.changed // VAR=x on its own sets it for the rest of the line
	}
	changed := st.changed
	name, args := baseCmd(argv), argv[1:]
	switch {
	case name == "unset" || name == "export" || name == "declare" || name == "typeset" || name == "local" || name == "readonly":
		for _, a := range args {
			// unset $n, export ${n}=x: a name we cannot read.
			if gateVarName(assignName(a)) || strings.ContainsAny(assignName(a), "$`") {
				return true
			}
		}
		return changed
	case name == "env":
		i := 0
	flags:
		for ; i < len(args); i++ {
			a := args[i]
			switch {
			case a == "-u" || a == "--unset":
				if i+1 < len(args) && gateVarName(args[i+1]) {
					st.changed = true
				}
				i++
			case strings.HasPrefix(a, "--unset="):
				st.changed = st.changed || gateVarName(a[len("--unset="):])
			case strings.HasPrefix(a, "-u"):
				st.changed = st.changed || gateVarName(a[2:])
			case a == "-C" || a == "--chdir":
				i++
			case a == "--":
				i++
				break flags
			case a == "-" || a == "--ignore-environment" || strings.HasPrefix(a, "--split-string"):
				return true
			case strings.HasPrefix(a, "--"):
			case strings.HasPrefix(a, "-"):
				// -i clears everything; -S and -C clusters cannot be read.
				if strings.ContainsAny(a[1:], "iSC") {
					return true
				}
			case isAssign(a):
				st.assign(a)
			default:
				break flags
			}
		}
		if i >= len(args) {
			return st.changed
		}
		return envChange(args[i:], st, depth)
	case wrappers[name] || boardWrappers[name]:
		// skipWrapper drops a wrapper's assignments; count them first.
		for _, a := range args {
			if isAssign(a) {
				st.assign(a)
			}
		}
		return envChange(skipWrapper(name, args), st, depth)
	case shells[name]:
		if ci, ok := shellCommandArg(baseCmd(argv), args); ok && stayEnvChange(args[ci], depth+1) {
			return true
		}
		return changed
	}
	return changed && (st.other || !goTestOrVet(argv))
}

// goShadowRe: a line that can make `go` something else (a function, an
// alias, PATH or GO* settings, an overlay): no go test exemption.
var goShadowRe = regexp.MustCompile(`\balias\b|\bfunction\b|\(\s*\)\s*\{|\bPATH=|\bGO\w*=|-overlay|\bgo\s+env\s+-w\b|\bread\b|\bhash\b|\bsource\b|(^|[;&|(\n]\s*)\.\s|\bexport\b[^;&|\n]*\b(GO\w*|PATH)\b`)

// goRunFlags: go test/vet flags that run another program (-exec, -toolexec,
// -vettool) or pass flags to the compiler or linker (-ldflags=-extld=x).
var goRunFlags = []string{"exec", "toolexec", "vettool", "ldflags", "gcflags", "asmflags", "gccgoflags", "compiler"}

// goTestOrVet reports `go test` or `go vet` without a goRunFlags flag.
func goTestOrVet(argv []string) bool {
	if len(argv) < 2 || argv[0] != "go" || (argv[1] != "test" && argv[1] != "vet") {
		return false
	}
	for _, a := range argv[2:] {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		f := strings.TrimLeft(a, "-")
		for _, r := range goRunFlags {
			if strings.HasPrefix(f, r) {
				return false
			}
		}
	}
	return true
}

var launchctlRe = regexp.MustCompile(`(?i)launchctl`)

// runsLaunchctl reports launchctl that may run (any subcommand: it starts,
// stops and replaces the daemon's LaunchAgent). grep launchctl does not.
func runsLaunchctl(code string, depth int) bool { return namedRun(code, launchctlRe, depth) }

var killRe = regexp.MustCompile(`(?i)\b(pkill|killall)\b`)

// selfProcNames are what pkill -f and killall match the daemon and the CLI
// against: the process names and the installed paths.
func selfProcNames() []string {
	names := []string{"staypointd", "staypoint"}
	if home, err := os.UserHomeDir(); err == nil {
		names = append(names, home+"/.local/bin/staypointd", home+"/.local/bin/staypoint")
	}
	return names
}

// killsSelf reports pkill or killall whose pattern may match the daemon or
// the CLI. Each pattern is read as a regexp and tried on selfProcNames, so
// pkill -f 'x|staypointd' and killall -m 'stay.*' hold and pkill -f
// staypoint-apitest-server does not. A pattern that does not compile, a
// pidfile, patterns from xargs or code that does not parse hold.
func killsSelf(code string, depth int) bool {
	if !killRe.MatchString(code) {
		return false
	}
	if depth > maxDepth {
		return true
	}
	segs, subs, err := parseShell(code)
	if err != nil {
		return true
	}
	for _, s := range subs {
		if killsSelf(s, depth+1) {
			return true
		}
	}
	for _, s := range segs {
		argv, viaXargs := unwrapArgv(s.argv)
		if len(argv) == 0 {
			continue
		}
		if shells[baseCmd(argv)] {
			if ci, ok := shellCommandArg(baseCmd(argv), argv[1:]); ok && killsSelf(argv[1+ci], depth+1) {
				return true
			}
			for _, r := range s.redirects {
				if r.heredoc && killsSelf(r.body, depth+1) {
					return true
				}
			}
			continue
		}
		data, off := dataArgs(argv), len(s.argv)-len(argv)
		for i, a := range argv {
			switch n := baseCmd([]string{a}); {
			case n == "pkill" || n == "killall":
				// find -exec pkill, sudo pkill: its patterns follow it. A
				// pattern that expands ("$P") cannot be read.
				if i == 0 && viaXargs || killPatternsSelf(argv[i+1:]) || anyTrue(s.dyn[off+i:]) {
					return true
				}
			case !data && killRe.MatchString(a) && killsSelf(a, depth+1):
				// python3 -c "os.system('pkill ...')"
				return true
			}
		}
	}
	return false
}

func anyTrue(bs []bool) bool {
	for _, b := range bs {
		if b {
			return true
		}
	}
	return false
}

// killPatternsSelf reads pkill/killall arguments. Every word that is not
// an option is tried as a pattern, option values (-u user) included: a
// signal such as -TERM or -HUP looks like an option taking one, so
// skipping values would skip the pattern. -F (a pidfile) holds.
func killPatternsSelf(args []string) bool {
	names := selfProcNames()
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--"):
			if strings.HasPrefix(a, "--pidfile") {
				return true
			}
			continue
		case len(a) > 1 && a[0] == '-':
			if strings.ContainsRune(a, 'F') {
				return true
			}
			continue
		}
		re, err := regexp.Compile("(?i)" + a)
		if err != nil {
			return true
		}
		for _, n := range names {
			if re.MatchString(n) {
				return true
			}
		}
	}
	return false
}

var daemonRe = regexp.MustCompile(`(?i)staypointd`)

// daemonProgRe: a program named like the daemon (staypointd, staypointd-x).
var daemonProgRe = regexp.MustCompile(`(?i)^staypointd`)

// isDaemonName reports a path whose name is the daemon's (staypointd*).
func isDaemonName(p string) bool { return daemonProgRe.MatchString(path.Base(p)) }

// runsDaemon reports a line that may start a StayPoint daemon, which takes
// over the live one's socket and DB: a program named staypointd*, go run
// of it, or such a name handed to find -exec, xargs or watch. A daemon
// built or copied under another name could run in a later command, so
// go build of it with -o to a name that is not staypointd* holds, and so
// does cp, mv or ln of one to such a name. Building it as staypointd* or
// naming it does not hold.
func runsDaemon(code string, depth int) bool {
	if !daemonRe.MatchString(code) {
		return false
	}
	if depth > maxDepth {
		return true
	}
	segs, subs, err := parseShell(code)
	if err != nil {
		return true
	}
	for _, s := range subs {
		if runsDaemon(s, depth+1) {
			return true
		}
	}
	// A cd into the daemon's directory makes go build . build it.
	cdDaemon := false
	for _, s := range segs {
		if argv, _ := unwrapArgv(s.argv); len(argv) > 1 && (argv[0] == "cd" || argv[0] == "pushd") && anyNames(daemonRe, argv[1:]) {
			cdDaemon = true
		}
	}
	for _, s := range segs {
		argv, viaXargs := unwrapArgv(s.argv)
		if len(argv) == 0 {
			continue
		}
		name := baseCmd(argv)
		runner := viaXargs || name == "watch"
		switch {
		case isDaemonName(argv[0]):
			return true
		case name == "go":
			// go -C dir build: the one flag before the subcommand.
			i, inDaemon := 1, cdDaemon
			for i < len(argv) && strings.HasPrefix(argv[i], "-C") {
				v := strings.TrimPrefix(strings.TrimPrefix(argv[i], "-C"), "=")
				if v == "" && i+1 < len(argv) {
					i++
					v = argv[i]
				}
				inDaemon = inDaemon || daemonRe.MatchString(v)
				i++
			}
			if i >= len(argv) || (argv[i] != "run" && argv[i] != "build") {
				continue
			}
			rest := argv[i+1:]
			if !inDaemon && !anyNames(daemonRe, rest) {
				continue
			}
			// GOFLAGS may carry -o; the env is not read.
			if argv[i] == "run" || strings.Contains(code, "GOFLAGS") || goBuildRenames(rest, s.dyn[len(s.argv)-len(rest):]) {
				return true
			}
			continue
		case name == "cp" || name == "mv" || name == "ln" || name == "install" || name == "ditto" || name == "rsync":
			dst := argv[len(argv)-1]
			if strings.HasSuffix(dst, "/") || isDaemonName(dst) {
				continue
			}
			for _, a := range argv[1 : len(argv)-1] {
				if isDaemonName(a) {
					return true
				}
			}
		case shells[name]:
			if ci, ok := shellCommandArg(name, argv[1:]); ok && runsDaemon(argv[1+ci], depth+1) {
				return true
			}
			for _, r := range s.redirects {
				if r.heredoc && runsDaemon(r.body, depth+1) {
					return true
				}
			}
			continue
		case name == "find":
			for _, a := range argv[1:] {
				runner = runner || a == "-exec" || a == "-execdir" || a == "-ok" || a == "-okdir"
			}
		}
		if runner {
			for _, a := range argv[1:] {
				if isDaemonName(a) {
					return true
				}
			}
			continue
		}
		// Code handed to another interpreter (python3 -c
		// "subprocess.run(['/tmp/staypointd'])"): read it as shell, and
		// hold where it does not parse.
		if dataArgs(argv) {
			continue
		}
		for _, a := range argv[1:] {
			if daemonRe.MatchString(a) && strings.ContainsAny(a, " \t\n;()[]'\"") && runsDaemon(a, depth+1) {
				return true
			}
		}
	}
	return false
}

// goBuildRenames reports go build -o to a name that is not staypointd*,
// or to one that expands ("$D"). A directory (trailing /) keeps the name.
func goBuildRenames(args []string, dyn []bool) bool {
	for i, a := range args {
		f := strings.TrimLeft(a, "-")
		out, ok := "", false
		switch {
		case a == f:
			continue
		case f == "o" && i+1 < len(args):
			out, ok = args[i+1], true
			if dyn[i+1] {
				return true
			}
		case strings.HasPrefix(f, "o="):
			out, ok = f[2:], true
			if dyn[i] {
				return true
			}
		}
		if ok && !strings.HasSuffix(out, "/") && !isDaemonName(out) {
			return true
		}
	}
	return false
}

var reinstallRe = regexp.MustCompile(`(?i)reinstall-daemon`)

// reinstallExec reports whether code runs the daemon reinstall script or
// makes a copy of it that could run later. Naming it does not: cat, grep,
// sed -n, git add, bash -n (task-800b532d).
func reinstallExec(code string, depth int) bool { return namedRun(code, reinstallRe, depth) }

// namedRun reports whether code may run what re names (a program or a
// script), or copy it somewhere it could run later. A segment that names it
// holds unless it only reads or records the name (namesOnly), and so does
// a pipe from it into anything but a filter.
func namedRun(code string, re *regexp.Regexp, depth int) bool {
	if !re.MatchString(code) {
		return false
	}
	if depth > maxDepth {
		return true
	}
	segs, subs, err := parseShell(code)
	if err != nil {
		return true
	}
	for _, s := range subs {
		if namedRun(s, re, depth+1) {
			return true
		}
	}
	// A file written where it may carry what a command naming it prints
	// may hold a copy. A segment's own write is namesOnly's, and a pipe from
	// it is fed's below. exec >f, { ...; } > f, ( ... ) > f and done > f
	// redirect more than their own segment, which the parse does not track,
	// so they hold; so does any write on a line that defines a function or
	// alias (f > out runs its body) or substitutes a command naming it.
	wide := shellFuncRe.MatchString(code)
	for _, s := range subs {
		wide = wide || re.MatchString(s)
	}
	for _, s := range segs {
		if writesFile(s) && (wide || groupWrite(s)) {
			return true
		}
	}
	fed := false // a pipe from a segment that read the script
	for _, s := range segs {
		argv, viaXargs := unwrapArgv(s.argv)
		named := anyNames(re, s.argv) || redirectNames(re, s) || heredocNames(re, s)
		if fed && (viaXargs || len(argv) == 0 || !pipeFilters[baseCmd(argv)] || argv[0] != baseCmd(argv) || writesFile(s) || filterWrites(argv)) {
			return true
		}
		// Named in an assignment or a wrapper's args (BASH_ENV=...,
		// GIT_EXTERNAL_DIFF=..., env, timeout), it may run.
		if anyNames(re, s.argv[:len(s.argv)-len(argv)]) {
			return true
		}
		if named && !namesOnly(re, s, argv, viaXargs, depth) {
			return true
		}
		fed = (named || fed) && s.piped
	}
	return false
}

func anyNames(re *regexp.Regexp, args []string) bool {
	for _, a := range args {
		if re.MatchString(a) {
			return true
		}
	}
	return false
}

func redirectNames(re *regexp.Regexp, s segment) bool {
	for _, r := range s.redirects {
		if !r.heredoc && re.MatchString(r.target) {
			return true
		}
	}
	return false
}

func heredocNames(re *regexp.Regexp, s segment) bool {
	for _, r := range s.redirects {
		if r.heredoc && re.MatchString(r.body) {
			return true
		}
	}
	return false
}

// shellFuncRe: a function or alias definition, whose body runs wherever
// its name does.
var shellFuncRe = regexp.MustCompile(`\(\s*\)|\bfunction\b|\balias\b`)

// groupClosers end a compound command; a redirect on one covers its body.
var groupClosers = map[string]bool{"}": true, ")": true, "done": true, "fi": true, "esac": true}

// groupWrite reports a redirect that covers more than its own segment: on
// a compound command's closer, or with no command at all (exec >f, ( ... ) >f).
func groupWrite(s segment) bool {
	argv, _ := unwrapArgv(s.argv)
	return len(argv) == 0 || groupClosers[argv[0]] || len(s.argv) > 0 && baseCmd(s.argv) == "exec"
}

// pipeFilters only print what they read, unless filterWrites.
var pipeFilters = map[string]bool{
	"cat": true, "head": true, "tail": true, "grep": true, "egrep": true, "fgrep": true, "wc": true,
	"sort": true, "uniq": true, "cut": true, "tr": true, "nl": true,
}

// filterWrites reports a pipe filter told to write a file or run a
// program: sort -o, --output or --compress-program (any prefix git-style
// abbreviation of either, any short cluster with an o), or uniq's second operand.
func filterWrites(argv []string) bool {
	switch argv[0] {
	case "sort":
		for _, a := range argv[1:] {
			if n, ok := strings.CutPrefix(a, "--"); ok {
				n, _, _ = strings.Cut(n, "=")
				if n != "" && (strings.HasPrefix("output", n) || strings.HasPrefix("compress-program", n)) {
					return true
				}
			} else if len(a) > 1 && a[0] == '-' && strings.ContainsRune(a[1:], 'o') {
				return true
			}
		}
	case "uniq":
		// uniq [opts] [in [out]]: -f/-s/-w values count too (fail closed).
		operands := 0
		for _, a := range argv[1:] {
			if a == "-" || !strings.HasPrefix(a, "-") {
				operands++
			}
		}
		return operands > 1
	}
	return false
}

// nameReaders read or print their arguments and never run them.
var nameReaders = map[string]bool{
	"cat": true, "head": true, "tail": true, "wc": true, "grep": true, "egrep": true, "fgrep": true,
	"ls": true, "stat": true, "file": true, "diff": true, "cmp": true, "shellcheck": true,
	"shasum": true, "sha256sum": true, "md5": true, "md5sum": true, "echo": true, "printf": true,
	"test": true, "[": true, "basename": true, "dirname": true, "realpath": true, "readlink": true,
}

// gitNameSubs only record or show the files they are given.
var gitNameSubs = map[string]bool{
	"add": true, "diff": true, "log": true, "show": true, "status": true, "blame": true, "commit": true,
	"rm": true, "restore": true, "checkout": true, "ls-files": true, "grep": true,
	"cat-file": true, "check-ignore": true, "ls-tree": true, "reset": true, "stash": true,
	"update-index": true, "hash-object": true, "shortlog": true, "annotate": true,
}

// namesOnly reports whether segment s (argv unwrapped) only names what re
// matches without running or copying it.
func namesOnly(re *regexp.Regexp, s segment, argv []string, viaXargs bool, depth int) bool {
	if viaXargs || len(argv) == 0 || writesFile(s) || re.MatchString(argv[0]) {
		return false
	}
	// A bare program name: ./cat is whatever the agent put there.
	if argv[0] != baseCmd(argv) {
		return false
	}
	name, args := argv[0], argv[1:]
	switch {
	case shells[name]:
		if shellNoExec(argv) {
			return true
		}
		for _, r := range s.redirects {
			if r.heredoc && namedRun(r.body, re, depth+1) {
				return false
			}
		}
		if ci, ok := shellCommandArg(baseCmd(argv), args); ok {
			return !namedRun(args[ci], re, depth+1) && !anyNames(re, args[:ci]) && !anyNames(re, args[ci+1:]) && !redirectNames(re, s)
		}
		// A script argument or stdin naming it runs it.
		return !anyNames(re, args) && !redirectNames(re, s)
	case nameReaders[name]:
		return true
	case name == "rg":
		for _, a := range args {
			if strings.HasPrefix(a, "--pre") {
				return false
			}
		}
		return true
	case name == "sed":
		return sedNamesOnly(re, args)
	case name == "git":
		return gitNamesOnly(re, args)
	case name == "go":
		if len(args) == 0 || !greenGoNames[args[0]] {
			return false
		}
		for i, a := range args {
			if !strings.HasPrefix(a, "-") {
				continue
			}
			// -exec, -toolexec and -vettool run their value.
			f := strings.TrimLeft(a, "-")
			runs := strings.HasPrefix(f, "exec") || strings.HasPrefix(f, "toolexec") || strings.HasPrefix(f, "vettool")
			if re.MatchString(a) || (runs && !strings.Contains(a, "=") && i+1 < len(args) && re.MatchString(args[i+1])) {
				return false
			}
		}
		return true
	case name == "staypoint":
		return true
	case name == "gh":
		for _, a := range args {
			if a == "alias" || a == "extension" {
				return false
			}
		}
		return true
	}
	return false
}

var greenGoNames = map[string]bool{"vet": true, "build": true, "test": true, "list": true, "doc": true, "fmt": true}

// sedPrintRe: one sed command that only prints or stops: an optional
// address or range (line, $, /regex/) and p, l, =, q or nothing. Anything
// else (s with any delimiter and its e/w flags, e, w, r, a, i, c, y, {})
// is not known to be harmless.
var sedPrintRe = regexp.MustCompile(`^\s*((\d+|\$|/[^/\\\n]*/)(\s*,\s*(\d+|\$|/[^/\\\n]*/))?)?\s*!?\s*[pl=qQ]?\s*\d*\s*$`)

func sedPrintsOnly(script string) bool {
	for _, c := range strings.FieldsFunc(script, func(r rune) bool { return r == ';' || r == '\n' }) {
		if !sedPrintRe.MatchString(c) {
			return false
		}
	}
	return true
}

// sedOptionsSafe are sed's short options that take no value and change
// nothing but output format.
const sedOptionsSafe = "nErsuz"

// sedNamesOnly: sed that only prints. Every script must be sedPrintsOnly,
// and -i, -f (a script we cannot read) or an unknown option means no.
func sedNamesOnly(re *regexp.Regexp, args []string) bool {
	hasScript := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if !hasScript && i+1 < len(args) && !sedPrintsOnly(args[i+1]) {
				return false
			}
			return true
		case a == "--expression" || strings.HasPrefix(a, "--expression="):
			hasScript = true
			s := strings.TrimPrefix(a, "--expression=")
			if a == "--expression" {
				i++
				if i >= len(args) {
					return false
				}
				s = args[i]
			}
			if !sedPrintsOnly(s) {
				return false
			}
		case strings.HasPrefix(a, "--"):
			switch a {
			case "--quiet", "--silent", "--regexp-extended", "--posix", "--debug", "--separate", "--unbuffered", "--null-data":
			default:
				return false
			}
		case len(a) > 1 && a[0] == '-':
			// A cluster like -ne: e takes the rest, or the next argument.
			for j := 1; j < len(a); j++ {
				if a[j] == 'e' {
					hasScript = true
					s := a[j+1:]
					if s == "" {
						i++
						if i >= len(args) {
							return false
						}
						s = args[i]
					}
					if !sedPrintsOnly(s) {
						return false
					}
					break
				}
				if !strings.ContainsRune(sedOptionsSafe, rune(a[j])) {
					return false
				}
			}
		case !hasScript:
			hasScript = true
			if !sedPrintsOnly(a) {
				return false
			}
		}
	}
	return true
}

// gitNamesOnly: a git subcommand that records or shows files, with no -c
// config (an alias or pager can run anything) naming the script.
func gitNamesOnly(re *regexp.Regexp, args []string) bool {
	sub, rest := gitSub(args)
	for _, a := range args[:len(args)-len(rest)] {
		if re.MatchString(a) {
			return false
		}
	}
	return gitNameSubs[sub] && !gitRunsProgram(sub, rest)
}

// gitNotGrep: builtins with no pager option: -O is a diff orderfile or
// not an option, and an O in a glued value (commit -m"OOM") is data.
var gitNotGrep = map[string]bool{
	"add": true, "am": true, "annotate": true, "apply": true, "archive": true, "bisect": true, "blame": true,
	"branch": true, "cat-file": true, "check-ignore": true, "checkout": true, "cherry-pick": true, "clean": true,
	"clone": true, "commit": true, "config": true, "describe": true, "diff": true, "diff-tree": true,
	"fetch": true, "format-patch": true, "hash-object": true, "init": true, "log": true, "ls-files": true,
	"ls-tree": true, "merge": true, "mv": true, "pull": true, "push": true, "rebase": true, "reflog": true,
	"remote": true, "reset": true, "restore": true, "rev-parse": true, "revert": true, "rm": true,
	"shortlog": true, "show": true, "stash": true, "status": true, "switch": true, "tag": true,
	"update-index": true, "whatchanged": true, "worktree": true,
}

// gitShortRunValue: short flags whose value git runs, by subcommand.
var gitShortRunValue = map[string]string{"difftool": "xt", "mergetool": "t", "rebase": "x",
	"clone": "u", "fetch": "u", "pull": "u", "ls-remote": "u"}

// gitGrepValueShort: git grep short flags whose value is the rest of the
// word (-e'pat', -A3, -m1); an O after one is data.
const gitGrepValueShort = "efABCm"

// gitShortRunsPager reports a short-flag cluster that may open a pager:
// grep's -O, read flag by flag. Any other builtin has no -O pager; an
// alias or an unknown subcommand may be grep, so any O there holds.
func gitShortRunsPager(sub, cluster string) bool {
	// difftool/mergetool/rebase -x, clone/fetch/pull -u run their value.
	if prog := gitShortRunValue[sub]; prog != "" && strings.ContainsAny(cluster, prog) {
		return true
	}
	if gitNotGrep[sub] {
		return false
	}
	if sub != "grep" {
		return strings.ContainsRune(cluster, 'O')
	}
	for _, r := range cluster {
		switch {
		case r == 'O':
			return true
		case strings.ContainsRune(gitGrepValueShort, r):
			return false
		}
	}
	return false
}

// gitGlobalValue and gitGlobalFlag: git's options before the subcommand,
// with and without a separate value.
var (
	gitGlobalValue = map[string]bool{"-c": true, "--config-env": true, "-C": true, "--git-dir": true,
		"--work-tree": true, "--namespace": true, "--attr-source": true, "--super-prefix": true, "--list-cmds": true}
	gitGlobalFlag = map[string]bool{"-p": true, "--paginate": true, "-P": true, "--no-pager": true, "--bare": true,
		"--no-replace-objects": true, "--literal-pathspecs": true, "--glob-pathspecs": true,
		"--noglob-pathspecs": true, "--icase-pathspecs": true, "--no-optional-locks": true,
		"--no-lazy-fetch": true, "--no-advice": true}
)

// gitSub splits git's arguments into the subcommand and its arguments. An
// option it does not know may take the next word as a value, so the
// subcommand is unknown: "" and every word from there on.
func gitSub(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, _, hasValue := strings.Cut(a, "=")
		switch {
		case gitGlobalValue[a]:
			i++
		case gitGlobalFlag[a] || hasValue && (gitGlobalValue[name] || gitGlobalFlag[name]):
		case strings.HasPrefix(a, "-"):
			return "", args[i:]
		default:
			return a, args[i+1:]
		}
	}
	return "", nil
}

// gitRunsProgram reports options of subcommand sub that run a program (grep
// -O opens a pager through the shell, --ext-diff an external diff) or copy
// what they show (--output). Every word is checked, past a "--" too: "--"
// may be an option's value (git grep -e -- -O...).
func gitRunsProgram(sub string, args []string) bool {
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--") && len(a) > 2:
			// git takes any unambiguous prefix: --open, --out, --ext.
			name, _, _ := strings.Cut(a[2:], "=")
			for _, o := range []string{"open-files-in-pager", "output", "ext-diff", "extcmd", "tool", "exec", "upload-pack", "receive-pack"} {
				if name != "" && strings.HasPrefix(o, name) || strings.HasPrefix(name, o) {
					return true
				}
			}
		case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--"):
			if gitShortRunsPager(sub, a[1:]) {
				return true
			}
		}
	}
	return false
}

var taskIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// handoffReaders only read the files they are given.
var handoffReaders = map[string]bool{
	"cat": true, "head": true, "tail": true, "wc": true, "ls": true, "grep": true, "egrep": true,
	"fgrep": true, "stat": true, "file": true,
}

// ownHandoffRead reports whether every StayPoint-state reference in code is
// a plain read of the run's own handoff files
// (~/.staypoint/handoffs/<taskID>/...): the agent resuming its own task.
func ownHandoffRead(code, taskID string) bool {
	if !taskIDRe.MatchString(taskID) || strings.Contains(code, "HOME=") {
		return false
	}
	segs, subs, err := parseShell(code)
	if err != nil || len(subs) > 0 {
		return false
	}
	dirRe := regexp.MustCompile(`^(~|\$HOME|\$\{HOME\}|/Users/[^/\s'"]+|/home/[^/\s'"]+|/var/root)/\.staypoint/handoffs/` + regexp.QuoteMeta(taskID) + `(/|$)`)
	own := func(tok string) bool {
		loc := dirRe.FindStringIndex(tok)
		if loc == nil || strings.ContainsAny(tok, "`") || strings.Contains(tok[:loc[1]], "..") {
			return false
		}
		// Braces and ? or [..] globs can spell .. ({..,x}, .? in bash 3.2);
		// * cannot start a name with a dot, so it may stand in one that
		// does not.
		rest := tok[loc[1]:]
		if strings.ContainsAny(rest, "$\\{}[]?") {
			return false
		}
		for _, e := range strings.Split(rest, "/") {
			if e == ".." || (strings.HasPrefix(e, ".") && strings.Contains(e, "*")) {
				return false
			}
		}
		return true
	}
	for _, s := range segs {
		var refs []string
		for _, a := range s.argv {
			if selfPathRe.MatchString(a) {
				refs = append(refs, a)
			}
		}
		for _, r := range s.redirects {
			if r.heredoc && selfPathRe.MatchString(r.body) {
				return false
			}
			if !r.heredoc && selfPathRe.MatchString(r.target) {
				refs = append(refs, r.target)
			}
		}
		if len(refs) == 0 {
			continue
		}
		if len(s.argv) == 0 || !handoffReaders[s.argv[0]] || writesFile(s) {
			return false
		}
		for _, r := range refs {
			if !own(r) {
				return false
			}
		}
	}
	return true
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
