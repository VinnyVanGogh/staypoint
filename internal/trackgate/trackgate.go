// Package trackgate implements the StayPoint tracking gate (STA-854): in
// gated work repos, write tools are blocked unless the agent session is
// attached to a StayPoint task.
//
// A session counts as attached when STAYPOINT_TASK_ID is set (daemon runs)
// or its session id (Claude session_id, agy conversationId) has a row in
// task_session_attachments for an active task (`staypoint task attach`).
//
// The gate is per company: on by default for work orgs (Managed Solution and
// config work_orgs), off by default for everyone else, toggled through
// settings_kv key "gates.tracking.<company>" (written only by the Board-only
// /api/settings/tracking-gate endpoint). Work-repo writes are gated under the
// Managed Solution toggle and override, and accept a task from any work org.
//
// A Board override is an approved security gate request whose run_id is
// OverrideRunID. Approval goes through WrapBoardAction (Board session cookie +
// passkey assertion), so an agent can create the request but cannot approve it.
package trackgate

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/geminiapproval"
	"github.com/VinnyVanGogh/staypoint/internal/geminiguard"
	"github.com/VinnyVanGogh/staypoint/internal/security"
	"github.com/VinnyVanGogh/staypoint/internal/workorgs"
)

const (
	CompanyManagedSolution = "Managed Solution"
	CompanyPersonal        = "Personal"

	// OverrideRunID marks a security gate request as a tracking-gate override.
	OverrideRunID = "tracking-gate-override"
	// MaxOverrideMinutes caps one override window.
	MaxOverrideMinutes = 240

	settingPrefix = "gates.tracking."
)

// Client identifies the agent CLI whose hook payload is being evaluated.
type Client string

const (
	ClientClaude Client = "claude"
	ClientGemini Client = "gemini" // agy / Antigravity CLI
)

// Request is one tool call as seen by the PreToolUse hook.
type Request struct {
	Client    Client
	ToolName  string
	Command   string   // shell command for Bash / run_command
	FilePaths []string // target files for file-write tools
	CWD       string
	SessionID string
	TaskID    string // STAYPOINT_TASK_ID from the hook's environment
}

// Decision is the gate's verdict for a Request.
type Decision struct {
	Block   bool
	Reason  string
	Company string
	Repo    string
}

// Gate evaluates Requests. Zero-value fields fall back to production defaults
// except OpenDB and IsWorkRepo, which callers must set.
type Gate struct {
	// OpenDB opens the StayPoint database. An error makes the gate fail
	// closed for companies whose default is on.
	OpenDB func() (*sql.DB, func(), error)
	// IsWorkRepo reports whether path is inside a Managed Solution repo.
	IsWorkRepo func(path string) bool
	Now        func() time.Time
}

var claudeWriteTools = map[string]bool{"edit": true, "write": true, "multiedit": true, "notebookedit": true}

// geminiWriteTools are agy tool names (lowercased step types without the
// CORTEX_STEP_TYPE_ prefix, plus the model-facing names) that write files.
var geminiWriteTools = map[string]bool{
	"code_action": true, "file_change": true, "write_blob": true, "edit_notebook": true,
	"propose_code": true, "move": true, "delete_directory": true, "git_commit": true,
	"write_to_file": true, "replace_file_content": true, "multi_replace_file_content": true,
}

var geminiShellTools = map[string]bool{"run_command": true, "shell_exec": true, "send_command_input": true}

// action is one write the request would perform and where it lands.
type action struct {
	what  string
	paths []string
}

