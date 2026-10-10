package opstools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/config"
)

func testGates() config.GatesConfig {
	return config.GatesConfig{
		Hosts: config.HostClasses{Dev: []string{"mansol-dev", "both-host"}, Prod: []string{"mansol-prod", "both-host"}},
		Ops: config.OpsConfig{DevHosts: map[string]config.DevHostConfig{
			"mansol-dev": {
				AppDir:   "/var/www/mansol_apps",
				Services: []string{"mansol-web", "mansol-worker"},
				Apps: map[string]config.DevAppConfig{
					"billing": {Dir: "/var/www/mansol_apps/billing", Python: "/var/www/mansol_apps/venv/bin/python"},
				},
			},
			"both-host":   {AppDir: "/srv/app"},
			"mansol-prod": {AppDir: "/srv/app"},
		}},
	}
}

func TestDecidePerEffect(t *testing.T) {
	cases := map[Effect]Decision{Read: Allow, DevWrite: Allow, ProdWrite: Board, ExternalWrite: Board, "": Board, "weird": Board}
	for e, want := range cases {
		if got := Decide(e); got != want {
			t.Errorf("Decide(%q) = %s, want %s", e, got, want)
		}
	}
}

func TestPlanDevHostRefuses(t *testing.T) {
	g := testGates()
	cases := []struct {
		name string
		req  DevHostRequest
		want string
	}{
		{"host not in dev list", DevHostRequest{Host: "mansol-prod", Action: "git_status"}, "not in [gates.hosts] dev"},
		{"unknown host", DevHostRequest{Host: "random-box", Action: "git_status"}, "not in [gates.hosts] dev"},
		{"host in dev and prod is prod", DevHostRequest{Host: "both-host", Action: "git_status"}, "not in [gates.hosts] dev"},
		{"host option injection", DevHostRequest{Host: "-oProxyCommand=sh", Action: "git_status"}, "not a valid host"},
		{"host with user", DevHostRequest{Host: "root@mansol-dev", Action: "git_status"}, "not a valid host"},
		{"unknown action", DevHostRequest{Host: "mansol-dev", Action: "shell"}, "not one of"},
		{"cat traversal", DevHostRequest{Host: "mansol-dev", Action: "cat_file", Path: "../../etc/passwd"}, "outside the app dir"},
		{"cat absolute outside", DevHostRequest{Host: "mansol-dev", Action: "cat_file", Path: "/etc/shadow"}, "outside the app dir"},
		{"cat sibling prefix", DevHostRequest{Host: "mansol-dev", Action: "cat_file", Path: "/var/www/mansol_apps_old/x"}, "outside the app dir"},
		{"cat dotenv", DevHostRequest{Host: "mansol-dev", Action: "cat_file", Path: ".env"}, "env/secret"},
		{"cat django.env", DevHostRequest{Host: "mansol-dev", Action: "cat_file", Path: "config/django.env"}, "env/secret"},
		{"cat env variant", DevHostRequest{Host: "mansol-dev", Action: "cat_file", Path: ".env.production"}, "env/secret"},
		{"cat secrets dir", DevHostRequest{Host: "mansol-dev", Action: "cat_file", Path: "Secrets/app.json"}, "env/secret"},
		{"cat key", DevHostRequest{Host: "mansol-dev", Action: "cat_file", Path: "certs/server.key"}, "env/secret"},
		{"cat newline", DevHostRequest{Host: "mansol-dev", Action: "cat_file", Path: "a\nb"}, "control characters"},
		{"cat empty", DevHostRequest{Host: "mansol-dev", Action: "cat_file"}, "path is required"},
		{"service not in list", DevHostRequest{Host: "mansol-dev", Action: "restart", Service: "sshd"}, "not one of the host's services"},
		{"service injection", DevHostRequest{Host: "mansol-dev", Action: "journal_tail", Service: "mansol-web;reboot"}, "not one of the host's services"},
		{"branch not allowed", DevHostRequest{Host: "mansol-dev", Action: "git_ff_pull", Branch: "main"}, "not one of"},
		{"branch option", DevHostRequest{Host: "mansol-dev", Action: "git_ff_pull", Branch: "--upload-pack=x"}, "not one of"},
		{"lines too many", DevHostRequest{Host: "mansol-dev", Action: "journal_tail", Service: "mansol-web", Lines: 99999}, "lines"},
		{"port zero", DevHostRequest{Host: "mansol-dev", Action: "http_get_local"}, "port"},
		{"url path injection", DevHostRequest{Host: "mansol-dev", Action: "http_get_local", Port: 8000, Path: "/x' -o /tmp/y '"}, "URL path"},
		{"collectstatic without app", DevHostRequest{Host: "mansol-dev", Action: "collectstatic"}, "needs app"},
		{"unknown app", DevHostRequest{Host: "mansol-dev", Action: "pip_sync", App: "payroll"}, "not configured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := PlanDevHost(g, tc.req)
			if err == nil {
				t.Fatalf("expected refusal, got plan %q", p.Remote)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestPlanDevHostEffects(t *testing.T) {
	g := testGates()
	cases := []struct {
		req    DevHostRequest
		effect Effect
		remote string
	}{
		{DevHostRequest{Host: "mansol-dev", Action: "git_status"}, Read, "git -C '/var/www/mansol_apps' status --short --branch"},
		{DevHostRequest{Host: "mansol-dev", Action: "git_log", Lines: 5}, Read, "log --oneline -n 5"},
		{DevHostRequest{Host: "mansol-dev", Action: "ls"}, Read, "ls -la"},
		{DevHostRequest{Host: "mansol-dev", Action: "cat_file", Path: "billing/views.py"}, Read, "'/var/www/mansol_apps/billing/views.py'"},
		{DevHostRequest{Host: "mansol-dev", Action: "journal_tail", Service: "mansol-web"}, Read, "journalctl -u 'mansol-web' -n 100 --no-pager"},
		{DevHostRequest{Host: "mansol-dev", Action: "systemctl_status", Service: "mansol-worker"}, Read, "systemctl status 'mansol-worker'"},
		{DevHostRequest{Host: "mansol-dev", Action: "http_get_local", Port: 8000, Path: "/health?x=1"}, Read, "'http://127.0.0.1:8000/health?x=1'"},
		{DevHostRequest{Host: "mansol-dev", Action: "git_ff_pull", Branch: "dev-server"}, DevWrite, "pull --ff-only origin 'dev-server'"},
		{DevHostRequest{Host: "mansol-dev", Action: "restart", Service: "mansol-web"}, DevWrite, "systemctl restart 'mansol-web'"},
		{DevHostRequest{Host: "mansol-dev", Action: "collectstatic", App: "billing"}, DevWrite, "cd '/var/www/mansol_apps/billing' && '/var/www/mansol_apps/venv/bin/python' manage.py collectstatic --noinput"},
		{DevHostRequest{Host: "mansol-dev", Action: "pip_sync", App: "billing"}, DevWrite, "-m pip install -r 'requirements.txt'"},
	}
	for _, tc := range cases {
		p, err := PlanDevHost(g, tc.req)
		if err != nil {
			t.Fatalf("%s: %v", tc.req.Action, err)
		}
		if p.Effect != tc.effect {
			t.Errorf("%s: effect %s, want %s", tc.req.Action, p.Effect, tc.effect)
		}
		if !strings.Contains(p.Remote, tc.remote) {
			t.Errorf("%s: remote %q lacks %q", tc.req.Action, p.Remote, tc.remote)
		}
		if d, _ := Declared("mcp__staypoint__dev_host_run", []byte(`{"action":"`+tc.req.Action+`"}`)); d != tc.effect {
			t.Errorf("%s: Declared = %s, want %s", tc.req.Action, d, tc.effect)
		}
	}
	if d, _ := Declared("dev_host_run", []byte(`{"action":"rm_rf"}`)); d != ProdWrite {
		t.Errorf("unknown action declared %s, want prod_write", d)
	}
}

func TestShQuoteSurvivesQuotes(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, s := range []string{"plain", "it's", `"; rm -rf / #`, "$(id)", "`id`", "a'b'c"} {
		out, err := exec.Command(sh, "-c", "printf '%s' "+shQuote(s)).Output()
		if err != nil || string(out) != s {
			t.Errorf("shQuote(%q) round-trip = %q, %v", s, out, err)
		}
	}
}

// TestResolvedUnderOnRealShell runs the remote guard script against a real
// directory tree: a symlink out of the app dir and a symlink to an env file
// are refused even though their names pass the local check.
func TestResolvedUnderOnRealShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	app := filepath.Join(root, "app")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{app, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(app, "views.py"), "print('ok')\n")
	write(filepath.Join(app, "django.env"), "SECRET_KEY=fixture\n")
	write(filepath.Join(outside, "passwd"), "root:x:0:0\n")
	if err := os.Symlink(filepath.Join(outside, "passwd"), filepath.Join(app, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(app, "django.env"), filepath.Join(app, "settings.txt")); err != nil {
		t.Fatal(err)
	}
	g := config.GatesConfig{
		Hosts: config.HostClasses{Dev: []string{"dev"}},
		Ops:   config.OpsConfig{DevHosts: map[string]config.DevHostConfig{"dev": {AppDir: app}}},
	}
	run := func(path string) (string, int) {
		p, err := PlanDevHost(g, DevHostRequest{Host: "dev", Action: "cat_file", Path: path})
		if err != nil {
			t.Fatalf("plan %s: %v", path, err)
		}
		cmd := exec.Command(sh, "-c", p.Remote)
		out, _ := cmd.CombinedOutput()
		return string(out), cmd.ProcessState.ExitCode()
	}
	if out, code := run("views.py"); code != 0 || !strings.Contains(out, "print('ok')") {
		t.Fatalf("views.py: exit %d %q", code, out)
	}
	if out, code := run("notes.txt"); code != 3 || strings.Contains(out, "root:x") {
		t.Fatalf("symlink out of app dir: exit %d %q", code, out)
	}
	if out, code := run("settings.txt"); code != 3 || strings.Contains(out, "SECRET_KEY") {
		t.Fatalf("symlink to env file: exit %d %q", code, out)
	}
	if _, code := run("missing.py"); code != 2 {
		t.Fatalf("missing file: exit %d, want 2", code)
	}
}

func TestRedactFixtureEnvFile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "django.env"))
	if err != nil {
		t.Fatal(err)
	}
	out := Redact(string(raw))
	if strings.Contains(out, "fixture-") {
		t.Fatalf("secret fragment survived redaction:\n%s", out)
	}
	for _, keep := range []string{"DJANGO_SETTINGS_MODULE=mansol.settings.dev", "DEBUG=True", "ALLOWED_HOSTS=dev.example.com", "SECRET_KEY=", "postgres://mansol:"} {
		if !strings.Contains(out, keep) {
			t.Errorf("redaction removed non-secret %q:\n%s", keep, out)
		}
	}
}

func TestRunDevHostUsesBatchSSHAndRedacts(t *testing.T) {
	raw, _ := os.ReadFile(filepath.Join("testdata", "django.env"))
	var got Cmd
	fake := func(ctx context.Context, c Cmd) Result {
		got = c
		return Result{Output: string(raw), ExitCode: 0}
	}
	p, err := PlanDevHost(testGates(), DevHostRequest{Host: "mansol-dev", Action: "journal_tail", Service: "mansol-web"})
	if err != nil {
		t.Fatal(err)
	}
	out := RunDevHost(context.Background(), fake, p)
	want := []string{"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "mansol-dev", p.Remote}
	if got.Name != "ssh" || strings.Join(got.Args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("ssh argv = %s %q", got.Name, got.Args)
	}
	if strings.Contains(out, "fixture-") || !strings.HasPrefix(out, "exit 0") {
		t.Fatalf("output not redacted or missing exit: %s", out)
	}
}

func TestMergeEffectAndPlan(t *testing.T) {
	for base, want := range map[string]Effect{"dev-server": DevWrite, "dev": DevWrite, "main": ProdWrite, "master": ProdWrite, "prod": ProdWrite, "feature/x": ProdWrite, "": ProdWrite} {
		if got := MergeEffect(base); got != want {
			t.Errorf("MergeEffect(%q) = %s, want %s", base, got, want)
		}
	}
	head := strings.Repeat("a", 40)
	req := PRMergeRequest{Repo: "/r", PR: 7, Base: "main", Method: "merge"}
	if _, err := PlanPRMerge(req, []byte(`{"baseRefName":"dev-server","headRefOid":"`+head+`","state":"OPEN"}`)); err == nil || !strings.Contains(err.Error(), "not the declared base") {
		t.Fatalf("declared base lie not refused: %v", err)
	}
	devReq := PRMergeRequest{Repo: "/r", PR: 7, Base: "dev-server", Method: "merge"}
	if _, err := PlanPRMerge(devReq, []byte(`{"baseRefName":"main","headRefOid":"`+head+`","state":"OPEN"}`)); err == nil {
		t.Fatal("main PR declared as dev-server was not refused")
	}
	if _, err := PlanPRMerge(req, []byte(`{"baseRefName":"main","headRefOid":"`+head+`","state":"MERGED"}`)); err == nil {
		t.Fatal("merged PR not refused")
	}
	p, err := PlanPRMerge(req, []byte(`{"baseRefName":"main","headRefOid":"`+head+`","state":"OPEN"}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Effect != ProdWrite {
		t.Fatalf("main merge effect %s", p.Effect)
	}
	if args := strings.Join(p.MergeArgs(), " "); args != "pr merge 7 --merge --match-head-commit "+head {
		t.Fatalf("merge args %q", args)
	}
}

func TestValidatePRMerge(t *testing.T) {
	dir := t.TempDir()
	bad := []PRMergeRequest{
		{Repo: dir, PR: 0, Base: "main"},
		{Repo: dir, PR: 1, Base: "-x"},
		{Repo: dir, PR: 1, Base: "a..b"},
		{Repo: dir, PR: 1, Base: "main", Method: "admin"},
		{Repo: "relative", PR: 1, Base: "main"},
		{Repo: filepath.Join(dir, "nope"), PR: 1, Base: "main"},
	}
	for _, r := range bad {
		if err := ValidatePRMerge(&r, ""); err == nil {
			t.Errorf("accepted %+v", r)
		}
	}
	ok := PRMergeRequest{PR: 3, Base: "dev-server"}
	if err := ValidatePRMerge(&ok, dir); err != nil || ok.Repo != dir || ok.Method != "merge" {
		t.Fatalf("default repo/method: %+v %v", ok, err)
	}
}

func TestVerifyValidation(t *testing.T) {
	dir := t.TempDir()
	bad := []VerifyRequest{
		{SHA: "xyz", PageChecks: []string{"/a=b"}},
		{SHA: "abc1234"},
		{SHA: "abc1234", PageChecks: []string{"a=b"}},
		{SHA: "abc1234", PageChecks: []string{"/a b=c"}},
		{SHA: "abc1234", PageChecks: []string{"/a=b\nc"}},
		{SHA: "-abc1234", PageChecks: []string{"/a=b"}},
	}
	for _, r := range bad {
		if err := ValidateVerify(&r, dir); err == nil {
			t.Errorf("accepted %+v", r)
		}
	}
}

// fakeGit answers the verify flow's git calls and records whether bash ran.
func fakeGit(devBlob, mainBlob string, ranBash *[]string) Runner {
	return func(ctx context.Context, c Cmd) Result {
		args := strings.Join(c.Args, " ")
		switch {
		case c.Name == "git" && strings.Contains(args, " fetch "):
			return Result{}
		case c.Name == "git" && strings.Contains(args, "origin/dev-server:"):
			return Result{Stdout: devBlob + "\n"}
		case c.Name == "git" && strings.Contains(args, "origin/main:"):
			if mainBlob == "" {
				return Result{ExitCode: 1}
			}
			return Result{Stdout: mainBlob + "\n"}
		case c.Name == "git" && strings.Contains(args, "cat-file blob"):
			return Result{Stdout: "#!/bin/bash\necho PASS\n"}
		case c.Name == "bash":
			*ranBash = append(*ranBash, string(c.Stdin))
			return Result{Output: "noise\nPASS /billing marker found\nFAIL /x\nDEV DEPLOY VERIFIED abc1234\n"}
		}
		return Result{ExitCode: 127}
	}
}

func TestRunVerifyTrustsOnlyReviewedScript(t *testing.T) {
	blobA, blobB := strings.Repeat("a", 40), strings.Repeat("b", 40)
	req := VerifyRequest{Repo: t.TempDir(), SHA: "abc1234", PageChecks: []string{"/billing=marker"}}

	var ran []string
	out := RunVerify(context.Background(), fakeGit(blobA, blobB, &ran), req, nil)
	if len(ran) != 0 || !strings.Contains(out, "NOT ON DEV") || !strings.Contains(out, blobA) {
		t.Fatalf("untrusted dev-server script ran or was not reported: ran=%d %s", len(ran), out)
	}
	out = RunVerify(context.Background(), fakeGit(blobA, "", &ran), req, nil)
	if len(ran) != 0 || !strings.Contains(out, "NOT ON DEV") {
		t.Fatalf("script missing on main ran: %s", out)
	}

	out = RunVerify(context.Background(), fakeGit(blobA, blobA, &ran), req, nil)
	if len(ran) != 1 || !strings.Contains(out, "DEV DEPLOY VERIFIED abc1234") || strings.Contains(out, "noise") {
		t.Fatalf("main-matching script: ran=%d %s", len(ran), out)
	}
	out = RunVerify(context.Background(), fakeGit(blobA, blobB, &ran), req, []string{blobA})
	if len(ran) != 2 || !strings.Contains(out, "PASS /billing") || !strings.Contains(out, "FAIL /x") {
		t.Fatalf("Board-trusted blob: ran=%d %s", len(ran), out)
	}
}

func TestSummarizeVerifyAddsMissingVerdict(t *testing.T) {
	out := summarizeVerify(Result{Output: "PASS a\nboom\n", ExitCode: 2})
	if !strings.Contains(out, "NOT ON DEV: verify script ended without a verdict (exit 2)") {
		t.Fatalf("no verdict line added: %s", out)
	}
}

func TestBashRedirect(t *testing.T) {
	hosts := config.HostClasses{Dev: []string{"mansol-dev"}, Prod: []string{"mansol-prod"}}
	redirect := map[string]string{
		"ssh mansol-dev 'cd /var/www && git status'":               "dev_host_run",
		"ssh -o ConnectTimeout=10 mansol-dev 'git pull --ff-only'": "dev_host_run",
		"ssh -p 22 deploy@mansol-dev systemctl restart web":        "dev_host_run",
		"ssh ssh://mansol-dev:22 uptime":                           "dev_host_run",
		"/usr/bin/ssh -T mansol-dev":                               "dev_host_run",
		"git fetch origin dev-server && git show origin/dev-server:scripts/verify_dev_deploy.sh | bash -s -- abc /x=y": "dev_deploy_verify",
		"cat ~/.staypoint/handoffs/task-1/plan.md":                    "staypoint_query",
		"sqlite3 $HOME/.staypoint/staypoint.db 'select * from tasks'": "staypoint_query",
		"ls -la /Users/someone/.staypoint/handoffs":                   "staypoint_query",
		"cat > /tmp/comment.md <<'EOF'\nhello\nEOF":                   "task_comment",
		"cat <<EOF > /tmp/pr-body.md\nbody\nEOF":                      "pr_body",
		"gh pr merge 12 --merge":                                      "pr_merge",
		"gh -R o/r pr merge 12":                                       "pr_merge",
	}
	for cmd, tool := range redirect {
		if got := BashRedirect(cmd, hosts); !strings.Contains(got, tool) {
			t.Errorf("BashRedirect(%q) = %q, want pointer to %s", cmd, got, tool)
		}
	}
	pass := []string{
		"ssh mansol-prod uptime",
		"ssh unknown-box uptime",
		"ssh-keygen -t ed25519",
		"cat README.md",
		"git log --oneline -5",
		"cat > notes.md <<EOF\nx\nEOF",
		"echo ~/.staypoint",
		"rm -rf ~/.staypoint/handoffs/x",
		"gh pr view 12",
		"gh pr create --title merge",
	}
	for _, cmd := range pass {
		if got := BashRedirect(cmd, hosts); got != "" {
			t.Errorf("BashRedirect(%q) = %q, want none", cmd, got)
		}
	}
}
