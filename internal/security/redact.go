// Package security is the safety floor for autonomous runs: secret redaction,
// child-process env sanitization, command tiering and worktree path boundaries.
package security

import (
	"bytes"
	"io"
	"regexp"
	"sync"
)

type redactRule struct {
	name string
	re   *regexp.Regexp
	// repl overrides the default "[REDACTED:<name>]" replacement.
	repl string
}

// Order matters: more specific prefixes (sk-ant-) must run before generic ones (sk-).
var redactRules = []redactRule{
	{name: "pem-private-key", re: regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----.*?-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)},
	{name: "anthropic-key", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{20,}`)},
	{name: "openai-key", re: regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{20,}`)},
	{name: "google-api-key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}`)},
	{name: "github-token", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})`)},
	{name: "aws-access-key", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	// The ops run ticket (opstools.RunTokenEnv) as printed by env/printenv/export -p.
	{name: "run-ticket", re: regexp.MustCompile(`(?i)\b(STAYPOINT_RUN_TICKET\s*[=:]\s*["']?)[0-9a-f]{16,}`), repl: "${1}[REDACTED:run-ticket]"},
	{name: "bearer-token", re: regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/=\-]{8,}`), repl: "${1}[REDACTED:bearer]"},
}

var (
	pemBegin = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
	pemEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)
)

// Redact returns s with every recognised secret replaced by a [REDACTED:kind] marker.
func Redact(s string) string {
	for _, r := range redactRules {
		repl := r.repl
		if repl == "" {
			repl = "[REDACTED:" + r.name + "]"
		}
		s = r.re.ReplaceAllString(s, repl)
	}
	return s
}

// RedactBytes is Redact for byte slices.
func RedactBytes(b []byte) []byte { return []byte(Redact(string(b))) }

const (
	// maxLine bounds how much a Writer holds while waiting for a newline.
	maxLine = 1 << 20
	// tailKeep is retained when a line overflows so a token straddling the cut is still caught.
	tailKeep = 512
	// maxPEMHold bounds how much is held while waiting for the END of a private key block.
	maxPEMHold = 256 << 10
)

// Writer is a streaming redactor: it buffers by line (and by whole PEM block)
// so secrets split across Write calls are still caught. Call Close to flush.
type Writer struct {
	mu  sync.Mutex
	dst io.Writer
	buf []byte
}

// NewWriter wraps dst so everything written through it is redacted.
func NewWriter(dst io.Writer) *Writer { return &Writer{dst: dst} }

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	if err := w.drain(false); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close flushes any buffered partial line. It does not close the destination.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.drain(true)
}

func (w *Writer) drain(final bool) error {
	for len(w.buf) > 0 {
		// An unterminated private key block is held back whole.
		if loc := pemBegin.FindIndex(w.buf); loc != nil {
			// A complete block is redacted as one unit; per-line redaction cannot see it.
			if end := pemEnd.FindIndex(w.buf[loc[1]:]); end != nil {
				n := loc[1] + end[1]
				if err := w.emit(RedactBytes(w.buf[:n]), n); err != nil {
					return err
				}
				continue
			}
			if final || len(w.buf)-loc[0] > maxPEMHold {
				return w.emit(append(RedactBytes(w.buf[:loc[0]]), []byte("[REDACTED:pem-private-key]")...), len(w.buf))
			}
			if loc[0] > 0 {
				if err := w.emit(RedactBytes(w.buf[:loc[0]]), loc[0]); err != nil {
					return err
				}
			}
			return nil
		}
		i := bytes.IndexByte(w.buf, '\n')
		switch {
		case i >= 0:
			if err := w.emit(RedactBytes(w.buf[:i+1]), i+1); err != nil {
				return err
			}
		case final:
			return w.emit(RedactBytes(w.buf), len(w.buf))
		case len(w.buf) > maxLine:
			n := len(w.buf) - tailKeep
			if err := w.emit(RedactBytes(w.buf[:n]), n); err != nil {
				return err
			}
		default:
			return nil
		}
	}
	return nil
}

func (w *Writer) emit(out []byte, consumed int) error {
	w.buf = w.buf[consumed:]
	_, err := w.dst.Write(out)
	return err
}
