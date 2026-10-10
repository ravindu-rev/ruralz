// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// The YAML 1.2 core schema (YAML 1.2.2 section 10.3.2) resolves a plain
// scalar by its text alone. Ruralz owns this resolution (01 req 12): goccy
// types 0777 as octal, 1_000 and 0b1 as integers and 1e3 as a string, so
// its token types are never consulted.

// scalarClass is the core schema class of a plain scalar's text.
type scalarClass uint8

const (
	classString scalarClass = iota
	classNull
	classBool
	classInt
	classFloat
	classInf
	classNaN
)

// classify returns the core schema class of plain scalar text.
func classify(s string) scalarClass {
	switch s {
	case "", "~", "null", "Null", "NULL":
		return classNull
	case "true", "True", "TRUE", "false", "False", "FALSE":
		return classBool
	case ".nan", ".NaN", ".NAN":
		return classNaN
	}
	if isCoreInt(s) {
		return classInt
	}
	if isCoreFloat(s) {
		return classFloat
	}
	if isCoreInf(s) {
		return classInf
	}
	return classString
}

// isCoreInt reports whether s matches [-+]?[0-9]+, 0o[0-7]+ or
// 0x[0-9a-fA-F]+.
func isCoreInt(s string) bool {
	switch {
	case strings.HasPrefix(s, "0o"):
		return len(s) > 2 && allDigits(s[2:], isOctal)
	case strings.HasPrefix(s, "0x"):
		return len(s) > 2 && allDigits(s[2:], isHex)
	}
	if s != "" && (s[0] == '-' || s[0] == '+') {
		s = s[1:]
	}
	return s != "" && allDigits(s, isDecimal)
}

// isCoreFloat reports whether s matches
// [-+]?(\.[0-9]+|[0-9]+(\.[0-9]*)?)([eE][-+]?[0-9]+)?.
func isCoreFloat(s string) bool {
	if s != "" && (s[0] == '-' || s[0] == '+') {
		s = s[1:]
	}
	intDigits := leadingDigits(s)
	s = s[intDigits:]
	fracDigits := 0
	dot := s != "" && s[0] == '.'
	if dot {
		s = s[1:]
		fracDigits = leadingDigits(s)
		s = s[fracDigits:]
	}
	switch {
	case intDigits == 0 && (!dot || fracDigits == 0):
		return false
	case s == "":
		return true
	case s[0] != 'e' && s[0] != 'E':
		return false
	}
	s = s[1:]
	if s != "" && (s[0] == '-' || s[0] == '+') {
		s = s[1:]
	}
	return s != "" && allDigits(s, isDecimal)
}

// isCoreInf reports whether s matches [-+]?\.(inf|Inf|INF).
func isCoreInf(s string) bool {
	if s != "" && (s[0] == '-' || s[0] == '+') {
		s = s[1:]
	}
	return s == ".inf" || s == ".Inf" || s == ".INF"
}

func isDecimal(c byte) bool { return c >= '0' && c <= '9' }
func isOctal(c byte) bool   { return c >= '0' && c <= '7' }
func isHex(c byte) bool     { return isDecimal(c) || (c|0x20 >= 'a' && c|0x20 <= 'f') }

func allDigits(s string, ok func(byte) bool) bool {
	for i := 0; i < len(s); i++ {
		if !ok(s[i]) {
			return false
		}
	}
	return true
}

func leadingDigits(s string) int {
	i := 0
	for i < len(s) && isDecimal(s[i]) {
		i++
	}
	return i
}

// normalizeInt returns the normalized decimal text of a core schema
// integer (0o17 is 15, -0 is 0, +007 is 7) and reports whether it fits in
// int64; an integer outside int64 is not representable in the JSON data
// model the rest of the pipeline uses (01 req 12).
func normalizeInt(s string) (string, bool) {
	switch {
	case strings.HasPrefix(s, "0o"):
		return parseUnsigned(s[2:], 8)
	case strings.HasPrefix(s, "0x"):
		return parseUnsigned(s[2:], 16)
	}
	neg := false
	if s[0] == '-' || s[0] == '+' {
		neg = s[0] == '-'
		s = s[1:]
	}
	s = strings.TrimLeft(s, "0")
	if s == "" {
		return "0", true
	}
	if neg {
		s = "-" + s
	}
	// Leading zeros are gone, so a literal of more than 20 characters is
	// out of range without parsing it.
	if len(s) > 20 {
		return s, false
	}
	if _, err := strconv.ParseInt(s, 10, 64); err != nil {
		return s, false
	}
	return s, true
}