// actions classifies a request into the writes it would perform.
func actions(req Request) []action {
	name := strings.ToLower(strings.TrimSpace(req.ToolName))
	isShell := name == "bash" && req.Client != ClientGemini
	isWrite := claudeWriteTools[name] && req.Client != ClientGemini
	if req.Client == ClientGemini {
		isShell = geminiShellTools[name]
		isWrite = geminiWriteTools[name]
	}
	switch {
	case isShell:
		if strings.TrimSpace(req.Command) == "" {
			return nil
		}
		var out []action
		for _, c := range security.StateChanges(req.Command, req.CWD) {
			out = append(out, action{what: c.Action, paths: c.Paths})
		}
		return out
	case isWrite:
		paths := make([]string, 0, len(req.FilePaths))
		for _, p := range req.FilePaths {
			if p == "" {
				continue
			}
			if !filepath.IsAbs(p) && req.CWD != "" {
				p = filepath.Join(req.CWD, p)
			}
			paths = append(paths, filepath.Clean(p))
		}
		if len(paths) == 0 {
			paths = []string{req.CWD}
		}
		return []action{{what: req.ToolName, paths: paths}}
	}
	return nil
}

// Evaluate returns the gate decision for req.
func (g *Gate) Evaluate(req Request) Decision {
	acts := actions(req)
	if len(acts) == 0 {
		return Decision{}
	}
	if d, denied := g.geminiWorkRepoDeny(req, acts); denied {
		return d
	}
	if d, denied := g.geminiPersonalCodeDeny(req, acts); denied {
		return d
	}
	if strings.TrimSpace(req.TaskID) != "" {
		return Decision{} // daemon run: the task is the run's own
	}

	// Find the first write that lands in a work repo; otherwise the first
	// write at all (it may still be gated if its company opted in).
	var hit action
	hitPath := ""
	work := false
	for _, a := range acts {
		for _, p := range a.paths {
			if p != "" && g.IsWorkRepo != nil && g.IsWorkRepo(p) {
				hit, hitPath, work = a, p, true
				break
			}
		}
		if work {
			break
		}
	}
	if !work {
		hit = acts[0]
		if len(hit.paths) > 0 {
			hitPath = hit.paths[0]
		}
	}

	conn, closeDB, err := g.openDB()
	if err != nil {
		if work {
			return g.block(req, hit, hitPath, CompanyManagedSolution,
				fmt.Sprintf("the StayPoint database is unreadable (%v), so attachment cannot be checked; failing closed", err))
		}
		return Decision{}
	}
	defer closeDB()

	company := CompanyManagedSolution
	if !work {
		company = companyForPath(conn, hitPath)
	}
	enabled, err := Enabled(conn, company)
	if err != nil {
		if DefaultEnabled(company) {
			return g.block(req, hit, hitPath, company,
				fmt.Sprintf("the tracking-gate setting could not be read (%v); failing closed", err))
		}
		return Decision{}
	}
	if !enabled {
		return Decision{}
	}

	if sid := strings.TrimSpace(req.SessionID); sid != "" {
		taskID, err := SessionTask(conn, sid)
		if err != nil {
			return g.block(req, hit, hitPath, company,
				fmt.Sprintf("session attachment could not be read (%v); failing closed", err))
		}
		if taskID != "" {
			// The attached task must belong to the company being gated, so
			// work is tracked under that company and not under whatever task
			// happened to be handy. A work repo accepts a task from any work
			// org (work_orgs: Managed Solution, Power Platform, ...).
			org := taskOrganization(conn, taskID)
			if work && !workorgs.IsWork(org) {
				return g.block(req, hit, hitPath, company,
					fmt.Sprintf("this session is attached to task %s, which belongs to %q, not a work org (%s); attach a work-org task",
						taskID, org, strings.Join(workorgs.List(), ", ")))
			}
			if !work && !sameCompany(org, company) {
				return g.block(req, hit, hitPath, company,
					fmt.Sprintf("this session is attached to task %s, which belongs to %q, not %q; attach a %s task", taskID, org, company, company))
			}
			return Decision{Company: company, Repo: hitPath}
		}
	}

	ov, err := ActiveOverride(conn, company, g.now())
	if err != nil {
		return g.block(req, hit, hitPath, company,
			fmt.Sprintf("Board overrides could not be read (%v); failing closed", err))
	}
	if ov != nil {
		return Decision{Company: company, Repo: hitPath}
	}
	return g.block(req, hit, hitPath, company, "this session is not attached to a StayPoint task")
}

