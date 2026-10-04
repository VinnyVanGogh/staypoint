// Package shipreview manages Ship Review cards: the Board-facing review gate
// that replaces silent agent self-merges. When a task's branch is ready an
// agent creates a card; the Board approves (merging exactly the pinned SHA),
// sends it back (agent iterates), or rejects (branch deleted).
package shipreview

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Status values for a Card.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusSentBack = "sent_back"
	StatusRejected = "rejected"
)

// CheckRun records the outcome of a single verification command.
type CheckRun struct {
	Command    string `json:"command"`
	ExitCode   int    `json:"exit_code"`
	OutputTail string `json:"output_tail,omitempty"`
}

// DevState values for a Card.
const (
	DevStateIdle     = ""           // no dev server running or starting
	DevStateStarting = "starting"   // async setup in progress
	DevStateReady    = "ready"      // dev server up
	DevStateError    = "error"      // setup failed
)

// Card represents a Ship Review card for a task.
type Card struct {
	ID              string     `json:"id"`
	TaskID          string     `json:"task_id"`
	Branch          string     `json:"branch"`
	HeadSHA         string     `json:"head_sha"`
	TestSteps       []string   `json:"test_steps"`
	DevURL          string     `json:"dev_url"`
	DevPID          int        `json:"dev_pid"`
	// DevState tracks async setup progress: "", "starting", "ready", "error".
	DevState        string     `json:"dev_state,omitempty"`
	// DevLog holds the last N progress lines from async setup.
	DevLog          []string   `json:"dev_log,omitempty"`
	Status          string     `json:"status"`
	ApprovedSHA     string     `json:"approved_sha,omitempty"`
	MainSHA         string     `json:"main_sha,omitempty"`
	SendBackComment string     `json:"send_back_comment,omitempty"`
	RejectComment   string     `json:"reject_comment,omitempty"`
	FilesChanged    []string   `json:"files_changed"`
	CheckRuns       []CheckRun `json:"check_runs"`
	// AgentSummary is the most recent agent-summary comment for this task (from
	// task_comments WHERE author='agent-summary'). Not stored on the card; populated
	// at read time by GetCard so the Board sees the agent's final summary.
	AgentSummary   string `json:"agent_summary,omitempty"`
	// HasDBMigration is true when any file in FilesChanged matches a DB migration
	// path pattern (supabase/migrations/, db/migrations/, prisma/migrations/, *.sql
	// inside a migrations/ dir). Computed at read time; not stored.
	HasDBMigration bool      `json:"has_db_migration"`
	// RunNumber is the 1-based count of ship-review cards for this task (i.e. run N).
	// Computed at read time; not stored.
	RunNumber      int       `json:"run_number"`
	// BranchDeleted is set once the task branch (remote + local) and its
	// worktrees are gone after Approve & merge (STA-637).
	BranchDeleted bool `json:"branch_deleted"`
	// BranchDeleteError holds the last cleanup failure; the merge still stands.
	BranchDeleteError string `json:"branch_delete_error,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ErrHeadMoved is returned when the branch HEAD changed after the card was rendered.
var ErrHeadMoved = errors.New("branch HEAD moved since card was rendered; re-render required")

// ErrNoCard is returned when no card exists for a task.
var ErrNoCard = errors.New("no ship review card found")

// ErrTestStepsRequired is returned when test_steps is empty.
var ErrTestStepsRequired = errors.New("test_steps are required to create a ship review card")

// ErrInvalidBranch is returned when a branch name is unsafe.
var ErrInvalidBranch = errors.New("invalid branch name")

// ErrProtectedBranch is returned when trying to delete a protected branch.
var ErrProtectedBranch = errors.New("refusing to delete protected branch")

// ErrInvalidDevURL is returned when dev_url is not a safe loopback http/https URL.
var ErrInvalidDevURL = errors.New("dev_url must be an http/https URL pointing to a loopback address")

// validateBranch rejects branch names that could be injected as git flags or
// path-traversal vectors.
func validateBranch(branch string) error {
	if branch == "" {
		return ErrInvalidBranch
	}
	if strings.HasPrefix(branch, "-") {
		return fmt.Errorf("%w: branch name may not start with '-'", ErrInvalidBranch)
	}
	// Disallow shell metacharacters; branch names are passed directly to exec.
	for _, c := range branch {
		if c == ' ' || c == '\t' || c == '\n' || c == ';' || c == '&' || c == '|' || c == '`' || c == '$' || c == '>' || c == '<' {
			return fmt.Errorf("%w: branch name contains disallowed character %q", ErrInvalidBranch, c)
		}
	}
	return nil
}

// ValidateDevURL rejects non-loopback or non-http(s) URLs.
func ValidateDevURL(rawURL string) error {
	return validateDevURL(rawURL)
}

// ValidateSQLEditorURL rejects non-http(s) URLs. Unlike ValidateDevURL it
// permits external hosts (e.g. supabase.com dashboard links).
func ValidateSQLEditorURL(rawURL string) error {
	if rawURL == "" {
		return nil
	}
	lower := strings.ToLower(rawURL)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return fmt.Errorf("sql_editor_url must start with http:// or https://")
	}
	// Require a non-empty host after the scheme.
	rest := lower[strings.Index(lower, "//")+2:]
	host := rest
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if host == "" {
		return fmt.Errorf("sql_editor_url must include a valid host")
	}
	return nil
}

// validateDevURL rejects non-loopback or non-http(s) URLs.
func validateDevURL(rawURL string) error {
	if rawURL == "" {
		return nil // empty is ok (no dev server)
	}
	// Only http/https, only loopback hosts allowed.
	lower := strings.ToLower(rawURL)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return ErrInvalidDevURL
	}
	// Allow 127.x.x.x, [::1], and "localhost" only.
	host := lower[strings.Index(lower, "//")+2:]
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	host = strings.Trim(host, "[]")
	if host != "localhost" && host != "::1" && !strings.HasPrefix(host, "127.") {
		return fmt.Errorf("%w: host %q is not a loopback address", ErrInvalidDevURL, host)
	}
	return nil
}

