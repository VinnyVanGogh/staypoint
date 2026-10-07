package security

import (
	"os"
	"path/filepath"
	"strings"
)

// StateChange is one state-changing action found in a shell command line,
// with the filesystem locations it lands in. The tracking gate (STA-854)
// decides per location whether it falls inside a gated work repo.
type StateChange struct {
	Action string   // e.g. "git commit", "redirect into file", "gh pr create"
	Paths  []string // absolute where resolvable; the effective cwd otherwise
}

// stateChangingGit lists git subcommands that change repo history or the
// remote. Read-only and working-tree-inspection subcommands are not listed.
var stateChangingGit = map[string]bool{
	"commit": true, "push": true, "merge": true, "rebase": true,
	"cherry-pick": true, "revert": true, "am": true, "pull": true,
}

// stateChangingGhPR lists `gh pr` subcommands that open or merge PRs.
var stateChangingGhPR = map[string]bool{"create": true, "merge": true}

// StateChanges reports the state-changing actions in a shell command line:
// history-changing git subcommands, `gh pr create|merge`, output redirection
// and `tee` into files, and direct sqlite3 writes to the StayPoint database.
// Unparseable input is returned as a single change at cwd (fail closed).
func StateChanges(line, cwd string) []StateChange {
	var out []StateChange
	stateChangesLine(line, cwd, 0, &out)
	return out
}

func stateChangesLine(line, cwd string, depth int, out *[]StateChange) {
	if depth > maxDepth {
		*out = append(*out, StateChange{Action: "command nesting too deep to analyse", Paths: []string{cwd}})
		return
	}
	segs, subs, err := parseShell(line)
	if err != nil {
		*out = append(*out, StateChange{Action: "unparseable command (" + err.Error() + ")", Paths: []string{cwd}})
		return
	}
	for _, s := range subs {
		stateChangesLine(s, cwd, depth+1, out)
	}
	dir := cwd
	for _, s := range segs {
		dir = stateChangesSegment(s, dir, depth, out)
	}
}

// stateChangesSegment inspects one simple command and returns the working
// directory for the commands after it (`cd x && git commit` commits in x).
func stateChangesSegment(s segment, cwd string, depth int, out *[]StateChange) string {
	for _, r := range s.redirects {
		if isOutputRedirect(r.op) && r.target != "" && r.target != "/dev/null" && !strings.HasPrefix(r.target, "/dev/fd/") {
			*out = append(*out, StateChange{Action: "redirect into " + r.target, Paths: []string{resolveAgainst(cwd, r.target)}})
		}
	}
	argv := stripPrefixes(s.argv)
	if len(argv) == 0 {
		return cwd
	}
	name := baseCmd(argv)
	args := argv[1:]

	switch {
	case name == "cd":
		if len(args) > 0 && args[0] != "-" {
			if d := resolveAgainst(cwd, args[0]); d != "" {
				return d
			}
		}
		return cwd
	case wrappers[name]:
		if inner := skipWrapper(name, args); len(inner) > 0 {
			stateChangesSegment(segment{argv: inner}, cwd, depth+1, out)
		}
	case shells[name]:
		for i, a := range args {
			if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") && i+1 < len(args) {
				stateChangesLine(args[i+1], cwd, depth+1, out)
				return cwd
			}
		}
		if len(args) > 0 {
			// `bash script.sh` runs code we cannot see: fail closed.
			*out = append(*out, StateChange{Action: name + " runs an opaque script", Paths: []string{cwd}})
		}
	case name == "eval":
		stateChangesLine(strings.Join(args, " "), cwd, depth+1, out)
	case name == "git":
		if sub, dir := gitSubcommand(args, cwd); stateChangingGit[sub] {
			*out = append(*out, StateChange{Action: "git " + sub, Paths: []string{dir}})
		}
	case name == "gh":
		if sub, act := ghPRSubcommand(args); sub == "pr" && stateChangingGhPR[act] {
			*out = append(*out, StateChange{Action: "gh pr " + act, Paths: []string{cwd}})
		}
	case name == "tee":
		for _, a := range args {
			if a == "" || strings.HasPrefix(a, "-") || a == "/dev/null" {
				continue
			}
			*out = append(*out, StateChange{Action: "tee into " + a, Paths: []string{resolveAgainst(cwd, a)}})
		}
	case name == "sqlite3":
		for _, a := range args {
			if strings.Contains(a, "staypoint.db") {
				*out = append(*out, StateChange{Action: "direct write to the StayPoint database", Paths: []string{cwd}})
				break
			}
		}
	}
	return cwd
}

func isOutputRedirect(op string) bool {
	if strings.HasPrefix(op, "<") || strings.HasPrefix(op, ">&") {
		return false
	}
	return strings.Contains(op, ">")
}

// gitSubcommand skips git's global options and returns the subcommand and
// the directory it operates in (after any -C).
func gitSubcommand(args []string, cwd string) (string, string) {
	dir := cwd
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		a := args[i]
		switch {
		case a == "-C":
			i++
			if i < len(args) {
				dir = resolveAgainst(dir, args[i])
			}
		case strings.HasPrefix(a, "-C"):
			dir = resolveAgainst(dir, a[2:])
		case a == "-c" || a == "--git-dir" || a == "--work-tree" || a == "--namespace":
			i++
		}
		i++
	}
	if i >= len(args) {
		return "", dir
	}
	return args[i], dir
}

// ghPRSubcommand returns the gh command group and its action, skipping
// global flags.
func ghPRSubcommand(args []string) (string, string) {
	var pos []string
	for i := 0; i < len(args) && len(pos) < 2; i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			if ghValueFlags[a] {
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	if len(pos) < 2 {
		if len(pos) == 1 {
			return pos[0], ""
		}
		return "", ""
	}
	return pos[0], pos[1]
}

// resolveAgainst makes p absolute against base, expanding a leading ~/.
func resolveAgainst(base, p string) string {
	if p == "" {
		return base
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, p[2:])
		}
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	if base == "" {
		return p
	}
	return filepath.Join(base, p)
}
