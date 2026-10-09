package adapter

import (
	"bufio"
	"bytes"
	"io"
)

// streamWithCommitWatch reads newline-delimited output from r and writes to
// dst, buffering lines until isCommit returns true for a line. Once committed,
// all subsequent lines are written directly to dst without buffering. Every
// line, before and after the commit, is also passed to watch (when non-nil).
//
// Returns (committed, prebuf):
//   - committed=true:  at least one line was forwarded to dst; prebuf is nil.
//   - committed=false: EOF arrived before any commit event; prebuf holds the
//     buffered lines (caller decides whether to forward or discard them).
//
// Write errors to dst are silently swallowed (matching the original io.Copy
// behaviour in the pre-streaming code). Scanner errors from a closed pipe are
// treated as normal EOF.
func streamWithCommitWatch(r io.Reader, dst io.Writer, isCommit func(line []byte) bool, watch func(line []byte)) (committed bool, prebuf []byte) {
	var buf bytes.Buffer
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 512*1024), 512*1024)

	for sc.Scan() {
		raw := sc.Bytes()
		// Copy before the next Scan() call overwrites the underlying slice.
		line := make([]byte, len(raw)+1)
		copy(line, raw)
		line[len(raw)] = '\n'

		if watch != nil {
			watch(raw)
		}
		if committed {
			_, _ = dst.Write(line)
			continue
		}
		if isCommit(raw) {
			committed = true
			if buf.Len() > 0 {
				_, _ = dst.Write(buf.Bytes())
				buf.Reset()
			}
			_, _ = dst.Write(line)
		} else {
			buf.Write(line)
		}
	}
	if !committed {
		prebuf = buf.Bytes()
	}
	return
}

// isAssistantEvent reports whether line signals that the model has begun
// generating a response. Used as the commit trigger for the fallback chain —
// once committed, we stop trying other candidates.
//
// We commit on real model output only: DeltaText, DeltaThinking, DeltaToolUse.
// We do NOT commit on DeltaInit: some CLIs (e.g. agy/Gemini) emit an init event
// before calling the model, so a quota failure after init would suppress fallback
// if we treated init as a commit. DeltaResult and DeltaError are also excluded —
// an error result before any model content means the provider rejected the request
// and the next candidate should be tried.
func isAssistantEvent(line []byte, parse func([]byte) ([]StreamDelta, error)) bool {
	deltas, err := parse(line)
	if err != nil || len(deltas) == 0 {
		return false
	}
	for _, d := range deltas {
		switch d.Kind {
		case DeltaText, DeltaThinking, DeltaToolUse:
			return true
		}
	}
	return false
}
