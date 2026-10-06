package shipreview

// STA-734: the merge test gate. Before a card can merge to main, the change
// is checked for CI, test changes and (when CI uploads one) coverage of the
// changed lines; testgate holds the rules, this file reads git, gh and the DB.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/testgate"
)

// ChangeForGate returns the files the card's head changes against its
// merge-base with main (or master), and the added/modified lines per file.
func ChangeForGate(ctx context.Context, repoDir, headSHA string) ([]string, map[string][]int, error) {
	if repoDir == "" || headSHA == "" {
		return nil, nil, errors.New("card has no repo or head")
	}
	// Report main's error: master is only a fallback, and its "unknown
	// revision" would hide why main failed (e.g. a git timeout).
	base, mainErr := gitOutput(ctx, repoDir, "merge-base", "main", headSHA)
	if mainErr != nil || base == "" {
		if out, err := gitOutput(ctx, repoDir, "merge-base", "master", headSHA); err == nil && out != "" {
			base = out
		} else {
			if mainErr == nil {
				mainErr = errors.New("empty merge-base")
			}
			return nil, nil, fmt.Errorf("no merge-base with main: %w", mainErr)
		}
	}
	names, err := gitOutput(ctx, repoDir, "-c", "core.quotePath=false", "diff", "--name-only", "--no-renames", base, headSHA)
	if err != nil {
		return nil, nil, err
	}
	var files []string
	for _, f := range strings.Split(names, "\n") {
		if f = strings.TrimSpace(f); f != "" {
			files = append(files, f)
		}
	}
	diff, err := gitOutput(ctx, repoDir, "-c", "core.quotePath=false", "diff", "-U0", "--no-color", "--no-ext-diff", "--no-renames", base, headSHA)
	if err != nil {
		return nil, nil, err
	}
	return files, testgate.ParseChangedLines(diff), nil
}

// WorkflowsAt returns the GitHub Actions workflow files at sha, by path.
func WorkflowsAt(ctx context.Context, repoDir, sha string) (map[string]string, error) {
	out, err := gitOutput(ctx, repoDir, "-c", "core.quotePath=false", "ls-tree", "--name-only", sha, "--", ".github/workflows/")
	if err != nil {
		return nil, err
	}
	wf := map[string]string{}
	for _, p := range strings.Split(out, "\n") {
		p = strings.TrimSpace(p)
		if !strings.HasSuffix(p, ".yml") && !strings.HasSuffix(p, ".yaml") {
			continue
		}
		body, err := gitOutput(ctx, repoDir, "show", sha+":"+p)
		if err != nil {
			return nil, err
		}
		wf[p] = body
	}
	return wf, nil
}

// ── CI coverage artifacts ───────────────────────────────────────────────────

var (
	// coverArtifactRe picks the CI artifacts worth downloading.
	coverArtifactRe = regexp.MustCompile(`(?i)cover|lcov`)
	// coverFileRe picks the files inside them worth parsing.
	coverFileRe = regexp.MustCompile(`(?i)(cover|lcov|\.out$|\.info$)`)
)

const (
	maxArtifactBytes = 200 << 20
	maxCoverFile     = 50 << 20
)

