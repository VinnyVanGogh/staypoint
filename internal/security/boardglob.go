package security

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Protected names spelled by a glob or brace group (task-97b4fa02): bash
// scripts/reinstall-d?emon.sh, bash scripts/rein*, /usr/bin/launch*ctl,
// cl*de -p x. The Board rules read no filesystem, so a pattern counts when
// it may match the protected name: its trailing path components are tried
// on a sample spelling, case-folded (APFS), leading dots included.

// globSamples are, per rule regexp, the names a glob word is tried on.
var globSamples map[*regexp.Regexp][]string

func init() {
	globSamples = map[*regexp.Regexp][]string{
		reinstallRe:   {"scripts/reinstall-daemon.sh"},
		launchctlRe:   {"launchctl"},
		daemonRe:      {"staypointd"},
		killRe:        {"pkill", "killall"},
		nestedAgentRe: {"claude", "claude-code", "gemini", "codex", "agy", "cursor-agent", "aider"},
		sshNameRe:     {"ssh", "mosh", "autossh"},
	}
}

// globMayName reports code with a glob or brace character, which a parse
// must then read for a spelled name (the text prefilters cannot).
func globMayName(code string) bool { return strings.ContainsAny(code, "*?[{") }

// globSpells reports code with a word (an argument, a redirect target, in
// a substitution or a shell's -c string or here-document too) that is a
// glob that may spell what re names: the prefilter for the parse-based
// rules, whose text check cannot see bash scripts/rein*.
func globSpells(re *regexp.Regexp, code string, depth int) bool {
	if !globMayName(code) {
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
		if globSpells(re, s, depth+1) {
			return true
		}
	}
	for _, s := range segs {
		for _, a := range s.argv {
			if globNames(re, a) || strings.ContainsAny(a, " \t\n;|&") && globSpells(re, a, depth+1) {
				return true
			}
		}
		for _, r := range s.redirects {
			if r.heredoc && globSpells(re, r.body, depth+1) || !r.heredoc && globNames(re, r.target) {
				return true
			}
		}
	}
	return false
}

// globNames reports a word with a glob or brace group that may expand to a
// name re protects.
func globNames(re *regexp.Regexp, w string) bool {
	if !globMayName(w) {
		return false
	}
	for _, alt := range braceExpand(w) {
		if re.MatchString(alt) {
			return true
		}
		if !hasWild(alt) {
			continue
		}
		for _, s := range globSamples[re] {
			if globTail(alt, s) {
				return true
			}
		}
	}
	return false
}

// globTail reports whether pattern's trailing components may match the
// sample's, as many as both have.
func globTail(pat, sample string) bool {
	pc := strings.Split(strings.Trim(pat, "/"), "/")
	sc := strings.Split(sample, "/")
	for i := 1; i <= len(pc) && i <= len(sc); i++ {
		if !compMatch(pc[len(pc)-i], sc[len(sc)-i], true) {
			return false
		}
	}
	return true
}

// nameMatch is re on a word, or a glob word that may spell what re names.
func nameMatch(re *regexp.Regexp, w string) bool { return re.MatchString(w) || globNames(re, w) }

// selfNames are what a path whose root is unknown may not spell in the
// Board rules: the StayPoint dir, LaunchAgents and the token files.
var selfNames = []string{".staypoint", "LaunchAgents"}

// selfPathSpelled reports a command that may touch StayPoint state or the
// agent guards however the path is spelled: the tier resolver (globs,
// case, braces, variables, cd and -C, recursive parents) run against the
// self-protected paths. cwd is the command's working directory ("" when
// unknown); the run's own handoff reads stay exempt.
func selfPathSpelled(code, cwd, taskID string) bool {
	home, _ := os.UserHomeDir()
	if home == "" {
		return false
	}
	c := &Classifier{Home: home, CWD: cwd, TaskID: taskID, selfPaths: true}
	for _, r := range c.Classify(code).Reasons {
		for _, p := range []string{"touches sensitive path", "may touch a sensitive path", "recurses into", "inline script names a sensitive path"} {
			if strings.Contains(r, p) {
				return true
			}
		}
	}
	return false
}

// selfDirs are the self-protected paths under home (selfPathRe's).
func selfDirs(home string) []string {
	var out []string
	for _, d := range []string{".staypoint", ".local/bin", ".claude/settings.json", ".claude/settings.local.json",
		".claude/hooks", ".gemini/settings.json", ".gemini/hooks", ".gemini/config/hooks", "Library/LaunchAgents"} {
		out = append(out, filepath.Join(home, d))
	}
	return out
}

// cwdSelfDirs are the agent guard files of the project the command runs in.
// They are matched as paths, not as recursive parents: grep -r . in a repo
// does not change its hooks.
func cwdSelfDirs(cwd string) []string {
	if cwd == "" {
		return nil
	}
	var out []string
	for _, d := range []string{".claude/settings.json", ".claude/settings.local.json", ".claude/hooks",
		".gemini/settings.json", ".gemini/hooks"} {
		out = append(out, filepath.Join(cwd, d))
	}
	return out
}
