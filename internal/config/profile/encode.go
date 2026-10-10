// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// ErrEncode reports a tree the encoder cannot write: a string that is not
// valid UTF-8, or a node of unknown kind.
var ErrEncode = errors.New("profile: cannot encode tree")

// EncodeOptions configures the encoders. The zero value is the rendered
// Bundle form.
type EncodeOptions struct {
	// Verbatim writes strings as they are. By default every "${" inside a
	// string value is written "$${", so a rendered Bundle loads back to the
	// same values after substitution (01 req 31, 02 req 46). Verbatim is
	// for trees substitution has not run on, such as the source files
	// "ruralz bundle convert" rewrites (02 req 53), whose "${VAR}" and
	// "$${" text must stay as authored.
	Verbatim bool
}

// Encode writes docs as a restricted-profile YAML stream: block style with
// two-space indentation, documents separated by "---" lines with no
// leading separator, a final newline, mapping members in tree order, and
// every "${" in a string value written "$${" (01 req 31; 02 req 44-46,
// architecture R-29). The output depends on no library release, and
// Parse reads it back to the same values.
func Encode(w io.Writer, docs []*tree.Node) error {
	return EncodeWith(w, docs, EncodeOptions{})
}

// EncodeWith is Encode with options.
//
// Strings are written plain when they match ^[A-Za-z_/][A-Za-z0-9_./@-]*$
// and are not a null or boolean word, and double-quoted with JSON escapes
// otherwise, so "1.3", "0777" and "yes" stay strings (02 req 45). Inside
// quotes, characters YAML does not allow raw (C0 and C1 controls, DEL,
// U+FEFF, U+FFFE, U+FFFF) and the line separators U+0085, U+2028 and
// U+2029 are written as \uXXXX. Integers are written in their normalized
// decimal text and floats in their RFC 8259 text, with a "." appended when
// that text has neither fraction nor exponent, so a float stays a float.
// Empty mappings and sequences are written {} and [].
func EncodeWith(w io.Writer, docs []*tree.Node, o EncodeOptions) error {
	e := &yamlEncoder{opts: o}
	for i, d := range docs {
		e.w.Reset()
		if i > 0 {
			e.w.WriteString("---\n")
		}
		e.document(d)
		if e.err != nil {
			return e.err
		}
		if _, err := w.Write(e.w.Bytes()); err != nil {
			return fmt.Errorf("profile: write: %w", err)
		}
	}
	return nil
}

// yamlEncoder builds one document at a time; the first error sticks.
type yamlEncoder struct {
	w    bytes.Buffer
	opts EncodeOptions
	err  error
}

// document writes one document and its final newline.
func (e *yamlEncoder) document(n *tree.Node) {
	switch {
	case n != nil && n.Kind == tree.KindMap && len(n.Members) > 0:
		e.mapping(n, 0, false)
	case n != nil && n.Kind == tree.KindList && len(n.Items) > 0:
		e.sequence(n, 0, false)
	default:
		e.scalar(n)
		e.w.WriteByte('\n')
	}
}

// indent writes n spaces.
func (e *yamlEncoder) indent(n int) {
	for range n {
		e.w.WriteByte(' ')
	}
}

// maxImplicitKey is the longest implicit key YAML 1.2 allows, in
// characters as written (production ns-s-implicit-yaml-key).
const maxImplicitKey = 1024

// mapping writes the members of a non-empty mapping at indentation ind.
// inline means the first member continues a "- " already written. A key
// whose written form passes maxImplicitKey characters is written as an
// explicit key ("? key" then ": value" on the next line), so the output
// stays valid YAML 1.2.
func (e *yamlEncoder) mapping(n *tree.Node, ind int, inline bool) {
	for i, m := range n.Members {
		if i > 0 || !inline {
			e.indent(ind)
		}
		start := e.w.Len()
		e.key(m.Key)
		if written := e.w.Bytes()[start:]; len(written) > maxImplicitKey && utf8.RuneCount(written) > maxImplicitKey {
			key := string(written)
			e.w.Truncate(start)
			e.w.WriteString("? ")
			e.w.WriteString(key)
			e.w.WriteByte('\n')
			e.indent(ind)
		}
		e.w.WriteByte(':')
		e.value(m.Value, ind+2)
	}
}

