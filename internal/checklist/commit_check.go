package checklist

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
)

var (
	// DefaultSectionCommits maps known checklist section prefixes to their required delivery commits.
	DefaultSectionCommits = map[string]string{
		"01. Automated & System Verifications (CTO Pre-Verified)":        "461d1fe",
		"02. Table Sorting & Deep Content Search (STA-191)":              "4697e36",
		"03. Projects Page Filters & Grouping (STA-192)":                 "883f3fe",
		"04. Recent Tasks Hierarchy & Filters (STA-193)":                 "b670115",
		"05. Executive Overview & Boss Card Carousel (STA-194)":          "88c1394",
		"06. Settings Quota Telemetry & Fleet Modal (STA-195)":           "d8fe427",
		"07. Detail Panel & Chat Experience (STA-190, STA-186, STA-187)": "8f5951f",
		"08. Agents Page Modernization (STA-178, STA-179, STA-180)":      "2da10ec",
		"09. Cost & Accounting Visualizations (STA-175, STA-177)":        "cbd60ef",
		"10. Organization Rolling Quota & Lockout (STA-171, STA-185)":    "cbd60ef",
		"11. Checklist Tooling & Divergence Engine (STA-168, STA-170)":   "bcd122b",

		// STA-236 Sprint sections
		"01. Table Sorting & Multi-Dimension Filters (STA-208, STA-191)":                      "f64df3e",
		"02. Recent Tasks Hierarchy & Subtask Tree (STA-209, STA-193)":                        "461d1fe",
		"03. Claude Personal Quota & Multi-Seat Telemetry (STA-210, STA-185)":                 "20fc95b",
		"04. Hierarchical URL Routing & Deep Linking (STA-211, STA-187)":                      "ba6fb81",
		"05. Cascading Project Filter & Agents Modernization (STA-212, STA-179)":              "aa1c1ff",
		"06. Full-Page Task View Mode & Boss Card Cache (STA-213, STA-194)":                   "a4d3bc9",
		"07. Verify Contracts Visual Feedback & Gemini Telemetry (STA-214, STA-170, STA-175)": "fd25533",
		"08. Checklist Commit-Hash Gate & DoD Enforcement (STA-236)":                          "7254abb",
		"09. Quota Seat Stability & Project Card Click-Through (STA-283)":                     "ec80ab5",
	}

	commitHashRegex = regexp.MustCompile(`\b([0-9a-f]{7,40})\b`)
)

// CommitStatus describes verification results for a single commit hash.
type CommitStatus struct {
	Commit            string `json:"commit"`
	FullSHA           string `json:"full_sha,omitempty"`
	InMain            bool   `json:"in_main"`
	InBinary          bool   `json:"in_binary"`
	Valid             bool   `json:"valid"`
	MissingFromMain   bool   `json:"missing_from_main"`
	MissingFromBinary bool   `json:"missing_from_binary"`
	Reason            string `json:"reason,omitempty"`
}

// MissingCommitWarning contains structured metadata about a blocked item/section.
type MissingCommitWarning struct {
	Commit            string `json:"commit"`
	Section           string `json:"section"`
	ItemID            string `json:"item_id,omitempty"`
	ItemTitle         string `json:"item_title,omitempty"`
	MissingFromMain   bool   `json:"missing_from_main"`
	MissingFromBinary bool   `json:"missing_from_binary"`
	Reason            string `json:"reason"`
}

// CommitVerificationSummary aggregates commit-hash checks across all checklist items.
type CommitVerificationSummary struct {
	RunningCommit   string                 `json:"running_commit"`
	MainCommit      string                 `json:"main_commit"`
	Verified        bool                   `json:"verified"`
	TotalCommits    int                    `json:"total_commits"`
	ValidCommits    int                    `json:"valid_commits"`
	MissingCommits  []MissingCommitWarning `json:"missing_commits"`
	BlockedItemIDs  []string               `json:"blocked_item_ids"`
	BlockedSections []string               `json:"blocked_sections"`
	Warnings        []string               `json:"warnings"`
}

// ResolveSectionCommit returns the associated commit hash for a section, if known.
func ResolveSectionCommit(section string) string {
	if sha, ok := DefaultSectionCommits[section]; ok {
		return sha
	}
	for secPrefix, sha := range DefaultSectionCommits {
		if strings.HasPrefix(section, secPrefix) || strings.HasPrefix(secPrefix, section) {
			return sha
		}
	}
	return ""
}

