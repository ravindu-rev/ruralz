// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// durationSyntax reports whether s matches the $defs/Duration pattern
// ^(0|(([0-9]+(\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h))+)$, so a
// time.ParseDuration failure on it is an overflow, not a syntax error.
func durationSyntax(s string) bool {
	if s == "0" {
		return true
	}
	if s == "" {
		return false
	}
	for s != "" {
		whole := len(s) - len(strings.TrimLeft(s, "0123456789"))
		s = s[whole:]
		frac := 0
		dot := strings.HasPrefix(s, ".")
		if dot {
			s = s[1:]
			frac = len(s) - len(strings.TrimLeft(s, "0123456789"))
			s = s[frac:]
		}
		if whole == 0 && frac == 0 {
			return false
		}
		unit := durationUnit(s)
		if unit == 0 {
			return false
		}
		s = s[unit:]
	}
	return true
}

// durationUnit returns the byte length of the duration unit s starts
// with, or 0.
func durationUnit(s string) int {
	for _, u := range [...]string{"ns", "us", "µs", "μs", "ms", "s", "m", "h"} {
		if strings.HasPrefix(s, u) {
			return len(u)
		}
	}
	return 0
}

// shortestDecimal returns the shortest decimal equal to s, a $defs/Decimal
// value ^[0-9]+(\.[0-9]+)?$ (02 req 15): leading zeros of the integer part
// stripped keeping one digit, trailing fraction zeros and a bare '.'
// stripped ("3.00" is "3", "0.30" is "0.3", "007" is "7"). ok is false
// when s does not match the pattern.
func shortestDecimal(s string) (string, bool) {
	whole, frac, hasFrac := strings.Cut(s, ".")
	if !digits(whole) || (hasFrac && !digits(frac)) {
		return "", false
	}
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	frac = strings.TrimRight(frac, "0")
	if frac == "" {
		return whole, true
	}
	return whole + "." + frac, true
}

// digits reports a non-empty run of ASCII digits.
func digits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// maxIntegerDigits is the digit count of jsonval.MaxSafeInteger
// (9007199254740991): an integer with more digits is outside the I-JSON
// range.
const maxIntegerDigits = 16

// integerText returns the integer text of the RFC 8259 number literal s
// when its value is an integer ("1.0" is "1", "1e3" is "1000", "-0.0" is
// "0"), working on the digits so no float64 rounding or big exponent ever
// happens. tooLarge reports an integral value with more digits than any
// integer the canonical form keeps; ok is false for a non-integral value
// or text outside the grammar.
func integerText(s string) (text string, ok, tooLarge bool) {
	neg := strings.HasPrefix(s, "-")
	body := strings.TrimPrefix(s, "-")
	mant, expText, hasExp := strings.Cut(strings.ToLower(body), "e")
	whole, frac, hasFrac := strings.Cut(mant, ".")
	if !digits(whole) || (hasFrac && !digits(frac)) {
		return "", false, false
	}
	exp := 0
	if hasExp {
		unsigned := expText
		if unsigned != "" && (unsigned[0] == '+' || unsigned[0] == '-') {
			unsigned = unsigned[1:]
		}
		if !digits(unsigned) {
			return "", false, false
		}
		e, err := strconv.Atoi(expText)
		if err != nil {
			// An exponent beyond the int range: the value is 0 or huge.
			if strings.TrimLeft(whole+frac, "0") == "" {
				return "0", true, false
			}
			if strings.HasPrefix(expText, "-") {
				return "", false, false
			}
			return "", false, true
		}
		exp = e
	}
	// value = d × 10^(exp − len(frac)) with d the digit string.
	d := strings.TrimLeft(whole+frac, "0")
	if d == "" {
		return "0", true, false
	}
	shift := exp - len(frac)
	if shift < 0 {
		trimmed := strings.TrimRight(d, "0")
		if len(d)-len(trimmed) < -shift {
			return "", false, false
		}
		d = d[:len(d)+shift]
		shift = 0
	}
	if len(d)+shift > maxIntegerDigits {
		return "", false, true
	}
	text = d + strings.Repeat("0", shift)
	if neg {
		text = "-" + text
	}
	return text, true, false
}

// lowerASCII lower-cases the ASCII letters of s, allocating only when s
// holds an upper-case letter.
func lowerASCII(s string) string {
	i := strings.IndexFunc(s, func(r rune) bool { return r >= 'A' && r <= 'Z' })
	if i < 0 {
		return s
	}
	b := []byte(s)
	for j := i; j < len(b); j++ {
		if c := b[j]; c >= 'A' && c <= 'Z' {
			b[j] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// maxShown bounds the code points of a value quoted in a message.
const maxShown = 64

// clip shortens s for a message.
func clip(s string) string {
	if utf8.RuneCountInString(s) <= maxShown {
		return s
	}
	r := []rune(s)
	return string(r[:maxShown]) + "..."
}