// devServerManager tracks running dev-server subprocesses by task ID.
var devServerManager = &procManager{
	procs:      make(map[string]*os.Process),
	repoPaths:  make(map[string]string),
	worktrees:  make(map[string]string),
	setups:     make(map[string]map[*devSetup]struct{}),
}

type procManager struct {
	mu         sync.Mutex
	procs      map[string]*os.Process
	repoPaths  map[string]string // taskID -> repoPath (for worktree cleanup)
	worktrees  map[string]string // taskID -> temp worktree path
	setups     map[string]map[*devSetup]struct{} // taskID -> in-flight setups
}

// devSetup is one in-flight startDevServerSync run, tracked so branch cleanup
// can stop it before it recreates the dev worktree (STA-648).
type devSetup struct {
	cancel   context.CancelFunc
	canceled bool // guarded by procManager.mu
	done     chan struct{}
}

// errDevSetupCanceled is returned by startDevServerSync when cancelSetups
// stopped it; the dev worktree has been swept by then.
var errDevSetupCanceled = errors.New("dev server setup canceled")

// devSetupWait bounds how long CleanupMergedBranch waits for a canceled setup
// to exit. A setup that outlives it sweeps its own worktree on exit.
var devSetupWait = 30 * time.Second

func (m *procManager) beginSetup(taskID string) (context.Context, *devSetup) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &devSetup{cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.setups[taskID] == nil {
		m.setups[taskID] = make(map[*devSetup]struct{})
	}
	m.setups[taskID][s] = struct{}{}
	return ctx, s
}

func (m *procManager) endSetup(taskID string, s *devSetup) {
	m.mu.Lock()
	delete(m.setups[taskID], s)
	if len(m.setups[taskID]) == 0 {
		delete(m.setups, taskID)
	}
	m.mu.Unlock()
	s.cancel()
	close(s.done)
}

