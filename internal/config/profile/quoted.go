// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"unicode/utf8"

	"github.com/goccy/go-yaml/token"
)

// Messages of the quoted scalar checks.
const (
	msgQuoteStart = "syntax error: cannot find the quoted scalar's opening quote"
	msgQuoteEnd   = "syntax error: the quoted scalar does not end at its closing quote"
	msgQuoteOpen  = "syntax error: the quoted scalar has no closing quote"
)

// readQuoted reads the quoted scalar whose opening quote is at byte offset
// at of text as YAML 1.2 reads it (01 req 12; YAML 1.2.2 sections 7.3.1
// and 7.3.2), into buf's storage. It returns the value, the offset just
// past the closing quote, and a message when YAML 1.2 refuses the scalar.
//
// goccy's scanner drops the white space an escape gives before a line
// break ("\ ", "\x20"), keeps the tabs before a line break of a single
// quoted scalar, and accepts invalid escapes such as "\xZZ", lone
// surrogates and code points past U+10FFFF, so the value is taken from
// here. A line break folds: the white space before it is dropped, as is
// the indentation after it, and it becomes a space, or one line feed per
// empty line that follows it. In a double-quoted scalar an escaped line
// break is dropped, keeping the white space before it, and the escapes are
// YAML 1.2's; a high and a low UTF-16 surrogate escaped one after the
// other join into one character, as in JSON (01 req 15).
func readQuoted(buf []byte, text string, at int) (value []byte, end int, msg string) {
	q := text[at]
	out := buf[:0]
	// trim is the length of out before the white space that ends the
	// current line so far, or -1.
	trim := -1
	for i := at + 1; i < len(text); {
		c := text[i]
		switch {
		case c == q && q == '\'' && i+1 < len(text) && text[i+1] == '\'':
			out = append(out, '\'')
			i += 2
			trim = -1
		case c == q:
			return out, i + 1, ""
		case c == '\n':
			if trim >= 0 {
				out = out[:trim]
			}
			var breaks int
			i, breaks = foldBreaks(text, i+1)
			if breaks == 0 {
				out = append(out, ' ')
			}
			for ; breaks > 0; breaks-- {
				out = append(out, '\n')
			}
			trim = -1
		case c == ' ' || c == '\t':
			if trim < 0 {
				trim = len(out)
			}
			out = append(out, c)
			i++
		case c == '\\' && q == '"':
			if i+1 < len(text) && text[i+1] == '\n' {
				// An escaped line break: the white space before it stays.
				var breaks int
				i, breaks = foldBreaks(text, i+2)
				for ; breaks > 0; breaks-- {
					out = append(out, '\n')
				}
				trim = -1
				continue
			}
			var size int
			out, size, msg = appendEscape(out, text[i+1:])
			if msg != "" {
				return nil, 0, msg
			}
			i += 1 + size
			trim = -1
		default:
			_, size := utf8.DecodeRuneInString(text[i:])
			out = append(out, text[i:i+size]...)
			i += size
			trim = -1
		}
	}
	return nil, 0, msgQuoteOpen
}

// foldBreaks skips the indentation of the line starting at byte offset i
// and of the empty lines before it, returning the offset of its first
// other character and the number of empty lines skipped.
func foldBreaks(text string, i int) (int, int) {
	breaks := 0
	for {
		j := i
		for j < len(text) && (text[j] == ' ' || text[j] == '\t') {
			j++
		}
		if j < len(text) && text[j] == '\n' {
			breaks++
			i = j + 1
			continue
		}
		return j, breaks
	}
}

