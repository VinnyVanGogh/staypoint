package security

import (
	"fmt"
	"strings"
)

type redirect struct {
	op     string // ">", ">>", "<", "<<", ">&" ...
	target string
	// heredoc is true for "<<" and "<<-": target is the delimiter and body
	// the document text (STA-868). quoted is true when the delimiter was
	// quoted, so the body is literal (no expansion or substitution).
	heredoc bool
	quoted  bool
	body    string
}

type segment struct {
	argv      []string
	redirects []*redirect
	// piped is true when this segment's stdout feeds the next segment.
	piped bool
}

// parseShell splits a shell command line into simple commands. It is a
// conservative approximation, not a full POSIX parser: quoting, operators
// (; & && | || newline, subshell parens), redirections and command
// substitution ($(...), backticks) are understood. Substitution bodies are
// returned in subs for independent classification. Errors mean "cannot
// reason about this", which callers must treat as Red.
func parseShell(line string) (segs []segment, subs []string, err error) {
	var (
		cur     segment
		tok     strings.Builder
		inTok   bool
		quoted  bool   // the current token contained quoting
		pending string // redirect operator awaiting its target
		// heredocs whose body starts after the next newline, in order.
		pendingDocs []*redirect
	)
	flushTok := func() {
		if !inTok {
			return
		}
		t := tok.String()
		tok.Reset()
		inTok = false
		q := quoted
		quoted = false
		if pending != "" {
			r := &redirect{op: pending, target: t}
			if pending == "<<" || pending == "<<-" {
				r.heredoc, r.quoted = true, q
				pendingDocs = append(pendingDocs, r)
			}
			cur.redirects = append(cur.redirects, r)
			pending = ""
			return
		}
		cur.argv = append(cur.argv, t)
	}
	endSeg := func(piped bool) {
		flushTok()
		if len(cur.argv) > 0 || len(cur.redirects) > 0 {
			cur.piped = piped
			segs = append(segs, cur)
		}
		cur = segment{}
		pending = ""
	}
	sub := func(body string) {
		subs = append(subs, body)
		inTok = true
		tok.WriteString("$SUBST")
	}

	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\'':
			inTok, quoted = true, true
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				tok.WriteRune(rs[j])
				j++
			}
			if j >= len(rs) {
				return nil, nil, fmt.Errorf("unterminated single quote")
			}
			i = j
		case c == '"':
			inTok, quoted = true, true
			j := i + 1
			for ; j < len(rs) && rs[j] != '"'; j++ {
				switch {
				case rs[j] == '\\' && j+1 < len(rs):
					j++
					tok.WriteRune(rs[j])
				case rs[j] == '$' && j+1 < len(rs) && rs[j+1] == '(':
					body, end, e := scanParen(rs, j+1)
					if e != nil {
						return nil, nil, e
					}
					sub(body)
					j = end
				case rs[j] == '`':
					body, end, e := scanBacktick(rs, j)
					if e != nil {
						return nil, nil, e
					}
					sub(body)
					j = end
				default:
					tok.WriteRune(rs[j])
				}
			}
			if j >= len(rs) {
				return nil, nil, fmt.Errorf("unterminated double quote")
			}
			i = j
		case c == '\\':
			if i+1 < len(rs) {
				i++
				if rs[i] != '\n' {
					inTok, quoted = true, true
					tok.WriteRune(rs[i])
				}
			}
		case c == '#' && !inTok:
			// A word starting with # is a comment to end of line. Without
			// this a "#!/bin/bash" shebang read as a bash call (STA-868).
			for i+1 < len(rs) && rs[i+1] != '\n' {
				i++
			}
		case c == '$' && i+1 < len(rs) && rs[i+1] == '(':
			body, end, e := scanParen(rs, i+1)
			if e != nil {
				return nil, nil, e
			}
			sub(body)
			i = end
		case c == '`':
			body, end, e := scanBacktick(rs, i)
			if e != nil {
				return nil, nil, e
			}
			sub(body)
			i = end
		case (c == '<' || c == '>') && i+1 < len(rs) && rs[i+1] == '(':
			// process substitution: body classified independently
			body, end, e := scanParen(rs, i+1)
			if e != nil {
				return nil, nil, e
			}
			sub(body)
			i = end
		case c == '<' || c == '>':
			// a pure-digit token before the operator is an fd number (2>)
			if inTok && isDigits(tok.String()) {
				tok.Reset()
				inTok = false
			} else {
				flushTok()
			}
			op := string(c)
			for i+1 < len(rs) && (rs[i+1] == c || rs[i+1] == '&' || rs[i+1] == '|') {
				i++
				op += string(rs[i])
			}
			if op == "<<" && i+1 < len(rs) && rs[i+1] == '-' {
				i++
				op = "<<-"
			}
			pending = op
		case c == '&' && i+1 < len(rs) && rs[i+1] == '>':
			flushTok()
			i++
			op := "&>"
			if i+1 < len(rs) && rs[i+1] == '>' {
				i++
				op = "&>>"
			}
			pending = op
		case c == '\n' && (len(pendingDocs) > 0 || (inTok && (pending == "<<" || pending == "<<-"))):
			endSeg(false) // flushes the delimiter word first
			next, err := readHeredocs(rs, i+1, pendingDocs)
			if err != nil {
				return nil, nil, err
			}
			for _, r := range pendingDocs {
				if !r.quoted {
					// An unquoted body still expands $(...) and `...`.
					s, err := heredocSubs(r.body)
					if err != nil {
						return nil, nil, err
					}
					subs = append(subs, s...)
				}
			}
			pendingDocs = nil
			i = next - 1
		case c == ';' || c == '\n' || c == '(' || c == ')':
			endSeg(false)
		case c == '&' || c == '|':
			op := c
			for i+1 < len(rs) && rs[i+1] == c {
				i++
			}
			endSeg(op == '|' && (i == 0 || rs[i-1] != '|'))
		case c == ' ' || c == '\t' || c == '\r':
			flushTok()
		default:
			inTok = true
			tok.WriteRune(c)
		}
	}
	endSeg(false)
	if len(pendingDocs) > 0 {
		return nil, nil, fmt.Errorf("unterminated here-document")
	}
	return segs, subs, nil
}

