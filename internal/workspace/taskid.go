package workspace

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidTaskID is returned when a task ID cannot safely be used as a
// single path segment under .worktrees.
var ErrInvalidTaskID = errors.New("invalid task id")

// ValidateTaskID rejects task IDs that would not stay a single path segment
// when joined under .worktrees (STA-649). filepath.Join(dir, "/") and
// filepath.Join(dir, "..") resolve to dir itself or its parent, so a removal
// built from such an ID would wipe every worktree or the repo.
//
// Real IDs are "task-<8 hex>", but tests and older rows use other names, so
// this rejects separators and dot segments rather than enforcing that shape.
func ValidateTaskID(taskID string) error {
	switch {
	case taskID == "":
		return fmt.Errorf("%w: empty", ErrInvalidTaskID)
	case strings.ContainsAny(taskID, `/\`+"\x00"):
		return fmt.Errorf("%w: %q contains a path separator", ErrInvalidTaskID, taskID)
	case strings.HasPrefix(taskID, "."):
		return fmt.Errorf("%w: %q starts with '.'", ErrInvalidTaskID, taskID)
	case strings.HasPrefix(taskID, "-"):
		return fmt.Errorf("%w: %q starts with '-'", ErrInvalidTaskID, taskID)
	}
	return nil
}
