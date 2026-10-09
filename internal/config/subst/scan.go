// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package subst

import "strings"

// segment is one piece of a scanned string: literal text (escapes
// resolved) or one expression.
type segment struct {
	// lit is the literal text when name is "".
	lit string
	// name is the variable of an expression.
	name string
	// def is the default of ${NAME:-default}.
	def string
	// hasDef reports the :- form.
	hasDef bool
}

// Reasons a ${ is malformed (01 req 26).
const (
	reasonUnterminated = "unterminated ${"
	reasonName         = "a variable name matches [A-Za-z_][A-Za-z0-9_]*"
	reasonOperator     = "only ${NAME} and ${NAME:-default} are supported"
	reasonNested       = "a default must not contain ${"
)

// scan splits s by the grammar of 01 req 26: $${ is a literal ${, ${NAME}
// and ${NAME:-default} are expressions (NAME matching
// [A-Za-z_][A-Za-z0-9_]*, default any run of characters without } and
// without ${), and a $ not followed by { is literal. Any other ${ is
// malformed: bad is the reason and segs is nil.
func scan(s string) (segs []segment, bad string) {
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			segs = append(segs, segment{lit: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "$${"):
			lit.WriteString("${")
			i += 3
		case strings.HasPrefix(s[i:], "${"):
			e, n, reason := expression(s[i+2:])
			if reason != "" {
				return nil, reason
			}
			flush()
			segs = append(segs, e)
			i += 2 + n
		default:
			lit.WriteByte(s[i])
			i++
		}
	}
	flush()
	return segs, ""
}

// expression parses the text after ${ and returns the expression and the
// number of bytes it used, closing } included.
func expression(s string) (segment, int, string) {
	n := nameLen(s)
	if n == 0 {
		if s == "" {
			return segment{}, 0, reasonUnterminated
		}
		return segment{}, 0, reasonName
	}
	e := segment{name: s[:n]}
	rest := s[n:]
	switch {
	case rest == "":
		return segment{}, 0, reasonUnterminated
	case rest[0] == '}':
		return e, n + 1, ""
	case strings.HasPrefix(rest, ":-"):
		end := strings.IndexByte(rest[2:], '}')
		if end < 0 {
			return segment{}, 0, reasonUnterminated
		}
		e.def, e.hasDef = rest[2:2+end], true
		if strings.Contains(e.def, "${") {
			return segment{}, 0, reasonNested
		}
		return e, n + 2 + end + 1, ""
	case rest == ":":
		return segment{}, 0, reasonUnterminated
	default:
		return segment{}, 0, reasonOperator
	}
}

// nameLen returns the length of the variable name at the start of s, 0
// when s does not start with one.
func nameLen(s string) int {
	for i := 0; i < len(s); i++ {
		c := s[i]
		letter := c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		if letter || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return i
	}
	return len(s)
}

// literal joins the literal segments; it is the text of a scan without
// expressions.
func literal(segs []segment) string {
	if len(segs) == 1 {
		return segs[0].lit
	}
	var b strings.Builder
	for _, sg := range segs {
		b.WriteString(sg.lit)
	}
	return b.String()
}

// hasExpression reports an expression segment.
func hasExpression(segs []segment) bool {
	for _, sg := range segs {
		if sg.name != "" {
			return true
		}
	}
	return false
}

// secretLike reports a variable name matching (?i)(secret|password|token|apikey)
// (01 req 30).
func secretLike(name string) bool {
	l := strings.ToLower(name)
	for _, w := range [...]string{"secret", "password", "token", "apikey"} {
		if strings.Contains(l, w) {
			return true
		}
	}
	return false
}
