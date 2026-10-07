package orchestrator

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"unicode/utf8"
)

// limitedWriter retains only the last maxSize bytes written (stderr tail capture).
// When maxSize is zero there is no limit.
type limitedWriter struct {
	buf     []byte
	maxSize int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if w.maxSize > 0 && len(w.buf) > w.maxSize {
		w.buf = w.buf[len(w.buf)-w.maxSize:]
	}
	return len(p), nil
}

func (w *limitedWriter) String() string { return string(w.buf) }

// exitCodeFrom extracts the numeric exit code from an error when available.
func exitCodeFrom(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}

// truncate returns s truncated to at most n bytes, preserving valid UTF-8.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	t := s[:n]
	for !utf8.ValidString(t) && len(t) > 0 {
		t = t[:len(t)-1]
	}
	return t
}

// plainAdapterError builds a user-readable error message for an adapter failure.
// Format: what failed, why (exit code + last stderr line), what to do next.
func plainAdapterError(err error, exitCode int, stderrTail string) string {
	hint := lastLine(strings.TrimSpace(stderrTail))
	if hint != "" {
		return fmt.Sprintf(
			"Adapter run failed (exit %d): %s. Check the run log for full output; if the model name or API key is wrong, update the provider config and retry.",
			exitCode, truncate(hint, 300),
		)
	}
	return fmt.Sprintf(
		"Adapter run failed (exit %d): %s. Check the run log for details.",
		exitCode, err.Error(),
	)
}

// lastLine returns the last non-empty line of s.
func lastLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

// noOutputMessage explains a run whose adapter exited without producing any
// answer, thought or tool call. unparsedBytes is the stdout the stream parser
// could not read, which points at a provider/parser mismatch.
func noOutputMessage(exitCode int, stderrTail string, unparsedBytes int) string {
	stderrTail = strings.TrimSpace(stderrTail)
	if len(stderrTail) > 500 {
		t := stderrTail[len(stderrTail)-500:]
		for !utf8.ValidString(t) && len(t) > 0 {
			t = t[1:]
		}
		stderrTail = "…" + t
	}
	if stderrTail == "" {
		stderrTail = "empty"
	}
	msg := fmt.Sprintf("Run ended with no output (exit %d, stderr: %s)", exitCode, stderrTail)
	if unparsedBytes > 0 {
		msg += fmt.Sprintf(". The agent wrote %d bytes to stdout that StayPoint could not read as a stream event.", unparsedBytes)
	}
	return msg
}
