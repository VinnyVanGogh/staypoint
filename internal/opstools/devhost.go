package opstools

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/config"
)

// DevHostRequest is a dev_host_run call.
type DevHostRequest struct {
	Host    string `json:"host"`
	Action  string `json:"action"`
	App     string `json:"app"`
	Path    string `json:"path"`
	Service string `json:"service"`
	Branch  string `json:"branch"`
	Lines   int    `json:"lines"`
	Port    int    `json:"port"`
}

// devActions are the only things dev_host_run does, with their effects.
var devActions = map[string]Effect{
	"git_status":       Read,
	"git_log":          Read,
	"ls":               Read,
	"cat_file":         Read,
	"journal_tail":     Read,
	"http_get_local":   Read,
	"systemctl_status": Read,
	"git_ff_pull":      DevWrite,
	"collectstatic":    DevWrite,
	"restart":          DevWrite,
	"pip_sync":         DevWrite,
}

// DevActions lists the dev_host_run actions, sorted.
func DevActions() []string {
	out := make([]string, 0, len(devActions))
	for a := range devActions {
		out = append(out, a)
	}
	slices.Sort(out)
	return out
}

// DevHostPlan is a validated dev_host_run call ready to run over ssh.
type DevHostPlan struct {
	Call
	Host string
	// HostName and SSHConfig pin where ssh goes (config.DevHostConfig).
	HostName  string
	SSHConfig string
	// Remote is the remote shell command; every value in it was validated
	// and single-quoted.
	Remote  string
	Timeout time.Duration
}

