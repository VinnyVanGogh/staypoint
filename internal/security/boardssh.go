package security

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// ssh by host class (Board, 2026-10-09). Every ssh used to hold as "may
// write prod", reads of the dev box included. Hosts are now classed by
// config.toml ([gates.hosts] dev = [...], prod = [...]); a host in neither
// list is unknown and treated as prod. ssh to a prod or unknown host, or
// through one (-J, ProxyJump), still holds. For a dev host the remote
// command is checked like a local one: reads pass, the dev deploy steps
// (git pull, systemctl of a listed app service, verify_dev_deploy.sh) pass
// under the unattended policy (dev deploys are allowed), anything else
// holds. Interactive ssh, tunnels and forwards, and options that run a
// program or pick another host hold whatever the class.

// GateHosts is the [gates.hosts] table: ssh destinations by class, and the
// services a dev deploy may restart.
type GateHosts struct {
	Dev         []string
	Prod        []string
	DevServices []string
}

var (
	gateHostsMu sync.RWMutex
	gateHosts   GateHosts
)

// SetGateHosts installs the host classes from config.toml. The CLI hook and
// the daemon call it when they load the config.
func SetGateHosts(h GateHosts) {
	gateHostsMu.Lock()
	defer gateHostsMu.Unlock()
	gateHosts = h
}

func currentGateHosts() GateHosts {
	gateHostsMu.RLock()
	defer gateHostsMu.RUnlock()
	return gateHosts
}

// hostClass returns "dev", "prod" or "unknown" for an ssh destination host.
func hostClass(host string) string {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	hosts := currentGateHosts()
	for _, p := range hosts.Prod {
		if strings.EqualFold(p, h) {
			return "prod"
		}
	}
	for _, d := range hosts.Dev {
		if strings.EqualFold(d, h) {
			return "dev"
		}
	}
	return "unknown"
}

// otherRemoteRe: remote shells and tunnels other than ssh, held whatever
// they run.
var otherRemoteRe = regexp.MustCompile(`(?i)(\bgcloud\s+compute\s+(ssh|scp)\b|\baws\s+ssm\s+(start-session|send-command)\b|\bkubectl\s+(exec|attach|port-forward)\b|\bdocker\s+(-H|--host|context)\b)`)

// sshNameRe is an ssh client at a program position.
var sshNameRe = regexp.MustCompile(`(?i)^(ssh|mosh|autossh)$`)

// sshWordRe is the prefilter: the line names an ssh client somewhere.
var sshWordRe = regexp.MustCompile(`(?i)\b(ssh|mosh|autossh)\b`)

// sshInWordRe: an ssh call inside one word (code for an interpreter, a
// watch or rsync -e string).
var sshInWordRe = regexp.MustCompile(`(?i)(^|[^\w.-])(ssh|mosh|autossh)(\s|$|['"])`)

const remoteHeld = "opens a remote shell or tunnel, which may write prod or delete data (Board rule: no prod writes or deletes)"

// remoteShellRule returns why code's remote shell breaks a Board rule, or
// "" when it has none or only reads and deploys on a dev host.
func remoteShellRule(code, taskID string) string {
	return remoteShellDepth(code, taskID, 0)
}

func remoteShellDepth(code, taskID string, depth int) string {
	if textMatch(otherRemoteRe, code) {
		return remoteHeld
	}
	if !textMatch(sshWordRe, code) && !globSpells(sshNameRe, code, 0) {
		return ""
	}
	if depth > maxDepth {
		return remoteHeld
	}
	segs, subs, err := parseShell(code)
	if err != nil {
		return remoteHeld
	}
	for _, s := range subs {
		if why := remoteShellDepth(s, taskID, depth+1); why != "" {
			return why
		}
	}
	found := false
	for k, s := range segs {
		argv, _ := unwrapArgv(s.argv)
		if len(argv) == 0 {
			continue
		}
		name := baseCmd(argv)
		if shells[name] {
			if ci, ok := shellCommandArg(name, argv[1:]); ok {
				if why := remoteShellDepth(argv[1+ci], taskID, depth+1); why != "" {
					return why
				}
			}
			for _, r := range s.redirects {
				if r.heredoc {
					if why := remoteShellDepth(r.body, taskID, depth+1); why != "" {
						return why
					}
				}
			}
			continue
		}
		if !nameMatch(sshNameRe, name) && !nameMatch(sshNameRe, argv[0]) {
			// ssh inside an interpreter's code or a watch string.
			if !dataArgs(argv) {
				for _, a := range argv[1:] {
					if sshInWordRe.MatchString(a) {
						return remoteHeld
					}
				}
			}
			continue
		}
		found = true
		if name != "ssh" {
			return name + " " + remoteHeld
		}
		fedInput := (k > 0 && segs[k-1].piped) || readsFile(s)
		off := len(s.argv) - len(argv)
		rest := sub(s, off+1)
		rest.redirects = s.redirects
		if why := sshRule(argv[1:], rest, fedInput, taskID, depth); why != "" {
			return why
		}
	}
	if !found && remoteShellRe.MatchString(code) {
		// The text names ssh at a program position the parse did not
		// reach (inside an interpreter's code, a watch string): hold.
		return remoteHeld
	}
	return ""
}