// readHeredocs fills each document's body from the lines starting at rs[start]
// and returns the index just past the last delimiter line.
func readHeredocs(rs []rune, start int, docs []*redirect) (int, error) {
	j := start
	for _, r := range docs {
		var body strings.Builder
		found := false
		for j <= len(rs) {
			k := j
			for k < len(rs) && rs[k] != '\n' {
				k++
			}
			line := string(rs[j:k])
			j = k + 1
			cmp := line
			if r.op == "<<-" {
				cmp = strings.TrimLeft(line, "\t")
			}
			if cmp == r.target {
				found = true
				break
			}
			body.WriteString(line)
			body.WriteByte('\n')
			if k >= len(rs) {
				break
			}
		}
		if !found {
			return 0, fmt.Errorf("unterminated here-document (%s)", r.target)
		}
		r.body = body.String()
	}
	if j > len(rs) {
		j = len(rs)
	}
	return j, nil
}

// heredocSubs returns the command substitutions in an unquoted heredoc body.
func heredocSubs(body string) ([]string, error) {
	var out []string
	rs := []rune(body)
	for i := 0; i < len(rs); i++ {
		switch {
		case rs[i] == '\\':
			i++
		case rs[i] == '$' && i+1 < len(rs) && rs[i+1] == '(':
			b, end, err := scanParen(rs, i+1)
			if err != nil {
				return nil, err
			}
			out = append(out, b)
			i = end
		case rs[i] == '`':
			b, end, err := scanBacktick(rs, i)
			if err != nil {
				return nil, err
			}
			out = append(out, b)
			i = end
		}
	}
	return out, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// scanParen returns the body of a balanced ( ... ) starting at rs[open]=='('
// and the index of the closing paren.
func scanParen(rs []rune, open int) (string, int, error) {
	depth := 0
	var q rune
	for j := open; j < len(rs); j++ {
		c := rs[j]
		switch {
		case q != 0:
			if c == q {
				q = 0
			} else if c == '\\' && q == '"' {
				j++
			}
		case c == '\'' || c == '"':
			q = c
		case c == '\\':
			j++
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth == 0 {
				return string(rs[open+1 : j]), j, nil
			}
		}
	}
	return "", 0, fmt.Errorf("unterminated command substitution")
}

func scanBacktick(rs []rune, open int) (string, int, error) {
	for j := open + 1; j < len(rs); j++ {
		if rs[j] == '\\' {
			j++
			continue
		}
		if rs[j] == '`' {
			return string(rs[open+1 : j]), j, nil
		}
	}
	return "", 0, fmt.Errorf("unterminated backtick substitution")
}