type ghRun struct {
	ID         int64  `json:"databaseId"`
	Workflow   string `json:"workflowName"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type ghArtifact struct {
	Name    string `json:"name"`
	Size    int64  `json:"size_in_bytes"`
	Expired bool   `json:"expired"`
}

// CICoverage looks for a coverage report among the CI artifacts uploaded for
// headSHA (Go -coverprofile, coverage.xml, lcov.info) and checks the changed
// source lines against it. With no report it says why instead of guessing.
func (a GHAuth) CICoverage(ctx context.Context, headSHA string, changed map[string][]int, src func(string) []byte) testgate.CoverageResult {
	out, err := a.gh(ctx, "run", "list", "--commit", headSHA, "--json", "databaseId,workflowName,status,conclusion", "--limit", "30")
	if err != nil {
		return testgate.CoverageResult{Note: "could not list CI runs for this commit (" + firstLine(err.Error()) + ")"}
	}
	var runs []ghRun
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &runs); err != nil {
		return testgate.CoverageResult{Note: "could not read CI runs for this commit (" + err.Error() + ")"}
	}
	if len(runs) == 0 {
		return testgate.CoverageResult{Note: "no CI run for this commit"}
	}
	allDone := true
	for _, r := range runs {
		if r.Status != "completed" {
			allDone = false
		}
	}
	tmp, err := os.MkdirTemp("", "staypoint-coverage-")
	if err != nil {
		return testgate.CoverageResult{Note: "could not make a temp dir for coverage (" + err.Error() + ")"}
	}
	defer os.RemoveAll(tmp)

	var covs []*testgate.Coverage
	var used, seenArtifacts []string
	for _, r := range runs {
		aout, err := a.gh(ctx, "api", "repos/{owner}/{repo}/actions/runs/"+strconv.FormatInt(r.ID, 10)+"/artifacts")
		if err != nil {
			continue
		}
		var resp struct {
			Artifacts []ghArtifact `json:"artifacts"`
		}
		if json.Unmarshal([]byte(aout), &resp) != nil {
			continue
		}
		for i, art := range resp.Artifacts {
			if art.Expired || art.Size > maxArtifactBytes || !coverArtifactRe.MatchString(art.Name) {
				continue
			}
			seenArtifacts = append(seenArtifacts, art.Name)
			dir := filepath.Join(tmp, fmt.Sprintf("%d-%d", r.ID, i))
			if _, err := a.gh(ctx, "run", "download", strconv.FormatInt(r.ID, 10), "-n", art.Name, "-D", dir); err != nil {
				continue
			}
			if found := parseCoverageDir(dir); len(found) > 0 {
				covs = append(covs, found...)
				used = append(used, art.Name)
			}
		}
	}
	switch {
	case len(covs) > 0:
		cov := testgate.MergeCoverage(covs...)
		return testgate.CoverageResult{Available: true, Final: true,
			Source:    "CI artifact " + strings.Join(used, ", "),
			Uncovered: testgate.FindUncovered(changed, cov, src)}
	case len(seenArtifacts) > 0:
		return testgate.CoverageResult{Final: allDone,
			Note: "CI artifact " + strings.Join(seenArtifacts, ", ") + " has no Go coverprofile, coverage.xml or lcov.info"}
	case !allDone:
		return testgate.CoverageResult{Note: "CI is still running and has uploaded no coverage artifact yet"}
	}
	return testgate.CoverageResult{Final: true, Note: "CI uploaded no coverage artifact (looked for artifacts named like coverage or lcov)"}
}

// parseCoverageDir parses every coverage report in a downloaded artifact.
func parseCoverageDir(dir string) []*testgate.Coverage {
	var out []*testgate.Coverage
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !coverFileRe.MatchString(d.Name()) {
			return nil
		}
		if fi, err := d.Info(); err != nil || fi.Size() > maxCoverFile {
			return nil
		}
		b, err := os.ReadFile(p) //nolint:gosec // inside our own temp dir
		if err != nil {
			return nil
		}
		if cov, err := testgate.ParseCoverage(d.Name(), b); err == nil {
			out = append(out, cov)
		}
		return nil
	})
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// ── evaluate + cache ────────────────────────────────────────────────────────

// coverageRecheck is how long a non-final coverage result is reused before
// CI artifacts are listed again.
var coverageRecheck = 2 * time.Minute

// GateDeps are the gate's outside inputs. Coverage is nil when GitHub can't
// be reached for this repo; the report then says there is no coverage data.
type GateDeps struct {
	ExtraExempt []string
	Coverage    func(ctx context.Context, headSHA string, changed map[string][]int, src func(string) []byte) testgate.CoverageResult
	// CoverageUnavailable explains a nil Coverage.
	CoverageUnavailable string
}

// EvaluateTestGate computes the card's "Test coverage" report for its head
// and stores it on the card. A coverage result already read for this head is
// reused (always once final, else for coverageRecheck). An error means the
// change itself could not be read; callers fail closed.
func EvaluateTestGate(ctx context.Context, db *sql.DB, card *Card, repoDir string, deps GateDeps) (*testgate.Report, error) {
	files, lines, err := ChangeForGate(ctx, repoDir, card.HeadSHA)
	if err != nil {
		return nil, err
	}
	in := testgate.Input{HeadSHA: card.HeadSHA, Files: files, ExtraExempt: deps.ExtraExempt, ChangedLines: lines}
	in.CI.Workflows, in.CI.WorkflowsErr = WorkflowsAt(ctx, repoDir, card.HeadSHA)
	if card.PRNumber > 0 {
		in.CI.PRNumber = card.PRNumber
		// Checks only describe this change once polled for this very head.
		if card.PRChecksSHA == card.HeadSHA && card.PRChecksAt != "" {
			in.CI.PRChecksRead = true
			in.CI.PRCheckCount = len(card.PRChecks)
		}
	}

	cls := testgate.Classify(files, deps.ExtraExempt)
	if len(cls.Sources) > 0 {
		prev, _ := GetTestGate(db, card.ID)
		switch {
		case prev != nil && prev.HeadSHA == card.HeadSHA && (prev.Coverage.Final || recent(prev.ComputedAt, coverageRecheck)):
			in.Coverage = prev.Coverage
		case deps.Coverage != nil:
			srcLines := map[string][]int{}
			for _, s := range cls.Sources {
				if l, ok := lines[s]; ok {
					srcLines[s] = l
				}
			}
			show := func(p string) []byte {
				out, err := gitOutput(ctx, repoDir, "show", card.HeadSHA+":"+p)
				if err != nil {
					return nil
				}
				return []byte(out)
			}
			in.Coverage = deps.Coverage(ctx, card.HeadSHA, srcLines, show)
		default:
			in.Coverage = testgate.CoverageResult{Note: deps.CoverageUnavailable}
		}
	}

	report := testgate.Evaluate(in)
	report.ComputedAt = time.Now().UTC().Format(time.RFC3339)
	if err := setTestGate(db, card.ID, report); err != nil {
		return nil, err
	}
	return report, nil
}

func recent(ts string, within time.Duration) bool {
	t, err := time.Parse(time.RFC3339, ts)
	return err == nil && time.Since(t) < within
}

// GetTestGate returns the last report stored on the card, or nil.
func GetTestGate(db *sql.DB, cardID string) (*testgate.Report, error) {
	var raw string
	if err := db.QueryRow(`SELECT COALESCE(test_gate_json,'') FROM ship_review_cards WHERE id = ?`, cardID).Scan(&raw); err != nil {
		return nil, err
	}
	if raw == "" {
		return nil, nil
	}
	var r testgate.Report
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func setTestGate(db *sql.DB, cardID string, r *testgate.Report) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE ship_review_cards SET test_gate_json = ? WHERE id = ?`, string(b), cardID)
	return err
}