// sequence writes the items of a non-empty sequence at indentation ind.
func (e *yamlEncoder) sequence(n *tree.Node, ind int, inline bool) {
	for i, it := range n.Items {
		if i > 0 || !inline {
			e.indent(ind)
		}
		e.w.WriteByte('-')
		switch {
		case it != nil && it.Kind == tree.KindMap && len(it.Members) > 0:
			e.w.WriteByte(' ')
			e.mapping(it, ind+2, true)
		case it != nil && it.Kind == tree.KindList && len(it.Items) > 0:
			e.w.WriteByte(' ')
			e.sequence(it, ind+2, true)
		default:
			e.w.WriteByte(' ')
			e.scalar(it)
			e.w.WriteByte('\n')
		}
	}
}

// value writes a mapping member's value after its ':'; nested
// collections start on the next line at indentation ind.
func (e *yamlEncoder) value(v *tree.Node, ind int) {
	switch {
	case v != nil && v.Kind == tree.KindMap && len(v.Members) > 0:
		e.w.WriteByte('\n')
		e.mapping(v, ind, false)
	case v != nil && v.Kind == tree.KindList && len(v.Items) > 0:
		e.w.WriteByte('\n')
		e.sequence(v, ind, false)
	default:
		e.w.WriteByte(' ')
		e.scalar(v)
		e.w.WriteByte('\n')
	}
}

// key writes a mapping key; keys are strings and are never escaped for
// substitution, which never runs on keys (01 req 27).
func (e *yamlEncoder) key(k string) {
	e.str(k)
}

// scalar writes a scalar or an empty collection.
func (e *yamlEncoder) scalar(n *tree.Node) {
	if n == nil {
		e.w.WriteString("null")
		return
	}
	switch n.Kind {
	case tree.KindNull:
		e.w.WriteString("null")
	case tree.KindBool:
		e.w.WriteString(strconv.FormatBool(n.Bool))
	case tree.KindInt:
		e.w.WriteString(n.Text)
	case tree.KindFloat:
		e.w.WriteString(n.Text)
		if !strings.ContainsAny(n.Text, ".eE") {
			e.w.WriteByte('.')
		}
	case tree.KindString:
		s := n.Text
		if !e.opts.Verbatim {
			s = escapeSubstitution(s)
		}
		e.str(s)
	case tree.KindMap:
		e.w.WriteString("{}")
	case tree.KindList:
		e.w.WriteString("[]")
	default:
		e.fail()
	}
}

func (e *yamlEncoder) fail() {
	if e.err == nil {
		e.err = ErrEncode
	}
}

// escapeSubstitution writes every "${" as "$${" (01 req 31).
func escapeSubstitution(s string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return strings.ReplaceAll(s, "${", "$${")
}

// str writes a string plain when the YAML 1.2 core schema reads it back
// as that string, and double-quoted otherwise (02 req 45).
func (e *yamlEncoder) str(s string) {
	if !utf8.ValidString(s) {
		e.fail()
		return
	}
	if plainSafe(s) {
		e.w.WriteString(s)
		return
	}
	e.w.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			e.w.WriteString(`\"`)
		case r == '\\':
			e.w.WriteString(`\\`)
		case r == '\n':
			e.w.WriteString(`\n`)
		case r == '\t':
			e.w.WriteString(`\t`)
		case r == '\r':
			e.w.WriteString(`\r`)
		case r == '\b':
			e.w.WriteString(`\b`)
		case r == '\f':
			e.w.WriteString(`\f`)
		case quoteEscape(r):
			e.w.WriteString(`\u`)
			e.w.WriteString(hex4(r))
		default:
			e.w.WriteRune(r)
		}
	}
	e.w.WriteByte('"')
}

// quoteEscape reports a character written \uXXXX inside double quotes:
// one the YAML pre-checks reject raw, or a line separator a reader might
// fold.
func quoteEscape(r rune) bool {
	switch {
	case r < 0x20, r == 0x7F:
		return true
	case r >= 0x80 && r <= 0x9F:
		return true
	case r == 0x2028, r == 0x2029, r == 0xFEFF, r == 0xFFFE, r == 0xFFFF:
		return true
	}
	return false
}

// hex4 returns r as four lower-case hexadecimal digits; r is in the BMP.
func hex4(r rune) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[r>>12&0xF], digits[r>>8&0xF], digits[r>>4&0xF], digits[r&0xF]})
}

