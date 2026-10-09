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
func retype(n *tree.Node, s *schemaidx.Node) {
	if s == nil {
		return
	}
	if sc := s.Scalar(); sc == schemaidx.ScalarByteSize || sc == schemaidx.ScalarIntOrString {
		setInt(n)
		return
	}
	t := s.Types()
	switch {
	case t == 0 || t.Has(schemaidx.TypeString):
	case t.Has(schemaidx.TypeNumber):
		// A core int outside int64 is not representable (01 req 12), as
		// in an authored literal: it stays a string rather than becoming a
		// double, and schema validation reports it naming the variable.
		if !setInt(n) && !intGrammar(n.Text) {
			if f, ok := coreFloat(n.Text); ok {
				n.Kind, n.Text = tree.KindFloat, f
			}
		}
	case t.Has(schemaidx.TypeInteger):
		setInt(n)
	case t.Has(schemaidx.TypeBoolean):
		if b, ok := coreBool(n.Text); ok {
			n.Kind, n.Text, n.Bool = tree.KindBool, "", b
		}
	default:
	}
}

// setInt makes n an integer when its text is a core int.
func setInt(n *tree.Node) bool {
	v, ok := coreInt(n.Text)
	if ok {
		n.Kind, n.Text = tree.KindInt, v
	}
	return ok
}

// coreInt parses the YAML 1.2 core int grammar ([-+]?[0-9]+, 0o[0-7]+,
// 0x[0-9a-fA-F]+; leading zeros allowed, so 0777 is 777) and returns the
// normalized decimal; false outside the grammar or outside int64 (the
// range 01 req 12 admits).
func coreInt(s string) (string, bool) {
	text, base, ok := splitInt(s)
	if !ok {
		return "", false
	}
	v, err := strconv.ParseInt(text, base, 64)
	if err != nil {
		return "", false
	}
	return strconv.FormatInt(v, 10), true
}

// intGrammar reports text in the core int grammar, whatever its range.
func intGrammar(s string) bool {
	_, _, ok := splitInt(s)
	return ok
}

// splitInt matches s against the core int grammar and returns the text
// strconv.ParseInt reads in base (the 0o or 0x prefix removed).
func splitInt(s string) (text string, base int, ok bool) {
	text, base = s, 10
	switch {
	case strings.HasPrefix(s, "0o"):
		text, base = s[2:], 8
	case strings.HasPrefix(s, "0x"):
		text, base = s[2:], 16
	}
	digits := text
	if base == 10 && digits != "" && (digits[0] == '+' || digits[0] == '-') {
		digits = digits[1:]
	}
	return text, base, digits != "" && allDigits(digits, base)
}

// allDigits reports whether s holds only digits of base 8, 10 or 16.
func allDigits(s string, base int) bool {
	for i := 0; i < len(s); i++ {
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

// coreFloat parses the finite YAML 1.2 core float grammar
// [-+]?(\.[0-9]+|[0-9]+(\.[0-9]*)?)([eE][-+]?[0-9]+)? and returns it in
// RFC 8259 number syntax, as the profile stores floats (architecture
// R-64): a leading + dropped, a 0 put before a leading '.', a '.' without
// fraction digits dropped and leading zeros of the integer part stripped
// keeping one digit. .inf and .nan are not JSON numbers and do not fit.
func coreFloat(s string) (string, bool) {
	sign := ""
	switch {
	case strings.HasPrefix(s, "-"):
		sign, s = "-", s[1:]
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}
	mant, exp := s, ""
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mant, exp = s[:i], s[i:]
		e := strings.TrimPrefix(strings.TrimPrefix(exp[1:], "+"), "-")
		if len(exp[1:])-len(e) > 1 || e == "" || !allDigits(e, 10) {
			return "", false
		}
	}
	intPart, frac, _ := strings.Cut(mant, ".")
	if !allDigits(intPart, 10) || !allDigits(frac, 10) || (intPart == "" && frac == "") {
		return "", false
	}
	intPart = strings.TrimLeft(intPart, "0")
	if intPart == "" {
		intPart = "0"
	}
	out := sign + intPart
	if frac != "" {
		out += "." + frac
	}
	return out + exp, true
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