// ── project exempt list ─────────────────────────────────────────────────────

// GetTestExemptGlobs returns the project's own exempt globs (on top of
// testgate.DefaultExemptGlobs).
func GetTestExemptGlobs(db *sql.DB, repoPath string) ([]string, error) {
	var raw string
	err := db.QueryRow(`SELECT COALESCE(test_exempt_globs_json,'[]') FROM project_dev_configs WHERE repo_path = ?`, repoPath).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []string{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return []string{}, nil
	}
	return out, nil
}

// SetTestExemptGlobsTx stores the project's exempt globs; the config row must
// already exist (UpsertProjectDevConfigTx runs first).
func SetTestExemptGlobsTx(tx *sql.Tx, repoPath string, globs []string) error {
	clean := []string{}
	for _, g := range globs {
		if g = strings.TrimSpace(g); g != "" {
			clean = append(clean, g)
		}
	}
	b, _ := json.Marshal(clean)
	_, err := tx.Exec(`UPDATE project_dev_configs SET test_exempt_globs_json = ? WHERE repo_path = ?`, string(b), repoPath)
	return err
}

// ── backlog task dedupe ─────────────────────────────────────────────────────

// ClaimTestGapTask reserves the dedupe key for a new "Add tests" task. If a
// task was already filed for the key it returns that task's id and false.
func ClaimTestGapTask(db *sql.DB, key, sourceTaskID, cardID string, prNumber int, headSHA string) (string, bool, error) {
	res, err := db.Exec(`
		INSERT OR IGNORE INTO ship_review_test_tasks (dedupe_key, task_id, source_task_id, card_id, pr_number, head_sha)
		VALUES (?, '', ?, ?, ?, ?)`, key, sourceTaskID, cardID, prNumber, headSHA)
	if err != nil {
		return "", false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return "", true, nil
	}
	var existing string
	err = db.QueryRow(`SELECT task_id FROM ship_review_test_tasks WHERE dedupe_key = ?`, key).Scan(&existing)
	return existing, false, err
}

// SetTestGapTaskID records the task filed under a claimed key.
func SetTestGapTaskID(db *sql.DB, key, taskID string) error {
	_, err := db.Exec(`UPDATE ship_review_test_tasks SET task_id = ? WHERE dedupe_key = ?`, taskID, key)
	return err
}

// ReleaseTestGapClaim drops a claim whose task could not be created.
func ReleaseTestGapClaim(db *sql.DB, key string) error {
	_, err := db.Exec(`DELETE FROM ship_review_test_tasks WHERE dedupe_key = ? AND task_id = ''`, key)
	return err
}

// TestGapTaskFor returns the "Add tests" task filed for a ship review task,
// or "" when none was.
func TestGapTaskFor(db *sql.DB, sourceTaskID string) (string, error) {
	var id string
	err := db.QueryRow(`
		SELECT task_id FROM ship_review_test_tasks
		WHERE source_task_id = ? AND task_id != ''
		ORDER BY created_at DESC LIMIT 1`, sourceTaskID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}
