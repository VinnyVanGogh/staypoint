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

	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
	"github.com/VinnyVanGogh/staypoint/internal/workspace"
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
	// MergeMode is the project's effective merge mode when the card was
	// approved (STA-717): "direct", "open_pr" or "pr_merge". Empty until then.
	MergeMode string `json:"merge_mode,omitempty"`
	// PRNumber / PRURL identify the GitHub PR a PR-mode Approve opened or reused.
	PRNumber int    `json:"pr_number,omitempty"`
	PRURL    string `json:"pr_url,omitempty"`
	// PRChecks is the last checks snapshot polled from GitHub, for the head
	// in PRChecksSHA. PRChecksSummary is derived at read time.
	PRChecks        []PRCheck `json:"pr_checks"`
	PRChecksSHA     string    `json:"pr_checks_sha,omitempty"`
	PRChecksAt      string    `json:"pr_checks_at,omitempty"`
	PRChecksSummary string    `json:"pr_checks_summary,omitempty"`
	// PRMergeError is GitHub's verbatim refusal from the last merge attempt.
	PRMergeError string `json:"pr_merge_error,omitempty"`
	// CIFixRequested is set when the Board sent CI failures back to the agent;
	// the agent's resubmitted card then re-pushes the PR and re-runs checks.
	CIFixRequested bool `json:"ci_fix_requested,omitempty"`

	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ErrHeadMoved is returned when the branch HEAD changed after the card was rendered.
var ErrHeadMoved = errors.New("branch HEAD moved since card was rendered; re-render required")

// ErrNoCard is returned when no card exists for a task.
var ErrNoCard = errors.New("no ship review card found")

// ErrNoChanges is returned when the task branch has no changes against its
// base: an answer-only run gets no card and no Approve (STA-774).
var ErrNoChanges = errors.New("No changes: answer-only run")

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
	procs:     make(map[string]*os.Process),
	repoPaths: make(map[string]string),
	worktrees: make(map[string]string),
	setups:    make(map[string]*devSetup),
}

type procManager struct {
	mu        sync.Mutex
	procs     map[string]*os.Process
	repoPaths map[string]string    // taskID -> repoPath (for worktree cleanup)
	worktrees map[string]string    // taskID -> temp worktree path
	setups    map[string]*devSetup // taskID -> dev-server setup still in flight
}

