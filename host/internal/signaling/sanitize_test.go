package signaling

import (
	"strings"
	"testing"
)

// r spells a code point without putting the raw character in the source.
func r(cp rune) string { return string(cp) }

// TestL5DisplayNamesLoseControlAndBidiCharacters [SL-SEC-1].
func TestL5DisplayNamesLoseControlAndBidiCharacters(t *testing.T) {
	const (
		lre, rle, pdf, lro, rlo = 0x202A, 0x202B, 0x202C, 0x202D, 0x202E
		lri, rli, pdi           = 0x2066, 0x2067, 0x2069
		zwsp, bom, nbsp         = 0x200B, 0xFEFF, 0x00A0
	)
	for in, want := range map[string]string{
		"  Ada  ":                                  "Ada",
		"Ada" + r(rlo) + "evil" + r(pdf):           "Adaevil",
		r(lri) + "Bob" + r(pdi):                    "Bob",
		"Car" + r(zwsp) + "ol":                     "Carol",
		r(bom) + "Dan":                             "Dan",
		"Eve\x00\x07\x1b[31m":                      "Eve[31m",
		"Line\none\ttwo\r\n  three":                "Line one two three",
		"Frank   " + r(nbsp) + "  Lee":             "Frank Lee",
		r(0x65E5) + r(0x672C) + " " + r(0x540D):    r(0x65E5) + r(0x672C) + " " + r(0x540D),
		"\xff\xfeGarbled":                          "Garbled",
		r(lre) + r(rle) + r(lro) + r(rlo) + r(rli): "",
	} {
		if got := clampName(in); got != want {
			t.Errorf("clampName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := []rune(clampName(strings.Repeat(r(0xE9), 100))); len(got) != MaxNameRunes {
		t.Fatalf("length %d, want %d", len(got), MaxNameRunes)
	}
	if got := SanitizeDisplayText("abc def", 4); got != "abc" {
		t.Fatalf("truncation trims trailing space: %q", got)
	}
}
