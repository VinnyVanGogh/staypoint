package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateTaskID(t *testing.T) {
	for _, id := range []string{"", "/", ".", "..", "../x", "a/b", `a\b`, "/abs", ".hidden", "-rf", "a\x00b"} {
		if err := ValidateTaskID(id); !errors.Is(err, ErrInvalidTaskID) {
			t.Errorf("ValidateTaskID(%q) = %v, want ErrInvalidTaskID", id, err)
		}
	}
	for _, id := range []string{"task-1a2b3c4d", "task-ghost-none", "t-clean", "a..b"} {
		if err := ValidateTaskID(id); err != nil {
			t.Errorf("ValidateTaskID(%q) = %v, want nil", id, err)
		}
	}
}

// STA-649: filepath.Join(".worktrees", "/") is .worktrees itself, so a
// path-like ID must never reach the removal code.
func TestWorktreeManager_RejectsPathLikeTaskID(t *testing.T) {
	root := t.TempDir()
	keep := filepath.Join(root, ".worktrees", "task-keep")
	if err := os.MkdirAll(keep, 0o755); err != nil {
		t.Fatal(err)
	}
	wm := NewWorktreeManager(root, nil)
	for _, id := range []string{"", "/", "..", "../x"} {
		if err := wm.Prune(id); !errors.Is(err, ErrInvalidTaskID) {
			t.Errorf("Prune(%q) = %v, want ErrInvalidTaskID", id, err)
		}
		if err := wm.PruneWorktreeDirContext(t.Context(), id); !errors.Is(err, ErrInvalidTaskID) {
			t.Errorf("PruneWorktreeDirContext(%q) = %v, want ErrInvalidTaskID", id, err)
		}
		if _, err := wm.Create(id, "sess"); !errors.Is(err, ErrInvalidTaskID) {
			t.Errorf("Create(%q) = %v, want ErrInvalidTaskID", id, err)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("sibling worktree removed: %v", err)
	}
}