// sshValueOpts are ssh's short options that take a value.
const sshValueOpts = "BbcDEeFIiJLlmOoPpQRSWw"

// sshSafeFlags are ssh's valueless options that neither forward anything
// nor change what runs: -f backgrounds after auth, -G only prints config.
const sshSafeFlags = "46CfGgKkqTtVvn"

// sshSafeKeys are -o options that only tune the connection.
var sshSafeKeys = map[string]bool{
	"connecttimeout": true, "batchmode": true, "stricthostkeychecking": true, "serveraliveinterval": true,
	"serveralivecountmax": true, "connectionattempts": true, "loglevel": true, "userknownhostsfile": true,
	"identityfile": true, "identitiesonly": true, "user": true, "port": true, "compression": true,
	"requesttty": true, "addressfamily": true, "preferredauthentications": true, "passwordauthentication": true,
	"kbdinteractiveauthentication": true, "pubkeyauthentication": true, "checkhostip": true,
	"updatehostkeys": true, "visualhostkey": true, "hashknownhosts": true, "tcpkeepalive": true,
}

// sshRule checks one ssh call: args after "ssh", s the segment from there
// (for dyn flags and redirects), fedInput when its stdin is a pipe or file.
func sshRule(args []string, s segment, fedInput bool, taskID string, depth int) string {
	var hops []string
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			i++
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			break
		}
		if segDyn(s, i) {
			return "ssh option " + a + " is built at run time; " + remoteHeld
		}
		for j := 1; j < len(a); j++ {
			ch := a[j]
			if strings.IndexByte(sshValueOpts, ch) >= 0 {
				val := a[j+1:]
				if val == "" {
					i++
					if i >= len(args) {
						return "ssh -" + string(ch) + " has no value; " + remoteHeld
					}
					if segDyn(s, i) {
						return "ssh option value is built at run time; " + remoteHeld
					}
					val = args[i]
				}
				switch ch {
				case 'L', 'R', 'D', 'W', 'w':
					return fmt.Sprintf("ssh -%c opens a tunnel or forward; %s", ch, remoteHeld)
				case 'F', 'I', 'O', 'S', 'E':
					return fmt.Sprintf("ssh -%c (another config, a library, a control socket or a log file); %s", ch, remoteHeld)
				case 'J':
					hops = append(hops, jumpHosts(val)...)
				case 'o':
					h, why := sshOption(val)
					if why != "" {
						return why
					}
					hops = append(hops, h...)
				}
				break
			}
			if strings.IndexByte(sshSafeFlags, ch) < 0 {
				// -A agent, -X/-Y X11 forwarding, -N/-M tunnels and masters,
				// -s subsystems, unknown letters.
				return fmt.Sprintf("ssh -%c forwards, multiplexes or runs a subsystem; %s", ch, remoteHeld)
			}
		}
	}
	if i >= len(args) {
		return "ssh with no destination; " + remoteHeld
	}
	if segDyn(s, i) || segMeta(s, i) {
		return "ssh destination " + args[i] + " is built at run time; " + remoteHeld
	}
	dest := sshHost(args[i])
	hops = append(hops, dest)
	for _, h := range hops {
		if c := hostClass(h); c != "dev" {
			return fmt.Sprintf("ssh to %s host %s: %s (dev hosts are listed in config.toml [gates.hosts] dev)", classWord(c), h, remoteHeld)
		}
	}
	cmd := args[i+1:]
	var docs []*redirect
	for _, r := range s.redirects {
		if r.heredoc {
			docs = append(docs, r)
		}
	}
	for j := range cmd {
		if segDyn(s, i+1+j) {
			return fmt.Sprintf("ssh to dev host %s: the remote command is built by a local expansion (%s), which cannot be checked", dest, cmd[j])
		}
	}
	inner := strings.Join(cmd, " ")
	switch {
	case len(cmd) == 0 && len(docs) == 1 && !fedInput:
		if !docs[0].quoted && strings.ContainsAny(docs[0].body, "$`\\") {
			return fmt.Sprintf("ssh to dev host %s: an unquoted here-document expands locally before it is sent, which cannot be checked", dest)
		}
		inner = docs[0].body
	case len(cmd) == 0:
		return fmt.Sprintf("ssh to dev host %s: an interactive shell or commands fed from input cannot be checked", dest)
	case len(docs) > 0 || fedInput:
		return fmt.Sprintf("ssh to dev host %s: the remote command reads local input, which cannot be checked", dest)
	}
	if why := devRemoteRule(inner, depth); why != "" {
		return fmt.Sprintf("ssh to dev host %s: %s", dest, why)
	}
	return ""
}

func classWord(c string) string {
	if c == "unknown" {
		return "unknown (treated as prod)"
	}
	return c
}

