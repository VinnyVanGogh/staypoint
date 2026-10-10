package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests run the real scripts/reinstall-daemon.sh (STA-805) in a throwaway
// repo with a throwaway HOME. go, codesign, launchctl and curl are stubs on
// PATH, so nothing is built, signed or loaded into launchd.

// The go stub stamps vcs.revision the way Go does: from the nearest enclosing
// .git *directory*, ignoring a worktree's .git file. STUB_VCS_REV overrides
// the stamp; STUB_NO_VCS leaves it out.
const stubGo = `#!/bin/sh
case "$1" in
build)
    out=""
    while [ $# -gt 0 ]; do
        case "$1" in
            -o) out="$2"; shift ;;
            -ldflags) printf '%s\n' "$2" >> "$STUB_STATE/ldflags" ;;
        esac
        shift
    done
    d="$PWD"
    while [ "$d" != / ] && [ ! -d "$d/.git" ]; do d="$(dirname "$d")"; done
    rev=""
    [ -d "$d/.git" ] && rev="$(git -C "$d" rev-parse HEAD)"
    [ -n "$STUB_VCS_REV" ] && rev="$STUB_VCS_REV"
    [ -n "$STUB_NO_VCS" ] && rev=""
    printf '#!/bin/sh\n#rev=%s\nexit 0\n' "$rev" > "$out"
    chmod +x "$out"
    echo "$PWD" >> "$STUB_STATE/built" ;;
version)
    printf '%s: go1.25\n' "$3"
    rev="$(sed -n 's/^#rev=//p' "$3")"
    [ -z "$rev" ] || printf '\tbuild\tvcs.revision=%s\n' "$rev" ;;
esac
`

const stubLaunchctl = `#!/bin/sh
[ "$1" = print ] && echo "	pid = 4242"
exit 0
`

// Answers /api/health with the label the stub go build was given, so the
// script's start check sees the commit it just built.
//
// /api/daemon/drain (task-db71fba9): POST and DELETE are logged to
// drain_calls; each GET takes the next live-run count from the live_seq file
// (empty: 0) and records the installed binary in binary_during_drain, so a
// test can see the swap waited for the drain. STUB_DRAIN=old answers like a
// daemon without drain mode, STUB_DRAIN=forbidden like a wrong board token.
// STUB_DOWN=1: no daemon answers until the new binary is installed. argv and
// stdin (headers sent with -H @-) are logged to curl_args and curl_stdin.
const stubCurl = `#!/bin/sh
case " $* " in *" @- "*) cat >> "$STUB_STATE/curl_stdin" ;; esac
printf '%s\n' "$*" >> "$STUB_STATE/curl_args"
bin="$HOME/.local/bin/staypointd"
case "$*" in
*/api/daemon/drain*)
    case "$*" in
    *"-X POST"*)
        printf 'POST %s\n' "$*" >> "$STUB_STATE/drain_calls"
        case "$STUB_DRAIN" in
        old) printf '404 page not found\n'; exit 0 ;;
        forbidden) printf '{"error":"board_session_required","message":"forbidden"}'; exit 0 ;;
        esac
        printf '{"draining":true,"mode":"finish","live":1,"queued":0,"live_tasks":["task-a"]}' ;;
    *"-X DELETE"*)
        printf 'DELETE\n' >> "$STUB_STATE/drain_calls"
        printf '{"draining":false,"mode":"off","live":0,"queued":0,"live_tasks":[]}' ;;
    *)
        live=0
        if [ -s "$STUB_STATE/live_seq" ]; then
            live="$(head -1 "$STUB_STATE/live_seq")"
            tail -n +2 "$STUB_STATE/live_seq" > "$STUB_STATE/live_seq.tmp"
            mv "$STUB_STATE/live_seq.tmp" "$STUB_STATE/live_seq"
        fi
        cat "$bin" >> "$STUB_STATE/binary_during_drain" 2>/dev/null
        printf '{"draining":true,"mode":"finish","live":%s,"queued":2,"live_tasks":["task-a","task-b"]}' "$live" ;;
    esac
    exit 0 ;;
esac
if [ -n "$STUB_DOWN" ] && [ -e "$bin.staging" ]; then
    exit 7
fi
label="$(sed -n 's/.*main.GitCommit=\([^ ]*\).*/\1/p' "$STUB_STATE/ldflags" | head -1)"
printf '{"git_commit":"%s","repo_access":{"checked":true,"inaccessible":[]}}' "$label"
`

