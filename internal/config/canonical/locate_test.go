// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// sameAsJSONVal asks the locator and jsonval.Locator for the positions of
// offs, in that order, over data, and reports the first difference.
func sameAsJSONVal(t *testing.T, name string, data []byte, offs []int) {
	t.Helper()
	ours, ref := newLocator(data), jsonval.NewLocator(data)
	for _, off := range offs {
		if got, want := ours.Position(off), ref.Position(off); got != want {
			t.Errorf("%s: Position(%d) = %+v, want %+v (jsonval)", name, off, got, want)
			return
		}
	}
}

// every returns the offsets -1 to len(data)+1 in increasing order.
func every(data []byte) []int {
	offs := make([]int, 0, len(data)+3)
	for off := -1; off <= len(data)+1; off++ {
		offs = append(offs, off)
	}
	return offs
}

// TestLocatorMatchesJSONVal: the decoder's positions, which locate every
// finding in the content and every node of the decoded trees, are
// jsonval's: in the leading printable run, past it, across every word
// boundary of the eight-byte fast path, for line breaks, multi-byte and
// invalid UTF-8, offsets past either end and offsets that go backwards.
func TestLocatorMatchesJSONVal(t *testing.T) {
	word := "abcdefgh"
	cases := map[string]string{
		"empty":                 "",
		"short ASCII":           "abc",
		"canonical line":        doc(`{"kind":"Route","metadata":{"name":"r"},"spec":{"timeout":"4s"}}`),
		"LF":                    "ab\ncd",
		"CR":                    "ab\rcd",
		"CRLF":                  "ab\r\ncd",
		"LF first":              "\nabcdefghijklmnop",
		"CR LF LF":              "a\r\n\nb",
		"lone CR at end":        "abcdefghijk\r",
		"tab and DEL":           "a\tb\x7fcdefghijklmn",
		"control bytes":         "\x00\x01\x1f\x20abcdefghij",
		"two-byte rune":         "abcdefghé" + word + "\n" + word,
		"three-byte rune":       word + "€" + word + "€",
		"four-byte rune":        "😀" + word + word,
		"invalid UTF-8":         word + "\xff\xfe" + word + "\xc3",
		"truncated rune at end": word + "\xe2\x82",
		"long ASCII after rune": "é" + strings.Repeat(word, 9) + "\r\n" + strings.Repeat(word, 3),
	}
	// Each byte that leaves the fast path, at each position of a word,
	// before and after the leading run ends.
	for _, b := range []string{"\n", "\r", "\x1f", "\x80", "é"} {
		for i := range 9 {
			cases["word "+strconv.Quote(b)+" at "+strconv.Itoa(i)] = strings.Repeat("x", i) + b + strings.Repeat(word, 3) + b + word
		}
	}
	for name, s := range cases {
		data := []byte(s)
		sameAsJSONVal(t, name, data, every(data))
		// Backwards and repeated queries restart the walk.
		back := every(data)
		for i, j := 0, len(back)-1; i < j; i, j = i+1, j-1 {
			back[i], back[j] = back[j], back[i]
		}
		sameAsJSONVal(t, name+" backwards", data, back)
		sameAsJSONVal(t, name+" jumps", data, []int{len(data), 0, len(data) / 2, len(data) / 2, 1, len(data) + 5, -3})
	}
}

// TestLocatorRandom compares the locator with jsonval.Locator on random
// inputs mixing printable ASCII, line breaks, control bytes and runes, at
// random offsets mostly in increasing order (seeded, so it is
// deterministic).
func TestLocatorRandom(t *testing.T) {
	r := rand.New(rand.NewPCG(39, 77)) //nolint:gosec // G404: deterministic test data
	pieces := []string{"a", "bcdefghijklmnopq", "\n", "\r", "\r\n", "\t", "é", "€", "😀", "\xff", "\x1f", " ", "\x7f"}
	for i := range 2000 {
		var b strings.Builder
		for range r.IntN(40) {
			b.WriteString(pieces[r.IntN(len(pieces))])
		}
		data := []byte(b.String())
		offs := make([]int, 0, 16)
		for range 16 {
			if r.IntN(5) == 0 {
				offs = append(offs, r.IntN(len(data)+3)-1)
				continue
			}
			last := 0
			if len(offs) > 0 {
				last = max(0, offs[len(offs)-1])
			}
			offs = append(offs, last+r.IntN(12))
		}
		sameAsJSONVal(t, "random "+strconv.Itoa(i), data, offs)
	}
}

// FuzzLocator: the locator and jsonval.Locator agree on every input and
// offset sequence.
func FuzzLocator(f *testing.F) {
	f.Add([]byte(doc(`{"kind":"Route","metadata":{"name":"r"},"spec":{}}`)), []byte{3, 9, 200, 1})
	f.Add([]byte("abcdefgh\r\nijklmnopé€😀\xff\rqrstuvwx\n"), []byte{0, 7, 8, 9, 10, 11, 20, 21, 22, 40, 2})
	f.Fuzz(func(t *testing.T, data, steps []byte) {
		offs := make([]int, len(steps))
		off := 0
		for i, s := range steps {
			if s >= 0xf0 {
				// A jump back.
				off = int(s - 0xf0)
			} else {
				off += int(s)
			}
			offs[i] = off
		}
		sameAsJSONVal(t, "fuzz", data, offs)
	})
}
