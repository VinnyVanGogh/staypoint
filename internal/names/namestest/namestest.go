// Package namestest builds lookalike spellings of names for fuzz tests of
// code that matches names through names.Normalize.
package namestest

import (
	"strings"
	"unicode"
)

// lookalikes are spellings of lower-case ASCII letters (Cyrillic, Greek,
// fullwidth, and compatibility letters) that names.Normalize folds back.
var lookalikes = map[rune][]rune{
	'a': {0x0430, 0x0410, 0xFF41, 0x03B1}, 'b': {0x0432, 0xFF42}, 'c': {0x0441, 0x0421, 0xFF43},
	'd': {0x0501, 0xFF44}, 'e': {0x0435, 0x0415, 0xFF45, 0x03B5}, 'h': {0x04BB, 0x041D, 0x0397},
	'i': {0x0456, 0x0131, 0xFF49, 0x0130}, 'j': {0x0458}, 'k': {0x043A, 0x212A, 0x039A},
	'm': {0x041C, 0xFF4D, 0x039C}, 'n': {0xFF4E, 0x039D}, 'o': {0x043E, 0x041E, 0x03BF, 0xFF4F},
	'p': {0x0440, 0x0420, 0x03C1, 0xFF50}, 'r': {0xFF52}, 's': {0x0455, 0xFF53, 0x017F},
	't': {0x0442, 0x03A4, 0xFF54}, 'u': {0xFF55, 0x03C5}, 'v': {0x03BD}, 'x': {0x0445, 0x03C7},
	'y': {0x0443}, 'z': {0x0396},
}

var (
	zwsp    = string(rune(0x200B)) // zero-width space
	bom     = string(rune(0xFEFF))
	nbsp    = string(rune(0x00A0))
	ideoSp  = string(rune(0x3000)) // ideographic space
	diaeres = string(rune(0x0308)) // combining diaeresis
)

// ASCIIName keeps the ASCII letters (lower-cased), digits and single spaces
// of s: a fuzz input turned into a name Variant can rewrite.
func ASCIIName(s string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		switch {
		case r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			return unicode.ToLower(r)
		case r == ' ':
			return r
		}
		return -1
	}, s)), " ")
}

// Variant rewrites the lower-case ASCII name s using seed: upper case,
// lookalike letters, accents, zero-width characters and odd whitespace.
// names.Normalize(Variant(s, seed)) == names.Normalize(s) for every seed.
func Variant(s string, seed []byte) string {
	var b strings.Builder
	for i, r := range s {
		k := byte(0)
		if len(seed) > 0 {
			k = seed[i%len(seed)]
		}
		switch k % 6 {
		case 1:
			b.WriteRune(unicode.ToUpper(r))
			continue
		case 2:
			if alts := lookalikes[r]; len(alts) > 0 {
				b.WriteRune(alts[int(k/6)%len(alts)])
				continue
			}
		case 3:
			b.WriteString(zwsp)
		case 4:
			if unicode.IsLetter(r) {
				b.WriteRune(r)
				b.WriteString(diaeres)
				continue
			}
		case 5:
			if r == ' ' {
				b.WriteString(nbsp + ideoSp)
				continue
			}
		}
		b.WriteRune(r)
	}
	if len(seed) > 0 && seed[0]&0x80 != 0 {
		return nbsp + " " + b.String() + "\t" + bom
	}
	return b.String()
}
