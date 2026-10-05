package checkpoint

import (
	"bytes"
	"context"
	"fmt"
	"github.com/VinnyVanGogh/staypoint/internal/gitexec"
	"github.com/VinnyVanGogh/staypoint/internal/security"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

func runGit(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := gitexec.Command(ctx, args...)
	cmd.Dir = dir
	cmd.Env = append(security.ChildEnv(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("git %s failed: %w (stderr: %s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func getGitPaths(ctx context.Context, dir string) (rootDir string, gitDir string, err error) {
	rootDir, err = runGit(ctx, dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", fmt.Errorf("not in a git repository: %w", err)
	}
	gitDir, err = runGit(ctx, dir, nil, "rev-parse", "--git-dir")
	if err != nil {
		return "", "", fmt.Errorf("failed to get git directory: %w", err)
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(rootDir, gitDir)
	}
	return rootDir, gitDir, nil
}

// CreateCheckpoint generates an ephemeral micro-snapshot in <15ms using pure Git plumbing.
func CreateCheckpoint(ctx context.Context, opts CreateOptions) (*Checkpoint, error) {
	start := time.Now()
	if opts.Timeout <= 0 {
		opts.Timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	workDir := opts.WorkDir
	if workDir == "" {
		var err error
		workDir, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	rootDir, gitDir, err := getGitPaths(ctx, workDir)
	if err != nil {
		return nil, err
	}

	// 1. Get HEAD commit if available
	headSHA, _ := runGit(ctx, rootDir, nil, "rev-parse", "HEAD")

	// 2. Prepare isolated index: <gitDir>/staypoint_index
	// 2. Check for index.lock to avoid copying a torn index during concurrent operations
	lockFile := filepath.Join(gitDir, "index.lock")
	if _, err := os.Stat(lockFile); err == nil {
		time.Sleep(50 * time.Millisecond)
		if _, err := os.Stat(lockFile); err == nil {
			return nil, fmt.Errorf("git index is locked by another process (.git/index.lock exists)")
		}
	}

	meshIndex := filepath.Join(gitDir, "staypoint_index")
	mainIndex := filepath.Join(gitDir, "index")

	// Copy main index to staypoint_index if it exists to preserve tracked file cache
	if _, err := os.Stat(mainIndex); err == nil {
		copyFile(mainIndex, meshIndex)
	}

	indexEnv := []string{fmt.Sprintf("GIT_INDEX_FILE=%s", meshIndex)}

	// 3. Stage all modifications (including untracked by default) into staypoint_index
	addArgs := []string{"add", "-A"}
	if opts.ExcludeUntracked {
		addArgs = []string{"add", "-u"}
	}
	if _, err := runGit(ctx, rootDir, indexEnv, addArgs...); err != nil {
		_ = os.Remove(meshIndex)
		return nil, fmt.Errorf("failed to stage into isolated index: %w", err)
	}

	// 4. Write tree object from staypoint_index
	treeSHA, err := runGit(ctx, rootDir, indexEnv, "write-tree")
	if err != nil {
		_ = os.Remove(meshIndex)
		return nil, fmt.Errorf("failed to write git tree: %w", err)
	}

	// Clean up staypoint_index
	_ = os.Remove(meshIndex)

	// Capture baseline ignored files to protect pre-existing configs during undo cleanup
	ignoredOut, _ := runGit(ctx, rootDir, nil, "status", "--porcelain", "--ignored")
	var baselineIgnored []string
	for _, l := range strings.Split(strings.TrimSpace(ignoredOut), "\n") {
		if strings.HasPrefix(l, "!! ") {
			f := strings.TrimSpace(strings.TrimPrefix(l, "!! "))
			f = strings.Trim(f, "\"")
			if f != "" {
				baselineIgnored = append(baselineIgnored, f)
			}
		}
	}

	// 5. Generate checkpoint ID
	sessionID := opts.SessionID
	if sessionID == "" {
		sessionID = "global"
	}
	sessionID = strings.ReplaceAll(sessionID, "/", "-")

	now := time.Now().UTC()
	cpID := fmt.Sprintf("cp_%s_%s", now.Format("20060102_150405"), uuid.New().String()[:6])

	msg := opts.Message
	if msg == "" {
		msg = fmt.Sprintf("Agent micro-checkpoint %s", cpID)
	}
	commitMsg := msg
	if len(baselineIgnored) > 0 {
		commitMsg = fmt.Sprintf("%s\n\nStaypoint-Baseline-Ignored:\n%s", msg, strings.Join(baselineIgnored, "\n"))
	}

	// 6. Create commit object
	commitArgs := []string{"commit-tree", treeSHA}
	if headSHA != "" {
		commitArgs = append(commitArgs, "-p", headSHA)
	}
	commitArgs = append(commitArgs, "-m", commitMsg)

	commitSHA, err := runGit(ctx, rootDir, nil, commitArgs...)
	if err != nil {
		return nil, fmt.Errorf("failed to commit tree: %w", err)
	}

	// 7. Update custom refs
	sessionRef := fmt.Sprintf("refs/staypoint/checkpoints/%s/%s", sessionID, cpID)
	sessionLatestRef := fmt.Sprintf("refs/staypoint/checkpoints/%s/latest", sessionID)
	globalLatestRef := "refs/staypoint/checkpoints/latest"

	_, _ = runGit(ctx, rootDir, nil, "update-ref", sessionRef, commitSHA)
	_, _ = runGit(ctx, rootDir, nil, "update-ref", sessionLatestRef, commitSHA)
	_, _ = runGit(ctx, rootDir, nil, "update-ref", globalLatestRef, commitSHA)

	// Count modified files compared to HEAD
	fileCount := 0
	if headSHA != "" {
		if diffOut, err := runGit(ctx, rootDir, nil, "diff-tree", "--no-commit-id", "--name-only", "-r", headSHA, commitSHA); err == nil {
			lines := strings.Split(strings.TrimSpace(diffOut), "\n")
			for _, l := range lines {
				if strings.TrimSpace(l) != "" {
					fileCount++
				}
			}
		}
	}

	cp := &Checkpoint{
		ID:        cpID,
		SessionID: sessionID,
		CommitSHA: commitSHA,
		TreeSHA:   treeSHA,
		ParentSHA: headSHA,
		Timestamp: now,
		Message:   msg,
		Duration:  time.Since(start),
		FileCount: fileCount,
		Ref:       sessionRef,
	}

	return cp, nil
}

// ListCheckpoints returns all checkpoints sorted descending by creation time.
func ListCheckpoints(ctx context.Context, workDir string, limit int) ([]Checkpoint, error) {
	rootDir, _, err := getGitPaths(ctx, workDir)
	if err != nil {
		return nil, err
	}

	out, err := runGit(ctx, rootDir, nil, "for-each-ref", "--format=%(refname) %(objectname) %(contents:subject) %(creatordate:iso8601)", "refs/staypoint/checkpoints/")
	if err != nil {
		return nil, fmt.Errorf("failed to list checkpoint refs: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	var checkpoints []Checkpoint

	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 4)
		if len(parts) < 3 {
			continue
		}
		ref := parts[0]
		commitSHA := parts[1]
		rest := parts[2]
		if len(parts) == 4 {
			rest = parts[2] + " " + parts[3]
		}

		if strings.HasSuffix(ref, "/latest") {
			continue // Skip alias refs
		}

		refParts := strings.Split(ref, "/")
		cpID := refParts[len(refParts)-1]
		sessID := "global"
		if len(refParts) >= 5 {
			sessID = refParts[len(refParts)-2]
		}

		checkpoints = append(checkpoints, Checkpoint{
			ID:        cpID,
			SessionID: sessID,
			CommitSHA: commitSHA,
			Message:   rest,
			Ref:       ref,
		})

		if limit > 0 && len(checkpoints) >= limit {
			break
		}
	}

	return checkpoints, nil
}

// GetLatestCheckpoint returns the most recent checkpoint.
func GetLatestCheckpoint(ctx context.Context, workDir string) (*Checkpoint, error) {
	rootDir, _, err := getGitPaths(ctx, workDir)
	if err != nil {
		return nil, err
	}

	commitSHA, err := runGit(ctx, rootDir, nil, "rev-parse", "refs/staypoint/checkpoints/latest")
	if err != nil {
		return nil, fmt.Errorf("no checkpoints found in repository")
	}

	// Read commit subject
	msg, _ := runGit(ctx, rootDir, nil, "log", "-1", "--format=%s", commitSHA)

	return &Checkpoint{
		ID:        "latest",
		CommitSHA: commitSHA,
		Message:   msg,
		Ref:       "refs/staypoint/checkpoints/latest",
	}, nil
}

// DiffCheckpoint returns a diff stat between working directory and the specified checkpoint commit.
func DiffCheckpoint(ctx context.Context, workDir, checkpointID string) (string, error) {
	rootDir, _, err := getGitPaths(ctx, workDir)
	if err != nil {
		return "", err
	}

	targetRef := checkpointID
	if !strings.HasPrefix(targetRef, "refs/") && len(targetRef) != 40 {
		// Try resolving ID or latest
		if targetRef == "" || targetRef == "latest" {
			targetRef = "refs/staypoint/checkpoints/latest"
		} else {
			// Find matching ref
			refs, _ := runGit(ctx, rootDir, nil, "for-each-ref", "--format=%(refname)", fmt.Sprintf("refs/staypoint/checkpoints/*/%s", targetRef))
			if len(strings.TrimSpace(refs)) > 0 {
				targetRef = strings.TrimSpace(refs)
			}
		}
	}

	return runGit(ctx, rootDir, nil, "diff", targetRef, "--stat")
}

// DiffCheckpointAgainstRef diffs the checkpoint against an explicit git ref (e.g. a
// task branch tip) rather than the working tree. Use this when the task worktree has
// been pruned but the branch still exists.
func DiffCheckpointAgainstRef(ctx context.Context, repoPath, checkpointID, ref string) (string, error) {
	rootDir, _, err := getGitPaths(ctx, repoPath)
	if err != nil {
		return "", err
	}

	targetRef := checkpointID
	if !strings.HasPrefix(targetRef, "refs/") && len(targetRef) != 40 {
		if targetRef == "" || targetRef == "latest" {
			targetRef = "refs/staypoint/checkpoints/latest"
		} else {
			refs, _ := runGit(ctx, rootDir, nil, "for-each-ref", "--format=%(refname)", fmt.Sprintf("refs/staypoint/checkpoints/*/%s", targetRef))
			if len(strings.TrimSpace(refs)) > 0 {
				targetRef = strings.TrimSpace(refs)
			}
		}
	}

	return runGit(ctx, rootDir, nil, "diff", targetRef, ref, "--stat")
}

// DiffCheckpointFilesAgainstRef returns per-file add/remove counts between the
// checkpoint and an explicit git ref using git diff --numstat.
func DiffCheckpointFilesAgainstRef(ctx context.Context, repoPath, checkpointID, ref string) ([]FileDiffStat, error) {
	rootDir, _, err := getGitPaths(ctx, repoPath)
	if err != nil {
		return nil, err
	}

	targetRef, err := resolveCheckpointRef(checkpointID)
	if err != nil {
		return nil, err
	}
	// Resolve bare cp_ IDs to full refs (e.g. refs/staypoint/checkpoints/<sess>/<id>).
	if !strings.HasPrefix(targetRef, "refs/") && len(targetRef) != 40 {
		if refs, _ := runGit(ctx, rootDir, nil, "for-each-ref", "--format=%(refname)", fmt.Sprintf("refs/staypoint/checkpoints/*/%s", targetRef)); strings.TrimSpace(refs) != "" {
			targetRef = strings.TrimSpace(refs)
		}
	}
	out, err := runGit(ctx, rootDir, nil, "diff", targetRef, ref, "--numstat", "-z", "--no-renames")
	if err != nil {
		return nil, err
	}
	return parseNumstat(out), nil
}

// DiffCheckpointFiles returns per-file add/remove counts between the working tree
// and the specified checkpoint using git diff --numstat.
func DiffCheckpointFiles(ctx context.Context, workDir, checkpointID string) ([]FileDiffStat, error) {
	rootDir, _, err := getGitPaths(ctx, workDir)
	if err != nil {
		return nil, err
	}

	targetRef, err := resolveCheckpointRef(checkpointID)
	if err != nil {
		return nil, err
	}
	// Resolve bare cp_ IDs to full refs (e.g. refs/staypoint/checkpoints/<sess>/<id>).
	if !strings.HasPrefix(targetRef, "refs/") && len(targetRef) != 40 {
		if refs, _ := runGit(ctx, rootDir, nil, "for-each-ref", "--format=%(refname)", fmt.Sprintf("refs/staypoint/checkpoints/*/%s", targetRef)); strings.TrimSpace(refs) != "" {
			targetRef = strings.TrimSpace(refs)
		}
	}
	out, err := runGit(ctx, rootDir, nil, "diff", targetRef, "--numstat", "-z", "--no-renames")
	if err != nil {
		return nil, err
	}
	return parseNumstat(out), nil
}

// parseNumstat parses git diff --numstat -z output into FileDiffStat records.
// In -z mode records are NUL-delimited and formatted as:
//
//	<added>\t<removed>\t<path>\0
//
// If rename detection was enabled without --no-renames, rename records appear as:
//
//	<added>\t<removed>\t\0<old_path>\0<new_path>\0
//
// parseNumstat handles both, correctly preserving paths with spaces and
// extracting the real destination path for renames.
func parseNumstat(out string) []FileDiffStat {
	parts := strings.Split(out, "\x00")
	var stats []FileDiffStat
	for i := 0; i < len(parts); i++ {
		chunk := parts[i]
		if chunk == "" {
			continue
		}
		tab1 := strings.IndexByte(chunk, '\t')
		if tab1 == -1 {
			continue
		}
		tab2 := strings.IndexByte(chunk[tab1+1:], '\t')
		if tab2 == -1 {
			continue
		}
		tab2 += tab1 + 1
		addedStr := chunk[:tab1]
		removedStr := chunk[tab1+1 : tab2]
		path := chunk[tab2+1:]

		added, _ := strconv.Atoi(addedStr)
		removed, _ := strconv.Atoi(removedStr)

		// Handle git diff -z rename records if present:
		// <added>\t<removed>\t\0<old_path>\0<new_path>\0
		if len(path) == 0 && i+2 < len(parts) {
			newPath := parts[i+2]
			i += 2
			stats = append(stats, FileDiffStat{
				Path:    newPath,
				Added:   added,
				Removed: removed,
			})
			continue
		}

		stats = append(stats, FileDiffStat{
			Path:    path,
			Added:   added,
			Removed: removed,
		})
	}
	return stats
}

// validCheckpointID reports whether s is safe to pass to git.
// Accepts: empty (resolves to latest), "latest", full 40-char SHA, or
// short IDs / refs containing only [A-Za-z0-9._/-].
func validCheckpointID(s string) bool {
	if s == "" || s == "latest" {
		return true
	}
	if strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '/' || r == '-') {
			return false
		}
	}
	return true
}

// LatestRef is the checkpoint a diff compares against when given no checkpoint ID.
const LatestRef = "refs/staypoint/checkpoints/latest"

// resolveCheckpointRef converts a short checkpoint ID to a git ref or SHA.
// Returns an error when checkpointID contains unsafe characters.
func resolveCheckpointRef(checkpointID string) (string, error) {
	if !validCheckpointID(checkpointID) {
		return "", fmt.Errorf("invalid checkpoint id")
	}
	if strings.HasPrefix(checkpointID, "refs/") || len(checkpointID) == 40 {
		return checkpointID, nil
	}
	if checkpointID == "" || checkpointID == "latest" {
		return LatestRef, nil
	}
	return checkpointID, nil
}

// RestoreFile restores a single file from the specified checkpoint into the working tree.
func RestoreFile(ctx context.Context, workDir, checkpointID, filePath string) error {
	if filePath == "" {
		return fmt.Errorf("file path is required")
	}
	// Guard against path traversal — reject anything with ".." segments.
	if strings.Contains(filePath, "..") {
		return fmt.Errorf("invalid file path")
	}
	rootDir, _, err := getGitPaths(ctx, workDir)
	if err != nil {
		return err
	}

	targetRef, err := resolveCheckpointRef(checkpointID)
	if err != nil {
		return err
	}

	// Resolve short ID to full ref if needed.
	if !strings.HasPrefix(targetRef, "refs/") && len(targetRef) != 40 {
		refs, refErr := runGit(ctx, rootDir, nil, "for-each-ref", "--format=%(refname)", fmt.Sprintf("refs/staypoint/checkpoints/*/%s", targetRef))
		if refErr == nil && len(strings.TrimSpace(refs)) > 0 {
			targetRef = strings.TrimSpace(refs)
		}
	}

	sha, err := runGit(ctx, rootDir, nil, "rev-parse", targetRef)
	if err != nil {
		return fmt.Errorf("checkpoint not found: %s", checkpointID)
	}

	_, err = runGit(ctx, rootDir, nil, "checkout", strings.TrimSpace(sha), "--", filePath)
	return err
}

// FindPreRunCheckpoint returns the ID of the checkpoint created as the pre-run
// baseline for taskID (message prefix "pre-run <taskID>"). Returns "" when none
// is found; the caller falls back to "latest" in that case.
func FindPreRunCheckpoint(ctx context.Context, repoPath, taskID string) (string, error) {
	cps, err := ListCheckpoints(ctx, repoPath, 0)
	if err != nil {
		return "", nil //nolint:nilerr // best-effort; caller uses fallback
	}
	prefix := "pre-run " + taskID
	// Checkpoints are newest-first; walk from oldest end to pick the earliest pre-run.
	for i := len(cps) - 1; i >= 0; i-- {
		if strings.HasPrefix(cps[i].Message, prefix) {
			return cps[i].ID, nil
		}
	}
	return "", nil
}

// PruneCheckpoints deletes older checkpoint refs, keeping keepLast checkpoints.
func PruneCheckpoints(ctx context.Context, workDir string, keepLast int) (int, error) {
	if keepLast <= 0 {
		keepLast = 10
	}
	rootDir, _, err := getGitPaths(ctx, workDir)
	if err != nil {
		return 0, err
	}

	cps, err := ListCheckpoints(ctx, rootDir, 0)
	if err != nil {
		return 0, err
	}

	if len(cps) <= keepLast {
		return 0, nil
	}

	pruned := 0
	for i := keepLast; i < len(cps); i++ {
		_, err := runGit(ctx, rootDir, nil, "update-ref", "-d", cps[i].Ref)
		if err == nil {
			pruned++
		}
	}

	return pruned, nil
}

func copyFile(src, dst string) {
	in, err := os.Open(src)
	if err != nil {
		return
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return
	}
	defer out.Close()

	_, _ = io.Copy(out, in)
}

// MigrateLegacyRefs detects legacy refs under refs/mesh/checkpoints and copies them to refs/staypoint/checkpoints.
func MigrateLegacyRefs(ctx context.Context, workDir string) (int, error) {
	rootDir, _, err := getGitPaths(ctx, workDir)
	if err != nil {
		return 0, err
	}

	out, err := runGit(ctx, rootDir, nil, "for-each-ref", "--format=%(refname) %(objectname)", "refs/mesh/checkpoints/")
	if err != nil || len(strings.TrimSpace(out)) == 0 {
		return 0, nil
	}

	migrated := 0
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, line := range lines {
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		oldRef := parts[0]
		sha := parts[1]
		newRef := strings.Replace(oldRef, "refs/mesh/checkpoints", "refs/staypoint/checkpoints", 1)
		if _, err := runGit(ctx, rootDir, nil, "update-ref", newRef, sha); err == nil {
			migrated++
		}
	}
	return migrated, nil
}

// DiffFileContent returns the unified diff for a single file between the working
// tree and the specified checkpoint. Returns an error on unsafe paths.
func DiffFileContent(ctx context.Context, workDir, checkpointID, filePath string) (string, error) {
	if strings.Contains(filePath, "..") || filePath == "" {
		return "", fmt.Errorf("invalid file path")
	}
	rootDir, _, err := getGitPaths(ctx, workDir)
	if err != nil {
		return "", err
	}
	targetRef, err := resolveCheckpointRef(checkpointID)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(targetRef, "refs/") && len(targetRef) != 40 {
		if refs, _ := runGit(ctx, rootDir, nil, "for-each-ref", "--format=%(refname)", fmt.Sprintf("refs/staypoint/checkpoints/*/%s", targetRef)); strings.TrimSpace(refs) != "" {
			targetRef = strings.TrimSpace(refs)
		}
	}
	return runGit(ctx, rootDir, nil, "diff", targetRef, "--", filePath)
}

// DiffFileContentAgainstRef returns the unified diff for a single file between
// two explicit git refs (checkpoint vs task branch tip). Use when the worktree
// has been pruned.
func DiffFileContentAgainstRef(ctx context.Context, repoPath, checkpointID, ref, filePath string) (string, error) {
	if strings.Contains(filePath, "..") || filePath == "" {
		return "", fmt.Errorf("invalid file path")
	}
	rootDir, _, err := getGitPaths(ctx, repoPath)
	if err != nil {
		return "", err
	}
	targetRef, err := resolveCheckpointRef(checkpointID)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(targetRef, "refs/") && len(targetRef) != 40 {
		if refs, _ := runGit(ctx, rootDir, nil, "for-each-ref", "--format=%(refname)", fmt.Sprintf("refs/staypoint/checkpoints/*/%s", targetRef)); strings.TrimSpace(refs) != "" {
			targetRef = strings.TrimSpace(refs)
		}
	}
	return runGit(ctx, rootDir, nil, "diff", targetRef, ref, "--", filePath)
}

// DiffCheckpointFull returns a full diff between working directory and the specified checkpoint commit.
func DiffCheckpointFull(ctx context.Context, workDir, checkpointID string) (string, error) {
	rootDir, _, err := getGitPaths(ctx, workDir)
	if err != nil {
		return "", err
	}

	targetRef := checkpointID
	if !strings.HasPrefix(targetRef, "refs/") && len(targetRef) != 40 {
		if targetRef == "" || targetRef == "latest" {
			targetRef = "refs/staypoint/checkpoints/latest"
		} else {
			refs, _ := runGit(ctx, rootDir, nil, "for-each-ref", "--format=%(refname)", fmt.Sprintf("refs/staypoint/checkpoints/*/%s", targetRef))
			if len(strings.TrimSpace(refs)) > 0 {
				targetRef = strings.TrimSpace(refs)
			}
		}
	}

	return runGit(ctx, rootDir, nil, "diff", targetRef)
}
