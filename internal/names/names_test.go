package names

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/VinnyVanGogh/staypoint/internal/names/namestest"
)

// Invisible and space characters, spelled as code points so the source
// shows them.
var (
	zwsp     = string(rune(0x200B)) // zero-width space
	zwj      = string(rune(0x200D)) // zero-width joiner
	bom      = string(rune(0xFEFF))
	shy      = string(rune(0x00AD)) // soft hyphen
	rlo      = string(rune(0x202E)) // right-to-left override
	nbsp     = string(rune(0x00A0))
	ideoSp   = string(rune(0x3000)) // ideographic space
	diaeres  = string(rune(0x0308)) // combining diaeresis
	kelvin   = string(rune(0x212A))
	cyrA     = string(rune(0x0430)) // а
	cyrCapA  = string(rune(0x0410)) // А
	greekMu  = string(rune(0x039C)) // Μ
	fullMain = string([]rune{0xFF4D, 0xFF41, 0xFF49, 0xFF4E})
	boldProd = string([]rune{0x1D429, 0x1D42B, 0x1D428, 0x1D41D})
)

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"main":                          "main",
		"  Main\t":                      "main",
		"MAIN":                          "main",
		"m" + cyrA + "in":               "main",
		"M" + cyrCapA + "IN":            "main",
		fullMain:                        "main",
		"ma" + zwsp + "in":              "main",
		bom + "main" + zwj:              "main",
		"ma" + shy + "in":               "main",
		"mai" + diaeres + "n":           "main",
		"maïn":                          "main",
		rlo + "main":                    "main",
		"Managed" + nbsp + "Solution":   "managed solution",
		"Managed  \n Solution":          "managed solution",
		greekMu + "anaged Solution":     "managed solution",
		boldProd:                        "prod",
		string(rune(0x0130)):            "i", // İ
		kelvin:                          "k",
		"":                              "",
		" " + zwsp + " ":                "",
		"feature/x":                     "feature/x",
		"ACme\x000":                     "acme0", // NUL dropped, never a cut
		"main" + ideoSp + "line" + zwsp: "main line",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
	if Equal("main", "mainline") || Equal("prod", "product") {
		t.Error("distinct names must stay distinct")
	}
}

func TestSplitCamel(t *testing.T) {
	cases := map[string]string{
		"deployToProd": "deploy To Prod",
		"DBProd":       "DB Prod",
		"PRODUCTION":   "PRODUCTION",
		"v2Prod":       "v2 Prod",
		"prod":         "prod",
		"":             "",
	}
	for in, want := range cases {
		if got := SplitCamel(in); got != want {
			t.Errorf("SplitCamel(%q) = %q, want %q", in, got, want)
		}
	}
}

func FuzzNormalize(f *testing.F) {
	for _, s := range []string{"main", "Managed Solution", "m" + cyrA + "in", fullMain, "ma" + zwsp + "in", "\xff\xfe", "a" + diaeres, "ß", "ς"} {
		f.Add(s, []byte{1, 2, 3, 0x84})
	}
	f.Fuzz(func(t *testing.T, s string, seed []byte) {
		n := Normalize(s)
		if !utf8.ValidString(n) {
			t.Fatalf("Normalize(%q) = %q: invalid UTF-8", s, n)
		}
		if again := Normalize(n); again != n {
			t.Fatalf("not idempotent: Normalize(%q) = %q, Normalize(that) = %q", s, n, again)
		}
		if n != strings.TrimSpace(n) || strings.Contains(n, "  ") {
			t.Fatalf("Normalize(%q) = %q: whitespace not collapsed", s, n)
		}
		for _, r := range n {
			if unicode.IsSpace(r) && r != ' ' {
				t.Fatalf("Normalize(%q) = %q: non-ASCII space kept", s, n)
			}
			if unicode.In(r, unicode.Cf, unicode.Mn, unicode.Cc) {
				t.Fatalf("Normalize(%q) = %q: invisible %U kept", s, n, r)
			}
		}
		if !Equal(s, zwsp+" "+s+ideoSp) {
			t.Fatalf("padding changed %q", s)
		}
		a := namestest.ASCIIName(s)
		if v := namestest.Variant(a, seed); !Equal(v, a) || Normalize(a) != a {
			t.Fatalf("Variant %q of %q normalises to %q, want %q", v, a, Normalize(v), a)
		}
	})
}
