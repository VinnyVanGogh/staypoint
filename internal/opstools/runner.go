package opstools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Cmd is one process an ops tool runs: an argv, never a shell line.
type Cmd struct {
	Name  string
	Args  []string
	Dir   string
	Stdin []byte
	// Env is added to the inherited environment.
	Env []string
}

// Result is a finished Cmd. Err is set when the process could not run or
// was cut off (not for a non-zero exit).
type Result struct {
	// Output is stdout and stderr interleaved; Stdout is stdout alone.
	Output   string
	Stdout   string
	ExitCode int
	Err      error
}

// Format is the redacted text a tool returns: the exit status, then output.
func (r Result) Format() string {
	head := fmt.Sprintf("exit %d", r.ExitCode)
	if r.Err != nil {
		head += " (" + r.Err.Error() + ")"
	}
	out := strings.TrimRight(Redact(r.Output), "\n")
	if out == "" {
		return head
	}
	return head + "\n" + capOutput(out)
}

// Runner runs a Cmd; tests replace ExecRunner with a fake.
type Runner func(ctx context.Context, c Cmd) Result

// TrustedPath is the PATH every ops tool process gets, so a caller's PATH
// cannot swap in its own gh, git, ssh or bash.
const TrustedPath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

// childEnvDrop are variables that would point gh, git, ssh, curl or a shell
// somewhere other than where the tool means to go (Board review #2 H1:
// GH_HOST / GH_CONFIG_DIR sent dev_deploy_verify's gh api to another host).
var childEnvDrop = map[string]bool{
	"XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true, "XDG_STATE_HOME": true,
	"BASH_ENV": true, "ENV": true, "SHELLOPTS": true, "BASHOPTS": true, "CDPATH": true,
	"IFS": true, "PS4": true, "PROMPT_COMMAND": true, "ZDOTDIR": true,
	"SSH_ASKPASS": true, "SSH_ASKPASS_REQUIRE": true, "CURL_HOME": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "CURL_CA_BUNDLE": true, "REQUESTS_CA_BUNDLE": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true,
	"http_proxy": true, "https_proxy": true, "all_proxy": true, "no_proxy": true,
	"PATH": true,
}

var childEnvDropPrefix = []string{"GH_", "GITHUB_", "GIT_", "DYLD_", "LD_", "BASH_FUNC_"}

// ChildEnv is environ with childEnvDrop and its prefixes removed and PATH
// set to TrustedPath.
func ChildEnv(environ []string) []string {
	out := make([]string, 0, len(environ)+1)
next:
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if childEnvDrop[name] {
			continue
		}
		for _, p := range childEnvDropPrefix {
			if strings.HasPrefix(name, p) {
				continue next
			}
		}
		out = append(out, kv)
	}
	return append(out, "PATH="+TrustedPath)
}

// lookTrusted resolves a bare command name on TrustedPath; exec.Command
// would search the server's own PATH, which the caller set.
func lookTrusted(name string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, dir := range filepath.SplitList(TrustedPath) {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found on %s", name, TrustedPath)
}

// capture collects a process's interleaved output and its stdout alone.
// One lock covers both, since exec copies stdout and stderr from separate
// goroutines. (An io.MultiWriter for stdout plus the same buffer for
// stderr lost every stdout line from the interleaved copy: Board review
// #2 L2, where dev_host_run returned only "exit 0".)
type capture struct {
	mu       sync.Mutex
	all, out bytes.Buffer
}

type captureWriter struct {
	c      *capture
	stdout bool
}

func (w captureWriter) Write(p []byte) (int, error) {
	w.c.mu.Lock()
	defer w.c.mu.Unlock()
	if w.stdout {
		w.c.out.Write(p)
	}
	return w.c.all.Write(p)
}

// ExecRunner runs c with its combined output captured.
func ExecRunner(ctx context.Context, c Cmd) Result {
	name, err := lookTrusted(c.Name)
	if err != nil {
		return Result{ExitCode: -1, Err: err}
	}
	cmd := exec.CommandContext(ctx, name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(ChildEnv(os.Environ()), c.Env...)
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	buf := &capture{}
	cmd.Stdout = captureWriter{buf, true}
	cmd.Stderr = captureWriter{buf, false}
	err = cmd.Run()
	res := Result{Output: buf.all.String(), Stdout: buf.out.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case ctx.Err() != nil:
		res.ExitCode, res.Err = -1, ctx.Err()
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
	default:
		res.ExitCode, res.Err = -1, err
	}
	return res
}
