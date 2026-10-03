// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// MaxSafeInteger is 2^53−1, the largest integer every IEEE 754 double
// represents exactly. RFC 8785 serializes numbers as doubles, so the
// canonical form rounds integer literals beyond ±MaxSafeInteger, and
// authored configuration refuses them (02 req 16; CheckNumber).
const MaxSafeInteger = 1<<53 - 1

// ValidNumber reports whether s is an RFC 8259 number literal.
func ValidNumber(s string) bool {
	if s == "" {
		return false
	}
	// scanNumber reads a prefix; the literal must be all of s.
	end, bad := scanNumber(s, 0)
	return bad < 0 && end == len(s)
}

// AppendFloat appends f in the ECMAScript Number.prototype.toString form
// that RFC 8785 section 3.2.2.3 mandates: shortest round-trip digits, "0"
// for ±0, plain decimal notation when 1e-6 <= |f| < 1e21, otherwise
// exponent notation with an explicit exponent sign ("1e+21", "1e-7"). NaN
// and infinities are ErrNumberRange (02 req 24; 07 req 60).
func AppendFloat(dst []byte, f float64) ([]byte, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return dst, ErrNumberRange
	}
	if f == 0 {
		return append(dst, '0'), nil
	}
	if f < 0 {
		dst = append(dst, '-')
		f = -f
	}
	// Shortest round-trip digits in scientific form: d[.ddd]e±XX.
	var buf [32]byte
	sci := strconv.AppendFloat(buf[:0], f, 'e', -1, 64)
	var digits [24]byte
	k, e := 0, 0
	for ; sci[e] != 'e'; e++ {
		if sci[e] != '.' {
			digits[k] = sci[e]
			k++
		}
	}
	exp := 0
	for _, c := range sci[e+2:] {
		exp = exp*10 + int(c-'0')
	}
	if sci[e+1] == '-' {
		exp = -exp
	}
	// The value is 0.d1..dk × 10^n.
	n := exp + 1
	d := digits[:k]
	switch {
	case k <= n && n <= 21:
		dst = append(dst, d...)
		for range n - k {
			dst = append(dst, '0')
		}
	case 0 < n && n <= 21:
		dst = append(dst, d[:n]...)
		dst = append(dst, '.')
		dst = append(dst, d[n:]...)
	case -6 < n && n <= 0:
		dst = append(dst, '0', '.')
		for range -n {
			dst = append(dst, '0')
		}
		dst = append(dst, d...)
	default:
		dst = append(dst, d[0])
		if k > 1 {
			dst = append(dst, '.')
			dst = append(dst, d[1:]...)
		}
		dst = append(dst, 'e')
		if n-1 < 0 {
			dst = append(dst, '-')
			dst = strconv.AppendInt(dst, int64(1-n), 10)
		} else {
			dst = append(dst, '+')
			dst = strconv.AppendInt(dst, int64(n-1), 10)
		}
	}
	return dst, nil
}

// AppendCanonicalNumber appends the RFC 8785 form of the number literal lit
// (02 req 24): the literal read as an IEEE 754 double and written by
// AppendFloat, as encoding/json/jsontext canonicalizes, so "1.0" is "1",
// "1e21" is "1e+21" and the integer literal "9007199254740993" is
// "9007199254740992". Integer literals within ±MaxSafeInteger take an exact
// decimal path ("-0" is "0"). The form is total over finite literals and
// idempotent: its output is a literal whose canonical form is itself (02 req
// 26). A literal outside the grammar is ErrInvalidNumber and one that
// overflows a double ErrNumberRange. Authored configuration must refuse the
// integer literals a double rounds before canonicalizing; CheckNumber
// applies that rule (02 req 16).
func AppendCanonicalNumber(dst []byte, lit json.Number) ([]byte, error) {
	s := string(lit)
	if !ValidNumber(s) {
		return dst, ErrInvalidNumber
	}
	if u, ok := safeInteger(s); ok {
		if u != 0 && s[0] == '-' {
			dst = append(dst, '-')
		}
		return strconv.AppendUint(dst, u, 10), nil
	}
	f, ok := finiteDouble(s)
	if !ok {
		return dst, ErrNumberRange
	}
	return AppendFloat(dst, f)
}

// CheckNumber applies the 02 req 16 number rule to the literal lit: the
// check stage G makes on authored numbers before canonicalization
// (RZ-CFG-005; 02 test plan item 3). It returns nil when the canonical form
// keeps the literal's value as a double does, ErrInvalidNumber outside the
// RFC 8259 grammar, and ErrNumberRange for an integer literal (no '.', 'e'
// or 'E') outside ±MaxSafeInteger, which AppendCanonicalNumber would round,
// or for a literal that overflows a double.
func CheckNumber(lit json.Number) error {
	s := string(lit)
	if !ValidNumber(s) {
		return ErrInvalidNumber
	}
	if !strings.ContainsAny(s, ".eE") {
		if _, ok := safeInteger(s); !ok {
			return ErrNumberRange
		}
		return nil
	}
	if _, ok := finiteDouble(s); !ok {
		return ErrNumberRange
	}
	return nil
}

// safeInteger returns the magnitude of a valid number literal that is an
// integer literal within ±MaxSafeInteger.
func safeInteger(s string) (uint64, bool) {
	digits := strings.TrimPrefix(s, "-")
	if len(digits) > len("9007199254740991") || strings.ContainsAny(digits, ".eE") {
		return 0, false
	}
	u, err := strconv.ParseUint(digits, 10, 64)
	return u, err == nil && u <= MaxSafeInteger
}

// finiteDouble reads a valid number literal as a double; ok is false when it
// overflows. Underflow reads as zero.
func finiteDouble(s string) (float64, bool) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && (math.IsInf(f, 0) || math.IsNaN(f)) {
		return 0, false
	}
	return f, true
}