// parseUnsigned converts octal or hexadecimal digits to decimal text,
// reporting whether the value fits in int64.
func parseUnsigned(digits string, base int) (string, bool) {
	trimmed := strings.TrimLeft(digits, "0")
	if trimmed == "" {
		return "0", true
	}
	if len(trimmed) > 22 {
		return "0" + string(prefixOf(base)) + digits, false
	}
	v, err := strconv.ParseInt(trimmed, base, 64)
	if err != nil {
		return "0" + string(prefixOf(base)) + digits, false
	}
	return strconv.FormatInt(v, 10), true
}

func prefixOf(base int) byte {
	if base == 8 {
		return 'o'
	}
	return 'x'
}

// normalizeFloat rewrites core schema float text in RFC 8259 number syntax
// without passing it through float64 (architecture R-64): it drops a
// leading '+', puts a '0' before a leading '.', drops a '.' with no
// fraction digits and strips leading zeros of the integer part keeping one
// digit. .5 is 0.5, 1. is 1, +1.5 is 1.5, 01.5 is 1.5, 1.e3 is 1e3. s
// must satisfy isCoreFloat.
func normalizeFloat(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 1)
	switch s[0] {
	case '-':
		b.WriteByte('-')
		s = s[1:]
	case '+':
		s = s[1:]
	}
	n := leadingDigits(s)
	intPart := strings.TrimLeft(s[:n], "0")
	if intPart == "" {
		intPart = "0"
	}
	b.WriteString(intPart)
	s = s[n:]
	if s != "" && s[0] == '.' {
		f := leadingDigits(s[1:])
		if f > 0 {
			b.WriteString(s[:1+f])
		}
		s = s[1+f:]
	}
	b.WriteString(s) // the exponent, already RFC 8259 syntax
	return b.String()
}

// typed is the result of resolving a scalar: the node fields to set, or a
// diagnostic message with its code when the value is not acceptable.
type typed struct {
	kind tree.Kind
	text string
	b    bool
	code string
	msg  string
}

// resolvePlain types plain scalar text by the core schema (01 req 12).
func resolvePlain(s string) typed {
	switch classify(s) {
	case classNull:
		return typed{kind: tree.KindNull}
	case classBool:
		return typed{kind: tree.KindBool, b: s[0] == 't' || s[0] == 'T'}
	case classInt:
		text, ok := normalizeInt(s)
		if !ok {
			return typed{code: codeSchema, msg: "integer " + clip(s) + " is outside the 64-bit range (not representable in the JSON data model)"}
		}
		return typed{kind: tree.KindInt, text: text}
	case classFloat:
		return typed{kind: tree.KindFloat, text: normalizeFloat(s)}
	case classInf, classNaN:
		return typed{code: codeSchema, msg: s + " is not representable in the JSON data model"}
	default:
		return typed{kind: tree.KindString, text: s}
	}
}

// Core tags accepted by the profile (01 req 8).
const (
	tagStr   = "!!str"
	tagInt   = "!!int"
	tagFloat = "!!float"
	tagBool  = "!!bool"
	tagNull  = "!!null"
	tagMap   = "!!map"
	tagSeq   = "!!seq"
)

// isCoreTag reports whether tag is one of the seven YAML 1.2 core tags.
func isCoreTag(tag string) bool {
	switch tag {
	case tagStr, tagInt, tagFloat, tagBool, tagNull, tagMap, tagSeq:
		return true
	}
	return false
}

// resolveTagged types scalar content under an explicit core tag: the tag
// forces the type, and content that does not match it is RZ-CFG-001 (01
// req 12). !!map and !!seq never match a scalar.
func resolveTagged(tag, s string) typed {
	mismatch := typed{code: codeParse, msg: "value " + strconv.Quote(clip(s)) + " does not match tag " + tag}
	switch tag {
	case tagStr:
		return typed{kind: tree.KindString, text: s}
	case tagNull:
		if classify(s) != classNull {
			return mismatch
		}
		return typed{kind: tree.KindNull}
	case tagBool:
		if classify(s) != classBool {
			return mismatch
		}
		return resolvePlain(s)
	case tagInt:
		if !isCoreInt(s) {
			return mismatch
		}
		return resolvePlain(s)
	case tagFloat:
		switch {
		case isCoreFloat(s):
			return typed{kind: tree.KindFloat, text: normalizeFloat(s)}
		case isCoreInf(s), classify(s) == classNaN:
			return typed{code: codeSchema, msg: s + " is not representable in the JSON data model"}
		}
		return mismatch
	default:
		return mismatch
	}
}

// clip shortens text quoted in a message.
func clip(s string) string {
	const limit = 64
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "..."
}
