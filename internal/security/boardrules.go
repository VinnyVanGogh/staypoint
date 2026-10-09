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

	// selfCmdRe: commands that reinstall, restart or replace the installed
	// StayPoint or the agent's own guards. Building a test binary under /tmp
	// or the worktree is not one, and neither is killing a test server
	// (staypoint-apitest-server): only staypoint and staypointd themselves.
	// Running the reinstall script is reinstallExec, launchctl runsLaunchctl.
	selfCmdRe = regexp.MustCompile(`(?i)(\bgo\s+install\b[^;&|\n]*staypoint|\bgo\s+(install|build)\b[^;&|\n]*\.local/bin|\b(cp|mv|ln|install|rsync|ditto)\b[^;&|\n]*\.local/bin|\b(kill|pkill|killall)\b[^;&|\n]*\bstaypointd?([^\w-]|$)|\bbrew\s+(re)?install\b[^;&|\n]*staypoint)`)
	// selfPathRe: paths StayPoint and the agent guards depend on. Any
	// reference holds, reads included: they hold tokens and the guards. The
	// one exception is reading the run's own handoff files (ownHandoffRead).
	selfPathRe = regexp.MustCompile(`(?i)((~|\$HOME|\$\{HOME\}|/Users/[^/\s'"]+|/home/[^/\s'"]+|/var/root)/\.staypoint\b|\.claude/settings[\w.-]*\.json|\.claude/hooks\b|\.gemini/settings|\.gemini/hooks\b|\.gemini/config/hooks\b|Library/LaunchAgents\b)`)
)

// progAt is a regexp prefix for "at a program position": a line or segment
// start, then any env assignments and wrappers (env, command, exec, nohup,
// sudo, npx, ...), then an optional backslash and directory, so `\ssh`,
// `/usr/bin/ssh` and `command ssh` read as ssh.
const gateVarPat = `STAYPOINT_(TASK_ID|SESSION_ID|RUN_ID|HOOK\w*|SCRATCH_ROOT|REPO_ROOT|CLI_BIN|CLAUDE_BIN|CODEX_BIN|AGY_BIN|GEMINI_BIN|LOCAL_URL|GATE\w*|TRUST\w*|SECURITY\w*)`

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
	case selfCmdRe.MatchString(code) || runsLaunchctl(code, 0) || reinstallExec(code, 0):
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

