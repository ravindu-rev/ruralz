// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package subst

import (
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// retype gives n, a string scalar that was exactly one expression, the
// rendered view's type at its position s (01 req 29): an integer position
// takes the YAML 1.2 core int grammar, a number position the core int or
// float grammar, a boolean position the core bool literals, and ByteSize
// and IntOrString an integer when the text is a core int. A string
// position, a position without schema and text that does not fit keep
// the string, which schema validation then reports naming the variable.
// The text is a variable's value, as long as the output limit allows, so
// every grammar check reads it in windows reported to read (01 req 53).
func retype(n *tree.Node, s *schemaidx.Node, read func(int)) {
	if s == nil {
		return
	}
	if sc := s.Scalar(); sc == schemaidx.ScalarByteSize || sc == schemaidx.ScalarIntOrString {
		setInt(n, read)
		return
	}
	t := s.Types()
	switch {
	case t == 0 || t.Has(schemaidx.TypeString):
	case t.Has(schemaidx.TypeNumber):
		// A core int outside int64 is not representable (01 req 12), as
		// in an authored literal: it stays a string rather than becoming a
		// double, and schema validation reports it naming the variable.
		if !setInt(n, read) && !intGrammar(n.Text, read) {
			if f, ok := coreFloat(n.Text, read); ok {
				n.Kind, n.Text = tree.KindFloat, f
			}
		}
	case t.Has(schemaidx.TypeInteger):
		setInt(n, read)
	case t.Has(schemaidx.TypeBoolean):
		if b, ok := coreBool(n.Text); ok {
			n.Kind, n.Text, n.Bool = tree.KindBool, "", b
		}
	default:
	}
}

// setInt makes n an integer when its text is a core int.
func setInt(n *tree.Node, read func(int)) bool {
	v, ok := coreInt(n.Text, read)
	if ok {
		n.Kind, n.Text = tree.KindInt, v
	}
	return ok
}

// maxIntDigits bounds the digits of an int64 without leading zeros in
// base 8, 10 or 16 (22 octal digits for the minimum).
const maxIntDigits = 22

// coreInt parses the YAML 1.2 core int grammar ([-+]?[0-9]+, 0o[0-7]+,
// 0x[0-9a-fA-F]+; leading zeros allowed, so 0777 is 777) and returns the
// normalized decimal; false outside the grammar or outside int64 (the
// range 01 req 12 admits). Leading zeros are skipped by a counted scan,
// so strconv.ParseInt reads at most maxIntDigits digits.
func coreInt(s string, read func(int)) (string, bool) {
	sign, digits, base, ok := splitInt(s, read)
	if !ok {
		return "", false
	}
	digits = trimZeros(digits, read)
	if len(digits) > maxIntDigits {
		return "", false
	}
	if digits == "" {
		digits = "0"
	}
	v, err := strconv.ParseInt(sign+digits, base, 64)
	if err != nil {
		return "", false
	}
	return strconv.FormatInt(v, 10), true
}

// intGrammar reports text in the core int grammar, whatever its range.
func intGrammar(s string, read func(int)) bool {
	_, _, _, ok := splitInt(s, read)
	return ok
}

// splitInt matches s against the core int grammar and returns its sign
// (base 10 only), its digits and their base.
func splitInt(s string, read func(int)) (sign, digits string, base int, ok bool) {
	digits, base = s, 10
	switch {
	case len(s) >= 2 && s[:2] == "0o":
		digits, base = s[2:], 8
	case len(s) >= 2 && s[:2] == "0x":
		digits, base = s[2:], 16
	case s != "" && (s[0] == '+' || s[0] == '-'):
		sign, digits = s[:1], s[1:]
	}
	return sign, digits, base, digits != "" && allDigits(digits, base, read)
}

// allDigits reports whether s holds only digits of base 8, 10 or 16,
// reading s in windows of textStep bytes.
func allDigits(s string, base int, read func(int)) bool {
	// counted: the digits are reported every textStep bytes.
	for i := 0; i < len(s); i++ {
		if i%textStep == 0 {
			read(min(textStep, len(s)-i))
		}
		c := s[i]
		var ok bool
		switch base {
		case 8:
			ok = c >= '0' && c <= '7'
		case 16:
			ok = (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		default:
			ok = c >= '0' && c <= '9'
		}
		if !ok {
			return false
		}
	}
	return true
}

// trimZeros returns s without its leading '0' bytes, reading them in
// windows of textStep bytes.
func trimZeros(s string, read func(int)) string {
	i := 0
	// counted: the zeros are reported every textStep bytes.
	for i < len(s) && s[i] == '0' {
		if i++; i%textStep == 0 {
			read(textStep)
		}
	}
	read(i % textStep)
	return s[i:]
}

// indexByte returns the index of the first c in s, or -1, reading s in
// windows of textStep bytes.
func indexByte(s string, c byte, read func(int)) int {
	return indexFrom(s, string(c), 0, read)
}

// coreFloat parses the finite YAML 1.2 core float grammar
// [-+]?(\.[0-9]+|[0-9]+(\.[0-9]*)?)([eE][-+]?[0-9]+)? and returns it in
// RFC 8259 number syntax, as the profile stores floats (architecture
// R-64): a leading + dropped, a 0 put before a leading '.', a '.' without
// fraction digits dropped and leading zeros of the integer part stripped
// keeping one digit. .inf and .nan are not JSON numbers and do not fit.
// Every scan and the copy of the result read the text in windows.
func coreFloat(s string, read func(int)) (string, bool) {
	sign := ""
	if s != "" && (s[0] == '-' || s[0] == '+') {
		if s[0] == '-' {
			sign = "-"
		}
		s = s[1:]
	}
	mant, exp := s, ""
	if i := min(uint(indexByte(s, 'e', read)), uint(indexByte(s, 'E', read))); i < uint(len(s)) {
		mant, exp = s[:i], s[i:]
		e := exp[1:]
		if e != "" && (e[0] == '+' || e[0] == '-') {
			e = e[1:]
		}
		if e == "" || !allDigits(e, 10, read) {
			return "", false
		}
	}
	intPart, frac := mant, ""
	if i := indexByte(mant, '.', read); i >= 0 {
		intPart, frac = mant[:i], mant[i+1:]
	}
	if !allDigits(intPart, 10, read) || !allDigits(frac, 10, read) || (intPart == "" && frac == "") {
		return "", false
	}
	intPart = trimZeros(intPart, read)
	if intPart == "" {
		intPart = "0"
	}
	var b strings.Builder
	b.Grow(len(sign) + len(intPart) + 1 + len(frac) + len(exp))
	appendText(&b, sign, read)
	appendText(&b, intPart, read)
	if frac != "" {
		appendText(&b, ".", read)
		appendText(&b, frac, read)
	}
	appendText(&b, exp, read)
	return b.String(), true
}

// coreBool parses the YAML 1.2 core bool literals.
func coreBool(s string) (value, ok bool) {
	switch s {
	case "true", "True", "TRUE":
		return true, true
	case "false", "False", "FALSE":
		return false, true
	default:
		return false, false
	}
}
