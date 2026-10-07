package orchestrator

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
)

// runAdapterTurn runs one adapter turn and recovers a panic into an error, so
// the post-turn Gemini guards (STA-856, STA-864) still check what the turn
// wrote. A panicking turn returns a non-nil panic error as well.
func runAdapterTurn(run func() error) (turnErr, panicErr error) {
	defer func() {
		if r := recover(); r != nil {
			panicErr = fmt.Errorf("agent turn crashed: %v", r)
			turnErr = panicErr
			slog.Error("adapter turn panicked", slog.Any("panic", r), slog.String("stack", string(debug.Stack())))
		}
	}()
	return run(), nil
}

// stopTurnForPanic ends the run after a turn panicked. It is not retried: the
// adapter's child may not have been reaped, and a later turn's snapshot would
// take any late writes as its baseline.
func (h *Harness) stopTurnForPanic(result *RunResult, panicErr error, taskID string, turn int, stdout io.Writer, sr *StepRecorder, runLog *slog.Logger) {
	if stw, ok := stdout.(*stepTeeWriter); ok {
		_ = stw.Close()
	}
	result.Turns++
	result.Disposition = "error"
	result.DiagnosticMsg = "Run stopped: " + panicErr.Error()
	runLog.Error("adapter turn panicked; run stopped", slog.Int("turn", turn), slog.Any("error", panicErr))
	if sr != nil {
		sr.EmitMessage("Run stopped: the agent turn crashed", panicErr.Error(), "error")
	}
	_, _ = h.DB.ExecContext(context.Background(),
		`INSERT INTO activity_log (task_id, event_type, details) VALUES (?, 'adapter_failure', ?)`,
		taskID, result.DiagnosticMsg,
	)
}
