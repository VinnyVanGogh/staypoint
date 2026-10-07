package orchestrator

import (
	"context"
	"io"
	"log/slog"

	"github.com/VinnyVanGogh/staypoint/internal/geminiguard"
	"github.com/VinnyVanGogh/staypoint/internal/workspace"
)

// Non-git tasks (STA-864): a task whose repo_path is an existing directory
// that is not a git repository, or that has no repo_path (it gets
// ~/.staypoint/scratch/<task-id>), runs directly in that directory. There is
// no worktree, no checkpoint, no git pre/post-flight and no ship review card.
// Nothing here ever runs `git init`.

type plainDirKey struct{}

// withPlainDir marks ctx as a run in a non-git directory, so completion
// checks that need git (sync, ship review card) have nothing to check.
func withPlainDir(ctx context.Context) context.Context {
	return context.WithValue(ctx, plainDirKey{}, true)
}

func isPlainDir(ctx context.Context) bool {
	v, _ := ctx.Value(plainDirKey{}).(bool)
	return v
}

// announceNonGit records the non-git warning on the timeline, as a harness
// comment and in the activity log.
func (h *Harness) announceNonGit(ctx context.Context, taskID string, td workspace.TaskDir, sr *StepRecorder) {
	body := "Running directly in " + td.Dir + "."
	if td.Scratch {
		body = "No repo set: running in the task scratch folder " + td.Dir + "."
	}
	body += " There is no worktree, no checkpoint and no ship review; edits land in place and cannot be undone from StayPoint." +
		" Gemini is not routed for code work here, and a Gemini turn that changes any non-doc file fails the run."
	if sr != nil {
		sr.EmitMessage(workspace.NonGitWarning, body, "done")
	}
	_, _ = h.DB.ExecContext(ctx,
		`INSERT INTO task_comments (task_id, author, message) VALUES (?, 'harness', ?)`,
		taskID, workspace.NonGitWarning+". "+body,
	)
	_, _ = h.DB.ExecContext(ctx,
		`INSERT INTO activity_log (task_id, event_type, details) VALUES (?, 'non_git_workspace', ?)`,
		taskID, td.Dir,
	)
}

// registerPlainDirProduct records the directory as the task's work product
// when the agent reports completion, so the Board can mark it done. Returns
// whether a product is now registered.
func (h *Harness) registerPlainDirProduct(ctx context.Context, taskID, dir string) bool {
	_, err := h.DB.ExecContext(ctx,
		`INSERT INTO task_work_products (task_id, product_type, reference) VALUES (?, 'workspace_file', ?)`,
		taskID, dir,
	)
	return err == nil
}

// blockGeminiNoGit fails the run after a Gemini turn changed non-doc files in
// a non-git directory. Nothing can be reverted (no checkpoint); the row says
// so. Board approvals that relax the git guard do not apply here.
func (h *Harness) blockGeminiNoGit(result *RunResult, nr geminiguard.NoGitResult, taskID string, turn int, stdout io.Writer, sr *StepRecorder, runLog *slog.Logger) {
	if stw, ok := stdout.(*stepTeeWriter); ok {
		_ = stw.Close()
	}
	result.Turns++
	result.Disposition = "error"
	result.DiagnosticMsg = nr.Title() + ". " + nr.Body()
	runLog.Warn("gemini code guard blocked turn in non-git dir",
		slog.Int("turn", turn),
		slog.Any("changed", nr.Changed),
		slog.Any("error", nr.Err),
	)
	if sr != nil {
		sr.EmitMessage(nr.Title(), nr.Body(), "error")
	}
	_, _ = h.DB.ExecContext(context.Background(),
		`INSERT INTO activity_log (task_id, event_type, details) VALUES (?, 'gemini_code_blocked', ?)`,
		taskID, nr.Title(),
	)
}
