// Package taskopen opens StayPoint task pages in the Board's browser
// (task-63a9779d): 'staypoint task open' and the staypoint_task_open MCP tool.
//
// It resolves what the caller typed (a task id, a reference, or a task name)
// to tasks that exist, builds the localhost web UI URL for each, and builds
// the argv that opens them in the configured browser and profile. Nothing is
// passed through a shell: ids never reach a command line, only URLs built
// from the task's own stored id.
package taskopen

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"unicode"

	"github.com/VinnyVanGogh/staypoint/internal/config"
)

// BaseURL is the web UI as the Board's browser reaches it. Always localhost,
// never 127.0.0.1: browsers flag the bare IP as an invalid domain.
const BaseURL = "http://localhost:41421"

// Target is one resolved task and the page that shows it.
type Target struct {
	Input        string `json:"input"`
	ID           string `json:"id"`
	Name         string `json:"name"`
	Organization string `json:"organization,omitempty"`
	Project      string `json:"project,omitempty"`
	URL          string `json:"url"`
}

var (
	taskIDRe = regexp.MustCompile(`^task-[0-9a-f]{8}$`)
	// refRe is a typed reference: STA-12, MAN-255, MANSOL-3.
	refRe = regexp.MustCompile(`^[A-Za-z]{2,8}-[1-9][0-9]{0,8}$`)
)

// ErrNotFound is returned (wrapped) when an input names no task.
var ErrNotFound = errors.New("no such task")

// Resolve looks up every input and returns one Target per distinct task, in
// input order. Any input that names no task, or more than one, fails the
// whole call: opening some tabs and silently skipping others hides a typo.
//
// An input is a task id (task-1234abcd), a reference recorded on the task
// (STA-775, the Paperclip label an imported task carries), or the exact task
// name (case-insensitive).
func Resolve(db *sql.DB, inputs []string) ([]Target, error) {
	if len(inputs) == 0 {
		return nil, errors.New("no task given")
	}
	var out []Target
	seen := map[string]bool{}
	for _, in := range inputs {
		in = strings.TrimSpace(in)
		if in == "" {
			return nil, errors.New("empty task id")
		}
		t, err := resolveOne(db, in)
		if err != nil {
			return nil, err
		}
		if seen[t.ID] {
			continue
		}
		seen[t.ID] = true
		t.URL = TaskURL(t.ID, t.Organization, t.Project)
		out = append(out, t)
	}
	return out, nil
}

func resolveOne(db *sql.DB, in string) (Target, error) {
	const cols = `SELECT id, name, COALESCE(organization, ''), COALESCE(project, '') FROM tasks `
	var (
		where string
		arg   string
		kind  string
	)
	switch {
	case taskIDRe.MatchString(strings.ToLower(in)):
		where, arg, kind = `WHERE id = ?`, strings.ToLower(in), "task id"
	case refRe.MatchString(in):
		where, arg, kind = `WHERE UPPER(COALESCE(source_ref, '')) = ?`, strings.ToUpper(in), "reference"
	default:
		where, arg, kind = `WHERE name = ? COLLATE NOCASE`, in, "task name"
	}
	rows, err := db.Query(cols+where+` ORDER BY created_at DESC LIMIT 6`, arg)
	if err != nil {
		return Target{}, fmt.Errorf("look up %q: %w", in, err)
	}
	defer rows.Close()
	var found []Target
	for rows.Next() {
		t := Target{Input: in}
		if err := rows.Scan(&t.ID, &t.Name, &t.Organization, &t.Project); err != nil {
			return Target{}, fmt.Errorf("look up %q: %w", in, err)
		}
		found = append(found, t)
	}
	if err := rows.Err(); err != nil {
		return Target{}, fmt.Errorf("look up %q: %w", in, err)
	}
	switch len(found) {
	case 0:
		if kind == "reference" {
			return Target{}, fmt.Errorf("%w: no task carries reference %q (use the task id)", ErrNotFound, in)
		}
		return Target{}, fmt.Errorf("%w: no task with %s %q", ErrNotFound, kind, in)
	case 1:
		return found[0], nil
	default:
		ids := make([]string, len(found))
		for i, t := range found {
			ids[i] = t.ID
		}
		return Target{}, fmt.Errorf("%s %q matches more than one task (%s): use the task id", kind, in, strings.Join(ids, ", "))
	}
}

// TaskURL is the web UI page for a task: the /tasks/<org>/<project>/<id>
// form app.js builds (taskToPath). The page looks the task up by id; the
// org and project segments are decoration it canonicalises.
func TaskURL(id, organization, project string) string {
	if project == "" {
		project = "default"
	}
	return BaseURL + "/tasks/" + url.PathEscape(OrgKey(organization)) + "/" +
		url.PathEscape(project) + "/" + url.PathEscape(id)
}

// orgKeys are the organization keys the web UI shows (the Paperclip issue
// prefixes), so a link names the org the way the Board reads it.
var orgKeys = map[string]string{
	"staypoint":        "STA",
	"managed solution": "MAN",
	"research":         "RES",
	"maintenance":      "PER",
	"runelite":         "RUN",
}