// cancelSetups cancels every in-flight setup for taskID and waits up to wait
// (or until ctx ends) for them to exit. Reports whether all of them exited.
func (m *procManager) cancelSetups(ctx context.Context, taskID string, wait time.Duration) bool {
	m.mu.Lock()
	var pending []*devSetup
	for s := range m.setups[taskID] {
		s.canceled = true
		pending = append(pending, s)
	}
	m.mu.Unlock()
	if len(pending) == 0 {
		return true
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for _, s := range pending {
		s.cancel()
	}
	for _, s := range pending {
		select {
		case <-s.done:
		case <-timer.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// storeIfActive registers the launched process unless s was canceled first.
// The check and the store share the lock, so cancelSetups followed by kill
// never misses a process.
func (m *procManager) storeIfActive(s *devSetup, taskID string, p *os.Process, repoPath, wtPath string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.canceled {
		return false
	}
	m.procs[taskID] = p
	if repoPath != "" {
		m.repoPaths[taskID] = repoPath
	}
	if wtPath != "" {
		m.worktrees[taskID] = wtPath
	}
	return true
}

func (m *procManager) kill(taskID string) {
	m.mu.Lock()
	p, ok := m.procs[taskID]
	delete(m.procs, taskID)
	repoPath := m.repoPaths[taskID]
	delete(m.repoPaths, taskID)
	wtPath := m.worktrees[taskID]
	delete(m.worktrees, taskID)
	m.mu.Unlock()
	if ok && p != nil {
		_ = p.Kill()
	}
	if wtPath != "" {
		removeDevWorktree(repoPath, wtPath)
	}
}

func (m *procManager) has(taskID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.procs[taskID]
	return ok
}

// paths returns the repoPath and worktree path for a running dev server.
// Both are empty strings if no server is running for taskID.
func (m *procManager) paths(taskID string) (repoPath, wtPath string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.repoPaths[taskID], m.worktrees[taskID]
}

// removeDevWorktree removes a temporary dev-server worktree.
// Uses `git worktree remove --force` when a repoPath is available, else os.RemoveAll.
func removeDevWorktree(repoPath, wtPath string) {
	if repoPath != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = gitOutput(ctx, repoPath, "worktree", "remove", "--force", wtPath)
		return
	}
	_ = os.RemoveAll(wtPath)
}

// CreateCard inserts a new ship review card or replaces an existing pending one.
// testSteps must be non-empty. headSHA is the current branch HEAD.
// repoDir is used to derive files changed via git diff; pass "" to skip.
// checkRuns is optional evidence from CI / verification commands.
func CreateCard(db *sql.DB, taskID, branch, headSHA string, testSteps []string, devURL string, repoDir string, checkRuns []CheckRun) (*Card, error) {
	if len(testSteps) == 0 {
		return nil, ErrTestStepsRequired
	}
	if err := validateBranch(branch); err != nil {
		return nil, err
	}
	if err := validateDevURL(devURL); err != nil {
		return nil, err
	}

	stepsJSON, err := json.Marshal(testSteps)
	if err != nil {
		return nil, fmt.Errorf("marshal test_steps: %w", err)
	}

	// Derive files changed from git diff against the merge-base.
	filesChanged := diffFilesChanged(repoDir, headSHA)
	filesJSON, err := json.Marshal(filesChanged)
	if err != nil {
		return nil, fmt.Errorf("marshal files_changed: %w", err)
	}

	if checkRuns == nil {
		checkRuns = []CheckRun{}
	}
	checksJSON, err := json.Marshal(checkRuns)
	if err != nil {
		return nil, fmt.Errorf("marshal check_runs: %w", err)
	}

	// Replace any existing pending card for this task (agent iterating).
	_, _ = db.Exec(`DELETE FROM ship_review_cards WHERE task_id = ? AND status IN ('pending', 'sent_back')`, taskID)

	id := uuid.New().String()
	now := time.Now().UTC()
	_, err = db.Exec(`
		INSERT INTO ship_review_cards
			(id, task_id, branch, head_sha, test_steps_json, dev_url, dev_pid, status,
			 files_changed_json, check_runs_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, 'pending', ?, ?, ?, ?)`,
		id, taskID, branch, headSHA, string(stepsJSON), devURL,
		string(filesJSON), string(checksJSON),
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, fmt.Errorf("insert card: %w", err)
	}
	return GetCard(db, taskID)
}

// diffFilesChanged returns the list of files changed between the merge-base of
// "main" (or "master") and headSHA. Returns an empty slice on any error.
func diffFilesChanged(repoDir, headSHA string) []string {
	if repoDir == "" || headSHA == "" {
		return []string{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Find merge-base against main (or master as fallback).
	var base string
	for _, target := range []string{"main", "master"} {
		out, err := gitOutput(ctx, repoDir, "merge-base", target, headSHA)
		if err == nil && out != "" {
			base = out
			break
		}
	}
	if base == "" {
		return []string{}
	}

	out, err := gitOutput(ctx, repoDir, "diff", "--name-only", base, headSHA)
	if err != nil || out == "" {
		return []string{}
	}
	files := strings.Split(out, "\n")
	result := make([]string, 0, len(files))
	for _, f := range files {
		if f != "" {
			result = append(result, f)
		}
	}
	return result
}

// GetCard returns the most recent ship review card for a task.
func GetCard(db *sql.DB, taskID string) (*Card, error) {
	row := db.QueryRow(`
		SELECT id, task_id, branch, head_sha, test_steps_json, dev_url, dev_pid,
		       status, approved_sha, main_sha, send_back_comment, reject_comment,
		       COALESCE(files_changed_json, '[]'), COALESCE(check_runs_json, '[]'),
		       COALESCE(dev_state,''), COALESCE(dev_log_json,'[]'),
		       COALESCE(branch_deleted,0), COALESCE(branch_delete_error,''),
		       created_at, updated_at
		FROM ship_review_cards
		WHERE task_id = ?
		ORDER BY created_at DESC LIMIT 1`, taskID)

	var c Card
	var stepsJSON, filesJSON, checksJSON, devLogJSON string
	var approvedSHA, mainSHA, sendBack, reject sql.NullString
	var createdAt, updatedAt string

	if err := row.Scan(
		&c.ID, &c.TaskID, &c.Branch, &c.HeadSHA, &stepsJSON, &c.DevURL, &c.DevPID,
		&c.Status, &approvedSHA, &mainSHA, &sendBack, &reject,
		&filesJSON, &checksJSON,
		&c.DevState, &devLogJSON,
		&c.BranchDeleted, &c.BranchDeleteError,
		&createdAt, &updatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNoCard
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(stepsJSON), &c.TestSteps); err != nil {
		c.TestSteps = []string{}
	}
	if err := json.Unmarshal([]byte(filesJSON), &c.FilesChanged); err != nil {
		c.FilesChanged = []string{}
	}
	if err := json.Unmarshal([]byte(checksJSON), &c.CheckRuns); err != nil {
		c.CheckRuns = []CheckRun{}
	}
	if err := json.Unmarshal([]byte(devLogJSON), &c.DevLog); err != nil {
		c.DevLog = []string{}
	}
	c.ApprovedSHA = approvedSHA.String
	c.MainSHA = mainSHA.String
	c.SendBackComment = sendBack.String
	c.RejectComment = reject.String
	c.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	c.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)

	// Populate AgentSummary from the most recent agent-summary comment that was
	// posted AFTER the previous card for this task (if any). This prevents a
	// re-created card (run 2+) from inheriting the summary posted during run 1
	// before run 2 has had a chance to post its own summary.
	// For run 1 (no previous card) this is equivalent to the most-recent query.
	var summary sql.NullString
	_ = db.QueryRow(`
		SELECT message FROM task_comments
		WHERE task_id = ? AND author = 'agent-summary'
		  AND created_at > COALESCE(
		    (SELECT created_at FROM ship_review_cards
		       WHERE task_id = ? ORDER BY created_at DESC LIMIT 1 OFFSET 1),
		    '0000-00-00T00:00:00Z'
		  )
		ORDER BY created_at DESC LIMIT 1`,
		taskID, taskID,
	).Scan(&summary)
	c.AgentSummary = summary.String

	// Compute HasDBMigration from FilesChanged.
	c.HasDBMigration = isMigrationInFiles(c.FilesChanged)

	// Compute RunNumber: how many cards (including this one) exist for the task.
	var runNum int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM ship_review_cards WHERE task_id = ?`, taskID,
	).Scan(&runNum); err != nil || runNum < 1 {
		runNum = 1
	}
	c.RunNumber = runNum

	return &c, nil
}

// isMigrationInFiles returns true when any path looks like a DB migration file.
func isMigrationInFiles(files []string) bool {
	for _, f := range files {
		lower := strings.ToLower(filepath.ToSlash(f))
		// supabase/migrations/, db/migrations/, prisma/migrations/, etc.
		if strings.Contains(lower, "/migrations/") {
			return true
		}
		// Bare top-level migrations/ dir
		if strings.HasPrefix(lower, "migrations/") {
			return true
		}
	}
	return false
}

// SetDevPID persists the dev server PID to the card.
func SetDevPID(db *sql.DB, cardID string, pid int) error {
	_, err := db.Exec(
		`UPDATE ship_review_cards SET dev_pid = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`,
		pid, cardID,
	)
	return err
}

// SetDevURL persists an auto-detected dev URL to the card.
func SetDevURL(db *sql.DB, cardID, devURL string) error {
	_, err := db.Exec(
		`UPDATE ship_review_cards SET dev_url = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`,
		devURL, cardID,
	)
	return err
}

// SetDevState persists the async dev-env state and appends a log line to the card.
// state is one of DevStateStarting, DevStateReady, DevStateError, DevStateIdle.
// logLine is appended to the last 50 stored lines (empty string skips append).
func SetDevState(db *sql.DB, cardID, state, logLine string) error {
	// Read existing log.
	var existing string
	_ = db.QueryRow(`SELECT COALESCE(dev_log_json,'[]') FROM ship_review_cards WHERE id = ?`, cardID).Scan(&existing)
	var lines []string
	if err := json.Unmarshal([]byte(existing), &lines); err != nil {
		lines = []string{}
	}
	if logLine != "" {
		lines = append(lines, logLine)
		if len(lines) > 50 {
			lines = lines[len(lines)-50:]
		}
	}
	logJSON, _ := json.Marshal(lines)
	_, err := db.Exec(
		`UPDATE ship_review_cards
		 SET dev_state = ?, dev_log_json = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE id = ?`,
		state, string(logJSON), cardID,
	)
	return err
}

// DevProgress is emitted during async dev-server setup.
type DevProgress struct {
	TaskID  string `json:"task_id"`
	Step    string `json:"step"`
	Message string `json:"message"`
	OK      bool   `json:"ok"`
}

// StartDevServerAsync launches dev-server setup in a background goroutine and
// returns immediately. Progress is streamed via the optional progress callback
// (typically h.hub.Publish). Card dev_state is set to "starting" on entry and
// updated to "ready" or "error" on completion.
//
// Returns the expected URL immediately so the caller can show it before the
// server is fully up.
func StartDevServerAsync(db *sql.DB, card *Card, cfg *ProjectDevConfig, repoPath string, progress func(DevProgress)) (string, error) {
	if cfg.DevCommand == "" {
		return "", fmt.Errorf("no dev_command configured for this project")
	}

	url := cfg.DevURL
	if url == "" {
		url = "http://localhost:3000"
	}

	_ = SetDevState(db, card.ID, DevStateStarting, "Dev server setup starting…")

	emit := func(step, msg string, ok bool) {
		_ = SetDevState(db, card.ID, DevStateStarting, msg)
		if progress != nil {
			progress(DevProgress{TaskID: card.TaskID, Step: step, Message: msg, OK: ok})
		}
	}

	go func() {
		if err := startDevServerSync(db, card, cfg, repoPath, emit); err != nil {
			if errors.Is(err, errDevSetupCanceled) {
				_ = SetDevState(db, card.ID, DevStateIdle, "Dev server setup canceled")
				return
			}
			_ = SetDevState(db, card.ID, DevStateError, "Error: "+err.Error())
			if progress != nil {
				progress(DevProgress{TaskID: card.TaskID, Step: "error", Message: err.Error(), OK: false})
			}
			return
		}
		_ = SetDevState(db, card.ID, DevStateReady, "Dev server ready at "+url)
		if progress != nil {
			progress(DevProgress{TaskID: card.TaskID, Step: "ready", Message: url, OK: true})
		}
	}()

	return url, nil
}

// StartDevServer is the synchronous form used by tests and the old code path.
// New callers should prefer StartDevServerAsync.
func StartDevServer(db *sql.DB, card *Card, cfg *ProjectDevConfig, repoPath string) (string, error) {
	if cfg.DevCommand == "" {
		return "", fmt.Errorf("no dev_command configured for this project")
	}
	noop := func(_, _ string, _ bool) {}
	if err := startDevServerSync(db, card, cfg, repoPath, noop); err != nil {
		return "", err
	}
	url := cfg.DevURL
	if url == "" {
		url = "http://localhost:3000"
	}
	_ = SetDevState(db, card.ID, DevStateReady, "Dev server ready at "+url)
	return url, nil
}

// ensureDevWorktree creates or reuses a detached worktree at wtPath for headSHA.
// Before calling git worktree add it clears any stale registration for wtPath
// (e.g. directory deleted by daemon restart while git still had it registered).
// If a registered worktree already exists at wtPath with the correct SHA it is
// reused without recreation. Only paths whose base name starts with "devserver-"
// are managed; anything else is rejected.
func ensureDevWorktree(ctx context.Context, repoPath, wtPath, headSHA string) error {
	if !strings.HasPrefix(filepath.Base(wtPath), "devserver-") {
		return fmt.Errorf("ensureDevWorktree: refusing non-devserver path %q", wtPath)
	}

	// Clear any stale git registration for this exact path.
	// --force succeeds even when the directory is missing (post-daemon-restart).
	_, _ = gitOutput(ctx, repoPath, "worktree", "remove", "--force", wtPath)
	// Prune any other orphaned registrations in this repo.
	_, _ = gitOutput(ctx, repoPath, "worktree", "prune")
	// Remove directory remnants if any.
	_ = os.RemoveAll(wtPath)

	_, err := gitOutput(ctx, repoPath, "worktree", "add", "--detach", wtPath, headSHA)
	return err
}

// startDevServerSync performs the full setup sequence: worktree, Supabase env,
// setup steps, then launches the dev process. Emits progress via emit.
func startDevServerSync(db *sql.DB, card *Card, cfg *ProjectDevConfig, repoPath string, emit func(step, msg string, ok bool)) error {
	// Register before touching the worktree so CleanupMergedBranch can stop
	// this run instead of racing it (STA-648).
	ctx, setup := devServerManager.beginSetup(card.TaskID)
	defer devServerManager.endSetup(card.TaskID, setup)

	// Kill any stale server (and clean up its worktree) for this task first.
	devServerManager.kill(card.TaskID)

	// Create a temporary detached worktree at the pinned commit SHA.
	wtPath := filepath.Join(repoPath, ".worktrees", "devserver-"+card.TaskID)
	bgCtx := context.Background()
	emit("worktree", "Creating dev worktree at "+card.HeadSHA[:min(len(card.HeadSHA), 12)]+"…", true)
	if err := ensureDevWorktree(bgCtx, repoPath, wtPath, card.HeadSHA); err != nil {
		return fmt.Errorf("create dev worktree at %s: %w", card.HeadSHA, err)
	}

	// Once canceled, sweep the worktree ourselves: cleanup may already have
	// removed and pruned it, so git alone might not know the directory.
	canceled := func() error {
		removeDevWorktree(repoPath, wtPath)
		_ = os.RemoveAll(wtPath)
		_, _ = gitOutput(bgCtx, repoPath, "worktree", "prune")
		return errDevSetupCanceled
	}
	if ctx.Err() != nil {
		return canceled()
	}

	// Built-in Supabase dev env (before custom setup steps).
	if cfg.SupabaseEnabled {
		emit("supabase", "Setting up local Supabase dev env…", true)
		if err := StartSupabaseDevEnv(repoPath, wtPath, cfg.DevCommand, func(p SupabaseProgress) {
			emit(p.Step, p.Message, p.OK)
		}); err != nil {
			if ctx.Err() != nil {
				return canceled()
			}
			removeDevWorktree(repoPath, wtPath)
			return fmt.Errorf("supabase dev env: %w", err)
		}
		if ctx.Err() != nil {
			return canceled()
		}
	}

	// Custom setup steps (now run via /bin/sh -c so pipes/quotes/&& work).
	for _, step := range cfg.SetupSteps {
		emit("setup", "Running: "+step, true)
		if err := runShellStep(ctx, step, wtPath); err != nil {
			if ctx.Err() != nil {
				return canceled()
			}
			removeDevWorktree(repoPath, wtPath)
			return fmt.Errorf("setup step %q failed: %w", step, err)
		}
	}
	if ctx.Err() != nil {
		return canceled()
	}

	// Launch the dev server process.
	emit("server", "Starting dev server…", true)
	cmd := exec.Command("/bin/sh", "-c", cfg.DevCommand) //nolint:gosec
	cmd.Dir = wtPath
	// Strip inherited VITE_* and SUPABASE_* vars: process env beats every .env
	// file in Vite, so an inherited VITE_SUPABASE_URL would route to production
	// even when .env.local points at localhost.
	cmd.Env = append(StripViteSupabaseEnv(os.Environ()),
		"FORCE_COLOR=1",
		"PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:"+os.Getenv("PATH"),
	)

	if err := cmd.Start(); err != nil {
		removeDevWorktree(repoPath, wtPath)
		return fmt.Errorf("start dev server: %w", err)
	}

	if !devServerManager.storeIfActive(setup, card.TaskID, cmd.Process, repoPath, wtPath) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return canceled()
	}
	_ = SetDevPID(db, card.ID, cmd.Process.Pid)

	go func() {
		_ = cmd.Wait()
		devServerManager.kill(card.TaskID)
	}()

	return nil
}

// min returns the smaller of a and b (Go 1.20 didn't have built-in min for int).
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// StopDevServer kills the dev server for a task and clears the PID.
// If the project config has SupabaseEnabled and !SupabaseKeepUp, also stops
// the local Supabase stack (data volumes are kept for fast next startup).
func StopDevServer(db *sql.DB, card *Card) {
	// Determine the worktree path to pass to supabase stop.
	repoPath, wtPath := devServerManager.paths(card.TaskID)

	devServerManager.kill(card.TaskID)
	_, _ = db.Exec(
		`UPDATE ship_review_cards
		 SET dev_pid = 0, dev_state = '', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		 WHERE id = ?`,
		card.ID,
	)

	if wtPath == "" || repoPath == "" {
		return
	}
	cfg, err := GetProjectDevConfig(db, repoPath)
	if err != nil || cfg == nil || !cfg.SupabaseEnabled || cfg.SupabaseKeepUp {
		return
	}
	// Fire-and-forget: supabase stop is slow; don't block the HTTP response.
	go func() {
		_ = StopSupabaseDevEnv(wtPath)
	}()
}

// BuildAndStartCard is the single entry point for creating a Ship Review card.
// It must be used by all callers (HTTP handler, MCP tool, CLI) so they all
// behave identically.
//
// Key invariant: the branch is ALWAYS "staypoint/<taskID>". task.GitBranch is
// set to the repo's active branch at task-creation time (usually "main") and
// must never be used here — doing so was the bug fixed by STA-571.
//
// It resolves HEAD from repoPath, creates the card, then auto-starts the dev
// server from the project config (if any) and persists dev_url.
func BuildAndStartCard(ctx context.Context, db *sql.DB, taskID, repoPath string, testSteps []string, devURL string, checkRuns []CheckRun) (*Card, error) {
	branch := "staypoint/" + taskID

	headSHA, err := CurrentBranchHEAD(ctx, repoPath, branch)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve branch HEAD for %q: %w", branch, err)
	}

	card, err := CreateCard(db, taskID, branch, headSHA, testSteps, devURL, repoPath, checkRuns)
	if err != nil {
		return nil, err
	}

	// Auto-start dev server only from an explicitly human-saved project config.
	// Auto-detection of Supabase projects is handled in the board-facing StartDev
	// HTTP endpoint (after the board user explicitly clicks Start), not here —
	// BuildAndStartCard is agent-reachable and must not silently run dev commands.
	cfg, _ := GetProjectDevConfig(db, repoPath)
	if cfg != nil && cfg.DevCommand != "" {
		if startedURL, startErr := StartDevServer(db, card, cfg, repoPath); startErr == nil && startedURL != "" && card.DevURL == "" {
			card.DevURL = startedURL
			_ = SetDevURL(db, card.ID, startedURL)
		}
	}

	return card, nil
}

// ApproveAndMerge merges branch into the target branch (usually main) only if
// the current HEAD matches card.HeadSHA. After merge, verifies the reviewed SHA
// is an ancestor of the new main HEAD.
func ApproveAndMerge(ctx context.Context, db *sql.DB, card *Card, repoDir, targetBranch string) (mainSHA string, err error) {
	// 1. Check that HEAD hasn't moved.
	currentHEAD, err := gitOutput(ctx, repoDir, "rev-parse", card.Branch)
	if err != nil {
		return "", fmt.Errorf("resolve branch HEAD: %w", err)
	}
	if currentHEAD != card.HeadSHA {
		return "", ErrHeadMoved
	}

	// 2. Fetch latest and check out target branch.
	// Tolerate repos with no remote (e.g. tests / offline).
	_, _ = gitOutput(ctx, repoDir, "fetch", "--all", "--prune")

	// 3. Merge the branch into target.
	if _, err := gitOutput(ctx, repoDir, "checkout", targetBranch); err != nil {
		return "", fmt.Errorf("checkout %s: %w", targetBranch, err)
	}
	// Pull only when a tracking branch exists; skip silently otherwise.
	if out, err := gitOutput(ctx, repoDir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err == nil && out != "" {
		if _, err := gitOutput(ctx, repoDir, "pull", "--ff-only"); err != nil {
			return "", fmt.Errorf("pull %s: %w", targetBranch, err)
		}
	}
	// Merge the exact pinned commit (not the branch ref) to prevent TOCTOU.
	if _, err := gitOutput(ctx, repoDir, "merge", "--no-ff", "-m",
		fmt.Sprintf("Merge branch '%s' (reviewed SHA %s)", card.Branch, card.HeadSHA),
		card.HeadSHA); err != nil {
		return "", fmt.Errorf("merge: %w", err)
	}

	// 4. Push.
	if _, err := gitOutput(ctx, repoDir, "push", "origin", targetBranch); err != nil {
		return "", fmt.Errorf("push: %w", err)
	}

	// 5. Capture new main HEAD.
	mainSHA, err = gitOutput(ctx, repoDir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve main HEAD after merge: %w", err)
	}

	// 6. Verify reviewed SHA is ancestor of main.
	if err := verifyAncestor(ctx, repoDir, card.HeadSHA, mainSHA); err != nil {
		return mainSHA, fmt.Errorf("ancestor check failed: %w", err)
	}

	// 7. Persist outcome.
	_, err = db.Exec(`
		UPDATE ship_review_cards
		SET status = 'approved', approved_sha = ?, main_sha = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?`,
		card.HeadSHA, mainSHA, card.ID,
	)
	return mainSHA, err
}

// SendBack marks the card sent_back with a comment, waking the agent.
func SendBack(db *sql.DB, card *Card, comment string) error {
	_, err := db.Exec(`
		UPDATE ship_review_cards
		SET status = 'sent_back', send_back_comment = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?`, comment, card.ID,
	)
	return err
}

// Reject marks the card rejected. The caller is responsible for deleting the branch.
func Reject(db *sql.DB, card *Card, comment string) error {
	_, err := db.Exec(`
		UPDATE ship_review_cards
		SET status = 'rejected', reject_comment = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?`, comment, card.ID,
	)
	return err
}

// DeleteBranch deletes the remote branch for a review.
// It refuses to delete main, master, or the remote default branch.
func DeleteBranch(ctx context.Context, repoDir, branch string) error {
	if err := guardDeletableBranch(ctx, repoDir, branch); err != nil {
		return err
	}
	// Use refs/heads/ form so the arg can never be misinterpreted as a flag.
	_, err := gitOutput(ctx, repoDir, "push", "origin", "--delete", "refs/heads/"+branch)
	return err
}

// guardDeletableBranch rejects branch names that must never be deleted:
// malformed names, main, master, and the remote default branch.
func guardDeletableBranch(ctx context.Context, repoDir, branch string) error {
	if err := validateBranch(branch); err != nil {
		return err
	}
	if branch == "main" || branch == "master" {
		return fmt.Errorf("%w: %q", ErrProtectedBranch, branch)
	}
	// Detect remote default branch (e.g. "origin/main" → "main").
	if remoteRef, err := gitOutput(ctx, repoDir, "rev-parse", "--abbrev-ref", "origin/HEAD"); err == nil {
		defaultBranch := strings.TrimPrefix(remoteRef, "origin/")
		if defaultBranch != "" && branch == defaultBranch {
			return fmt.Errorf("%w: %q is the remote default branch", ErrProtectedBranch, branch)
		}
	}
	return nil
}

// CleanupMergedBranch removes what is left of a task once its review has been
// approved and merged into mainSHA: the dev-server worktree, the agent
// worktree, the local branch, and the remote branch (STA-637). Only
// card.Branch is ever touched, never the target or default branch.
//
// It is idempotent so the Board can retry after a failure: refs and worktrees
// that are already gone count as deleted. Both branch tips are checked against
// mainSHA before anything is removed; if either holds commits main does not,
// nothing is touched, so the worktree (and any uncommitted changes in it)
// survives along with the branch (STA-648).
func CleanupMergedBranch(ctx context.Context, repoDir string, card *Card, mainSHA string) error {
	branch := card.Branch
	if err := guardDeletableBranch(ctx, repoDir, branch); err != nil {
		return err
	}
	if mainSHA == "" {
		return errors.New("cleanup: merged main SHA unknown")
	}
	// The task ID becomes a path segment under .worktrees; an empty, dotted,
	// or separator-bearing value (even a bare "/") would point removal at the
	// wrong directory, up to .worktrees itself.
	if card.TaskID == "" || strings.ContainsAny(card.TaskID, `/\`) || strings.HasPrefix(card.TaskID, ".") {
		return fmt.Errorf("cleanup: invalid task id %q", card.TaskID)
	}
	// git refuses to delete a branch that is checked out, so never touch the
	// branch repoDir itself is on (the merge leaves it on main).
	if cur, err := gitOutput(ctx, repoDir, "symbolic-ref", "--short", "-q", "HEAD"); err == nil && cur == branch {
		return fmt.Errorf("%w: %q is checked out in %s", ErrProtectedBranch, branch, repoDir)
	}

	// Check both tips before removing anything.
	ref := "refs/heads/" + branch
	localTip, _ := gitOutput(ctx, repoDir, "rev-parse", "--verify", "-q", ref)
	if localTip != "" {
		if err := verifyAncestor(ctx, repoDir, localTip, mainSHA); err != nil {
			return fmt.Errorf("local branch %s has commits not in main (tip %s); nothing deleted", branch, localTip)
		}
	}
	_, originErr := gitOutput(ctx, repoDir, "remote", "get-url", "origin")
	hasOrigin := originErr == nil
	remoteTip := ""
	if hasOrigin {
		// Ask the remote rather than trusting the tracking ref, which a retry
		// may see stale. A tip whose objects were never fetched fails the
		// ancestor check, so unseen pushes are kept.
		tip, err := remoteBranchTip(ctx, repoDir, ref)
		if err != nil {
			return fmt.Errorf("read remote branch %s: %w; nothing deleted", branch, err)
		}
		if tip != "" {
			if err := verifyAncestor(ctx, repoDir, tip, mainSHA); err != nil {
				return fmt.Errorf("remote branch %s has commits not in main (tip %s); nothing deleted", branch, tip)
			}
		}
		remoteTip = tip
	}

	// A Start Dev setup still in flight would recreate the dev worktree after
	// it is removed below. Cancel it, then kill any server it launched.
	devServerManager.cancelSetups(ctx, card.TaskID, devSetupWait)
	devServerManager.kill(card.TaskID)

	// Worktrees next: they hold the branch checked out.
	wtDir := filepath.Join(repoDir, ".worktrees")
	for _, wt := range []string{filepath.Join(wtDir, "devserver-"+card.TaskID), filepath.Join(wtDir, card.TaskID)} {
		_, _ = gitOutput(ctx, repoDir, "worktree", "remove", "--force", wt)
		if err := os.RemoveAll(wt); err != nil {
			return fmt.Errorf("remove worktree %s: %w", wt, err)
		}
	}
	_, _ = gitOutput(ctx, repoDir, "worktree", "prune")

	var errs []error

	if localTip != "" {
		// Compare-and-delete: a commit made since the check keeps the branch.
		if _, err := gitOutput(ctx, repoDir, "update-ref", "-d", ref, localTip); err != nil {
			errs = append(errs, fmt.Errorf("delete local branch: %w", err))
		} else {
			_, _ = gitOutput(ctx, repoDir, "config", "--remove-section", "branch."+branch)
		}
	}

	if remoteTip != "" {
		if err := deleteRemoteBranchAt(ctx, repoDir, branch, remoteTip); err != nil && !strings.Contains(err.Error(), "remote ref does not exist") {
			errs = append(errs, fmt.Errorf("delete remote branch: %w", err))
		}
	}

	return errors.Join(errs...)
}

// remoteBranchTip returns the SHA origin holds for ref, or "" if it has none.
func remoteBranchTip(ctx context.Context, repoDir, ref string) (string, error) {
	out, err := gitOutput(ctx, repoDir, "ls-remote", "origin", ref)
	if err != nil {
		return "", err
	}
	// ls-remote matches patterns by suffix, so require an exact ref name.
	for _, line := range strings.Split(out, "\n") {
		if sha, name, ok := strings.Cut(line, "\t"); ok && name == ref {
			return sha, nil
		}
	}
	return "", nil
}

// deleteRemoteBranchAt deletes branch on origin only while it still points at
// tip, so a push landing after tip was read is not lost. It applies the same
// guards as DeleteBranch.
func deleteRemoteBranchAt(ctx context.Context, repoDir, branch, tip string) error {
	if err := guardDeletableBranch(ctx, repoDir, branch); err != nil {
		return err
	}
	ref := "refs/heads/" + branch
	_, err := gitOutput(ctx, repoDir, "push", "--force-with-lease="+ref+":"+tip, "origin", "--delete", ref)
	return err
}

// SetBranchCleanup records the outcome of CleanupMergedBranch on the card.
func SetBranchCleanup(db *sql.DB, cardID string, deleted bool, errMsg string) error {
	_, err := db.Exec(`
		UPDATE ship_review_cards
		SET branch_deleted = ?, branch_delete_error = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?`, deleted, errMsg, cardID,
	)
	return err
}

// CurrentBranchHEAD resolves the HEAD SHA for a branch in repoDir.
func CurrentBranchHEAD(ctx context.Context, repoDir, branch string) (string, error) {
	return gitOutput(ctx, repoDir, "rev-parse", branch)
}

// verifyAncestor returns nil if sha is an ancestor of ref in repoDir.
func verifyAncestor(ctx context.Context, repoDir, sha, ref string) error {
	_, err := gitOutput(ctx, repoDir, "merge-base", "--is-ancestor", sha, ref)
	return err
}

// gitOutput runs git in dir with a 60s timeout and returns trimmed stdout.
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w (stderr: %s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// runShellStep runs a single setup step via /bin/sh -c so that quotes,
// pipes, and && work as expected. workDir is set as the working directory.
// Canceling ctx kills the step.
func runShellStep(ctx context.Context, step, workDir string) error {
	if strings.TrimSpace(step) == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", step) //nolint:gosec
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), "PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:"+os.Getenv("PATH"))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ProjectDevConfig holds per-project dev-server and migration settings.
type ProjectDevConfig struct {
	RepoPath        string   `json:"repo_path"`
	DevCommand      string   `json:"dev_command"`
	DevURL          string   `json:"dev_url"`
	SetupSteps      []string `json:"setup_steps"`
	// MigrationGlobs is the list of glob patterns used to detect migration files
	// in the task's diff. When empty the package-level defaults are used.
	MigrationGlobs  []string `json:"migration_globs"`
	// SQLEditorURL is the project's SQL editor deep-link (e.g. Supabase dashboard).
	SQLEditorURL    string   `json:"sql_editor_url"`
	// SupabaseEnabled enables the built-in Supabase dev env (Docker + local DB).
	// Auto-set when supabase/config.toml is detected and no config exists yet.
	SupabaseEnabled bool     `json:"supabase_enabled"`
	// SupabaseKeepUp prevents auto-stop of the local DB after review. Default false.
	SupabaseKeepUp  bool     `json:"supabase_keep_up"`
}

// GetProjectDevConfig loads the dev config for a repo path, or returns defaults.
func GetProjectDevConfig(db *sql.DB, repoPath string) (*ProjectDevConfig, error) {
	var stepsJSON, devCommand, devURL, migGlobsJSON, sqlEditorURL string
	var supabaseEnabled, supabaseKeepUp int
	err := db.QueryRow(
		`SELECT dev_command, dev_url, setup_steps_json,
		        COALESCE(migration_globs_json,'[]'), COALESCE(sql_editor_url,''),
		        COALESCE(supabase_enabled,0), COALESCE(supabase_keep_up,0)
		 FROM project_dev_configs WHERE repo_path = ?`,
		repoPath,
	).Scan(&devCommand, &devURL, &stepsJSON, &migGlobsJSON, &sqlEditorURL, &supabaseEnabled, &supabaseKeepUp)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &ProjectDevConfig{RepoPath: repoPath}, nil
		}
		return nil, err
	}
	cfg := &ProjectDevConfig{
		RepoPath:        repoPath,
		DevCommand:      devCommand,
		DevURL:          devURL,
		SQLEditorURL:    sqlEditorURL,
		SupabaseEnabled: supabaseEnabled != 0,
		SupabaseKeepUp:  supabaseKeepUp != 0,
	}
	if err := json.Unmarshal([]byte(stepsJSON), &cfg.SetupSteps); err != nil {
		cfg.SetupSteps = []string{}
	}
	if err := json.Unmarshal([]byte(migGlobsJSON), &cfg.MigrationGlobs); err != nil {
		cfg.MigrationGlobs = []string{}
	}
	return cfg, nil
}

// devExecer is satisfied by both *sql.DB and *sql.Tx.
type devExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// UpsertProjectDevConfig saves a project dev config.
func UpsertProjectDevConfig(db *sql.DB, cfg *ProjectDevConfig) error {
	return upsertDevConfig(db, cfg)
}

// UpsertProjectDevConfigTx saves a project dev config inside an existing transaction.
func UpsertProjectDevConfigTx(tx *sql.Tx, cfg *ProjectDevConfig) error {
	return upsertDevConfig(tx, cfg)
}

func upsertDevConfig(exec devExecer, cfg *ProjectDevConfig) error {
	stepsJSON, err := json.Marshal(cfg.SetupSteps)
	if err != nil {
		return err
	}
	migGlobsJSON, err := json.Marshal(cfg.MigrationGlobs)
	if err != nil {
		return err
	}
	supabaseEnabled := 0
	if cfg.SupabaseEnabled {
		supabaseEnabled = 1
	}
	supabaseKeepUp := 0
	if cfg.SupabaseKeepUp {
		supabaseKeepUp = 1
	}
	_, err = exec.Exec(`
		INSERT INTO project_dev_configs
			(repo_path, dev_command, dev_url, setup_steps_json, migration_globs_json,
			 sql_editor_url, supabase_enabled, supabase_keep_up, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		ON CONFLICT(repo_path) DO UPDATE SET
			dev_command          = excluded.dev_command,
			dev_url              = excluded.dev_url,
			setup_steps_json     = excluded.setup_steps_json,
			migration_globs_json = excluded.migration_globs_json,
			sql_editor_url       = excluded.sql_editor_url,
			supabase_enabled     = excluded.supabase_enabled,
			supabase_keep_up     = excluded.supabase_keep_up,
			updated_at           = excluded.updated_at`,
		cfg.RepoPath, cfg.DevCommand, cfg.DevURL,
		string(stepsJSON), string(migGlobsJSON), cfg.SQLEditorURL,
		supabaseEnabled, supabaseKeepUp,
	)
	return err
}

// ListProjectDevConfigs returns all project dev configs.
func ListProjectDevConfigs(db *sql.DB) ([]*ProjectDevConfig, error) {
	rows, err := db.Query(`
		SELECT repo_path, dev_command, dev_url, setup_steps_json,
		       COALESCE(migration_globs_json,'[]'), COALESCE(sql_editor_url,''),
		       COALESCE(supabase_enabled,0), COALESCE(supabase_keep_up,0)
		FROM project_dev_configs ORDER BY repo_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ProjectDevConfig
	for rows.Next() {
		var c ProjectDevConfig
		var stepsJSON, migGlobsJSON string
		var supEnabled, supKeepUp int
		if err := rows.Scan(&c.RepoPath, &c.DevCommand, &c.DevURL, &stepsJSON, &migGlobsJSON, &c.SQLEditorURL, &supEnabled, &supKeepUp); err != nil {
			continue
		}
		c.SupabaseEnabled = supEnabled != 0
		c.SupabaseKeepUp = supKeepUp != 0
		if err := json.Unmarshal([]byte(stepsJSON), &c.SetupSteps); err != nil {
			c.SetupSteps = []string{}
		}
		if err := json.Unmarshal([]byte(migGlobsJSON), &c.MigrationGlobs); err != nil {
			c.MigrationGlobs = []string{}
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

