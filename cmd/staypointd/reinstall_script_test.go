package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

// These tests run the real scripts/reinstall-daemon.sh (STA-805) in a throwaway
// repo with a throwaway HOME. go, codesign, launchctl and curl are stubs on
// PATH, so nothing is built, signed or loaded into launchd.

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
    printf '#!/bin/sh\nexit 0\n' > "$out"
    chmod +x "$out"
    echo "$out" >> "$STUB_STATE/built" ;;
version)
    printf '%s: go1.25\n' "$3"
    [ -n "$STUB_NO_VCS" ] || printf '\tbuild\tvcs.revision=%s\n' "$(git rev-parse HEAD)" ;;
esac
`

const stubLaunchctl = `#!/bin/sh
[ "$1" = print ] && echo "	pid = 4242"
exit 0
`

// Answers /api/health with the label the stub go build was given, so the
// script's start check sees the commit it just built.
const stubCurl = `#!/bin/sh
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

	f.env = append(os.Environ(),
		"HOME="+f.home,
		"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"STUB_STATE="+f.state,
		"STAYPOINT_START_TIMEOUT=5",
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
	writeExec(t, filepath.Join(f.repo, "scripts", "check-signing-cert.sh"), "#!/bin/sh\necho IDENTITY=-\nexit 0\n")
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
	f.t.Helper()
	cmd := exec.Command("bash", append([]string{filepath.Join(f.repo, "scripts", "reinstall-daemon.sh")}, args...)...)
	cmd.Dir = f.repo
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
	for _, want := range []string{"result=installed", "sha=" + sha, "dirty=false", "in_main=true", "dev_build=false", "user=", "host="} {
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

func TestReinstallScript_RejectsUnknownArgument(t *testing.T) {
	f := newDeployFixture(t)
	out, code := f.run(nil, "--allow-devbuild")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2\n%s", code, out)
	}
}

func TestDevBuildReasonFrom(t *testing.T) {
	modified := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "abc"}, {Key: "vcs.modified", Value: "true"},
	}}
	clean := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "abc"}, {Key: "vcs.modified", Value: "false"},
	}}
	cases := []struct {
		name string
		flag string
		info *debug.BuildInfo
		want string
	}{
		{"flag", "true", clean, "deployed with --allow-dev-build"},
		{"dirty tree without the script", "false", modified, "built from a tree with uncommitted changes"},
		{"clean main build", "false", clean, ""},
		{"no build info", "false", nil, ""},
	}
	for _, c := range cases {
		if got := devBuildReasonFrom(c.flag, c.info); got != c.want {
			t.Errorf("%s: devBuildReasonFrom = %q, want %q", c.name, got, c.want)
		}
	}
}