var (
	hostRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	serviceRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._-]*$`)
	branchRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	appRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	urlPathRe = regexp.MustCompile(`^/[A-Za-z0-9._~%/?&=+,:@-]*$`)
	// secretPathRe matches a path with an env, key or credential file (or
	// directory) in it. The same expression runs on the dev host against the
	// resolved path, so a symlink cannot rename its way past it.
	secretPathRe = `(^|/)(\.env(\.[^/]*)?|[^/]*\.env|[^/]*[Ss][Ee][Cc][Rr][Ee][Tt][^/]*|[^/]*[Cc][Rr][Ee][Dd][Ee][Nn][Tt][Ii][Aa][Ll][^/]*|[^/]*\.(pem|key|p12|pfx|jks|keystore)|id_(rsa|dsa|ecdsa|ed25519)[^/]*|\.netrc|\.pgpass|\.htpasswd|\.ssh)(/|$)`
	secretPath   = regexp.MustCompile(secretPathRe)
	// textFileRe is what cat_file reads: source, config and docs by
	// extension, plus a few well-known extensionless names. Anything else
	// (db.sqlite3, *.db, *.log, *.pyc, *.pickle, dumps, archives) is
	// refused rather than returned unredacted (Board review #2 M2). Like
	// secretPathRe it runs on the host too, against the resolved path.
	textFileRe = `(^|/)([^/]+\.(py|txt|md|rst|html|htm|css|scss|js|mjs|cjs|ts|tsx|jsx|json|yaml|yml|toml|cfg|ini|conf|service|timer|socket|sh|bash|xml|svg|j2|jinja|jinja2|tmpl|tpl|po|go|rb|java|kt|swift|c|h|cpp|hpp|rs|lock)|Dockerfile|Makefile|Procfile|Pipfile|Gemfile|README|LICENSE|CHANGELOG|VERSION)$`
	textFile   = regexp.MustCompile(textFileRe)
)

// defaultDevBranches are what git_ff_pull may pull when a host lists none.
var defaultDevBranches = []string{"dev-server", "dev"}

const (
	readTimeout  = 60 * time.Second
	writeTimeout = 10 * time.Minute
	// maxFileBytes bounds cat_file on the host itself.
	maxFileBytes = 256 << 10
)

// PlanDevHost validates r against the configured dev hosts and builds the
// remote command. Nothing about r reaches the remote shell unvalidated.
func PlanDevHost(gates config.GatesConfig, dataDir string, r DevHostRequest) (*DevHostPlan, error) {
	if !hostRe.MatchString(r.Host) {
		return nil, fmt.Errorf("host %q is not a valid host alias", r.Host)
	}
	if !gates.Hosts.IsDevHost(r.Host) {
		return nil, fmt.Errorf("host %q is not in [gates.hosts] dev; dev_host_run only reaches dev hosts", r.Host)
	}
	hc, ok := gates.Ops.DevHosts[r.Host]
	if !ok {
		return nil, fmt.Errorf("host %q has no [gates.ops.dev_hosts.%s] table", r.Host, r.Host)
	}
	effect, ok := devActions[r.Action]
	if !ok {
		return nil, fmt.Errorf("action %q is not one of %s", r.Action, strings.Join(DevActions(), ", "))
	}
	appDir, err := cleanAbsDir(hc.AppDir, "app_dir")
	if err != nil {
		return nil, fmt.Errorf("host %s: %w", r.Host, err)
	}

	if !hostRe.MatchString(hc.HostName) {
		return nil, fmt.Errorf("host %s: [gates.ops.dev_hosts.%s] host_name must be set to the address to connect to", r.Host, r.Host)
	}
	// ssh -F is required and must be StayPoint's own file: without it ssh
	// reads ~/.ssh/config, whose Match exec runs a local command and whose
	// Port, User, KnownHostsCommand, PKCS11Provider and IdentityAgent the
	// -o pins below do not cover (Board review #2 #6).
	sshConfig := filepath.Clean(hc.SSHConfig)
	switch {
	case hc.SSHConfig == "":
		return nil, fmt.Errorf("host %s: [gates.ops.dev_hosts.%s] ssh_config is required (a StayPoint-owned ssh config under %s)", r.Host, r.Host, dataDir)
	case !filepath.IsAbs(hc.SSHConfig):
		return nil, fmt.Errorf("host %s: ssh_config must be an absolute path", r.Host)
	case dataDir == "" || !strings.HasPrefix(sshConfig, filepath.Clean(dataDir)+string(filepath.Separator)):
		return nil, fmt.Errorf("host %s: ssh_config %s must be inside the StayPoint data dir %s", r.Host, hc.SSHConfig, dataDir)
	}
	p := &DevHostPlan{Host: r.Host, HostName: hc.HostName, SSHConfig: sshConfig, Timeout: readTimeout}
	p.Tool = "dev_host_run"
	p.Effect = effect
	if effect == DevWrite {
		p.Timeout = writeTimeout
	}
	summary := []string{"host=" + r.Host, "action=" + r.Action}

	// dir is the checkout the git and file actions work in.
	dir := appDir
	var app config.DevAppConfig
	if r.App != "" {
		if !appRe.MatchString(r.App) {
			return nil, fmt.Errorf("app %q is not a valid app name", r.App)
		}
		a, ok := hc.Apps[r.App]
		if !ok {
			return nil, fmt.Errorf("app %q is not configured for host %s", r.App, r.Host)
		}
		if dir, err = cleanAbsDir(a.Dir, "apps."+r.App+".dir"); err != nil {
			return nil, err
		}
		app = a
		summary = append(summary, "app="+r.App)
	}

	switch r.Action {
	case "git_status":
		p.Remote = "git -C " + shQuote(dir) + " status --short --branch"
	case "git_log":
		n, err := lines(r.Lines, 20, 200)
		if err != nil {
			return nil, err
		}
		p.Remote = "git -C " + shQuote(dir) + " log --oneline -n " + strconv.Itoa(n)
		summary = append(summary, "lines="+strconv.Itoa(n))
	case "ls", "cat_file":
		target, err := underDir(dir, r.Path, r.Action == "ls")
		if err != nil {
			return nil, err
		}
		cmd := "ls -la -- \"$p\""
		if r.Action == "cat_file" {
			if !textFile.MatchString(target) {
				return nil, fmt.Errorf("path %q is not a text source/config/doc file; cat_file never returns data files (databases, logs, pickles, dumps)", r.Path)
			}
			cmd = "if ! printf '%s' \"$p\" | grep -Eq " + shQuote(textFileRe) + "; then echo 'refused: not a text file' >&2; exit 3; fi; " +
				"head -c " + strconv.Itoa(maxFileBytes) + " -- \"$p\""
		}
		p.Remote = resolvedUnder(target, dir) + cmd
		summary = append(summary, "path="+target)
	case "journal_tail":
		svc, err := service(hc, r.Service)
		if err != nil {
			return nil, err
		}
		n, err := lines(r.Lines, 100, 2000)
		if err != nil {
			return nil, err
		}
		p.Remote = "journalctl -u " + shQuote(svc) + " -n " + strconv.Itoa(n) + " --no-pager"
		summary = append(summary, "service="+svc, "lines="+strconv.Itoa(n))
	case "systemctl_status":
		svc, err := service(hc, r.Service)
		if err != nil {
			return nil, err
		}
		p.Remote = "systemctl status " + shQuote(svc) + " --no-pager -n 20"
		summary = append(summary, "service="+svc)
	case "http_get_local":
		if r.Port < 1 || r.Port > 65535 {
			return nil, fmt.Errorf("port %d is not 1-65535", r.Port)
		}
		pth := r.Path
		if pth == "" {
			pth = "/"
		}
		if !urlPathRe.MatchString(pth) {
			return nil, fmt.Errorf("path %q must start with / and hold only URL path characters", pth)
		}
		url := "http://127.0.0.1:" + strconv.Itoa(r.Port) + pth
		p.Remote = "curl -sS -m 10 -w " + shQuote(`\nHTTP %{http_code}\n`) + " -- " + shQuote(url)
		summary = append(summary, "port="+strconv.Itoa(r.Port), "path="+pth)
	case "git_ff_pull":
		allowed := hc.Branches
		if len(allowed) == 0 {
			allowed = defaultDevBranches
		}
		if !branchRe.MatchString(r.Branch) || strings.Contains(r.Branch, "..") || !slices.Contains(allowed, r.Branch) {
			return nil, fmt.Errorf("branch %q is not one of %s", r.Branch, strings.Join(allowed, ", "))
		}
		d, b := shQuote(dir), shQuote(r.Branch)
		p.Remote = "cur=$(git -C " + d + " branch --show-current) && [ \"$cur\" = " + b + " ] || { echo \"refused: checkout is on branch $cur, not \"" + b + " >&2; exit 3; }; " +
			"git -C " + d + " pull --ff-only origin " + b + " && git -C " + d + " log --oneline -1"
		summary = append(summary, "branch="+r.Branch)
	case "restart":
		svc, err := service(hc, r.Service)
		if err != nil {
			return nil, err
		}
		pre := ""
		if hc.SudoRestart {
			pre = "sudo -n "
		}
		p.Remote = pre + "systemctl restart " + shQuote(svc) + " && systemctl is-active " + shQuote(svc)
		summary = append(summary, "service="+svc)
	case "collectstatic", "pip_sync":
		if r.App == "" {
			return nil, fmt.Errorf("%s needs app (one of %s)", r.Action, strings.Join(appNames(hc), ", "))
		}
		py, err := cleanAbsDir(app.Python, "apps."+r.App+".python")
		if err != nil {
			return nil, err
		}
		if r.Action == "collectstatic" {
			p.Remote = "cd " + shQuote(dir) + " && " + shQuote(py) + " manage.py collectstatic --noinput"
			break
		}
		req := app.Requirements
		if req == "" {
			req = "requirements.txt"
		}
		if path.IsAbs(req) || strings.Contains(req, "..") || strings.HasPrefix(req, "-") {
			return nil, fmt.Errorf("apps.%s.requirements must be a relative path inside the app", r.App)
		}
		p.Remote = "cd " + shQuote(dir) + " && " + shQuote(py) + " -m pip install -r " + shQuote(req)
	}
	p.Summary = strings.Join(summary, " ")
	return p, nil
}

// resolvedUnder resolves target on the host and refuses it unless it is
// still under dir and is not a secret file; it leaves the path in $p.
func resolvedUnder(target, dir string) string {
	// The app dir is resolved too, so a dir that is itself a symlink (a
	// current-release link) compares resolved to resolved (Board review #3).
	return "d=$(readlink -f -- " + shQuote(dir) + ") && [ -d \"$d\" ] || { echo 'no such app dir' >&2; exit 2; }; " +
		"p=$(readlink -f -- " + shQuote(target) + ") && [ -e \"$p\" ] || { echo 'no such path' >&2; exit 2; }; " +
		"case \"$p\" in \"$d\"|\"$d\"/*) ;; *) echo 'refused: path resolves outside the app dir' >&2; exit 3;; esac; " +
		"if printf '%s' \"$p\" | grep -Eq " + shQuote(secretPathRe) + "; then echo 'refused: env/secret file' >&2; exit 3; fi; "
}

// underDir joins rel onto dir and refuses anything that leaves dir or names
// a secret file. allowEmpty lets ls default to dir itself.
func underDir(dir, rel string, allowEmpty bool) (string, error) {
	if rel == "" {
		if allowEmpty {
			return dir, nil
		}
		return "", errors.New("path is required")
	}
	if strings.ContainsAny(rel, "\x00\n\r") {
		return "", errors.New("path holds control characters")
	}
	full := rel
	if !path.IsAbs(rel) {
		full = path.Join(dir, rel)
	}
	full = path.Clean(full)
	if full != dir && !strings.HasPrefix(full, dir+"/") {
		return "", fmt.Errorf("path %q is outside the app dir %s", rel, dir)
	}
	if secretPath.MatchString(full) {
		return "", fmt.Errorf("path %q is an env/secret file; dev_host_run never reads those", rel)
	}
	return full, nil
}

func cleanAbsDir(p, key string) (string, error) {
	if p == "" || !path.IsAbs(p) || strings.ContainsAny(p, "\x00\n\r") {
		return "", fmt.Errorf("%s must be an absolute path", key)
	}
	c := path.Clean(p)
	if c == "/" {
		return "", fmt.Errorf("%s must not be /", key)
	}
	return c, nil
}

func service(hc config.DevHostConfig, s string) (string, error) {
	if !serviceRe.MatchString(s) || !slices.Contains(hc.Services, s) {
		return "", fmt.Errorf("service %q is not one of the host's services (%s)", s, strings.Join(hc.Services, ", "))
	}
	return s, nil
}

func lines(n, def, max int) (int, error) {
	if n == 0 {
		return def, nil
	}
	if n < 1 || n > max {
		return 0, fmt.Errorf("lines %d is not 1-%d", n, max)
	}
	return n, nil
}

func appNames(hc config.DevHostConfig) []string {
	out := make([]string, 0, len(hc.Apps))
	for a := range hc.Apps {
		out = append(out, a)
	}
	slices.Sort(out)
	return out
}

// shQuote single-quotes s for a POSIX shell.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// RunDevHost runs p over ssh in batch mode and returns its redacted output.
func RunDevHost(ctx context.Context, run Runner, p *DevHostPlan) string {
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	res := run(ctx, Cmd{Name: "ssh", Args: p.SSHArgs()})
	return res.Format()
}

// SSHArgs is the ssh argv for p. Command-line options win over any config
// file, so the configured HostName is where it connects, with no proxy,
// shared control socket or local command an ssh config could add.
func (p *DevHostPlan) SSHArgs() []string {
	args := []string{"-T", "-F", p.SSHConfig}
	for _, o := range []string{
		"BatchMode=yes", "ConnectTimeout=10", "HostName=" + p.HostName,
		"ProxyCommand=none", "ProxyJump=none", "ControlMaster=no", "ControlPath=none",
		"PermitLocalCommand=no", "StrictHostKeyChecking=yes",
	} {
		args = append(args, "-o", o)
	}
	return append(args, p.Host, p.Remote)
}