// isWorkRepoDocPath reports whether Gemini may write p in a work repo. It
// uses the same rule as the post-turn guard (geminiguard.IsDocPath) on the
// repo-relative path, so docs/** non-code files pass and code-named .txt files
// such as requirements.txt do not. Outside a git checkout only the file name
// is judged. The path is lowercased first: on a case-insensitive filesystem
// .GITHUB/ is .github/. Unlike the post-turn guard, .staypoint/ is held to the
// file-type rule here.
func isWorkRepoDocPath(p string) bool {
	rel := filepath.Base(p)
	if root, ok := gitRoot(p); ok {
		r, err := filepath.Rel(root, p)
		if err != nil {
			return false
		}
		rel = r
	}
	rel = strings.ToLower(filepath.ToSlash(rel))
	if !geminiguard.IsDocPath(rel) {
		return false
	}
	if strings.HasPrefix(path.Clean(rel), ".staypoint/") {
		return geminiguard.IsDocPath(path.Base(rel))
	}
	return true
}

// geminiWorkRepoDeny enforces the Board decision of 2026-10-06: Gemini (agy)
// never writes code in a Managed Solution work repo. It applies with or
// without a task, attachment, toggle or override; only doc files pass through
// to the normal tracking rules. Shell state changes (git commit, push, ...)
// land on the repo directory, which is not a doc file, so they are denied.
func (g *Gate) geminiWorkRepoDeny(req Request, acts []action) (Decision, bool) {
	if req.Client != ClientGemini || g.IsWorkRepo == nil {
		return Decision{}, false
	}
	for _, a := range acts {
		for _, p := range a.paths {
			if p == "" || !g.IsWorkRepo(p) || isWorkRepoDocPath(p) {
				continue
			}
			msg := fmt.Sprintf("Gemini may not write code in work repos: %s\n"+
				"STAYPOINT: %s denied (%s). Board decision 2026-10-06: Gemini/agy never writes code in "+
				"Managed Solution work repos, with or without a StayPoint task. Reading is allowed, and so is "+
				"writing documentation (*.md, *.txt and other doc files, non-code files under docs/).\n"+
				"Hand code changes to Claude on the work seat instead:  claude --work", p, a.what, CompanyManagedSolution)
			return Decision{Block: true, Reason: msg, Company: CompanyManagedSolution, Repo: p}, true
		}
	}
	return Decision{}, false
}

// geminiPersonalCodeDeny enforces the Board rule in personal repos for an
// interactive agy session: Gemini never writes code unless the Board approved
// it with Touch ID for this conversationId (geminiapproval, up to
// MaxSessionHours). Non-code files (geminiguard.IsDocPath) pass. Daemon runs
// (STAYPOINT_TASK_ID set) are checked after each turn by the harness guard
// instead. Writes outside any git repo are not repo code and pass.
func (g *Gate) geminiPersonalCodeDeny(req Request, acts []action) (Decision, bool) {
	if req.Client != ClientGemini || strings.TrimSpace(req.TaskID) != "" {
		return Decision{}, false
	}
	for _, a := range acts {
		for _, p := range a.paths {
			if p == "" || (g.IsWorkRepo != nil && g.IsWorkRepo(p)) {
				continue
			}
			root, ok := gitRoot(p)
			if !ok {
				continue
			}
			rel, err := filepath.Rel(root, p)
			if err == nil && rel != "." && geminiguard.IsDocPath(filepath.ToSlash(rel)) {
				continue
			}
			approved, why := g.geminiSessionApproved(req.SessionID, root)
			if approved {
				return Decision{}, false
			}
			return Decision{Block: true, Reason: geminiCodeDenyMessage(a.what, p, req.SessionID, why), Repo: p}, true
		}
	}
	return Decision{}, false
}