// devSetup is one in-flight run of startDevServerSync. cancel is only ever
// called with procManager.mu held, so a setup that sees ctx.Err() == nil under
// the lock knows nobody has cancelled it yet.
type devSetup struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// beginSetup registers a new dev-server setup for taskID, cancelling and
// waiting for any earlier one so two setups never share the worktree path.
//
// The swap and the cancel happen in one critical section: the new setup is
// visible to cancelSetup the moment the old one is superseded, so two
// overlapping restarts can never leave a live setup outside the map that
// CleanupMergedBranch could not reach.
func (m *procManager) beginSetup(taskID string) *devSetup {
	ctx, cancel := context.WithCancel(context.Background())
	s := &devSetup{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	prev := m.setups[taskID]
	m.setups[taskID] = s
	if prev != nil {
		prev.cancel()
	}
	m.mu.Unlock()
	if prev != nil {
		select {
		case <-prev.done:
		case <-time.After(devSetupWait):
		}
	}
	return s
}

// endSetup marks s finished and unregisters it if it is still current.
func (m *procManager) endSetup(taskID string, s *devSetup) {
	m.mu.Lock()
	if m.setups[taskID] == s {
		delete(m.setups, taskID)
	}
	s.cancel()
	m.mu.Unlock()
	close(s.done)
}

// cancelSetup cancels the in-flight setup for taskID, if any, and waits up to
// wait for it to finish. A cancelled setup removes its own worktree and never
// starts the server, so returning before it finishes is still safe: the
// worktree it may be creating is removed when it notices the cancellation.
func (m *procManager) cancelSetup(taskID string, wait time.Duration) {
	m.mu.Lock()
	s := m.setups[taskID]
	delete(m.setups, taskID)
	if s != nil {
		s.cancel()
	}
	m.mu.Unlock()
	if s == nil {
		return
	}
	select {
	case <-s.done:
	case <-time.After(wait):
	}
}

// storeIfActive records the started server unless its setup was cancelled
// in the meantime. Returns false when the caller must kill p instead.
func (m *procManager) storeIfActive(taskID string, s *devSetup, p *os.Process, repoPath, wtPath string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.ctx.Err() != nil {
		return false
	}
	m.procs[taskID] = p
	m.repoPaths[taskID] = repoPath
	m.worktrees[taskID] = wtPath
	return true
}

// devSetupWait bounds how long CleanupMergedBranch and StopDevServer wait for
// a cancelled dev-server setup to unwind. Variable so tests can shorten it.
var devSetupWait = 30 * time.Second

// ErrDevSetupCanceled is returned by dev-server setup that was cancelled,
// e.g. because the review was approved while it was still running.
var ErrDevSetupCanceled = errors.New("dev server setup canceled")

func (m *procManager) store(taskID string, p *os.Process, repoPath, wtPath string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.procs[taskID] = p
	if repoPath != "" {
		m.repoPaths[taskID] = repoPath
	}
	if wtPath != "" {
		m.worktrees[taskID] = wtPath
	}
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
		killDevProcessGroup(p)
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

// removeDevWorktree removes a temporary dev-server worktree, and its git
// registration when a repoPath is available.
func removeDevWorktree(repoPath, wtPath string) {
	if repoPath != "" {
		_ = deleteDevWorktree(context.Background(), repoPath, wtPath)
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

	// Files changed come from the task's recorded base, the same resolver
	// the Diff tab and Approve use (STA-774). A tampered base or an
	// answer-only run gets no card.
	filesChanged, err := diffFilesChanged(db, repoDir, taskID, headSHA)
	if err != nil {
		return nil, err
	}
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

	// The PR outlives the card it was opened from: a resubmitted card keeps
	// it, with its checks reset until the new head is pushed (STA-717).
	prev := previousOpenPR(db, taskID)

	// Replace any existing pending card for this task (agent iterating).
	_, _ = db.Exec(`DELETE FROM ship_review_cards WHERE task_id = ? AND status IN ('pending', 'sent_back')`, taskID)

	id := uuid.New().String()
	now := time.Now().UTC()
	_, err = db.Exec(`
		INSERT INTO ship_review_cards
			(id, task_id, branch, head_sha, test_steps_json, dev_url, dev_pid, status,
			 files_changed_json, check_runs_json, merge_mode, pr_number, pr_url, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, 'pending', ?, ?, ?, ?, ?, ?, ?)`,
		id, taskID, branch, headSHA, string(stepsJSON), devURL,
		string(filesJSON), string(checksJSON), prev.mode, prev.number, prev.url,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
	)
	if err != nil {
		return nil, fmt.Errorf("insert card: %w", err)
	}
	return GetCard(db, taskID)
}

// diffFilesChanged returns the files headSHA changes against the task's
// recorded base (workspace.TaskBase). It fails closed: a base that is missing,
// tampered with or unreadable is an error (no card), and ErrNoChanges is
// returned when nothing differs. repoDir "" skips the diff for DB-only
// callers; such a card still cannot be approved, since Approve re-verifies
// the base (VerifyCardChanges).
func diffFilesChanged(db *sql.DB, repoDir, taskID, headSHA string) ([]string, error) {
	if repoDir == "" {
		return []string{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return taskChanges(ctx, db, repoDir, taskID, headSHA)
}

// taskChanges lists the files head changes against the task's verified base:
// ErrNoChanges when there are none, the workspace error when the base cannot
// be verified.
func taskChanges(ctx context.Context, db *sql.DB, repoDir, taskID, head string) ([]string, error) {
	if head == "" {
		return nil, fmt.Errorf("task %s: no head to diff", taskID)
	}
	files, _, err := workspace.TaskFilesChanged(ctx, db, repoDir, taskID, head)
	if err != nil {
		return nil, fmt.Errorf("verify task base: %w", err)
	}
	if len(files) == 0 {
		return nil, ErrNoChanges
	}
	return files, nil
}

// VerifyCardChanges re-checks, at Approve time, that card's pinned head still
// has changes against the task's recorded base. Any failure to verify the
// base (missing record, moved pin, git or DB error) is returned, and Approve
// must refuse: the Board is never asked to merge something measured against
// a base the agent could have chosen.
func VerifyCardChanges(ctx context.Context, db *sql.DB, card *Card, repoDir string) ([]string, error) {
	if card == nil {
		return nil, ErrNoCard
	}
	return taskChanges(ctx, db, repoDir, card.TaskID, card.HeadSHA)
}

// GetCard returns the most recent ship review card for a task.
func GetCard(db *sql.DB, taskID string) (*Card, error) {
	row := db.QueryRow(`
		SELECT id, task_id, branch, head_sha, test_steps_json, dev_url, dev_pid,
		       status, approved_sha, main_sha, send_back_comment, reject_comment,
		       COALESCE(files_changed_json, '[]'), COALESCE(check_runs_json, '[]'),
		       COALESCE(dev_state,''), COALESCE(dev_log_json,'[]'),
		       COALESCE(branch_deleted,0), COALESCE(branch_delete_error,''),
		       COALESCE(merge_mode,''), COALESCE(pr_number,0), COALESCE(pr_url,''),
		       COALESCE(pr_checks_json,'[]'), COALESCE(pr_checks_sha,''), COALESCE(pr_checks_at,''),
		       COALESCE(pr_merge_error,''), COALESCE(ci_fix_requested,0),
		       created_at, updated_at
		FROM ship_review_cards
		WHERE task_id = ?
		ORDER BY created_at DESC LIMIT 1`, taskID)

	var c Card
	var stepsJSON, filesJSON, checksJSON, devLogJSON, prChecksJSON string
	var approvedSHA, mainSHA, sendBack, reject sql.NullString
	var createdAt, updatedAt string

	if err := row.Scan(
		&c.ID, &c.TaskID, &c.Branch, &c.HeadSHA, &stepsJSON, &c.DevURL, &c.DevPID,
		&c.Status, &approvedSHA, &mainSHA, &sendBack, &reject,
		&filesJSON, &checksJSON,
		&c.DevState, &devLogJSON,
		&c.BranchDeleted, &c.BranchDeleteError,
		&c.MergeMode, &c.PRNumber, &c.PRURL,
		&prChecksJSON, &c.PRChecksSHA, &c.PRChecksAt,
		&c.PRMergeError, &c.CIFixRequested,
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
	if err := json.Unmarshal([]byte(prChecksJSON), &c.PRChecks); err != nil || c.PRChecks == nil {
		c.PRChecks = []PRCheck{}
	}
	if c.PRNumber > 0 && c.PRChecksSHA != "" {
		// Pushed but not polled yet: CI is starting.
		c.PRChecksSummary = ChecksRunning
		if c.PRChecksAt != "" {
			c.PRChecksSummary = SummarizeChecks(c.PRChecks)
		}
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
			if errors.Is(err, ErrDevSetupCanceled) {
				_ = SetDevState(db, card.ID, "", "Dev server setup canceled")
				if progress != nil {
					progress(DevProgress{TaskID: card.TaskID, Step: "canceled", Message: err.Error(), OK: false})
				}
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

// ensureDevWorktree creates a fresh detached worktree at wtPath for headSHA,
// first deleting whatever tree or stale git registration is already there
// (e.g. a directory left by a daemon restart while git still had it
// registered). Only paths whose base name starts with "devserver-" are
// managed; anything else is rejected.
func ensureDevWorktree(ctx context.Context, repoPath, wtPath, headSHA string) error {
	if err := deleteDevWorktree(ctx, repoPath, wtPath); err != nil {
		return err
	}
	_, err := gitOutput(ctx, repoPath, "worktree", "add", "--detach", wtPath, headSHA)
	return err
}

// deleteDevWorktree deletes the devserver worktree at wtPath and its git
// registration.
//
// The tree is deleted with os.RemoveAll, not `git worktree remove`: a dev
// worktree holding node_modules can take longer to delete than git's quick
// timeout (STA-710), and the directory is StayPoint's own. Git then only has
// to prune the registration, which is quick.
func deleteDevWorktree(ctx context.Context, repoPath, wtPath string) error {
	if !strings.HasPrefix(filepath.Base(wtPath), "devserver-") {
		return fmt.Errorf("deleteDevWorktree: refusing non-devserver path %q", wtPath)
	}
	// Prune before touching the directory. It is a quick git call, so a repo
	// git cannot read (STA-685) fails here within the quick timeout instead of
	// blocking the unbounded RemoveAll below on the same folder. Its other
	// errors are not fatal: the add reports anything that still matters.
	if _, err := gitOutput(ctx, repoPath, "worktree", "prune"); gitexec.IsTimeout(err) {
		return err
	}
	if err := os.RemoveAll(wtPath); err != nil {
		return fmt.Errorf("remove dev worktree %s: %w", wtPath, err)
	}
	// Drop the registration of the tree just deleted, and any other orphan.
	if _, err := gitOutput(ctx, repoPath, "worktree", "prune"); gitexec.IsTimeout(err) {
		return err
	}
	return nil
}

// startDevServerSync performs the full setup sequence: worktree, Supabase env,
// setup steps, then launches the dev process. Emits progress via emit.
//
// The setup is registered with devServerManager so CleanupMergedBranch can
// cancel it (STA-649): after every step a cancelled setup removes the
// devserver-<id> worktree it created and returns ErrDevSetupCanceled, and the
// server process is only recorded if the setup is still live.
func startDevServerSync(db *sql.DB, card *Card, cfg *ProjectDevConfig, repoPath string, emit func(step, msg string, ok bool)) error {
	if err := workspace.ValidateTaskID(card.TaskID); err != nil {
		return err
	}
	setup := devServerManager.beginSetup(card.TaskID)
	defer devServerManager.endSetup(card.TaskID, setup)
	ctx := setup.ctx

	// Kill any stale server (and clean up its worktree) for this task first.
	devServerManager.kill(card.TaskID)

	// Create a temporary detached worktree at the pinned commit SHA.
	wtPath := filepath.Join(repoPath, ".worktrees", "devserver-"+card.TaskID)
	// canceled removes the worktree once the setup has been cancelled.
	canceled := func() bool {
		if ctx.Err() == nil {
			return false
		}
		// removeDevWorktree also deletes a directory git never registered,
		// e.g. one left by a killed `worktree add`. No extra RemoveAll here:
		// it would run unbounded on a folder git just timed out reading.
		removeDevWorktree(repoPath, wtPath)
		return true
	}
	if canceled() {
		return ErrDevSetupCanceled
	}
	emit("worktree", "Creating dev worktree at "+card.HeadSHA[:min(len(card.HeadSHA), 12)]+"…", true)
	if err := ensureDevWorktree(ctx, repoPath, wtPath, card.HeadSHA); err != nil {
		if canceled() {
			return ErrDevSetupCanceled
		}
		return fmt.Errorf("create dev worktree at %s: %w", card.HeadSHA, err)
	}
	if canceled() {
		return ErrDevSetupCanceled
	}

	// Built-in Supabase dev env (before custom setup steps).
	if cfg.SupabaseEnabled {
		emit("supabase", "Setting up local Supabase dev env…", true)
		if err := StartSupabaseDevEnv(repoPath, wtPath, cfg.DevCommand, func(p SupabaseProgress) {
			emit(p.Step, p.Message, p.OK)
		}); err != nil {
			if canceled() {
				return ErrDevSetupCanceled
			}
			removeDevWorktree(repoPath, wtPath)
			return fmt.Errorf("supabase dev env: %w", err)
		}
		if canceled() {
			return ErrDevSetupCanceled
		}
	}

	// Custom setup steps (now run via /bin/sh -c so pipes/quotes/&& work).
	for _, step := range cfg.SetupSteps {
		emit("setup", "Running: "+step, true)
		if err := runShellStep(ctx, step, wtPath); err != nil {
			if canceled() {
				return ErrDevSetupCanceled
			}
			removeDevWorktree(repoPath, wtPath)
			return fmt.Errorf("setup step %q failed: %w", step, err)
		}
		if canceled() {
			return ErrDevSetupCanceled
		}
	}

	// Launch the dev server process.
	emit("server", "Starting dev server…", true)
	cmd := exec.Command("/bin/sh", "-c", cfg.DevCommand) //nolint:gosec
	cmd.Dir = wtPath
	startDevInOwnGroup(cmd)
	// Strip inherited VITE_* and SUPABASE_* vars: process env beats every .env
	// file in Vite, so an inherited VITE_SUPABASE_URL would route to production
	// even when .env.local points at localhost.
	cmd.Env = append(StripViteSupabaseEnv(os.Environ()),
		"FORCE_COLOR=1",
		"PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:"+os.Getenv("PATH"),
	)

	if canceled() {
		return ErrDevSetupCanceled
	}
	if err := cmd.Start(); err != nil {
		removeDevWorktree(repoPath, wtPath)
		return fmt.Errorf("start dev server: %w", err)
	}

	if !devServerManager.storeIfActive(card.TaskID, setup, cmd.Process, repoPath, wtPath) {
		killDevProcessGroup(cmd.Process)
		_ = cmd.Wait()
		removeDevWorktree(repoPath, wtPath)
		return ErrDevSetupCanceled
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
	// A setup still running from a Restart would otherwise go on to create
	// devserver-<id> and start a server for a card that is now closed.
	devServerManager.cancelSetup(card.TaskID, devSetupWait)

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
	if repoPath == "" {
		// Files changed cannot be measured against a verified base.
		return nil, fmt.Errorf("task %s has no repo path: %w", taskID, workspace.ErrNoTaskBase)
	}

	headSHA, err := CurrentBranchHEAD(ctx, repoPath, branch)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve branch HEAD for %q: %w", branch, err)
	}

	prev := previousOpenPR(db, taskID)
	card, err := CreateCard(db, taskID, branch, headSHA, testSteps, devURL, repoPath, checkRuns)
	if err != nil {
		return nil, err
	}

	// STA-717: the Board sent CI failures back with "Send failures to agent",
	// which asks for exactly this: push the fix to the PR and re-run checks.
	// Fast-forward only; anything else waits for the Board's Approve.
	if prev.ciFixReq && prev.mode == MergeModePRMerge && prev.number > 0 {
		cfg, _ := GetProjectDevConfig(db, repoPath)
		if auth, aErr := ResolveGHAuth(cfg, repoPath); aErr != nil {
			_ = SetPRMergeError(db, card.ID, "re-push to PR failed: "+aErr.Error())
		} else if pr, pErr := OpenOrUpdatePR(ctx, auth, card, false); pErr != nil {
			_ = SetPRMergeError(db, card.ID, "re-push to PR failed: "+pErr.Error())
		} else {
			_ = SetPROpened(db, card.ID, MergeModePRMerge, pr, card.HeadSHA)
		}
		if refreshed, gErr := GetCard(db, taskID); gErr == nil {
			card = refreshed
		}
	}

	// Auto-start dev server only from an explicitly human-saved project config.
	// Auto-detection of Supabase projects is handled in the board-facing StartDev
	// HTTP endpoint (after the board user explicitly clicks Start), not here —
	// BuildAndStartCard is agent-reachable and must not silently run dev commands.
	// A live_credentials project is never auto-started: its previews hit
	// production, so only a confirmed Board start-dev may run it (STA-727),
	// and neither is a repo path that cannot be told apart from one (STA-767).
	cfg, gated, err := LiveGateConfig(db, repoPath)
	if err == nil && !gated && cfg.DevCommand != "" {
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
// that are already gone count as deleted.
//
// Nothing is removed until every copy of the branch is known to be contained
// in mainSHA (STA-649): the agent worktree's HEAD, the local branch, and the
// remote branch as it is right now. If any holds work main does not, the
// whole cleanup is refused, so commits made after the review and uncommitted
// edits in an unmerged agent worktree are never lost. The remote delete is
// leased to the tip that was checked, so a push that lands in between makes
// the delete fail instead of discarding it.
func CleanupMergedBranch(ctx context.Context, repoDir string, card *Card, mainSHA string) error {
	branch := card.Branch
	if err := guardDeletableBranch(ctx, repoDir, branch); err != nil {
		return err
	}
	if mainSHA == "" {
		return errors.New("cleanup: merged main SHA unknown")
	}
	// The task ID becomes a path segment under .worktrees; an empty or
	// path-like value would point removal at the wrong directory.
	if err := workspace.ValidateTaskID(card.TaskID); err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}
	// git refuses to delete a branch that is checked out, so never touch the
	// branch repoDir itself is on (the merge leaves it on main).
	if cur, err := gitOutput(ctx, repoDir, "symbolic-ref", "--short", "-q", "HEAD"); err == nil && cur == branch {
		return fmt.Errorf("%w: %q is checked out in %s", ErrProtectedBranch, branch, repoDir)
	}

	// A dev-server setup still running from before the approve would
	// recreate devserver-<id> after it is removed below; stop it first.
	devServerManager.cancelSetup(card.TaskID, devSetupWait)
	devServerManager.kill(card.TaskID)

	wtDir := filepath.Join(repoDir, ".worktrees")
	agentWT := filepath.Join(wtDir, card.TaskID)
	devWT := filepath.Join(wtDir, "devserver-"+card.TaskID)

	// ── Checks: nothing below this block is destructive until all pass. ──

	if _, err := os.Stat(agentWT); err == nil {
		// Fail closed: a worktree whose HEAD cannot be read may hold anything.
		head, err := gitOutput(ctx, agentWT, "rev-parse", "--verify", "-q", "HEAD")
		if err != nil || head == "" {
			return fmt.Errorf("agent worktree %s: cannot read HEAD (%v); nothing deleted", agentWT, err)
		}
		if verifyAncestor(ctx, repoDir, head, mainSHA) != nil {
			return fmt.Errorf("agent worktree %s is at %s, which is not in main; nothing deleted", agentWT, head)
		}
	}

	localTip, _ := gitOutput(ctx, repoDir, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	if localTip != "" && verifyAncestor(ctx, repoDir, localTip, mainSHA) != nil {
		return fmt.Errorf("local branch %s has commits not in main (tip %s); nothing deleted", branch, localTip)
	}

	hasOrigin := false
	remoteTip := ""
	if _, err := gitOutput(ctx, repoDir, "remote", "get-url", "origin"); err == nil {
		hasOrigin = true
		if _, err := gitOutput(ctx, repoDir, "fetch", "--prune", "origin"); err != nil {
			return fmt.Errorf("fetch --prune origin: %w; nothing deleted", err)
		}
		// Ask the remote directly rather than trusting the tracking ref, which
		// a narrow fetch refspec may not maintain.
		out, err := gitOutput(ctx, repoDir, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
		if err != nil {
			return fmt.Errorf("read remote branch %s: %w; nothing deleted", branch, err)
		}
		if fields := strings.Fields(out); len(fields) > 0 {
			remoteTip = fields[0]
		}
		// A tip missing locally cannot be part of main's history, so the
		// ancestor check fails closed for it too.
		if remoteTip != "" && verifyAncestor(ctx, repoDir, remoteTip, mainSHA) != nil {
			return fmt.Errorf("remote branch %s has commits not in main (tip %s); nothing deleted", branch, remoteTip)
		}
	}

	// ── Removal. ──

	// Worktrees first: they hold the branch checked out.
	for _, wt := range []string{devWT, agentWT} {
		_, _ = gitOutput(ctx, repoDir, "worktree", "remove", "--force", wt)
		if err := os.RemoveAll(wt); err != nil {
			return fmt.Errorf("remove worktree %s: %w", wt, err)
		}
	}
	_, _ = gitOutput(ctx, repoDir, "worktree", "prune")

	var errs []error

	if localTip != "" {
		// update-ref skips branch -D's "checked out in a worktree" refusal,
		// so check that ourselves: the task's own worktrees are gone by now,
		// any other checkout of the branch must keep it.
		if wt, err := worktreeWithBranch(ctx, repoDir, branch); err != nil {
			errs = append(errs, fmt.Errorf("delete local branch: %w", err))
		} else if wt != "" {
			errs = append(errs, fmt.Errorf("local branch %s is checked out in %s; not deleted", branch, wt))
		} else if _, err := gitOutput(ctx, repoDir, "update-ref", "-d", "refs/heads/"+branch, localTip); err != nil {
			// Like the remote lease: only delete the tip that was checked.
			errs = append(errs, fmt.Errorf("delete local branch: %w", err))
		} else {
			// branch -D would have dropped the branch's config too.
			_, _ = gitOutput(ctx, repoDir, "config", "--remove-section", "branch."+branch)
		}
	}

	if hasOrigin && remoteTip != "" {
		if err := deleteRemoteBranchLeased(ctx, repoDir, branch, remoteTip); err != nil {
			errs = append(errs, fmt.Errorf("delete remote branch: %w", err))
		}
	}

	return errors.Join(errs...)
}

// worktreeWithBranch returns the path of a worktree that has branch checked
// out, or is rebasing or bisecting it, or "" when none does. These are the
// cases where branch -D refuses; a rebase or bisect detaches HEAD, so the
// worktree list alone does not show them.
func worktreeWithBranch(ctx context.Context, repoDir, branch string) (string, error) {
	ref := "refs/heads/" + branch
	// -z: a worktree path may itself contain a newline.
	out, err := gitOutput(ctx, repoDir, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return "", err
	}
	wt := ""
	for _, field := range strings.Split(out, "\x00") {
		if p, ok := strings.CutPrefix(field, "worktree "); ok {
			wt = p
		} else if field == "branch "+ref {
			return wt, nil
		}
	}

	commonDir, err := gitOutput(ctx, repoDir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	gitDirs := []string{commonDir}
	linked, _ := filepath.Glob(filepath.Join(commonDir, "worktrees", "*"))
	gitDirs = append(gitDirs, linked...)
	for _, gd := range gitDirs {
		if !worktreeGitDirHolds(gd, ref) {
			continue
		}
		if gd == commonDir {
			return filepath.Dir(commonDir) + " (rebase or bisect in progress)", nil
		}
		// gitdir holds the path of the linked worktree's .git file.
		if b, err := os.ReadFile(filepath.Join(gd, "gitdir")); err == nil {
			return filepath.Dir(strings.TrimSpace(string(b))) + " (rebase or bisect in progress)", nil
		}
		return gd + " (rebase or bisect in progress)", nil
	}
	return "", nil
}

// worktreeGitDirHolds reports whether the worktree whose git dir is gd is
// rebasing or bisecting ref, mirroring git's is_worktree_being_rebased and
// is_worktree_being_bisected.
func worktreeGitDirHolds(gd, ref string) bool {
	for _, f := range []string{"rebase-merge/head-name", "rebase-apply/head-name"} {
		if b, err := os.ReadFile(filepath.Join(gd, f)); err == nil && strings.TrimSpace(string(b)) == ref {
			return true
		}
	}
	// BISECT_START holds the short name of the branch bisect started from.
	b, err := os.ReadFile(filepath.Join(gd, "BISECT_START"))
	return err == nil && strings.TrimSpace(string(b)) == strings.TrimPrefix(ref, "refs/heads/")
}

// deleteRemoteBranchLeased deletes refs/heads/<branch> on origin only while
// it still points at tip.
func deleteRemoteBranchLeased(ctx context.Context, repoDir, branch, tip string) error {
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

// gitOutput runs git in dir and returns trimmed stdout. Each call is bounded by
// gitexec.TimeoutFor: quick for reads, longer for fetch, push, merge and
// worktree add/remove (STA-710).
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitexec.TimeoutFor(args...))
	defer cancel()
	cmd := gitexec.Command(ctx, args...) //nolint:gosec
	cmd.Dir = dir
	if env := gitEnvFrom(ctx); env != nil {
		cmd.Env = env
	}
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
func runShellStep(ctx context.Context, step, workDir string) error {
	if strings.TrimSpace(step) == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", step) //nolint:gosec
	killStepGroupOnCancel(cmd)
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
	// MergeMode is how Approve lands the branch (STA-717): "direct" merges
	// locally and pushes main, "open_pr" only opens a GitHub PR, "pr_merge"
	// opens a PR and merges it once CI is green. Empty means unset; see
	// EffectiveMergeMode for the default.
	MergeMode string `json:"merge_mode"`
	// GHConfigDir is the GH_CONFIG_DIR used for this repo's gh and git push
	// calls. Work repos must set it in the PR modes so the personal gh login
	// is never used on them.
	GHConfigDir string `json:"gh_config_dir"`
	// LiveCredentials marks a project whose previews run against production
	// credentials (STA-727). Set only through the Board-gated PUT. Starting
	// its dev server needs the Board passkey plus an explicit confirm, and
	// BuildAndStartCard never auto-starts it.
	LiveCredentials bool `json:"live_credentials"`
}

// GetProjectDevConfig loads the dev config for a repo path, or returns defaults.
//
// Rows are matched by directory, not by string (STA-767): a path that reaches
// a configured repo through a symlink, a trailing "/", "/./" or a different
// case on a case-insensitive volume loads that repo's row. When several rows
// name the same directory, a live_credentials row wins, so an alias row can
// never hide the live flag. The returned RepoPath is the matched row's key, so
// saving the config back updates that row instead of adding an alias row.
func GetProjectDevConfig(db *sql.DB, repoPath string) (*ProjectDevConfig, error) {
	cfg, _, err := lookupDevConfig(db, repoPath)
	return cfg, err
}

// Reasons LiveGate reports for gating a dev server start (STA-799).
const (
	// LiveGateLiveCredentials: the project is flagged live_credentials.
	LiveGateLiveCredentials = "live_credentials"
	// LiveGateUnverifiedPath: repoPath cannot be ruled out as an alias of a
	// live_credentials project.
	LiveGateUnverifiedPath = "unverified_path"
)

// LiveGate loads repoPath's dev config for starting a dev server and reports
// why the start needs the Board gate, or "" when it does not:
// LiveGateLiveCredentials when the project is live_credentials, else
// LiveGateUnverifiedPath when repoPath cannot be compared with a live project
// (repoPath cannot be stat'ed, or the live project's path fails to stat for a
// reason other than not existing).
func LiveGate(db *sql.DB, repoPath string) (cfg *ProjectDevConfig, reason string, err error) {
	cfg, unverified, err := lookupDevConfig(db, repoPath)
	if err != nil {
		return nil, "", err
	}
	switch {
	case cfg.LiveCredentials:
		reason = LiveGateLiveCredentials
	case unverified:
		reason = LiveGateUnverifiedPath
	}
	return cfg, reason, nil
}

// LiveGateConfig is LiveGate reduced to whether the start is gated. It fails
// closed: an error reports gated.
func LiveGateConfig(db *sql.DB, repoPath string) (cfg *ProjectDevConfig, gated bool, err error) {
	cfg, reason, err := LiveGate(db, repoPath)
	if err != nil {
		return nil, true, err
	}
	return cfg, reason != "", nil
}

const devConfigColumns = `repo_path, dev_command, dev_url, setup_steps_json,
	COALESCE(migration_globs_json,'[]'), COALESCE(sql_editor_url,''),
	COALESCE(supabase_enabled,0), COALESCE(supabase_keep_up,0),
	COALESCE(merge_mode,''), COALESCE(gh_config_dir,''), COALESCE(live_credentials,0)`

func scanDevConfig(rows *sql.Rows) (*ProjectDevConfig, error) {
	var c ProjectDevConfig
	var stepsJSON, migGlobsJSON string
	var supEnabled, supKeepUp, live int
	if err := rows.Scan(&c.RepoPath, &c.DevCommand, &c.DevURL, &stepsJSON, &migGlobsJSON, &c.SQLEditorURL, &supEnabled, &supKeepUp, &c.MergeMode, &c.GHConfigDir, &live); err != nil {
		return nil, err
	}
	c.SupabaseEnabled = supEnabled != 0
	c.SupabaseKeepUp = supKeepUp != 0
	c.LiveCredentials = live != 0
	if err := json.Unmarshal([]byte(stepsJSON), &c.SetupSteps); err != nil {
		c.SetupSteps = []string{}
	}
	if err := json.Unmarshal([]byte(migGlobsJSON), &c.MigrationGlobs); err != nil {
		c.MigrationGlobs = []string{}
	}
	return &c, nil
}

// lookupDevConfig returns the row for repoPath's directory (see
// GetProjectDevConfig). unverified is true when that directory could not be
// compared with every live row; LiveGateConfig gates on it.
func lookupDevConfig(db *sql.DB, repoPath string) (cfg *ProjectDevConfig, unverified bool, err error) {
	rows, err := db.Query(`SELECT ` + devConfigColumns + ` FROM project_dev_configs`)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	want := filepath.Clean(repoPath)
	wantInfo, _ := os.Stat(repoPath)

	var exact, alias, live *ProjectDevConfig
	for rows.Next() {
		c, err := scanDevConfig(rows)
		if err != nil {
			return nil, false, err
		}
		same := c.RepoPath == repoPath || filepath.Clean(c.RepoPath) == want
		if !same && wantInfo == nil && c.LiveCredentials {
			// repoPath cannot be stat'ed, so it cannot be ruled out as an
			// alias of this live repo.
			unverified = true
		}
		if !same && wantInfo != nil {
			info, err := os.Stat(c.RepoPath)
			switch {
			case err == nil:
				same = os.SameFile(wantInfo, info)
			case c.LiveCredentials && !errors.Is(err, os.ErrNotExist):
				// A live repo we cannot stat could be repoPath itself.
				unverified = true
			}
		}
		if !same {
			continue
		}
		switch {
		case c.RepoPath == repoPath:
			exact = c
		case alias == nil:
			alias = c
		}
		if c.LiveCredentials && (live == nil || c.RepoPath == repoPath) {
			live = c
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	for _, c := range []*ProjectDevConfig{live, exact, alias} {
		if c != nil {
			return c, unverified, nil
		}
	}
	return &ProjectDevConfig{RepoPath: repoPath}, unverified, nil
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
	liveCredentials := 0
	if cfg.LiveCredentials {
		liveCredentials = 1
	}
	_, err = exec.Exec(`
		INSERT INTO project_dev_configs
			(repo_path, dev_command, dev_url, setup_steps_json, migration_globs_json,
			 sql_editor_url, supabase_enabled, supabase_keep_up, merge_mode, gh_config_dir, live_credentials, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		ON CONFLICT(repo_path) DO UPDATE SET
			dev_command          = excluded.dev_command,
			dev_url              = excluded.dev_url,
			setup_steps_json     = excluded.setup_steps_json,
			migration_globs_json = excluded.migration_globs_json,
			sql_editor_url       = excluded.sql_editor_url,
			supabase_enabled     = excluded.supabase_enabled,
			supabase_keep_up     = excluded.supabase_keep_up,
			merge_mode           = excluded.merge_mode,
			gh_config_dir        = excluded.gh_config_dir,
			live_credentials     = excluded.live_credentials,
			updated_at           = excluded.updated_at`,
		cfg.RepoPath, cfg.DevCommand, cfg.DevURL,
		string(stepsJSON), string(migGlobsJSON), cfg.SQLEditorURL,
		supabaseEnabled, supabaseKeepUp, cfg.MergeMode, cfg.GHConfigDir, liveCredentials,
	)
	return err
}

// ListProjectDevConfigs returns all project dev configs.
func ListProjectDevConfigs(db *sql.DB) ([]*ProjectDevConfig, error) {
	rows, err := db.Query(`SELECT ` + devConfigColumns + ` FROM project_dev_configs ORDER BY repo_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ProjectDevConfig
	for rows.Next() {
		c, err := scanDevConfig(rows)
		if err != nil {
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

