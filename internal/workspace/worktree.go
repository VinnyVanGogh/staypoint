package workspace

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/VinnyVanGogh/staypoint/internal/security"
)

// WorktreeManager handles isolation of agent runs into separate git worktrees.
type WorktreeManager struct {
	RepoRoot string
	DB       *sql.DB
}

// NewWorktreeManager creates a manager for the given repository root.
func NewWorktreeManager(repoRoot string, db *sql.DB) *WorktreeManager {
	return &WorktreeManager{RepoRoot: repoRoot, DB: db}
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = security.ChildEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w (output: %s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// Create sets up a new git worktree for the given task.
// Branch: staypoint/<taskID>, path: <repoRoot>/.worktrees/<taskID>.
func (w *WorktreeManager) Create(taskID string, sessionID string) (string, error) {
	return w.CreateContext(context.Background(), taskID, sessionID)
}

// CreateContext is the context-aware version of Create.
func (w *WorktreeManager) CreateContext(ctx context.Context, taskID string, sessionID string) (string, error) {
	wtPath := filepath.Join(w.RepoRoot, ".worktrees", taskID)
	branch := fmt.Sprintf("staypoint/%s", taskID)

	if err := os.MkdirAll(filepath.Join(w.RepoRoot, ".worktrees"), 0o755); err != nil {
		return "", fmt.Errorf("create .worktrees dir: %w", err)
	}

	// Clean up stale worktree from a previous crash. Keep the branch: it holds
	// the task's committed work, and while a ship review is open its remote
	// copy backs the preview deployments (STA-637). The branch is deleted only
	// after Approve & merge.
	if _, err := os.Stat(wtPath); err == nil {
		if pruneErr := w.PruneWorktreeDirContext(ctx, taskID); pruneErr != nil {
			return "", fmt.Errorf("stale worktree cleanup: %w", pruneErr)
		}
	}

	// Try with -b first; fall back to existing branch.
	if _, err := runGit(ctx, w.RepoRoot, "worktree", "add", "-b", branch, wtPath, "HEAD"); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			if _, err2 := runGit(ctx, w.RepoRoot, "worktree", "add", wtPath, branch); err2 != nil {
				return "", fmt.Errorf("git worktree add: %w (original: %v)", err2, err)
			}
		} else {
			return "", fmt.Errorf("git worktree add: %w", err)
		}
	}

	return wtPath, nil
}

// Prune removes the worktree for the given task and deletes its branch.
func (w *WorktreeManager) Prune(taskID string) error {
	return w.PruneContext(context.Background(), taskID)
}

// PruneContext is the context-aware version of Prune.
// It removes the worktree directory AND deletes the branch.
// Use this for orphan sweep only.
// For normal run teardown use PruneWorktreeDirContext to preserve the branch.
func (w *WorktreeManager) PruneContext(ctx context.Context, taskID string) error {
	if err := w.PruneWorktreeDirContext(ctx, taskID); err != nil {
		return err
	}
	branch := fmt.Sprintf("staypoint/%s", taskID)
	_, _ = runGit(ctx, w.RepoRoot, "branch", "-D", branch)
	return nil
}

// PruneWorktreeDirContext removes the worktree directory for the given task
// but leaves the branch intact so committed work remains reachable.
func (w *WorktreeManager) PruneWorktreeDirContext(ctx context.Context, taskID string) error {
	wtPath := filepath.Join(w.RepoRoot, ".worktrees", taskID)

	if _, err := runGit(ctx, w.RepoRoot, "worktree", "remove", "--force", wtPath); err != nil {
		// If the path still exists after the command, remove it forcibly.
		if _, statErr := os.Stat(wtPath); statErr == nil {
			_ = os.RemoveAll(wtPath)
		} else if !strings.Contains(err.Error(), "does not exist") && !strings.Contains(err.Error(), "is not a working tree") {
			return fmt.Errorf("git worktree remove: %w", err)
		}
	} else {
		_ = os.RemoveAll(wtPath) // belt-and-suspenders
	}
	return nil
}

// SweepOrphans removes worktrees that have no active session in the DB.
func (w *WorktreeManager) SweepOrphans() error {
	wtDir := filepath.Join(w.RepoRoot, ".worktrees")
	entries, err := os.ReadDir(wtDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read .worktrees: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		taskID := e.Name()
		active := false
		if w.DB != nil {
			relPath := filepath.Join(".worktrees", taskID)
			var n int
			if err := w.DB.QueryRow(
				`SELECT COUNT(1) FROM agent_working_files wf
				 JOIN agent_sessions s ON wf.session_id = s.id
				 WHERE wf.file_path = ? AND s.status = 'active'`, relPath).Scan(&n); err != nil {
				fmt.Fprintf(os.Stderr, "warn: sweep orphan check %s: %v; skipping prune\n", taskID, err)
				continue
			}
			active = n > 0
		}
		if !active {
			if pruneErr := w.Prune(taskID); pruneErr != nil {
				fmt.Fprintf(os.Stderr, "warn: sweep orphan %s: %v\n", taskID, pruneErr)
			}
		}
	}
	return nil
}