// plainSafe reports whether s may be written as a plain scalar: it matches
// ^[A-Za-z_/][A-Za-z0-9_./@-]*$ and is not a null or boolean word (02 req
// 45). Such text never contains a YAML indicator sequence or a number.
func plainSafe(s string) bool {
	if s == "" {
		return false
	}
	if c := s[0]; !isLetter(c) && c != '_' && c != '/' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; !isLetter(c) && !isDecimal(c) && !strings.ContainsRune("_./@-", rune(c)) {
			return false
		}
	}
	switch s {
	case "null", "Null", "NULL", "true", "True", "TRUE", "false", "False", "FALSE":
		return false
	}
	return true
}

func isLetter(c byte) bool { return c|0x20 >= 'a' && c|0x20 <= 'z' }

// EncodeJSON writes v as JSON indented by two spaces, with mapping members
// in tree order, no HTML escaping and a final newline: the --output json
// form of "ruralz bundle render" (02 req 47; the caller passes the list of
// resources) and the form of a converted .json source file (02 req 53).
// Strings follow EncodeOptions as in EncodeWith.
func EncodeJSON(w io.Writer, v *tree.Node, o EncodeOptions) error {
	e := &jsonEncoder{opts: o}
	e.value(v, 0)
	if e.err != nil {
		return e.err
	}
	e.w.WriteByte('\n')
	if _, err := w.Write(e.w.Bytes()); err != nil {
		return fmt.Errorf("profile: write: %w", err)
	}
	return nil
}

// jsonEncoder builds one JSON value; the first error sticks.
type jsonEncoder struct {
	w    bytes.Buffer
	opts EncodeOptions
	err  error
}

func (e *jsonEncoder) newline(ind int) {
	e.w.WriteByte('\n')
	for range ind {
		e.w.WriteByte(' ')
	}
}

func (e *jsonEncoder) value(n *tree.Node, ind int) {
	if n == nil {
		e.w.WriteString("null")
		return
	}
	switch n.Kind {
	case tree.KindNull:
		e.w.WriteString("null")
	case tree.KindBool:
		e.w.WriteString(strconv.FormatBool(n.Bool))
	case tree.KindInt:
		e.w.WriteString(n.Text)
	case tree.KindFloat:
		// A float without fraction or exponent ("1" from YAML "1.")
		// stays a float, and an integer-looking float beyond int64 stays
		// readable.
		e.w.WriteString(n.Text)
		if !strings.ContainsAny(n.Text, ".eE") {
			e.w.WriteString(".0")
		}
	case tree.KindString:
		s := n.Text
		if !e.opts.Verbatim {
			s = escapeSubstitution(s)
		}
		e.str(s)
	case tree.KindMap:
		if len(n.Members) == 0 {
			e.w.WriteString("{}")
			return
		}
		e.w.WriteByte('{')
		for i, m := range n.Members {
			if i > 0 {
				e.w.WriteByte(',')
			}
			e.newline(ind + 2)
			e.str(m.Key)
			e.w.WriteString(": ")
			e.value(m.Value, ind+2)
		}
		e.newline(ind)
		e.w.WriteByte('}')
	case tree.KindList:
		if len(n.Items) == 0 {
			e.w.WriteString("[]")
			return
		}
		e.w.WriteByte('[')
		for i, it := range n.Items {
			if i > 0 {
				e.w.WriteByte(',')
			}
			e.newline(ind + 2)
			e.value(it, ind+2)
		}
		e.newline(ind)
		e.w.WriteByte(']')
	default:
		if e.err == nil {
			e.err = ErrEncode
		}
	}
}

// str writes a JSON string, escaping only '"', '\' and control characters.
func (e *jsonEncoder) str(s string) {
	if !utf8.ValidString(s) {
		if e.err == nil {
			e.err = ErrEncode
		}
		return
	}
	e.w.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			e.w.WriteString(`\"`)
		case r == '\\':
			e.w.WriteString(`\\`)
		case r == '\n':
			e.w.WriteString(`\n`)
		case r == '\t':
			e.w.WriteString(`\t`)
		case r == '\r':
			e.w.WriteString(`\r`)
		case r == '\b':
			e.w.WriteString(`\b`)
		case r == '\f':
			e.w.WriteString(`\f`)
		case r < 0x20:
			e.w.WriteString(`\u`)
			e.w.WriteString(hex4(r))
		default:
			e.w.WriteRune(r)
		}
	}
	e.w.WriteByte('"')
}