func (g *Gate) geminiSessionApproved(sessionID, root string) (bool, string) {
	if strings.TrimSpace(sessionID) == "" {
		return false, "this agy call has no conversationId, so it cannot be approved"
	}
	conn, closeDB, err := g.openDB()
	if err != nil {
		return false, fmt.Sprintf("the StayPoint database is unreadable (%v); failing closed", err)
	}
	defer closeDB()
	isWork := g.IsWorkRepo
	if isWork == nil {
		isWork = func(string) bool { return false }
	}
	ok, err := geminiapproval.SessionApproved(conn, sessionID, root, isWork, g.now())
	if err != nil {
		return false, fmt.Sprintf("Board approvals could not be read (%v); failing closed", err)
	}
	return ok, ""
}

func geminiCodeDenyMessage(what, path, sessionID, why string) string {
	msg := fmt.Sprintf("STAYPOINT: %s denied (%s).\n"+
		"Board rule 2026-10-06: Gemini never writes code. In a personal repo the Board may allow it for this agy session with Touch ID.\n", what, path)
	if sessionID != "" {
		msg += "Request approval for this session:  " + geminiapproval.RequestCommand(sessionID) + "\n" +
			"Once the Board approves, code writes in this repo are allowed for this conversation (up to " + fmt.Sprint(geminiapproval.MaxSessionHours) + "h)."
	}
	if why != "" {
		msg += "\n(" + why + ")"
	}
	return msg
}

// gitRoot returns the nearest directory at or above p (or p's directory)
// holding a .git entry.
func gitRoot(p string) (string, bool) {
	dir := filepath.Clean(p)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		dir = filepath.Dir(dir)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func (g *Gate) openDB() (*sql.DB, func(), error) {
	if g.OpenDB == nil {
		return nil, nil, errors.New("no database configured")
	}
	conn, closeFn, err := g.OpenDB()
	if err != nil {
		return nil, nil, err
	}
	if closeFn == nil {
		closeFn = func() {}
	}
	return conn, closeFn, nil
}

func (g *Gate) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *Gate) block(req Request, a action, path, company, why string) Decision {
	return Decision{
		Block:   true,
		Company: company,
		Repo:    path,
		Reason:  BlockMessage(req, a.what, path, company, why),
	}
}

// BlockMessage is the text the agent sees. It names the exact commands that
// unblock the session.
func BlockMessage(req Request, what, path, company, why string) string {
	sid := strings.TrimSpace(req.SessionID)
	if sid == "" {
		sid = "<session-id>"
	}
	client := string(req.Client)
	if client == "" {
		client = string(ClientClaude)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "STAYPOINT TRACKING GATE: %s blocked in %s (%s).\n", what, path, company)
	fmt.Fprintf(&b, "Reason: %s.\n", why)
	fmt.Fprintf(&b, "%s work must be done under a StayPoint task. Do ONE of these, then retry:\n", company)
	fmt.Fprintf(&b, "  1. Create a task and attach this session to it:\n")
	fmt.Fprintf(&b, "       staypoint task create --org '%s' --session %s --client %s \"<one-line title>\"\n", company, sid, client)
	fmt.Fprintf(&b, "  2. Attach this session to an existing task (ids: staypoint task tree):\n")
	fmt.Fprintf(&b, "       staypoint task attach <task-id> --session %s --client %s\n", sid, client)
	fmt.Fprintf(&b, "Read-only tools are not affected. Do not try to bypass this gate; a Board-only, passkey-approved\n")
	fmt.Fprintf(&b, "override exists for the human (staypoint gate override --minutes N), not for agents.")
	return b.String()
}

// DefaultEnabled is the gate's state for a company with no explicit setting:
// on for every work org (Managed Solution and work_orgs), off otherwise.
func DefaultEnabled(company string) bool {
	return workorgs.IsWork(company)
}

