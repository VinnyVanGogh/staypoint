// Package names is the one normaliser for names that safety checks compare:
// branch names refused for deletion, organizations on hold, and task names
// that target production.
//
// Every one of those checks is a deny list: a name that normalises to a
// protected one is refused. So the folding is deliberately aggressive (letter
// case, every kind of whitespace, invisible characters, accents, compatibility
// forms such as fullwidth letters, and Latin lookalikes from Cyrillic and
// Greek). A false match costs a refusal or a Touch ID; a missed match lets
// "Mаin" (Cyrillic а) through. Do not use Normalize for allow lists (org
// trusts, rules): folding there widens what is allowed.
package names

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// confusables maps lower-case Cyrillic and Greek letters that look like a
// Latin letter to that letter. Upper-case forms reach it through
// unicode.ToLower. Compatibility forms (fullwidth, mathematical, the Kelvin
// sign, long s) are already folded by NFKD.
var confusables = map[rune]rune{
	// Cyrillic
	'а': 'a', 'в': 'b', 'е': 'e', 'ё': 'e', 'һ': 'h', 'і': 'i', 'ї': 'i',
	'ј': 'j', 'к': 'k', 'м': 'm', 'н': 'h', 'о': 'o', 'р': 'p', 'с': 'c',
	'ѕ': 's', 'т': 't', 'у': 'y', 'х': 'x', 'ԁ': 'd', 'ԛ': 'q', 'ԝ': 'w',
	'ӏ': 'l', 'ɡ': 'g',
	// Greek
	'α': 'a', 'β': 'b', 'ε': 'e', 'η': 'n', 'ι': 'i', 'κ': 'k', 'μ': 'u',
	'ν': 'v', 'ο': 'o', 'ρ': 'p', 'τ': 't', 'υ': 'u', 'χ': 'x', 'ζ': 'z',
	// Latin
	'ı': 'i', 'ȷ': 'j', 'ł': 'l', 'ø': 'o', 'đ': 'd', 'ħ': 'h',
}

// upperConfusables are capitals that look like a different Latin letter
// than their lower case does (Greek Μ is M but μ is u), so they are mapped
// before lower-casing.
var upperConfusables = map[rune]rune{
	'Μ': 'm', 'Η': 'h', 'Ν': 'n', 'Υ': 'y',
}

// Normalize folds s for comparison: compatibility decomposition (NFKD),
// combining marks, format characters (zero-width space and joiners, BOM, bidi
// controls, soft hyphen) and other control characters dropped, lookalike
// letters mapped to Latin, lower case, every run of whitespace collapsed to
// one ASCII space, and the result trimmed. It is idempotent.
func Normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	// Invalid bytes become U+FFFD first: NFKD leaves the rune after an
	// invalid byte undecomposed, which made Normalize not idempotent.
	for _, r := range norm.NFKD.String(strings.ToValidUTF8(s, string(utf8.RuneError))) {
		switch {
		case unicode.IsSpace(r):
			space = true
			continue
		case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Cc):
			continue
		}
		if c, ok := upperConfusables[r]; ok {
			r = c
		}
		r = unicode.ToLower(r)
		if c, ok := confusables[r]; ok {
			r = c
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return norm.NFC.String(b.String())
}

// Equal reports whether a and b are the same name once normalised.
func Equal(a, b string) bool {
	return Normalize(a) == Normalize(b)
}

// SplitCamel puts a space at each lower-to-upper case change and before the
// last capital of an upper-case run that starts a word ("deployToProd" ->
// "deploy To Prod", "DBProd" -> "DB Prod"), so word matching sees the parts
// of camel-cased names.
func SplitCamel(s string) string {
	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i, r := range rs {
		if i > 0 && unicode.IsUpper(r) {
			prev := rs[i-1]
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				b.WriteByte(' ')
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}