type deployFixture struct {
	t     *testing.T
	repo  string
	home  string
	state string
	env   []string
}

func newDeployFixture(t *testing.T) *deployFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("reinstall-daemon.sh is a bash script for macOS/Linux")
	}
	for _, tool := range []string{"bash", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}
	script, err := filepath.Abs("../../scripts/reinstall-daemon.sh")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	f := &deployFixture{
		t:     t,
		repo:  filepath.Join(root, "repo"),
		home:  filepath.Join(root, "home"),
		state: filepath.Join(root, "state"),
	}
	stubs := filepath.Join(root, "stubs")
	for _, d := range []string{f.repo, f.state, stubs, filepath.Join(f.repo, "scripts"),
		filepath.Join(f.home, ".staypoint"), filepath.Join(f.home, ".local", "bin"),
		filepath.Join(f.home, "Library", "LaunchAgents")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExec(t, filepath.Join(stubs, "go"), stubGo)
	writeExec(t, filepath.Join(stubs, "launchctl"), stubLaunchctl)
	writeExec(t, filepath.Join(stubs, "curl"), stubCurl)
	writeExec(t, filepath.Join(stubs, "codesign"), "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(stubs, "staypointd"), "#!/bin/sh\necho stub\n")
	if err := os.WriteFile(filepath.Join(f.home, ".staypoint", "auth_token"), []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.home, ".staypoint", "board_token"), []byte(stubBoardToken), 0o600); err != nil {
		t.Fatal(err)
	}

	f.env = append(os.Environ(),
		"HOME="+f.home,
		"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_STATE="+f.state,
		"STAYPOINT_START_TIMEOUT=5",
		"STAYPOINT_DRAIN_POLL=0.05",
		"STAYPOINT_REPO_CHECK_TIMEOUT=2",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)

	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	writeExec(t, filepath.Join(f.repo, "scripts", "reinstall-daemon.sh"), string(body))
	writeExec(t, filepath.Join(f.repo, "scripts", "check-signing-cert.sh"), "#!/bin/sh\necho IDENTITY=-\nexit \"${STUB_CERT_STATUS:-0}\"\n")
	if err := os.WriteFile(filepath.Join(f.repo, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	origin := filepath.Join(root, "origin.git")
	f.git(root, "init", "-q", "--bare", "-b", "main", origin)
	f.git(f.repo, "init", "-q", "-b", "main")
	f.git(f.repo, "add", ".")
	f.git(f.repo, "commit", "-q", "-m", "initial")
	f.git(f.repo, "remote", "add", "origin", origin)
	f.git(f.repo, "push", "-q", "origin", "main")
	f.git(f.repo, "fetch", "-q", "origin")
	return f
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (f *deployFixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = f.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// run executes the script and returns its combined output and exit code.
func (f *deployFixture) run(extraEnv []string, args ...string) (string, int) {
	return f.runIn(f.repo, extraEnv, args...)
}

// runIn runs the copy of the script checked out in dir.
func (f *deployFixture) runIn(dir string, extraEnv []string, args ...string) (string, int) {
	f.t.Helper()
	cmd := exec.Command("bash", append([]string{filepath.Join(dir, "scripts", "reinstall-daemon.sh")}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(append([]string{}, f.env...), extraEnv...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		f.t.Fatalf("run script: %v", err)
	}
	return string(out), code
}

func (f *deployFixture) read(path string) string {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		f.t.Fatal(err)
	}
	return string(data)
}

func (f *deployFixture) deployLog() string {
	return f.read(filepath.Join(f.home, ".staypoint", "deploys.log"))
}

func (f *deployFixture) installedBinary() string {
	return f.read(filepath.Join(f.home, ".local", "bin", "staypointd"))
}

// assertRefused checks the script exited 1, built nothing, left the live
// binary alone and logged the refusal.
func (f *deployFixture) assertRefused(out string, code int, reason, wantOutput string) {
	f.t.Helper()
	if code != 1 {
		f.t.Fatalf("exit code = %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, wantOutput) {
		f.t.Errorf("output does not mention %q:\n%s", wantOutput, out)
	}
	if got := f.installedBinary(); got != "old binary\n" {
		f.t.Errorf("live binary was replaced: %q", got)
	}
	log := f.deployLog()
	if !strings.Contains(log, "result=refused:"+reason) {
		f.t.Errorf("deploys.log lacks result=refused:%s:\n%s", reason, log)
	}
	if strings.Contains(log, "result=installed") {
		f.t.Errorf("deploys.log records an install:\n%s", log)
	}
}

func (f *deployFixture) seedLiveBinary() {
	writeExec(f.t, filepath.Join(f.home, ".local", "bin", "staypointd"), "old binary\n")
}

func TestReinstallScript_CleanMainInstallsAndLogs(t *testing.T) {
	f := newDeployFixture(t)
	out, code := f.run(nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if strings.Contains(out, "DEV BUILD") {
		t.Errorf("clean main build printed the dev banner:\n%s", out)
	}
	if !strings.Contains(f.read(filepath.Join(f.state, "ldflags")), "-X main.DevBuild=false") {
		t.Errorf("daemon not built with DevBuild=false")
	}
	sha := f.git(f.repo, "rev-parse", "HEAD")
	log := f.deployLog()
	for _, want := range []string{"result=installed", "sha=" + sha, "dirty=false", "in_main=true", "at_main=true", "dev_build=false", "user=", "host="} {
		if !strings.Contains(log, want) {
			t.Errorf("deploys.log lacks %q:\n%s", want, log)
		}
	}
}

func TestReinstallScript_RefusesDirtyTree(t *testing.T) {
	cases := map[string]func(f *deployFixture){
		"modified tracked file": func(f *deployFixture) {
			writeExec(f.t, filepath.Join(f.repo, "main.go"), "package main // edited\n")
		},
		"untracked file": func(f *deployFixture) {
			writeExec(f.t, filepath.Join(f.repo, "patch.go"), "package main\n")
		},
	}
	for name, dirty := range cases {
		t.Run(name, func(t *testing.T) {
			f := newDeployFixture(t)
			f.seedLiveBinary()
			dirty(f)
			out, code := f.run(nil)
			f.assertRefused(out, code, "dirty", "uncommitted changes")
			if f.read(filepath.Join(f.state, "built")) != "" {
				t.Errorf("script ran go build on a dirty tree")
			}
		})
	}
}

func TestReinstallScript_RefusesCommitNotOnOriginMain(t *testing.T) {
	f := newDeployFixture(t)
	f.seedLiveBinary()
	writeExec(t, filepath.Join(f.repo, "main.go"), "package main // unmerged\n")
	f.git(f.repo, "commit", "-q", "-am", "local only")
	out, code := f.run(nil)
	f.assertRefused(out, code, "not-in-main", "is not on origin/main")
	if f.read(filepath.Join(f.state, "built")) != "" {
		t.Errorf("script ran go build on a commit not on origin/main")
	}
}

func TestReinstallScript_AllowDevBuildOverrides(t *testing.T) {
	f := newDeployFixture(t)
	f.seedLiveBinary()
	writeExec(t, filepath.Join(f.repo, "main.go"), "package main // unmerged\n")
	f.git(f.repo, "commit", "-q", "-am", "local only")
	writeExec(t, filepath.Join(f.repo, "patch.go"), "package main\n")

	out, code := f.run(nil, "--allow-dev-build")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "DEV BUILD (--allow-dev-build)") {
		t.Errorf("no dev build banner:\n%s", out)
	}
	if !strings.Contains(f.read(filepath.Join(f.state, "ldflags")), "-X main.DevBuild=true") {
		t.Errorf("daemon not built with DevBuild=true, so /api/health would not report dev_build")
	}
	if got := f.installedBinary(); got == "old binary\n" {
		t.Errorf("dev build was not installed")
	}
	log := f.deployLog()
	for _, want := range []string{"result=installed", "dirty=true", "in_main=false", "dev_build=true"} {
		if !strings.Contains(log, want) {
			t.Errorf("deploys.log lacks %q:\n%s", want, log)
		}
	}
}

func TestReinstallScript_RefusesBinaryWithoutVCSRevision(t *testing.T) {
	f := newDeployFixture(t)
	f.seedLiveBinary()
	out, code := f.run([]string{"STUB_NO_VCS=1"})
	f.assertRefused(out, code, "no-vcs-revision", "has no vcs.revision")
	if _, err := os.Stat(filepath.Join(f.home, ".local", "bin", "staypointd.staging")); !os.IsNotExist(err) {
		t.Errorf("staged binary left behind (err=%v)", err)
	}
}

func TestReinstallScript_RefusesBinaryWithWrongVCSRevision(t *testing.T) {
	f := newDeployFixture(t)
	f.seedLiveBinary()
	out, code := f.run([]string{"STUB_VCS_REV=0123456789abcdef0123456789abcdef01234567"})
	f.assertRefused(out, code, "vcs-mismatch", "but HEAD is")
}

// A worktree nested in the shared checkout (.worktrees/deploy-main) must
// deploy with its own commit stamped, even when the shared checkout is dirty
// and at another commit. Go would stamp the shared checkout's HEAD for an
// in-place build, which is what made the restored 245f42c daemon report
// vcs.revision=88792ec, vcs.modified=true.
func TestReinstallScript_NestedWorktreeStampsItsOwnCommit(t *testing.T) {
	f := newDeployFixture(t)
	deploy := filepath.Join(f.repo, ".worktrees", "deploy-main")
	f.git(f.repo, "worktree", "add", "-q", "--detach", deploy, "main")
	// Main moves on in the deploy worktree, so the deploy is at origin/main's
	// tip while the shared checkout is left one commit behind and dirty.
	writeExec(t, filepath.Join(deploy, "main.go"), "package main // newer\n")
	f.git(deploy, "commit", "-q", "-am", "newer main")
	f.git(deploy, "push", "-q", "origin", "HEAD:main")
	want := f.git(deploy, "rev-parse", "HEAD")
	writeExec(t, filepath.Join(f.repo, "patch.go"), "package main\n") // shared checkout now dirty
	if shared := f.git(f.repo, "rev-parse", "HEAD"); shared == want {
		t.Fatalf("shared checkout should be at another commit than the deploy worktree")
	}

	out, code := f.runIn(deploy, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\n%s", code, out)
	}
	if got := f.installedBinary(); !strings.Contains(got, "#rev="+want) {
		t.Errorf("installed binary stamped %q, want vcs.revision %s", got, want)
	}
	for _, dir := range strings.Fields(f.read(filepath.Join(f.state, "built"))) {
		if strings.HasPrefix(dir, f.repo) {
			t.Errorf("built inside the shared checkout (%s), so Go would stamp its HEAD", dir)
		}
	}
	if log := f.deployLog(); !strings.Contains(log, "sha="+want) || !strings.Contains(log, "result=installed") {
		t.Errorf("deploys.log does not record the install of %s:\n%s", want, log)
	}
}

// A clean checkout that is behind origin/main is on main's history, but
// deploying it rolls the daemon back past merged PRs: the STA-805 incident
// through a clean tree (git stash in the stale shared checkout, then deploy).
func TestReinstallScript_RefusesCleanCheckoutBehindMain(t *testing.T) {
	f := newDeployFixture(t)
	f.seedLiveBinary()
	writeExec(t, filepath.Join(f.repo, "main.go"), "package main // newer\n")
	f.git(f.repo, "commit", "-q", "-am", "newer main")
	f.git(f.repo, "push", "-q", "origin", "main")
	f.git(f.repo, "checkout", "-q", "--detach", "HEAD~1")

	out, code := f.run(nil)
	f.assertRefused(out, code, "behind-main", "1 commit(s) behind origin/main")
	if f.read(filepath.Join(f.state, "built")) != "" {
		t.Errorf("script ran go build on a commit behind origin/main")
	}
	if log := f.deployLog(); !strings.Contains(log, "in_main=true") || !strings.Contains(log, "at_main=false") {
		t.Errorf("deploys.log should record in_main=true at_main=false:\n%s", log)
	}

	out, code = f.run(nil, "--allow-dev-build")
	if code != 0 {
		t.Fatalf("--allow-dev-build: exit code = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "origin/main tip: false") {
		t.Errorf("dev banner does not say HEAD is not origin/main's tip:\n%s", out)
	}
	if log := f.deployLog(); !strings.Contains(log, "result=installed") || !strings.Contains(log, "dev_build=true") {
		t.Errorf("deploys.log does not record the dev-build install:\n%s", log)
	}
}

func TestReinstallScript_RejectsUnknownArgument(t *testing.T) {
	f := newDeployFixture(t)
	f.seedLiveBinary()
	out, code := f.run(nil, "--allow-devbuild")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2\n%s", code, out)
	}
	if !strings.Contains(f.deployLog(), "result=refused:bad-arg") {
		t.Errorf("deploys.log lacks result=refused:bad-arg:\n%s", f.deployLog())
	}
	if got := f.installedBinary(); got != "old binary\n" {
		t.Errorf("live binary was replaced: %q", got)
	}
}

// The signing-certificate check only runs on macOS, so a uname stub makes the
// script take that path on any host.
func TestReinstallScript_RefusesWithoutSigningCertAndLogs(t *testing.T) {
	f := newDeployFixture(t)
	f.seedLiveBinary()
	darwin := t.TempDir()
	writeExec(t, filepath.Join(darwin, "uname"), "#!/bin/sh\necho Darwin\n")
	path := ""
	for _, kv := range f.env {
		if strings.HasPrefix(kv, "PATH=") {
			path = strings.TrimPrefix(kv, "PATH=")
		}
	}
	out, code := f.run([]string{"STUB_CERT_STATUS=1", "PATH=" + darwin + string(os.PathListSeparator) + path})
	f.assertRefused(out, code, "no-cert", "no valid signing certificate")
	if f.read(filepath.Join(f.state, "built")) != "" {
		t.Errorf("script ran go build without a signing certificate")
	}
}

func TestDevBuildReasonFrom(t *testing.T) {
	const sha = "245f42c0123456789abcdef0123456789abcdef0"
	cases := []struct {
		name, flag, commit, rev, modified, want string
	}{
		{"flag", "true", "245f42c", sha, "false", "deployed with --allow-dev-build"},
		{"raw go build (the 2026-10-06 binary)", "false", "none", "88792ec", "true", "not built by reinstall-daemon.sh: no commit stamped"},
		{"script main build", "false", "245f42c", sha, "false", ""},
		{"raw build from a dirty tree with GitCommit set", "false", "245f42c", sha, "true", "built from a tree with uncommitted changes (vcs.modified=true)"},
		{"raw build of another commit with GitCommit set", "false", "245f42c", "88792ec0123456789abcdef0123456789abcdef0", "false",
			"vcs.revision 88792ec0123456789abcdef0123456789abcdef0 does not match the stamped commit 245f42c"},
		{"raw build with -buildvcs=false", "false", "245f42c", "", "", "no vcs.revision stamped: reinstall-daemon.sh refuses such builds"},
	}
	for _, c := range cases {
		if got := devBuildReasonFrom(c.flag, c.commit, c.rev, c.modified); got != c.want {
			t.Errorf("%s: devBuildReasonFrom = %q, want %q", c.name, got, c.want)
		}
	}
}
