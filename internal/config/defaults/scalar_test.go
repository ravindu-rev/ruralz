// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestIntegerText covers integerText, the integer text and the I-JSON
// range, on every shape of literal: plain and fractional integers,
// exponents that move the point either way, zeros, the digit limit, text
// outside the grammar, and exponents at and beyond the int range, which
// must decide the result before any arithmetic on them, so
// 1e9223372036854775807 and 1e-9223372036854775808 never panic.
func TestIntegerText(t *testing.T) {
	const (
		integer  = "integer"
		fraction = "not integral"
		large    = "too large"
		invalid  = "invalid"
	)
	tests := []struct {
		in, want, kind string
	}{
		// Integers and integral literals.
		{"1", "1", integer},
		{"-7", "-7", integer},
		{"1.0", "1", integer},
		{"1e3", "1000", integer},
		{"1E3", "1000", integer},
		{"1e+3", "1000", integer},
		{"100e-2", "1", integer},
		{"120e-1", "12", integer},
		{"10e-1", "1", integer},
		{"1.0e-0", "1", integer},
		{"0.5e16", "5000000000000000", integer},
		{"9e15", "9000000000000000", integer},
		{"1e15", "1000000000000000", integer},
		{"9007199254740991.000", "9007199254740991", integer},
		// Zeros, whatever the exponent.
		{"-0.0", "0", integer},
		{"0e999999999999999999999", "0", integer},
		{"0.0e-9223372036854775808", "0", integer},
		{"000e9223372036854775807", "0", integer},
		// Not integral.
		{"1.5", "", fraction},
		{"12e-1", "", fraction},
		{"100e-3", "", fraction},
		{"5e-1", "", fraction},
		{"1e-999999999999999999", "", fraction},
		{"1e-9223372036854775808", "", fraction},
		{"1.5e-9223372036854775808", "", fraction},
		{"-1e-9223372036854775807", "", fraction},
		// More digits than the canonical form keeps.
		{"1e16", "", large},
		{"1.5e16", "", large},
		{"0.5e17", "", large},
		{"1e999999999999999999999", "", large},
		{"1e9223372036854775800", "", large},
		{"1e9223372036854775807", "", large},
		{"-1e9223372036854775807", "", large},
		{"1E+9223372036854775807", "", large},
		{"1.5e9223372036854775807", "", large},
		{"10e9223372036854775807", "", large},
		// Outside the grammar.
		{"", "", invalid},
		{"a", "", invalid},
		{"1e", "", invalid},
		{"1e+", "", invalid},
		{"1.e3", "", invalid},
		{".5", "", invalid},
		{"1e3.5", "", invalid},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			// A non-integral value and text outside the grammar both give
			// ok and tooLarge false.
			wantOK, wantLarge := tc.kind == integer, tc.kind == large
			text, ok, tooLarge := integerText(tc.in)
			if text != tc.want || ok != wantOK || tooLarge != wantLarge {
				t.Errorf("integerText(%q) = %q, %v, %v; want %q, %v, %v (%s)", tc.in, text, ok, tooLarge, tc.want, wantOK, wantLarge, tc.kind)
			}
		})
	}
}

// TestClipBounded covers clip, which never converts the whole string to
// runes (64 MB for a 16 MiB value, once per message that quotes an
// authored value) and never returns a short value that is not UTF-8 as it
// is, raw bytes in a message: it gives what the whole-string conversion
// gives for short, exact, long, multi-byte and invalid UTF-8 values, and
// allocates only its result for a 16 MiB value.
func TestClipBounded(t *testing.T) {
	ref := func(s string) string {
		if len([]rune(s)) <= maxShown {
			return string([]rune(s))
		}
		return string([]rune(s)[:maxShown]) + "..."
	}
	for _, s := range []string{
		"", "90s", strings.Repeat("a", maxShown), strings.Repeat("a", maxShown+1), strings.Repeat("é", maxShown+1),
		strings.Repeat("\xff", maxShown+3), strings.Repeat("a", maxShown-1) + "\xffbc", strings.Repeat("\xff", maxShown),
		// Short values that are not UTF-8, which clip must not return as
		// they are, raw bytes in the message.
		"caf\xe9", "\xff", "a\xc3", "\xed\xa0\x80",
	} {
		if got, want := clip(s), ref(s); got != want || !utf8.ValidString(got) {
			t.Errorf("clip(%q) = %q, want %q, valid UTF-8", s, got, want)
		}
	}
	long := strings.Repeat("1", 16<<20)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := clip(long)
	runtime.ReadMemStats(&after)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 4096 || got != strings.Repeat("1", maxShown)+"..." {
		t.Errorf("clip of 16 MiB = %d bytes, allocating %d; want %d bytes and at most 4096", len(got), alloc, maxShown+3)
	}
}