// ExtractCommitFromContract extracts an optional commit_hash from raw contract JSON.
func ExtractCommitFromContract(rawContract string) string {
	rawContract = strings.TrimSpace(rawContract)
	if rawContract == "" {
		return ""
	}
	var data struct {
		CommitHash string `json:"commit_hash"`
	}
	if err := json.Unmarshal([]byte(rawContract), &data); err == nil && data.CommitHash != "" {
		return strings.TrimSpace(data.CommitHash)
	}
	return ""
}

// ResolveItemCommit determines the effective commit hash for a checklist item.
// Priority:
// 1. Explicit item.CommitHash
// 2. Contract JSON commit_hash field
// 3. Section-level commit mapping
func ResolveItemCommit(item Item) string {
	if h := strings.TrimSpace(item.CommitHash); h != "" {
		return h
	}
	if h := ExtractCommitFromContract(item.Contract); h != "" {
		return h
	}
	return ResolveSectionCommit(item.Section)
}

// resolveRepoRoot ensures the repository root is properly determined even when running from launchd.
func resolveRepoRoot(repoRoot string) string {
	if repoRoot != "" && repoRoot != "." {
		if _, err := os.Stat(filepath.Join(repoRoot, ".git")); err == nil {
			return repoRoot
		}
	}
	if env := os.Getenv("STAYPOINT_REPO_ROOT"); env != "" {
		if _, err := os.Stat(filepath.Join(env, ".git")); err == nil {
			return env
		}
	}
	if _, err := os.Stat(".git"); err == nil {
		return "."
	}
	if _, err := os.Stat("/Users/vincevasile/Documents/dev/agent-mesh/.git"); err == nil {
		return "/Users/vincevasile/Documents/dev/agent-mesh"
	}
	if repoRoot != "" {
		return repoRoot
	}
	return "."
}

// GetMainCommitSHA returns the HEAD commit of main/origin/main.
func GetMainCommitSHA(repoRoot string) string {
	repoRoot = resolveRepoRoot(repoRoot)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, ref := range []string{"main", "origin/main", "HEAD"} {
		cmd := gitexec.Command(ctx, "rev-parse", "--short", ref)
		cmd.Dir = repoRoot
		out, err := cmd.Output()
		if err == nil {
			trimmed := strings.TrimSpace(string(out))
			if trimmed != "" {
				return trimmed
			}
		}
	}
	return "unknown"
}

// VerifyCommits verifies all items against main and the running binary commit.
func VerifyCommits(ctx context.Context, repoRoot string, binaryCommit string, items []Item) (*CommitVerificationSummary, error) {
	repoRoot = resolveRepoRoot(repoRoot)
	binaryCommit = strings.TrimSpace(binaryCommit)

	// Prefer build-time data: it needs no repo access and answers instantly.
	manifest := manifestFor(binaryCommit)
	repoErr := error(nil)
	mainSHA := "unknown"
	if manifest != nil {
		if manifest.MainSHA != "" {
			mainSHA = manifest.MainSHA
		}
	} else if repoErr = probeRepo(ctx, repoRoot); repoErr == nil {
		mainSHA = GetMainCommitSHA(repoRoot)
	}

	summary := &CommitVerificationSummary{
		RunningCommit:   binaryCommit,
		MainCommit:      mainSHA,
		Verified:        true,
		MissingCommits:  []MissingCommitWarning{},
		BlockedItemIDs:  []string{},
		BlockedSections: []string{},
		Warnings:        []string{},
	}

	blockedSectionSet := make(map[string]bool)
	blockedItemSet := make(map[string]bool)
	commitCache := make(map[string]CommitStatus)

	for _, it := range items {
		targetCommit := ResolveItemCommit(it)
		if targetCommit == "" {
			continue
		}

		status, cached := commitCache[targetCommit]
		if !cached {
			switch {
			case manifest != nil:
				status = checkCommitAgainstManifest(manifest, targetCommit)
			case repoErr != nil:
				status = CommitStatus{
					Commit: targetCommit,
					Reason: fmt.Sprintf("cannot verify %s: %v. Run scripts/reinstall-daemon.sh to write the build manifest", targetCommit, repoErr),
				}
			default:
				status = checkCommit(ctx, repoRoot, targetCommit, binaryCommit)
			}
			commitCache[targetCommit] = status
		}

		if !status.Valid {
			summary.Verified = false
			warn := MissingCommitWarning{
				Commit:            targetCommit,
				Section:           it.Section,
				ItemID:            it.ID,
				ItemTitle:         it.Title,
				MissingFromMain:   status.MissingFromMain,
				MissingFromBinary: status.MissingFromBinary,
				Reason:            status.Reason,
			}
			summary.MissingCommits = append(summary.MissingCommits, warn)

			if !blockedItemSet[it.ID] {
				blockedItemSet[it.ID] = true
				summary.BlockedItemIDs = append(summary.BlockedItemIDs, it.ID)
			}
			if !blockedSectionSet[it.Section] {
				blockedSectionSet[it.Section] = true
				summary.BlockedSections = append(summary.BlockedSections, it.Section)
			}
		}
	}

	summary.TotalCommits = len(commitCache)
	for _, st := range commitCache {
		if st.Valid {
			summary.ValidCommits++
		}
	}

	if !summary.Verified {
		if len(summary.BlockedSections) > 0 {
			summary.Warnings = append(summary.Warnings,
				fmt.Sprintf("%d sections blocked by missing/unmerged commits or unrebuilt binary", len(summary.BlockedSections)))
		}
	}

	return summary, nil
}

