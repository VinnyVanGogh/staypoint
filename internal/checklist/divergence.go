package checklist

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/paperclip"
)

// Item represents a database row in checklist_items.
type Item struct {
	ID          string `json:"id"`
	Sprint      string `json:"sprint"`
	Section     string `json:"section"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	HowToTest   string `json:"how_to_test,omitempty"`
	Contract    string `json:"contract,omitempty"`
	CommitHash  string `json:"commit_hash,omitempty"`
	Status      string `json:"status"`
	Notes       string `json:"notes"`
	Version     int    `json:"version"`
}

// ItemEvaluationResult describes the result of evaluating one item's contract.
type ItemEvaluationResult struct {
	ItemID         string `json:"item_id"`
	Sprint         string `json:"sprint"`
	Section        string `json:"section"`
	Title          string `json:"title"`
	Contract       string `json:"contract"`
	PreviousStatus string `json:"previous_status"`
	NewStatus      string `json:"new_status"`
	Diverged       bool   `json:"diverged"`
	Passed         bool   `json:"passed"`
	Reason         string `json:"reason,omitempty"`
	Details        string `json:"details,omitempty"`
	CommitSHA      string `json:"commit_sha,omitempty"`
	EvaluatedMs    int64  `json:"evaluated_ms"`  // wall time for this contract (ms)
	CacheSource    string `json:"cache_source"` // "cache" or "live"
}

// EvaluationSummary aggregates the evaluation outcomes for a sprint.
type EvaluationSummary struct {
	Sprint             string                     `json:"sprint"`
	Total              int                        `json:"total"`
	Passed             int                        `json:"passed"`
	Failed             int                        `json:"failed"`
	Divergences        int                        `json:"divergences"`
	CommitSHA          string                     `json:"commit_sha"`
	TotalMs            int64                      `json:"total_ms"`       // wall time for all contracts (ms)
	MaxContractMs      int64                      `json:"max_contract_ms"` // slowest single contract (ms)
	CacheHits          int                        `json:"cache_hits"`
	CacheMisses        int                        `json:"cache_misses"`
	CommitVerification *CommitVerificationSummary `json:"commit_verification,omitempty"`
	Results            []ItemEvaluationResult     `json:"results"`
}

// EvaluateOptions configures divergence evaluation.
type EvaluateOptions struct {
	RepoRoot           string
	RunningCommit      string
	BaseURL            string
	HTTPClient         *http.Client
	Downgrade          bool
	NotifyPaperclip    bool
	PaperclipClient    *paperclip.Client
	PaperclipCompanyID string
	BroadcastFn        func(event string, data any)
}

// GetGitCommitSHA returns the current HEAD commit SHA, or "unknown".
func GetGitCommitSHA(dir string) string {
	if dir == "" {
		dir = "."
	}
	cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

// EvaluateSprint evaluates all items in a sprint with defined contracts and detects regressions.
func EvaluateSprint(ctx context.Context, dbConn *sql.DB, sprint string, opts EvaluateOptions) (*EvaluationSummary, error) {
	if sprint == "" {
		sprint = "STA-168"
	}
	commitSHA := opts.RunningCommit
	if commitSHA == "" {
		commitSHA = GetGitCommitSHA(opts.RepoRoot)
	}

	// Ensure checklist_items has contract & commit_hash columns
	var contractCount int
	_ = dbConn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('checklist_items') WHERE name='contract'").Scan(&contractCount)
	if contractCount == 0 {
		_, _ = dbConn.ExecContext(ctx, "ALTER TABLE checklist_items ADD COLUMN contract TEXT;")
	}
	var commitColCount int
	_ = dbConn.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_table_info('checklist_items') WHERE name='commit_hash'").Scan(&commitColCount)
	if commitColCount == 0 {
		_, _ = dbConn.ExecContext(ctx, "ALTER TABLE checklist_items ADD COLUMN commit_hash TEXT;")
	}

	query := `SELECT id, sprint, section, title, description, how_to_test, contract, commit_hash, status, notes, version
		FROM checklist_items
		WHERE sprint = ? AND contract IS NOT NULL AND contract != ''
		ORDER BY section, rowid ASC`

	rows, err := dbConn.QueryContext(ctx, query, sprint)
	if err != nil {
		return nil, fmt.Errorf("failed to query checklist items with contracts: %w", err)
	}
	defer rows.Close()

	var items []Item
	for rows.Next() {
		var it Item
		var desc, howTo, contract, commitHash, notes sql.NullString
		if err := rows.Scan(&it.ID, &it.Sprint, &it.Section, &it.Title, &desc, &howTo, &contract, &commitHash, &it.Status, &notes, &it.Version); err != nil {
			return nil, fmt.Errorf("scan checklist item failed: %w", err)
		}
		it.Description = desc.String
		it.HowToTest = howTo.String
		it.Contract = contract.String
		it.CommitHash = commitHash.String
		it.Notes = notes.String
		items = append(items, it)
	}

	commitVer, _ := VerifyCommits(ctx, opts.RepoRoot, opts.RunningCommit, items)
	blockedSet := make(map[string]string)
	if commitVer != nil {
		for _, w := range commitVer.MissingCommits {
			if w.ItemID != "" {
				blockedSet[w.ItemID] = w.Reason
			}
		}
	}

	// Load the pre-evaluated contract cache written by reinstall-daemon.sh.
	// Under launchd, macOS blocks reads of ~/Documents until TCC is granted;
	// os.ReadFile/exec.Command hang rather than error, burning ~1.2 s per item.
	// The cache was evaluated in the user's shell (full TCC) at install time.
	cache := LoadContractCache(ContractCachePath())

	summary := &EvaluationSummary{
		Sprint:             sprint,
		Total:              len(items),
		CommitSHA:          commitSHA,
		CommitVerification: commitVer,
		Results:            make([]ItemEvaluationResult, 0, len(items)),
	}

	now := time.Now().UTC().Format(time.RFC3339)
	evalStart := time.Now()

	for _, it := range items {
		contract, err := ParseContract(it.Contract)
		if err != nil {
			summary.Failed++
			summary.Results = append(summary.Results, ItemEvaluationResult{
				ItemID:         it.ID,
				Sprint:         it.Sprint,
				Section:        it.Section,
				Title:          it.Title,
				Contract:       it.Contract,
				PreviousStatus: it.Status,
				NewStatus:      it.Status,
				Diverged:       false,
				Passed:         false,
				Reason:         fmt.Sprintf("malformed contract: %v", err),
				CommitSHA:      commitSHA,
			})
			continue
		}

		// Prefer the pre-evaluated cache for file_pattern and command contracts,
		// which require ~/Documents access the daemon may not have under launchd.
		// HTTP contracts are always re-evaluated live (no TCC issue, live state).
		var eval EvaluationResult
		var itemMs int64
		var cacheSource string
		if contract.Type != ContractTypeHTTP {
			if entry := cache.LookupEntry(it.ID, it.Contract); entry != nil {
				eval = EvaluationResult{
					Passed:  entry.Passed,
					Reason:  entry.Reason,
					Details: entry.Details,
				}
				itemMs = entry.EvaluatedMs
				cacheSource = "cache"
				summary.CacheHits++
			}
		}
		if cacheSource == "" {
			itemStart := time.Now()
			eval = EvaluateContract(ctx, *contract, opts.RepoRoot, opts.HTTPClient, opts.BaseURL)
			itemMs = time.Since(itemStart).Milliseconds()
			cacheSource = "live"
			summary.CacheMisses++
		}
		if itemMs > summary.MaxContractMs {
			summary.MaxContractMs = itemMs
		}
		if eval.Passed {
			if blockReason, isBlocked := blockedSet[it.ID]; isBlocked {
				eval.Passed = false
				eval.Reason = fmt.Sprintf("commit verification gate: %s", blockReason)
			}
		}
		itemRes := ItemEvaluationResult{
			ItemID:         it.ID,
			Sprint:         it.Sprint,
			Section:        it.Section,
			Title:          it.Title,
			Contract:       it.Contract,
			PreviousStatus: it.Status,
			NewStatus:      it.Status,
			Passed:         eval.Passed,
			Reason:         eval.Reason,
			Details:        eval.Details,
			CommitSHA:      commitSHA,
			EvaluatedMs:    itemMs,
			CacheSource:    cacheSource,
		}

		if eval.Passed {
			summary.Passed++
		} else {
			summary.Failed++
			// Check for divergence: previously verified item marked 'pass' now fails!
			if it.Status == "pass" {
				itemRes.Diverged = true
				summary.Divergences++

				if opts.Downgrade {
					itemRes.NewStatus = "fail"
					divergenceNote := fmt.Sprintf("[REGRESSION DIVERGENCE at %s (commit %s)] Contract failed: %s", now, commitSHA, eval.Reason)
					newNotes := divergenceNote
					if it.Notes != "" {
						newNotes = it.Notes + "\n\n" + divergenceNote
					}
					newVersion := it.Version + 1

					// Update database
					tx, err := dbConn.BeginTx(ctx, nil)
					if err == nil {
						_, _ = tx.ExecContext(ctx,
							`UPDATE checklist_items SET status='fail', notes=?, version=?, updated_at=? WHERE id=?`,
							newNotes, newVersion, now, it.ID)
						_, _ = tx.ExecContext(ctx,
							`INSERT INTO checklist_history (item_id, status, notes, changed_by, changed_at) VALUES (?,?,?,?,?)`,
							it.ID, "fail", divergenceNote, "divergence-detector", now)
						_ = tx.Commit()
					}

					// Notify via broadcast
					if opts.BroadcastFn != nil {
						opts.BroadcastFn("checklist:divergence", map[string]any{
							"item_id":   it.ID,
							"sprint":    it.Sprint,
							"title":     it.Title,
							"commit":    commitSHA,
							"reason":    eval.Reason,
							"downgrade": true,
						})
					}

					// Surface as Paperclip issue for triage
					if opts.NotifyPaperclip && opts.PaperclipClient != nil {
						issueReq := paperclip.CreateIssueRequest{
							Title: fmt.Sprintf("[REGRESSION] Checklist Divergence: %s (%s)", it.Title, it.Sprint),
							Description: fmt.Sprintf("## Automated Checklist Regression Alert\n\n"+
								"A previously verified checklist item has regressed in commit `%s`.\n\n"+
								"- **Sprint**: %s\n"+
								"- **Section**: %s\n"+
								"- **Title**: %s\n"+
								"- **Contract Type**: `%s`\n"+
								"- **Failure Reason**: %s\n\n"+
								"### Contract Specification\n```json\n%s\n```\n\n"+
								"The item was automatically downgraded from `pass` to `fail` in StayPoint.\n",
								commitSHA, it.Sprint, it.Section, it.Title, contract.Type, eval.Reason, it.Contract),
							Priority: "high",
							Labels:   []string{"regression", "checklist-divergence", strings.ToLower(it.Sprint)},
						}
						_, _ = opts.PaperclipClient.CreateIssue(ctx, opts.PaperclipCompanyID, issueReq)
					}
				}
			}
		}

		summary.Results = append(summary.Results, itemRes)
	}

	summary.TotalMs = time.Since(evalStart).Milliseconds()
	return summary, nil
}
