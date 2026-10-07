package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// NonGitWarning is the timeline row and task page banner for a task that runs
// in a directory that is not a git repository.
const NonGitWarning = "Warning: not a git repository — changes are not checkpointed and can't be reviewed or undone"

// TaskDir is where a task runs.
type TaskDir struct {
	// Dir is the absolute directory. For a git task it is the repo the task
	// worktree is created from; otherwise the agent runs in it directly.
	Dir string
	// Git is true when Dir is inside a git work tree.
	Git bool
	// Scratch is true for the per-task scratch dir used when repo_path is
	// empty. A scratch dir is never treated as git, even under a parent repo.
	Scratch bool
}

// ScratchRootEnv overrides the scratch root (~/.staypoint/scratch), for tests.
const ScratchRootEnv = "STAYPOINT_SCRATCH_ROOT"

// ScratchDir is the per-task directory used when a task has no repo_path:
// ~/.staypoint/scratch/<task-id>.
func ScratchDir(taskID string) (string, error) {
	if err := ValidateTaskID(taskID); err != nil {
		return "", err
	}
	if root := os.Getenv(ScratchRootEnv); root != "" {
		return filepath.Join(root, taskID), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("scratch dir: %w", err)
	}
	return filepath.Join(home, ".staypoint", "scratch", taskID), nil
}

// InGitRepo reports whether dir is inside a git work tree: dir or one of its
// parents has a .git entry (a directory, or the pointer file of a linked
// worktree or submodule). It only stats, so it never runs git and cannot hang
// on a repo git cannot open; the git commands that follow report those.
func InGitRepo(dir string) bool {
	d := filepath.Clean(dir)
	for {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return false
		}
		d = parent
	}
}

// ResolveTaskDir decides where a task runs from its stored repo_path.
//   - empty: the per-task scratch dir, created if needed, run as non-git.
//   - an existing directory inside a git work tree: a git task (worktree,
//     checkpoints, ship review), exactly as before.
//   - an existing directory that is not: run directly in it, non-git.
//   - a path that is missing or not a directory: the git path, so worktree
//     creation fails with its usual error.
//
// It never creates a git repository.
func ResolveTaskDir(repoPath, taskID string) (TaskDir, error) {
	if strings.TrimSpace(repoPath) == "" {
		td, err := DescribeTaskDir(repoPath, taskID)
		if err != nil {
			return TaskDir{}, err
		}
		if err := os.MkdirAll(td.Dir, 0o700); err != nil {
			return TaskDir{}, fmt.Errorf("create scratch dir: %w", err)
		}
		return td, nil
	}
	return DescribeTaskDir(repoPath, taskID)
}

// DescribeTaskDir is ResolveTaskDir without creating the scratch dir, for
// read-only callers such as the task page.
func DescribeTaskDir(repoPath, taskID string) (TaskDir, error) {
	if strings.TrimSpace(repoPath) == "" {
		dir, err := ScratchDir(taskID)
		if err != nil {
			return TaskDir{}, err
		}
		return TaskDir{Dir: dir, Scratch: true}, nil
	}
	dir := repoPath
	if strings.HasPrefix(dir, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, dir[2:])
		}
	}
	// A path that cannot be stat'ed keeps the git path: worktree creation
	// reports why, as it did before non-git tasks existed.
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return TaskDir{Dir: repoPath, Git: true}, nil
	}
	if InGitRepo(dir) {
		return TaskDir{Dir: repoPath, Git: true}, nil // unchanged git path
	}
	return TaskDir{Dir: dir}, nil
}