// Companies lists the companies the gate always reports: every work org,
// then Personal.
func Companies() []string {
	return append(workorgs.List(), CompanyPersonal)
}

// SettingKey is the settings_kv key holding a company's toggle.
func SettingKey(company string) string {
	return settingPrefix + strings.ToLower(strings.TrimSpace(company))
}

// Enabled reports whether the gate is on for company.
func Enabled(conn *sql.DB, company string) (bool, error) {
	var v string
	err := conn.QueryRow(`SELECT value FROM settings_kv WHERE key = ?`, SettingKey(company)).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultEnabled(company), nil
	}
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "on":
		return true, nil
	case "false", "0", "off":
		return false, nil
	}
	return DefaultEnabled(company), nil
}

// SetEnabled writes a company's toggle. Only the Board-only settings
// endpoint calls this.
func SetEnabled(conn *sql.DB, company string, enabled bool) error {
	_, err := conn.Exec(
		`INSERT INTO settings_kv (key, value, updated_at) VALUES (?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		SettingKey(company), strconv.FormatBool(enabled))
	return err
}

// ListSettings returns every explicit company toggle.
func ListSettings(conn *sql.DB) (map[string]bool, error) {
	rows, err := conn.Query(`SELECT key, value FROM settings_kv WHERE key LIKE ?`, settingPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[strings.TrimPrefix(k, settingPrefix)] = strings.EqualFold(v, "true")
	}
	return out, rows.Err()
}

// companyForPath resolves a non-work path to the organization of the most
// recently updated task in that repo, or CompanyPersonal.
func companyForPath(conn *sql.DB, path string) string {
	if path == "" {
		return CompanyPersonal
	}
	var org sql.NullString
	err := conn.QueryRow(
		`SELECT organization FROM tasks
		 WHERE organization IS NOT NULL AND organization != '' AND repo_path != ''
		   AND (? = repo_path OR ? LIKE repo_path || '/%')
		 ORDER BY updated_at DESC LIMIT 1`, path, path).Scan(&org)
	if err != nil || !org.Valid || strings.TrimSpace(org.String) == "" {
		return CompanyPersonal
	}
	return org.String
}

// SessionTask returns the active task a session is attached to, or "".
func SessionTask(conn *sql.DB, sessionID string) (string, error) {
	var taskID string
	err := conn.QueryRow(
		`SELECT a.task_id FROM task_session_attachments a
		 JOIN tasks t ON t.id = a.task_id
		 WHERE a.session_id = ? AND t.status = 'active'`, sessionID).Scan(&taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return taskID, err
}

// Attach records session -> task and logs it on the task timeline.
func Attach(conn *sql.DB, sessionID, taskID string, client Client, repoPath string) error {
	sessionID = strings.TrimSpace(sessionID)
	taskID = strings.TrimSpace(taskID)
	if sessionID == "" || taskID == "" {
		return errors.New("session id and task id are required")
	}
	if client == "" {
		client = ClientClaude
	}
	var status string
	if err := conn.QueryRow(`SELECT status FROM tasks WHERE id = ?`, taskID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("task %s not found in the StayPoint database", taskID)
		}
		return err
	}
	if status != "active" {
		return fmt.Errorf("task %s is %s; attach to an active task", taskID, status)
	}
	if _, err := conn.Exec(
		`INSERT INTO task_session_attachments (session_id, task_id, client, repo_path, attached_at)
		 VALUES (?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		 ON CONFLICT(session_id) DO UPDATE SET task_id = excluded.task_id, client = excluded.client,
		   repo_path = excluded.repo_path, attached_at = excluded.attached_at`,
		sessionID, taskID, string(client), repoPath); err != nil {
		return err
	}
	details := fmt.Sprintf("Interactive session attached (%s session %s", client, sessionID)
	if repoPath != "" {
		details += ", repo " + repoPath
	}
	details += ")"
	if _, err := conn.Exec(`INSERT INTO activity_log (task_id, event_type, details) VALUES (?, 'interactive_session_attached', ?)`, taskID, details); err != nil {
		return err
	}
	// STA-861: an attached task is interactive. A todo task nothing has
	// claimed yet is parked in backlog so the daemon does not start a run that
	// duplicates the session's work; the Board's Run Now still runs it.
	// Attaching never wakes the daemon.
	res, err := conn.Exec(
		`UPDATE tasks SET execution_stage = 'backlog', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		  WHERE id = ? AND execution_stage = 'todo' AND checkout_run_id IS NULL`, taskID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		_, err = conn.Exec(`INSERT INTO activity_log (task_id, event_type, details) VALUES (?, 'stage_change', ?)`,
			taskID, "execution stage set to backlog (interactive session attached; the daemon will not run it)")
	}
	return err
}

// Override is an active Board override window.
type Override struct {
	GateRequestID string
	Company       string
	Minutes       int
	ExpiresAt     time.Time
}

// OverrideCmdline is the exact text the Board sees and approves.
func OverrideCmdline(company string, minutes int) string {
	return fmt.Sprintf("staypoint gate override --minutes %d --company %q", minutes, company)
}

var overrideRe = regexp.MustCompile(`^staypoint gate override --minutes (\d+) --company "((?:[^"\\]|\\.)*)"$`)

