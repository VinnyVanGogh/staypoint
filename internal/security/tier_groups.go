package security

// groupKeywords start a command without being one: `{ staypoint mcp; }`,
// `if staypoint mcp; then`, `! staypoint mcp` all run staypoint.
var groupKeywords = map[string]bool{
	"{": true, "if": true, "then": true, "else": true, "elif": true,
	"do": true, "while": true, "until": true, "!": true,
}

// normalizeGroups strips leading shell keywords and function-definition
// headers from each segment, so the command they introduce is classified as
// the command it is (Board review #5 M1). It returns the index of the first
// segment inside a function definition, or -1: a function body runs when the
// function is called, possibly after env edits later on the line
// (`f() { staypoint status; }; export HOME=/tmp; f`), so the caller treats
// everything from there on as running under an unknown environment.
func normalizeGroups(segs []segment) (out []segment, funcAt int) {
	funcAt = -1
	out = make([]segment, 0, len(segs))
	for i, s := range segs {
		n := 0
	words:
		for n < len(s.argv) {
			switch a := s.argv[n]; {
			case groupKeywords[a] || a == "}":
				n++
			case a == "function":
				// function NAME [()] { BODY; the parser ends the segment
				// at the parens, so the body may be in the next one.
				if funcAt < 0 {
					funcAt = len(out)
				}
				n += 2
			default:
				break words
			}
		}
		// NAME() { BODY: the parser splits at the parens, leaving a
		// one-word segment followed by one that opens a group. The word is
		// still classified as a command (it may be one: `reboot` on one
		// line, `{ ...; }` on the next); only the function flag is set.
		if n == 0 && len(s.argv) == 1 && i+1 < len(segs) && len(segs[i+1].argv) > 0 && segs[i+1].argv[0] == "{" && funcAt < 0 {
			funcAt = len(out)
		}
		ns := trimSegment(s, min(n, len(s.argv)))
		if len(ns.argv) == 0 && len(ns.redirects) == 0 {
			continue
		}
		out = append(out, ns)
	}
	return out, funcAt
}

// trimSegment drops the first n words of s, keeping the per-word flags aligned.
func trimSegment(s segment, n int) segment {
	if n == 0 {
		return s
	}
	cut := func(b []bool) []bool {
		if len(b) < n {
			return nil
		}
		return b[n:]
	}
	s.argv = s.argv[n:]
	s.dyn, s.udyn, s.meta = cut(s.dyn), cut(s.udyn), cut(s.meta)
	return s
}
