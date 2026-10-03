// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

// referenceCompareUTF16 is the obvious implementation: encode both strings
// to UTF-16 and compare code units.
func referenceCompareUTF16(a, b string) int {
	return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b)))
}

func TestCompareUTF16(t *testing.T) {
	// RFC 8785 section 3.2.3 and 02 test plan item 4.
	names := []string{"\u20ac", "\r", "\ufb33", "1", "\u1F600", "\u0080", "\u00f6"}
	for i, n := range names {
		names[i] = strings.NewReplacer("\u20ac", "€", "\ufb33", "דּ", "\u1F600", "\U0001F600", "\u0080", "\u0080", "\u00f6", "ö").Replace(n)
	}
	slices.SortFunc(names, CompareUTF16)
	want := []string{"\r", "1", "\u0080", "ö", "€", "\U0001F600", "דּ"}
	if !slices.Equal(names, want) {
		t.Fatalf("order = %q, want %q", names, want)
	}
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"a", "", 1},
		{"", "a", -1},
		{"abc", "abd", -1},
		{"ab", "abc", -1},
		{"\U0001F600", "￿", -1},
		{"￿", "\U0001F600", 1},
		{"", "\U00010000", 1},
		{"\U0001F600", "\U0001F601", -1},
		{"\U00010400", "\U0001F600", -1},
		{"x\U0001F600", "xדּ", -1},
		{"é", "ê", -1},
		{"é", "e", 1},
		{"a\xffb", "a\xffc", -1},
		{"a\xff", "a\xfe", 1},
		{"\xff", "�", 1},
	} {
		if got := CompareUTF16(c.a, c.b); got != c.want {
			t.Fatalf("CompareUTF16(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if utf8.ValidString(c.a) && utf8.ValidString(c.b) {
			if ref := referenceCompareUTF16(c.a, c.b); ref != c.want {
				t.Fatalf("reference(%q, %q) = %d, want %d", c.a, c.b, ref, c.want)
			}
		}
	}
}

func TestCompareUTF16Random(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // G404: deterministic test data
	alphabet := []rune{'a', 'b', 0x7f, 0x80, 0x7ff, 0x800, 0xd7ff, 0xe000, 0xfb33, 0xffff, 0x10000, 0x1f600, 0x10ffff}
	gen := func() string {
		var b strings.Builder
		for range r.IntN(5) {
			b.WriteRune(alphabet[r.IntN(len(alphabet))])
		}
		return b.String()
	}
	for range 20000 {
		a, b := gen(), gen()
		if got, want := CompareUTF16(a, b), referenceCompareUTF16(a, b); got != want {
			t.Fatalf("CompareUTF16(%q, %q) = %d, want %d", a, b, got, want)
		}
	}
}