// sshHost returns the host of an ssh destination: [ssh://][user@]host[:port].
func sshHost(d string) string {
	if rest, ok := strings.CutPrefix(strings.ToLower(d), "ssh://"); ok {
		d = d[len(d)-len(rest):]
		if k := strings.IndexByte(d, '/'); k >= 0 {
			d = d[:k]
		}
		if at := strings.LastIndexByte(d, '@'); at >= 0 {
			d = d[at+1:]
		}
		if strings.HasPrefix(d, "[") {
			if k := strings.IndexByte(d, ']'); k > 0 {
				return d[1:k]
			}
		}
		if k := strings.LastIndexByte(d, ':'); k >= 0 {
			d = d[:k]
		}
		return d
	}
	if at := strings.LastIndexByte(d, '@'); at >= 0 {
		d = d[at+1:]
	}
	return d
}

// jumpHosts splits a -J / ProxyJump list into hosts.
func jumpHosts(v string) []string {
	var out []string
	for _, h := range strings.Split(v, ",") {
		out = append(out, sshHost(strings.TrimSpace(h)))
	}
	return out
}

// sshOption reads one -o option: hosts it adds as hops (ProxyJump,
// HostName), or why it holds.
func sshOption(v string) ([]string, string) {
	key, val, ok := strings.Cut(v, "=")
	if !ok {
		key, val, _ = strings.Cut(strings.TrimSpace(v), " ")
	}
	key = strings.ToLower(strings.TrimSpace(key))
	val = strings.TrimSpace(val)
	switch {
	case key == "proxyjump":
		if strings.EqualFold(val, "none") {
			return nil, ""
		}
		return jumpHosts(val), ""
	case key == "hostname":
		// The host actually reached, whatever alias is named.
		return []string{val}, ""
	case sshSafeKeys[key]:
		return nil, ""
	}
	return nil, "ssh -o " + key + " may run a program, forward or change where ssh connects; " + remoteHeld
}

// devRemoteRule checks a command to run on a dev host: the Board rules,
// then each command must be a read (tier Green) or a dev deploy step.
func devRemoteRule(inner string, depth int) string {
	if why := boardRuleText(inner, "the remote command", "", ""); why != "" {
		return strings.TrimPrefix(why, "the remote command ")
	}
	return devRemoteSegs(inner, depth)
}

func devRemoteSegs(inner string, depth int) string {
	if depth > maxDepth {
		return "remote command nesting too deep to analyse"
	}
	segs, subs, err := parseShell(inner)
	if err != nil {
		return "remote command does not parse: " + err.Error()
	}
	for _, s := range subs {
		if why := devRemoteSegs(s, depth+1); why != "" {
			return why
		}
	}
	// The remote home is not ours: no path of this machine is resolved.
	c := &Classifier{Home: "/nonexistent-remote-home", CWD: "/nonexistent-remote-cwd"}
	for _, s := range segs {
		argv, _ := unwrapArgv(s.argv)
		if len(argv) == 0 || groupClosers[argv[0]] || devDeployStep(argv) && !writesFile(s) {
			continue
		}
		var v Verdict
		c.classifySegment(s, &v, depth+1)
		if v.Tier != Green {
			return "remote command `" + strings.Join(s.argv, " ") + "` is not a read or a known dev deploy step (git pull, systemctl restart of a [gates.hosts] dev_services service, verify_dev_deploy.sh)"
		}
	}
	return ""
}

// gitPullFlags are git pull options a dev deploy may use.
var gitPullFlags = map[string]bool{"--ff-only": true, "--ff": true, "-q": true, "--quiet": true, "--prune": true,
	"--no-edit": true, "--no-rebase": true, "-v": true, "--verbose": true}

// devDeployStep reports a dev deploy step: git pull (fast-forward
// friendly flags), systemctl status or restart of a listed service, or
// verify_dev_deploy.sh.
func devDeployStep(argv []string) bool {
	name := baseCmd(argv)
	switch {
	case name == "git":
		sub, rest := gitSub(argv[1:])
		if len(rest) != len(argv)-2 {
			return false // global options (-c, -C ...) before pull
		}
		if sub != "pull" {
			return false
		}
		pos := 0
		for _, a := range rest {
			if strings.HasPrefix(a, "-") {
				if !gitPullFlags[a] {
					return false
				}
				continue
			}
			pos++
		}
		return pos <= 2
	case name == "systemctl":
		if len(argv) < 3 {
			return false
		}
		switch argv[1] {
		case "status", "is-active", "is-failed", "show":
			return true
		case "restart", "reload", "try-restart":
		default:
			return false
		}
		for _, svc := range argv[2:] {
			if !devService(svc) {
				return false
			}
		}
		return true
	case name == "verify_dev_deploy.sh":
		return true
	case name == "bash" || name == "sh":
		return len(argv) >= 2 && baseCmd(argv[1:]) == "verify_dev_deploy.sh"
	}
	return false
}

func devService(svc string) bool {
	svc = strings.TrimSuffix(svc, ".service")
	for _, s := range currentGateHosts().DevServices {
		if strings.TrimSuffix(s, ".service") == svc {
			return true
		}
	}
	return false
}
