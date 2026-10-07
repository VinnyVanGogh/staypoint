package security

import (
	"fmt"
	"regexp"
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
	// fd is the explicit file descriptor before the operator ("2" in 2>), "".
	fd string
	// dyn is true when target contains an unquoted (or double-quoted)
	// expansion: its runtime value is not the text we see.
	dyn bool
	// udyn: an expansion outside any quotes (word splitting and globbing
	// apply to its value); meta: unquoted glob, brace or leading tilde.
	udyn, meta bool
}

type segment struct {
	argv []string
	// dyn[i] is true when argv[i] contains an expansion ($VAR, ${..},
	// $(...), backticks, $'..') outside single quotes (STA-868).
	dyn       []bool
	udyn      []bool // argv[i] has an expansion outside any quotes
	meta      []bool // argv[i] has an unquoted *, ?, [, { or leading ~
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
		dyn     bool   // the current token contains an expansion
		udyn    bool   // ... outside any quotes
		meta    bool   // the current token has unquoted glob/brace/tilde
		pending string // redirect operator awaiting its target
		pendFd  string // fd digits of the pending redirect
		bad     string // set when the line has a construct we refuse
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
		q, d, ud, m := quoted, dyn, udyn, meta
		quoted, dyn, udyn, meta = false, false, false, false
		if pending != "" {
			r := &redirect{op: pending, target: t, fd: pendFd, dyn: d, udyn: ud, meta: m}
			if pending == "<<" || pending == "<<-" {
				r.heredoc, r.quoted = true, q
				// bash never expands a delimiter word; ours would not match
				// what bash ends the document on, so refuse it (STA-868).
				if d || strings.ContainsAny(t, "$`") {
					bad = "here-document delimiter with expansion characters"
				}
				pendingDocs = append(pendingDocs, r)
			}
			cur.redirects = append(cur.redirects, r)
			pending, pendFd = "", ""
			return
		}
		cur.argv = append(cur.argv, t)
		cur.dyn = append(cur.dyn, d)
		cur.udyn = append(cur.udyn, ud)
		cur.meta = append(cur.meta, m)
	}
	endSeg := func(piped bool) {
		flushTok()
		if len(cur.argv) > 0 || len(cur.redirects) > 0 {
			cur.piped = piped
			segs = append(segs, cur)
		}
		cur = segment{}
		pending, pendFd = "", ""
	}
	inDQ := false
	sub := func(body string) {
		subs = append(subs, body)
		inTok, dyn = true, true
		if !inDQ {
			udyn = true
		}
		tok.WriteString("$SUBST")
	}
	// expands reports whether a $ at rs[k] starts an expansion.
	expands := func(rs []rune, k int) bool {
		if k+1 >= len(rs) {
			return false
		}
		n := rs[k+1]
		return n == '{' || n == '_' || n == '\'' || n == '"' || (n >= 'a' && n <= 'z') || (n >= 'A' && n <= 'Z') ||
			(n >= '0' && n <= '9') || strings.ContainsRune("@*#?$!-", n)
	}

	if strings.ContainsRune(line, '\r') {
		// bash keeps \r inside words (so "x\r#" is not a comment there, and
		// "EOF\r" is not a delimiter); we would split differently. Refuse.
		return nil, nil, fmt.Errorf("carriage return in command")
	}
	if strings.ContainsRune(line, 0) {
		return nil, nil, fmt.Errorf("NUL byte in command")
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
			inDQ = true
			j := i + 1
			for ; j < len(rs) && rs[j] != '"'; j++ {
				switch {
				case rs[j] == '\\' && j+1 < len(rs):
					// Inside "...", \ escapes only $ ` " \ and newline.
					j++
					if !strings.ContainsRune("$`\"\\\n", rs[j]) {
						tok.WriteRune('\\')
					}
					if rs[j] != '\n' {
						tok.WriteRune(rs[j])
					}
				case rs[j] == '$' && expands(rs, j):
					if e := checkBraceExpansion(rs, j); e != nil {
						return nil, nil, e
					}
					dyn = true
					if rs[j+1] != '(' {
						tok.WriteRune(rs[j])
						continue
					}
					fallthrough
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
			inDQ = false
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
		case c == '$' && i+1 < len(rs) && rs[i+1] == '\'':
			// $'...' (ANSI-C quoting): \' does not end it, unlike '...'.
			// The value after escape processing is unknown to us: dynamic.
			inTok, dyn, udyn, quoted = true, true, true, true
			j := i + 2
			for ; j < len(rs) && rs[j] != '\''; j++ {
				if rs[j] == '\\' {
					j++
				}
				if j < len(rs) {
					tok.WriteRune(rs[j])
				}
			}
			if j >= len(rs) {
				return nil, nil, fmt.Errorf("unterminated $'...' quote")
			}
			tok.WriteString("$ANSI")
			i = j
		case c == '$' && expands(rs, i):
			if e := checkBraceExpansion(rs, i); e != nil {
				return nil, nil, e
			}
			inTok, dyn, udyn = true, true, true
			tok.WriteRune(c)
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
				pendFd = tok.String()
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
			if bad != "" {
				return nil, nil, fmt.Errorf("%s", bad)
			}
			next, err := readHeredocs(rs, i+1, pendingDocs)
			if err != nil {
				return nil, nil, err
			}
			for _, r := range pendingDocs {
				if !r.quoted && unquotedContinuationRe.MatchString(r.body) {
					// bash joins "\<newline>" in an unquoted body, which can
					// move where the document ends.
					return nil, nil, fmt.Errorf("line continuation in an unquoted here-document")
				}
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
			if strings.ContainsRune("*?[{", c) || (c == '~' && tok.Len() == 0) {
				meta = true
			}
			inTok = true
			tok.WriteRune(c)
		}
	}
	endSeg(false)
	if bad != "" {
		return nil, nil, fmt.Errorf("%s", bad)
	}
	if len(pendingDocs) > 0 {
		return nil, nil, fmt.Errorf("unterminated here-document")
	}
	return segs, subs, nil
}

var unquotedContinuationRe = regexp.MustCompile(`(?m)\\$`)

// checkBraceExpansion refuses ${...} (starting at rs[k] == '$') whose body
// contains quotes, backticks or $(: bash parses nested quoting inside ${...}
// that our tokenizer does not, so where the word ends could differ.
func checkBraceExpansion(rs []rune, k int) error {
	if k+1 >= len(rs) || rs[k+1] != '{' {
		return nil
	}
	depth := 0
	for j := k + 1; j < len(rs); j++ {
		switch rs[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return nil
			}
		case '\'', '"', '`', '\\', '\n':
			return fmt.Errorf("quoting or escapes inside ${...}")
		case '(':
			if rs[j-1] == '$' {
				return fmt.Errorf("command substitution inside ${...}")
			}
		}
	}
	return fmt.Errorf("unterminated ${...}")
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
