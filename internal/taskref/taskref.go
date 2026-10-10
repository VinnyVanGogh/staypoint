// Package taskref is the human task reference: an organization key plus a
// per-organization sequence number (STA-123, MAN-45), and the decorative slug
// that follows it in a task URL (/STA-123/playwright-ui-specs).
//
// The internal id (task-xxxxxxxx) stays the primary key everywhere; a
// reference is only a display and URL identifier. Lookups use the number
// alone, so a renamed task or a stale slug still resolves.
package taskref

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// DefaultBaseURL is where the daemon's web UI is reached from this machine.
// StayPoint links use localhost, never 127.0.0.1 (browsers flag it as an
// invalid domain). STAYPOINT_BASE_URL overrides it (task-f9e79cee: a stable
// local hostname).
const DefaultBaseURL = "http://localhost:41421"

// orgKeys maps organization names to their key. The keys match the Paperclip
// issue prefixes, so an imported label (STA-775) names the same organization.
var orgKeys = map[string]string{
	"staypoint":        "STA",
	"managed solution": "MAN",
	"research":         "RES",
	"maintenance":      "PER",
	"runelite":         "RUN",
}

var keyRe = regexp.MustCompile(`^[A-Z]{2,5}$`)

// OrgKey returns the reference prefix for an organization. A task with no
// organization is filed under StayPoint, as the fleet view does. An unknown
// organization uses the first three letters of its name; one that is already
// a key (STA) keeps it. Two organizations whose names derive the same key
// share one sequence, which keeps every reference unique.
func OrgKey(org string) string {
	org = strings.TrimSpace(org)
	if org == "" {
		return "STA"
	}
	if k, ok := orgKeys[strings.ToLower(org)]; ok {
		return k
	}
	if keyRe.MatchString(org) {
		return org
	}
	var b strings.Builder
	for _, r := range org {
		if r < unicode.MaxASCII && unicode.IsLetter(r) {
			b.WriteRune(unicode.ToUpper(r))
			if b.Len() == 3 {
				break
			}
		}
	}
	if b.Len() < 2 {
		return "TSK"
	}
	return b.String()
}

// refRe is a reference as typed: key, hyphen, number. Case-insensitive so a
// hand-typed sta-12 still resolves.
var refRe = regexp.MustCompile(`^([A-Za-z]{2,5})-([1-9][0-9]{0,8})$`)

// Parse splits "STA-123" into its upper-cased key and number.
func Parse(s string) (key string, number int, ok bool) {
	m := refRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return "", 0, false
	}
	return strings.ToUpper(m[1]), n, true
}

// Format joins a key and number: Format("STA", 123) = "STA-123".
func Format(key string, number int) string {
	return fmt.Sprintf("%s-%d", key, number)
}

// legacyLabelRe is the "[STA-775] " prefix the Paperclip import puts on a
// title; it is not part of the slug.
var legacyLabelRe = regexp.MustCompile(`^\s*\[[A-Za-z]+-[0-9]+\]\s*`)

// slugWords caps a slug at five words: enough to recognise the task in a
// pasted link, short enough to type.
const slugWords = 5

// Slugify makes the URL slug for a task name: lowercase ASCII words joined by
// hyphens, at most five. Accented Latin letters lose their accent; other
// non-ASCII text is dropped. Deterministic, so the same name always gives
// the same slug. May be empty (a name with no ASCII letters or digits).
func Slugify(name string) string {
	name = legacyLabelRe.ReplaceAllString(name, "")
	var words []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
	}
	for _, r := range strings.ToLower(name) {
		if f, ok := fold[r]; ok {
			r = f
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			cur.WriteRune(r)
		case r == '\'' || r == '’':
			// "don't" -> "dont", not "don-t".
		default:
			flush()
		}
		if len(words) == slugWords {
			break
		}
	}
	flush()
	if len(words) > slugWords {
		words = words[:slugWords]
	}
	return strings.Join(words, "-")
}

// fold strips the accent from common Latin letters.
var fold = map[rune]rune{
	'à': 'a', 'á': 'a', 'â': 'a', 'ã': 'a', 'ä': 'a', 'å': 'a',
	'ç': 'c', 'è': 'e', 'é': 'e', 'ê': 'e', 'ë': 'e',
	'ì': 'i', 'í': 'i', 'î': 'i', 'ï': 'i', 'ñ': 'n',
	'ò': 'o', 'ó': 'o', 'ô': 'o', 'õ': 'o', 'ö': 'o', 'ø': 'o',
	'ù': 'u', 'ú': 'u', 'û': 'u', 'ü': 'u', 'ý': 'y', 'ÿ': 'y',
}

// Path is the canonical web path for a reference: /STA-123/slug, or /STA-123
// when the slug is empty.
func Path(ref, slug string) string {
	if slug == "" {
		return "/" + ref
	}
	return "/" + ref + "/" + slug
}

// BaseURL is the web UI base URL links are printed with.
func BaseURL() string {
	if u := strings.TrimRight(strings.TrimSpace(os.Getenv("STAYPOINT_BASE_URL")), "/"); u != "" {
		return u
	}
	return DefaultBaseURL
}

// URL is the full link for a reference: http://localhost:41421/STA-123/slug.
func URL(ref, slug string) string {
	return BaseURL() + Path(ref, slug)
}
