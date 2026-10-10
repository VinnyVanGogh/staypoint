package security

import (
	"regexp"
	"strings"
)

// git config that runs a program (task-97b4fa02): git -c core.fsmonitor=./x
// status runs ./x, and so do core.hooksPath, core.sshCommand, core.pager,
// diff.external, filter.<x>.clean, credential.helper, gpg.program and the
// rest, set by -c, --config-env, git config, or the GIT_* variables that
// stand in for them. The program is not in the command text the gate
// reads, so these hold.

// gitRunKeyRe matches config keys (lowercased) whose value git runs, loads
// as config or hooks, or uses to pick what it connects to.
var gitRunKeyRe = regexp.MustCompile(`^(alias\.|pager\.|include\.|includeif\.|` +
	`core\.(fsmonitor|hookspath|sshcommand|pager|editor|askpass|gitproxy|alternaterefscommand|worktree)$|` +
	`sequence\.editor$|diff\.external$|protocol\.|uploadpack\.|receivepack\.|init\.templatedir$|ssh\.variant$|` +
	`url\.|.*\.(command|program|helper|cmd|driver|textconv|clean|smudge|process|path|editor|pager|proxy|` +
	`sshcommand|askpass|uploadpack|receivepack|packobjectshook|external|browser|viewer|insteadof|pushinsteadof|hookspath|fsmonitor)$)`)

// gitRunVars are environment variables git runs or reads config from.
var gitRunVars = map[string]bool{
	"GIT_SSH": true, "GIT_SSH_COMMAND": true, "GIT_EXTERNAL_DIFF": true, "GIT_EDITOR": true,
	"GIT_SEQUENCE_EDITOR": true, "GIT_ASKPASS": true, "SSH_ASKPASS": true, "GIT_PROXY_COMMAND": true,
	"GIT_CONFIG": true, "GIT_CONFIG_PARAMETERS": true, "GIT_CONFIG_COUNT": true, "GIT_CONFIG_GLOBAL": true,
	"GIT_CONFIG_SYSTEM": true, "GIT_EXEC_PATH": true, "GIT_TEMPLATE_DIR": true,
}

// pagerVars name a pager or editor git runs; harmless values pass.
var pagerVars = map[string]bool{"GIT_PAGER": true, "PAGER": true, "EDITOR": true, "VISUAL": true, "MANPAGER": true}

var harmlessPagers = map[string]bool{"": true, "cat": true, "less": true, "more": true, "true": true, "less -R": true,
	"less -FRX": true, "less -FRSX": true, "vi": true, "vim": true, "nano": true}

var gitWordRe = regexp.MustCompile(`(?i)\bgit\b|GIT_|PAGER|EDITOR|VISUAL|SSH_ASKPASS`)

// gitRunsConfig reports git config set (by -c, --config-env or git config)
// to a key in gitRunKeyRe, or a gitRunVars variable set, or a pager or
// editor variable set to something else than a plain pager on a line that
// runs git.
func gitRunsConfig(code string, depth int) bool {
	if !textMatch(gitWordRe, code) {
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
		if gitRunsConfig(s, depth+1) {
			return true
		}
	}
	runsGit := false
	for _, s := range segs {
		if argv, _ := unwrapArgv(s.argv); len(argv) > 0 && baseCmd(argv) == "git" {
			runsGit = true
		}
	}
	for _, s := range segs {
		// Assignments: prefixes, env's, export's and friends.
		var assigns []string
		words := dropKeywords(s.argv)
		for _, a := range words {
			if isAssign(a) {
				assigns = append(assigns, a)
			}
		}
		argv, _ := unwrapArgv(s.argv)
		if len(argv) > 0 && exportLike[baseCmd(argv)] {
			assigns = append(assigns, argv[1:]...)
		}
		for _, a := range assigns {
			name, val, ok := strings.Cut(a, "=")
			if !ok {
				continue
			}
			if gitRunVars[name] || strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
				return true
			}
			if pagerVars[name] && runsGit && !harmlessPagers[strings.TrimSpace(val)] {
				return true
			}
		}
		if len(argv) == 0 {
			continue
		}
		if shells[baseCmd(argv)] {
			if ci, ok := shellCommandArg(baseCmd(argv), argv[1:]); ok && gitRunsConfig(argv[1+ci], depth+1) {
				return true
			}
			for _, r := range s.redirects {
				if r.heredoc && gitRunsConfig(r.body, depth+1) {
					return true
				}
			}
			continue
		}
		if baseCmd(argv) != "git" {
			continue
		}
		if gitArgsRunConfig(argv[1:]) {
			return true
		}
	}
	return false
}

// gitArgsRunConfig reads git's global options and a git config call.
func gitArgsRunConfig(args []string) bool {
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c" || a == "--config-env":
			if i+1 >= len(args) {
				return true
			}
			i++
			if gitKeyRuns(args[i]) {
				return true
			}
		case strings.HasPrefix(a, "--config-env="):
			if gitKeyRuns(strings.TrimPrefix(a, "--config-env=")) {
				return true
			}
		case strings.HasPrefix(a, "-c") && len(a) > 2:
			if gitKeyRuns(a[2:]) {
				return true
			}
		case gitGlobalValue[a]:
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a == "config" && gitConfigSetRuns(args[i+1:])
		}
	}
	return false
}

// gitKeyRuns reports a key=value (or key=ENVVAR) whose key git runs.
func gitKeyRuns(kv string) bool {
	key, _, _ := strings.Cut(kv, "=")
	return key == "" || gitRunKeyRe.MatchString(strings.ToLower(key))
}

// gitConfigSetRuns reports git config writing a key git runs: the first
// non-option word is the key unless the call only reads (--get, --list,
// get, list).
func gitConfigSetRuns(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--get" || a == "--get-all" || a == "--get-regexp" || a == "--list" || a == "-l" || a == "get" || a == "list" ||
			a == "--show-origin" && i == len(args)-1:
			return false
		case a == "-f" || a == "--file" || a == "--blob" || a == "--type" || a == "--default" || a == "--comment":
			i++
		case a == "set" || a == "unset" || a == "--add" || a == "--replace-all" || a == "--unset" || a == "--unset-all":
		case strings.HasPrefix(a, "-"):
		default:
			return gitRunKeyRe.MatchString(strings.ToLower(a))
		}
	}
	return false
}