// probeRepo checks, once and quickly, that git can read repoRoot. Without
// this, a daemon blocked from ~/Documents spends the full timeout on every
// commit and reports each one as nonexistent.
func probeRepo(ctx context.Context, repoRoot string) error {
	probeCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	cmd := gitexec.Command(probeCtx, "rev-parse", "--git-dir")
	cmd.Dir = repoRoot
	if err := cmd.Run(); err != nil {
		if probeCtx.Err() != nil {
			return fmt.Errorf("staypointd could not read the git repo at %s (git timed out; macOS blocks background daemons from ~/Documents until staypointd is granted Documents access)", repoRoot)
		}
		return fmt.Errorf("staypointd could not read the git repo at %s: %v", repoRoot, err)
	}
	return nil
}

func checkCommit(ctx context.Context, repoRoot, commitSHA, binaryCommit string) CommitStatus {
	repoRoot = resolveRepoRoot(repoRoot)
	status := CommitStatus{
		Commit: commitSHA,
	}

	gitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	// 1. Resolve commit object in git
	cmdRev := gitexec.Command(gitCtx, "rev-parse", "--verify", commitSHA+"^{commit}")
	cmdRev.Dir = repoRoot
	outRev, err := cmdRev.CombinedOutput()
	if err != nil {
		status.MissingFromMain = true
		status.MissingFromBinary = true
		status.Reason = fmt.Sprintf("commit %s does not exist in git repository (repoRoot=%s, err=%v, out=%s)", commitSHA, repoRoot, err, strings.TrimSpace(string(outRev)))
		return status
	}
	status.FullSHA = strings.TrimSpace(string(outRev))

	// 2. Check if commit is ancestor of / in main
	inMain := false
	for _, mainRef := range []string{"main", "origin/main", "HEAD"} {
		cmdMain := gitexec.Command(gitCtx, "merge-base", "--is-ancestor", commitSHA, mainRef)
		cmdMain.Dir = repoRoot
		if err := cmdMain.Run(); err == nil {
			inMain = true
			break
		}
	}
	status.InMain = inMain
	if !inMain {
		status.MissingFromMain = true
		status.Reason = fmt.Sprintf("commit %s is on an unmerged branch and not present in main", commitSHA)
	}

	// 3. Check if commit is present in running binary commit history
	cleanBinary := strings.TrimSpace(binaryCommit)
	if cleanBinary == "" || cleanBinary == "none" || cleanBinary == "unknown" {
		status.MissingFromBinary = true
		if status.Reason == "" {
			status.Reason = fmt.Sprintf("staypointd binary was not compiled with GitCommit (-X main.GitCommit=...); active binary commit is %q", cleanBinary)
		} else {
			status.Reason += fmt.Sprintf("; also running staypointd binary has unverified commit %q", cleanBinary)
		}
	} else {
		cmdBinary := gitexec.Command(gitCtx, "merge-base", "--is-ancestor", commitSHA, cleanBinary)
		cmdBinary.Dir = repoRoot
		if err := cmdBinary.Run(); err != nil {
			status.MissingFromBinary = true
			if status.Reason == "" {
				status.Reason = fmt.Sprintf("commit %s is not in running staypointd binary commit history (running binary commit: %s; binary needs rebuild)", commitSHA, cleanBinary)
			} else {
				status.Reason += fmt.Sprintf("; and commit %s is not in running staypointd binary history (%s)", commitSHA, cleanBinary)
			}
		} else {
			status.InBinary = true
		}
	}

	status.Valid = status.InMain && status.InBinary
	return status
}