var keyRe = regexp.MustCompile(`^[A-Z]{2,5}$`)

// OrgKey returns the URL key for an organization. No organization files
// under StayPoint, as the fleet view does; an unknown one uses the first
// three letters of its name.
func OrgKey(org string) string {
	org = strings.TrimSpace(org)
	if org == "" {
		return "STA"
	}
	if k, ok := orgKeys[strings.ToLower(org)]; ok {
		return k
	}
	if keyRe.MatchString(org) {
		return org
	}
	var b strings.Builder
	for _, r := range org {
		if r < unicode.MaxASCII && unicode.IsLetter(r) {
			b.WriteRune(unicode.ToUpper(r))
			if b.Len() == 3 {
				break
			}
		}
	}
	if b.Len() < 2 {
		return "TSK"
	}
	return b.String()
}

// IsTaskURL reports whether u is a task page on the local web UI. Only
// these are ever handed to a browser.
func IsTaskURL(u string) bool {
	if !strings.HasPrefix(u, BaseURL+"/tasks/") {
		return false
	}
	p, err := url.Parse(u)
	return err == nil && p.Scheme == "http" && p.Host == "localhost:41421" &&
		p.User == nil && p.RawQuery == "" && p.Fragment == ""
}

// chromium maps the browsers that take --profile-directory to the folder
// under ~/Library/Application Support that holds their Local State file.
var chromium = map[string]string{
	"microsoft edge": "Microsoft Edge",
	"google chrome":  filepath.Join("Google", "Chrome"),
}

// Command returns the program and argv that open urls in the configured
// browser:
//   - no app configured: macOS 'open <urls>' (the default browser);
//   - Edge or Chrome with a profile: the app binary with
//     --profile-directory=<dir> <urls>, which hands the URLs to the running
//     instance's window for that profile;
//   - any other app, or no profile: 'open -a <app> <urls>'.
//
// home is the user's home directory, used to read the browser's Local State
// when the profile is given by display name.
func Command(b config.BrowserConfig, urls []string, home string) (string, []string, error) {
	if len(urls) == 0 {
		return "", nil, errors.New("no URLs to open")
	}
	for _, u := range urls {
		if !IsTaskURL(u) {
			return "", nil, fmt.Errorf("refusing to open %q: not a StayPoint task page", u)
		}
	}
	app := strings.TrimSpace(b.App)
	if app == "" {
		return "open", append([]string{}, urls...), nil
	}
	dir := strings.TrimSpace(b.ProfileDirectory)
	support, isChromium := chromium[strings.ToLower(app)]
	if isChromium && dir == "" && strings.TrimSpace(b.Profile) != "" {
		var err error
		dir, err = ProfileDirByName(filepath.Join(home, "Library", "Application Support", support, "Local State"), b.Profile)
		if err != nil {
			return "", nil, err
		}
	}
	if !isChromium || dir == "" {
		return "open", append([]string{"-a", app}, urls...), nil
	}
	bin := filepath.Join("/Applications", app+".app", "Contents", "MacOS", app)
	return bin, append([]string{"--profile-directory=" + dir}, urls...), nil
}

// ProfileDirByName reads a Chromium Local State file and returns the
// profile folder whose display name is name (case-insensitive), so
// profile = "Main" in config resolves to "Profile 4".
func ProfileDirByName(localStatePath, name string) (string, error) {
	data, err := os.ReadFile(localStatePath)
	if err != nil {
		return "", fmt.Errorf("resolve browser profile %q: %w", name, err)
	}
	var ls struct {
		Profile struct {
			InfoCache map[string]struct {
				Name string `json:"name"`
			} `json:"info_cache"`
		} `json:"profile"`
	}
	if err := json.Unmarshal(data, &ls); err != nil {
		return "", fmt.Errorf("resolve browser profile %q: parse %s: %w", name, localStatePath, err)
	}
	var names []string
	for dir, p := range ls.Profile.InfoCache {
		if strings.EqualFold(strings.TrimSpace(p.Name), strings.TrimSpace(name)) {
			return dir, nil
		}
		names = append(names, fmt.Sprintf("%q (%s)", p.Name, dir))
	}
	sort.Strings(names)
	return "", fmt.Errorf("no browser profile named %q; profiles: %s", name, strings.Join(names, ", "))
}

// Launch runs the browser command without waiting for a newly started
// browser to exit. The child gets no stdio (an MCP server's stdout is its
// protocol stream) and its own process group, so it outlives the caller.
func Launch(name string, args []string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open browser: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// Open builds the browser command for targets and runs it with launch.
func Open(b config.BrowserConfig, targets []Target, home string, launch func(string, []string) error) error {
	urls := make([]string, len(targets))
	for i, t := range targets {
		urls[i] = t.URL
	}
	name, args, err := Command(b, urls, home)
	if err != nil {
		return err
	}
	return launch(name, args)
}
