package taskopen

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/VinnyVanGogh/staypoint/internal/config"
	meshContext "github.com/VinnyVanGogh/staypoint/internal/context"
	"github.com/VinnyVanGogh/staypoint/internal/db"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store.DB()
}

func mkTask(t *testing.T, conn *sql.DB, opts meshContext.TaskCreateOptions) *meshContext.Task {
	t.Helper()
	opts.RepoPath, opts.GitBranch, opts.AccountRole = "/tmp/repo", "main", "personal"
	task, err := meshContext.CreateTaskWithOptions(conn, opts)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestResolve_UnknownIDFailsWholeCall(t *testing.T) {
	conn := testDB(t)
	known := mkTask(t, conn, meshContext.TaskCreateOptions{Name: "Known"})
	_, err := Resolve(conn, []string{known.ID, "task-00000000"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// A shell-looking input is only ever a parameter to a name lookup: it finds
// nothing, and never reaches a URL or argv.
func TestResolve_ShellMetacharactersAreNotInterpolated(t *testing.T) {
	conn := testDB(t)
	mkTask(t, conn, meshContext.TaskCreateOptions{Name: "Real"})
	for _, in := range []string{
		"task-1234abcd; rm -rf ~", "$(touch /tmp/pwned)", "`id`", "' OR 1=1 --", "task-%", "task-_______",
	} {
		if _, err := Resolve(conn, []string{in}); !errors.Is(err, ErrNotFound) {
			t.Errorf("Resolve(%q) err = %v, want ErrNotFound", in, err)
		}
	}
}

// GetTask matches id prefixes; opening must not: "task-" alone is no task.
func TestResolve_NoPrefixMatching(t *testing.T) {
	conn := testDB(t)
	task := mkTask(t, conn, meshContext.TaskCreateOptions{Name: "Real"})
	for _, in := range []string{"task-", task.ID[:9], strings.TrimPrefix(task.ID, "task-")} {
		if _, err := Resolve(conn, []string{in}); err == nil {
			t.Errorf("Resolve(%q) succeeded, want an error", in)
		}
	}
}

func TestResolve_AmbiguousNameRefused(t *testing.T) {
	conn := testDB(t)
	mkTask(t, conn, meshContext.TaskCreateOptions{Name: "Same"})
	mkTask(t, conn, meshContext.TaskCreateOptions{Name: "same"})
	_, err := Resolve(conn, []string{"SAME"})
	if err == nil || !strings.Contains(err.Error(), "more than one task") {
		t.Fatalf("err = %v, want ambiguity error", err)
	}
}

func TestResolve_EmptyInputs(t *testing.T) {
	conn := testDB(t)
	if _, err := Resolve(conn, nil); err == nil {
		t.Error("nil inputs accepted")
	}
	if _, err := Resolve(conn, []string{"  "}); err == nil {
		t.Error("blank input accepted")
	}
}

func TestResolve_URLPerOrgAndInputKinds(t *testing.T) {
	conn := testDB(t)
	sta := mkTask(t, conn, meshContext.TaskCreateOptions{Name: "Plain", Organization: "StayPoint", Project: "StayPoint/Integrations"})
	man := mkTask(t, conn, meshContext.TaskCreateOptions{Name: "Work item", Organization: "Managed Solution", SourceRef: "MAN-255"})
	none := mkTask(t, conn, meshContext.TaskCreateOptions{Name: "No org"})
	other := mkTask(t, conn, meshContext.TaskCreateOptions{Name: "Acme thing", Organization: "acme corp", Project: "core"})

	got, err := Resolve(conn, []string{
		strings.ToUpper(sta.ID[:5]) + sta.ID[5:], // case-insensitive id
		"man-255",                                // reference
		"no ORG",                                 // name
		other.ID,
		sta.ID, // duplicate: opened once
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"http://localhost:41421/tasks/STA/StayPoint%2FIntegrations/" + sta.ID,
		"http://localhost:41421/tasks/MAN/default/" + man.ID,
		"http://localhost:41421/tasks/STA/default/" + none.ID,
		"http://localhost:41421/tasks/ACM/core/" + other.ID,
	}
	var urls []string
	for _, g := range got {
		urls = append(urls, g.URL)
		if !IsTaskURL(g.URL) {
			t.Errorf("%s is not a task URL", g.URL)
		}
		if strings.Contains(g.URL, "127.0.0.1") {
			t.Errorf("%s uses 127.0.0.1", g.URL)
		}
	}
	if !reflect.DeepEqual(urls, want) {
		t.Fatalf("urls = %v\nwant %v", urls, want)
	}
}

func TestIsTaskURL(t *testing.T) {
	for u, want := range map[string]bool{
		"http://localhost:41421/tasks/STA/default/task-1234abcd":      true,
		"http://127.0.0.1:41421/tasks/STA/default/task-1234abcd":      false,
		"http://localhost:41421/settings":                             false,
		"http://localhost:41421/tasks/STA/default/task-1?token=x":     false,
		"http://localhost:41421.evil.com/tasks/STA/default/x":         false,
		"https://example.com/?u=http://localhost:41421/tasks/STA/x/y": false,
		"--flag": false,
	} {
		if got := IsTaskURL(u); got != want {
			t.Errorf("IsTaskURL(%q) = %v, want %v", u, got, want)
		}
	}
}

const testURL = "http://localhost:41421/tasks/STA/default/task-1234abcd"

func TestCommand_EdgeProfileDirectory(t *testing.T) {
	name, args, err := Command(config.BrowserConfig{App: "Microsoft Edge", ProfileDirectory: "Profile 4"}, []string{testURL, testURL}, "/nohome")
	if err != nil {
		t.Fatal(err)
	}
	if name != "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge" {
		t.Errorf("name = %q", name)
	}
	if want := []string{"--profile-directory=Profile 4", testURL, testURL}; !reflect.DeepEqual(args, want) {
		t.Errorf("args = %q, want %q", args, want)
	}
}

func TestCommand_ProfileByNameFromLocalState(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "Library", "Application Support", "Microsoft Edge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ls := `{"profile":{"info_cache":{"Default":{"name":"Work"},"Profile 4":{"name":"Main"}}}}`
	if err := os.WriteFile(filepath.Join(dir, "Local State"), []byte(ls), 0o600); err != nil {
		t.Fatal(err)
	}
	_, args, err := Command(config.BrowserConfig{App: "microsoft edge", Profile: "main"}, []string{testURL}, home)
	if err != nil {
		t.Fatal(err)
	}
	if args[0] != "--profile-directory=Profile 4" {
		t.Errorf("args = %q", args)
	}
	_, _, err = Command(config.BrowserConfig{App: "Microsoft Edge", Profile: "Nope"}, []string{testURL}, home)
	if err == nil || !strings.Contains(err.Error(), `"Main" (Profile 4)`) {
		t.Errorf("unknown profile err = %v", err)
	}
}

func TestCommand_ChromeAndFallbacks(t *testing.T) {
	cases := []struct {
		b        config.BrowserConfig
		wantName string
		wantArgs []string
	}{
		{config.BrowserConfig{}, "open", []string{testURL}},
		{config.BrowserConfig{App: "Google Chrome", ProfileDirectory: "Default"},
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", []string{"--profile-directory=Default", testURL}},
		{config.BrowserConfig{App: "Microsoft Edge"}, "open", []string{"-a", "Microsoft Edge", testURL}},
		{config.BrowserConfig{App: "Safari", ProfileDirectory: "Profile 4"}, "open", []string{"-a", "Safari", testURL}},
	}
	for _, c := range cases {
		name, args, err := Command(c.b, []string{testURL}, "/nohome")
		if err != nil {
			t.Fatal(err)
		}
		if name != c.wantName || !reflect.DeepEqual(args, c.wantArgs) {
			t.Errorf("%+v: got %q %q, want %q %q", c.b, name, args, c.wantName, c.wantArgs)
		}
	}
}

func TestCommand_RefusesNonTaskURLs(t *testing.T) {
	for _, u := range []string{"https://example.com", "--new-window", "file:///etc/passwd"} {
		if _, _, err := Command(config.BrowserConfig{}, []string{testURL, u}, "/nohome"); err == nil {
			t.Errorf("Command accepted %q", u)
		}
	}
	if _, _, err := Command(config.BrowserConfig{}, nil, "/nohome"); err == nil {
		t.Error("Command accepted no URLs")
	}
}

func TestOpen_PassesArgvToLauncher(t *testing.T) {
	var gotName string
	var gotArgs []string
	err := Open(config.BrowserConfig{App: "Microsoft Edge", ProfileDirectory: "Profile 4"},
		[]Target{{ID: "task-1234abcd", URL: testURL}}, "/nohome",
		func(n string, a []string) error { gotName, gotArgs = n, a; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(gotName, "/Microsoft Edge") || len(gotArgs) != 2 || gotArgs[1] != testURL {
		t.Fatalf("launch(%q, %q)", gotName, gotArgs)
	}
}