// ParseOverrideCmdline extracts company and minutes from an override request.
func ParseOverrideCmdline(cmdline string) (company string, minutes int, ok bool) {
	m := overrideRe.FindStringSubmatch(strings.TrimSpace(cmdline))
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 || n > MaxOverrideMinutes {
		return "", 0, false
	}
	c, err := strconv.Unquote(`"` + m[2] + `"`)
	if err != nil {
		return "", 0, false
	}
	return c, n, true
}

// ActiveOverride returns the Board-approved override covering company at now.
// The window starts at approval (decided_at), not at request time.
func ActiveOverride(conn *sql.DB, company string, now time.Time) (*Override, error) {
	since := now.Add(-time.Duration(MaxOverrideMinutes) * time.Minute).UTC().Format(time.RFC3339Nano)
	rows, err := conn.Query(
		`SELECT id, cmdline, decided_at FROM security_gate_requests
		 WHERE run_id = ? AND status = 'approved' AND decided_at IS NOT NULL AND decided_at >= ?
		 ORDER BY decided_at DESC`, OverrideRunID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, cmdline, decided string
		if err := rows.Scan(&id, &cmdline, &decided); err != nil {
			return nil, err
		}
		c, mins, ok := ParseOverrideCmdline(cmdline)
		if !ok || !strings.EqualFold(c, company) {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, decided)
		if err != nil {
			continue
		}
		exp := at.Add(time.Duration(mins) * time.Minute)
		if now.Before(exp) {
			return &Override{GateRequestID: id, Company: c, Minutes: mins, ExpiresAt: exp}, nil
		}
	}
	return nil, rows.Err()
}

// taskOrganization returns a task's organization, or "" when unset or unreadable.
func taskOrganization(conn *sql.DB, taskID string) string {
	var org sql.NullString
	if err := conn.QueryRow(`SELECT organization FROM tasks WHERE id = ?`, taskID).Scan(&org); err != nil {
		return ""
	}
	return strings.TrimSpace(org.String)
}

// companyAliases maps Paperclip issue prefixes still found on older tasks to
// their company names.
var companyAliases = map[string]string{"man": "managed solution", "sta": "staypoint"}

// sameCompany compares a task organization with a gated company name,
// case-insensitively and allowing the old prefixes. An empty organization
// never matches.
func sameCompany(org, company string) bool {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		if a, ok := companyAliases[s]; ok {
			return a
		}
		return s
	}
	o := norm(org)
	return o != "" && o == norm(company)
}