// appendEscape appends the character of the escape sequence that s starts
// after its backslash, and returns the number of bytes it used, or a
// message for an escape YAML 1.2 does not define.
func appendEscape(out []byte, s string) ([]byte, int, string) {
	if s == "" {
		return out, 0, msgQuoteOpen
	}
	switch s[0] {
	case '0':
		return append(out, 0), 1, ""
	case 'a':
		return append(out, 7), 1, ""
	case 'b':
		return append(out, 8), 1, ""
	case 't', '\t':
		return append(out, '\t'), 1, ""
	case 'n':
		return append(out, '\n'), 1, ""
	case 'v':
		return append(out, 11), 1, ""
	case 'f':
		return append(out, 12), 1, ""
	case 'r':
		return append(out, '\r'), 1, ""
	case 'e':
		return append(out, 0x1B), 1, ""
	case ' ', '"', '/', '\\':
		return append(out, s[0]), 1, ""
	case 'N':
		return utf8.AppendRune(out, 0x85), 1, ""
	case '_':
		return utf8.AppendRune(out, 0xA0), 1, ""
	case 'L':
		return utf8.AppendRune(out, 0x2028), 1, ""
	case 'P':
		return utf8.AppendRune(out, 0x2029), 1, ""
	case 'x':
		return appendCodePoint(out, s, 2)
	case 'u':
		return appendCodePoint(out, s, 4)
	case 'U':
		return appendCodePoint(out, s, 8)
	default:
		r, _ := utf8.DecodeRuneInString(s)
		return out, 0, "syntax error: unknown escape \\" + string(r) + " in a double-quoted scalar"
	}
}

// appendCodePoint appends the code point of a \x, \u or \U escape whose
// letter starts s and which has digits hexadecimal digits. A \u high
// surrogate followed by a \u low surrogate gives the pair's code point; a
// lone surrogate or a code point past U+10FFFF is refused.
func appendCodePoint(out []byte, s string, digits int) ([]byte, int, string) {
	bad := "syntax error: invalid \\" + s[:1] + " escape in a double-quoted scalar"
	v, ok := hexValue(s[1:], digits)
	if !ok {
		return out, 0, bad
	}
	size := 1 + digits
	switch {
	case digits == 4 && v >= 0xD800 && v <= 0xDBFF:
		rest := s[size:]
		low, ok := uint32(0), len(rest) >= 6 && rest[0] == '\\' && rest[1] == 'u'
		if ok {
			low, ok = hexValue(rest[2:], 4)
		}
		if !ok || low < 0xDC00 || low > 0xDFFF {
			return out, 0, bad
		}
		v = 0x10000 + (v-0xD800)<<10 + (low - 0xDC00)
		size += 6
	case v >= 0xD800 && v <= 0xDFFF, v > utf8.MaxRune:
		return out, 0, bad
	}
	return utf8.AppendRune(out, rune(v)), size, "" //nolint:gosec // G115: v is at most utf8.MaxRune here
}

// hexValue returns the value of the first digits hexadecimal digits of s.
func hexValue(s string, digits int) (uint32, bool) {
	if len(s) < digits {
		return 0, false
	}
	var v uint32
	for i := range digits {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | uint32(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | uint32(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v<<4 | uint32(c-'A'+10)
		default:
			return 0, false
		}
	}
	return v, true
}

// quoted reads the quoted scalar token idx from the source (readQuoted)
// and records its value for the converter when it differs from goccy's (01
// req 12). An escape YAML 1.2 does not define is RZ-CFG-001 at the scalar,
// as are an opening quote goccy reports elsewhere and a next token inside
// the scalar, which would mean goccy read another scalar. Like the other
// checks that only keep goccy from misreading a file, it is skipped once
// the file has a finding.
func (t *tokenPass) quoted(idx int) {
	if t.errs > 0 {
		return
	}
	tk := t.tks[idx]
	at := t.posOf(tk)
	off := t.src.offset(tk.Position.Line, t.col(tk))
	q := byte('"')
	if tk.Type == token.SingleQuoteType {
		q = '\''
	}
	if off < 0 || t.src.text[off] != q {
		t.fail(at, msgQuoteStart)
		return
	}
	value, end, msg := readQuoted(t.buf, t.src.text, off)
	if msg != "" {
		t.fail(at, msg)
		return
	}
	t.buf = value
	if string(value) != tk.Value {
		t.fix(tk, scalarFix{text: string(value), set: true})
	}
	if idx+1 < len(t.tks) {
		next := t.tks[idx+1]
		line, col := advance(t.src.text, off, end, tk.Position.Line, t.col(tk))
		if nl, nc := next.Position.Line, t.col(next); nl < line || (nl == line && nc < col) {
			t.fail(at, msgQuoteEnd)
		}
	}
}