// unwrapArgv drops leading assignments and wrapper commands (env, nohup,
// timeout, xargs, sudo, ...). viaXargs reports xargs or parallel: the
// command's arguments then come from its input.
func unwrapArgv(argv []string) (out []string, viaXargs bool) {
	argv = stripPrefixes(argv)
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
		if len(argv) == 0 {
			continue
		}
		if nestedAgentRe.MatchString(argv[0]) {
			return true
		}
		if shells[baseCmd(argv)] {
			if ci, ok := shellCommandArg(argv[1:]); ok && runsAgent(argv[1+ci], depth+1) {
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

// shellCommandArg returns the index of a shell's -c command string in its
// args.
func shellCommandArg(args []string) (int, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-o" || a == "+o":
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
		if r.target == "/dev/null" || r.target == "-" || (strings.HasSuffix(r.op, "&") && isDigits(r.target)) {
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
			if ci, ok := shellCommandArg(args); ok {
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

var envWordRe = regexp.MustCompile(`\benv\b`)

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
		if segEnvChange(s.argv, false, depth) {
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

// segEnvChange is stayEnvChange for one simple command. changed is true
// once a wrapper or assignment has changed a gate variable.
func segEnvChange(argv []string, changed bool, depth int) bool {
	for len(argv) > 0 && isAssign(argv[0]) {
		if gateVarRe.MatchString(assignName(argv[0])) {
			changed = true
		}
		argv = argv[1:]
	}
	if len(argv) == 0 {
		return changed // VAR=x on its own sets it for the rest of the line
	}
	name, args := baseCmd(argv), argv[1:]
	switch {
	case name == "unset" || name == "export" || name == "declare" || name == "typeset" || name == "local" || name == "readonly":
		for _, a := range args {
			if gateVarRe.MatchString(assignName(a)) {
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
				if i+1 < len(args) && gateVarRe.MatchString(args[i+1]) {
					changed = true
				}
				i++
			case strings.HasPrefix(a, "--unset="):
				changed = changed || gateVarRe.MatchString(a[len("--unset="):])
			case strings.HasPrefix(a, "-u"):
				changed = changed || gateVarRe.MatchString(a[2:])
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
				changed = changed || gateVarRe.MatchString(assignName(a))
			default:
				break flags
			}
		}
		if i >= len(args) {
			return changed
		}
		return segEnvChange(args[i:], changed, depth)
	case wrappers[name] || boardWrappers[name]:
		return segEnvChange(skipWrapper(name, args), changed, depth)
	case shells[name]:
		if ci, ok := shellCommandArg(args); ok && stayEnvChange(args[ci], depth+1) {
			return true
		}
		return changed
	}
	return changed && !goTestOrVet(argv)
}

// goTestOrVet reports `go test` or `go vet` without a flag that runs
// another program (-exec, -toolexec, -vettool).
func goTestOrVet(argv []string) bool {
	if len(argv) < 2 || argv[0] != "go" || (argv[1] != "test" && argv[1] != "vet") {
		return false
	}
	for _, a := range argv[2:] {
		f := strings.TrimLeft(a, "-")
		if strings.HasPrefix(a, "-") && (strings.HasPrefix(f, "exec") || strings.HasPrefix(f, "toolexec") || strings.HasPrefix(f, "vettool")) {
			return false
		}
	}
	return true
}

var launchctlRe = regexp.MustCompile(`(?i)\blaunchctl\b`)

// runsLaunchctl reports launchctl that may run (any subcommand: it starts,
// stops and replaces the daemon's LaunchAgent). grep launchctl does not.
func runsLaunchctl(code string, depth int) bool { return namedRun(code, launchctlRe, depth) }

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
	fed := false // a pipe from a segment that read the script
	for _, s := range segs {
		argv, viaXargs := unwrapArgv(s.argv)
		named := anyNames(re, s.argv) || redirectNames(re, s) || heredocNames(re, s)
		if fed && (viaXargs || len(argv) == 0 || !pipeFilters[baseCmd(argv)] || argv[0] != baseCmd(argv)) {
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

// pipeFilters only print what they read.
var pipeFilters = map[string]bool{
	"cat": true, "head": true, "tail": true, "grep": true, "egrep": true, "fgrep": true, "wc": true,
	"sort": true, "uniq": true, "cut": true, "tr": true, "nl": true,
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
	"mv": true, "rm": true, "restore": true, "checkout": true, "ls-files": true, "grep": true,
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
		if ci, ok := shellCommandArg(args); ok {
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

// sedRunsRe: a sed script that may run a command or write a file: GNU
// sed's e and w commands and s///e, s///w flags. Over-broad on purpose.
var sedRunsRe = regexp.MustCompile(`(^|[;{}\n0-9$/!,])\s*[ewW](\s|$|[;}])|[/|#,:][gpiImM0-9]*[ewW]`)

// sedNamesOnly: sed that only prints: no -i, no script file, and no script
// that can run a command or write a file.
func sedNamesOnly(re *regexp.Regexp, args []string) bool {
	hasScript := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "-i") || strings.HasPrefix(a, "--in-place") || strings.HasPrefix(a, "-f") || strings.HasPrefix(a, "--file"):
			return false
		case a == "-e" || a == "--expression":
			hasScript = true
			if i+1 < len(args) && sedRunsRe.MatchString(args[i+1]) {
				return false
			}
			i++
		case strings.HasPrefix(a, "-e") || strings.HasPrefix(a, "--expression="):
			hasScript = true
			if sedRunsRe.MatchString(strings.TrimPrefix(strings.TrimPrefix(a, "--expression="), "-e")) {
				return false
			}
		case strings.HasPrefix(a, "-"):
		case !hasScript:
			hasScript = true
			if sedRunsRe.MatchString(a) {
				return false
			}
		}
	}
	return true
}

// gitNamesOnly: a git subcommand that records or shows files, with no -c
// config (an alias or pager can run anything) naming the script.
func gitNamesOnly(re *regexp.Regexp, args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c" || a == "--config-env":
			if i+1 < len(args) && re.MatchString(args[i+1]) {
				return false
			}
			i++
		case a == "-C" || a == "--git-dir" || a == "--work-tree" || a == "--namespace":
			i++
		case strings.HasPrefix(a, "-"):
			if re.MatchString(a) {
				return false
			}
		default:
			return gitNameSubs[a]
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
		rest := tok[loc[1]:]
		if strings.Contains(rest, "$") {
			return false
		}
		for _, e := range strings.Split(rest, "/") {
			if e == ".." {
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
