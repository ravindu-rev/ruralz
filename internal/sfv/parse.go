// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package sfv

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// parser walks one field value (RFC 9651 section 4.2).
type parser struct {
	s   string
	pos int
}

func (p *parser) eof() bool { return p.pos >= len(p.s) }

func (p *parser) peek() byte { return p.s[p.pos] }

func (p *parser) fail(format string, args ...any) error {
	return fmt.Errorf("%w: offset %d: %s", ErrParse, p.pos, fmt.Sprintf(format, args...))
}

func (p *parser) skipSP() {
	for !p.eof() && p.peek() == ' ' {
		p.pos++
	}
}

func (p *parser) skipOWS() {
	for !p.eof() && (p.peek() == ' ' || p.peek() == '\t') {
		p.pos++
	}
}

// start checks the input is ASCII and skips leading SP (RFC 9651 section
// 4.2 steps 1 and 2). A field sent in several lines is parsed from the
// lines joined with ", ".
func (p *parser) start() error {
	for i := range len(p.s) {
		if p.s[i] >= utf8.RuneSelf {
			return fmt.Errorf("%w: offset %d: byte %q is not ASCII", ErrParse, i, p.s[i])
		}
	}
	p.skipSP()
	return nil
}

// finish skips trailing SP and requires the end of input (RFC 9651
// section 4.2 steps 6 and 7).
func (p *parser) finish() error {
	p.skipSP()
	if !p.eof() {
		return p.fail("unexpected %q", p.peek())
	}
	return nil
}

// ParseList parses a List field value (RFC 9651 section 4.2.1). The empty
// string is the empty List.
func ParseList(s string) (List, error) {
	p := parser{s: s}
	if err := p.start(); err != nil {
		return nil, err
	}
	var l List
	for !p.eof() {
		m, err := p.memberOrInner()
		if err != nil {
			return nil, err
		}
		l = append(l, m)
		p.skipOWS()
		if p.eof() {
			return l, nil
		}
		if p.peek() != ',' {
			return nil, p.fail("expected \",\" between members, found %q", p.peek())
		}
		p.pos++
		p.skipOWS()
		if p.eof() {
			return nil, p.fail("trailing comma")
		}
	}
	return l, nil
}

// ParseItem parses an Item field value (RFC 9651 section 4.2).
func ParseItem(s string) (Item, error) {
	p := parser{s: s}
	if err := p.start(); err != nil {
		return Item{}, err
	}
	v, params, err := p.item()
	if err != nil {
		return Item{}, err
	}
	if err := p.finish(); err != nil {
		return Item{}, err
	}
	return Item{Value: v, Params: params}, nil
}

func (p *parser) memberOrInner() (Member, error) {
	if !p.eof() && p.peek() == '(' {
		return p.innerList()
	}
	v, params, err := p.item()
	if err != nil {
		return Member{}, err
	}
	return Member{Value: v, Params: params}, nil
}

// innerList parses an Inner List (RFC 9651 section 4.2.1.2).
func (p *parser) innerList() (Member, error) {
	p.pos++ // "("
	m := Member{Inner: true, Items: []Item{}}
	for !p.eof() {
		p.skipSP()
		if p.eof() {
			break
		}
		if p.peek() == ')' {
			p.pos++
			params, err := p.params()
			if err != nil {
				return Member{}, err
			}
			m.Params = params
			return m, nil
		}
		v, params, err := p.item()
		if err != nil {
			return Member{}, err
		}
		m.Items = append(m.Items, Item{Value: v, Params: params})
		if p.eof() {
			break
		}
		if c := p.peek(); c != ' ' && c != ')' {
			return Member{}, p.fail("expected SP or \")\" in inner list, found %q", c)
		}
	}
	return Member{}, p.fail("inner list is not closed")
}

// item parses an Item (RFC 9651 section 4.2.3).
func (p *parser) item() (BareItem, []Param, error) {
	v, err := p.bareItem()
	if err != nil {
		return BareItem{}, nil, err
	}
	params, err := p.params()
	if err != nil {
		return BareItem{}, nil, err
	}
	return v, params, nil
}

// params parses Parameters (RFC 9651 section 4.2.3.2). A repeated key
// overwrites the earlier value in place.
func (p *parser) params() ([]Param, error) {
	var out []Param
	for !p.eof() && p.peek() == ';' {
		p.pos++
		p.skipSP()
		key, err := p.key()
		if err != nil {
			return nil, err
		}
		v := Boolean(true)
		if !p.eof() && p.peek() == '=' {
			p.pos++
			if v, err = p.bareItem(); err != nil {
				return nil, err
			}
		}
		replaced := false
		for i := range out {
			if out[i].Key == key {
				out[i].Value, replaced = v, true
				break
			}
		}
		if !replaced {
			out = append(out, Param{Key: key, Value: v})
		}
	}
	return out, nil
}

// key parses a Key (RFC 9651 section 4.2.3.3).
func (p *parser) key() (string, error) {
	if p.eof() || (!isLCAlpha(p.peek()) && p.peek() != '*') {
		return "", p.fail("key must start with a lowercase letter or \"*\"")
	}
	start := p.pos
	p.pos++
	for !p.eof() && isKeyChar(p.peek()) {
		p.pos++
	}
	return p.s[start:p.pos], nil
}

// bareItem parses a Bare Item (RFC 9651 section 4.2.3.1).
func (p *parser) bareItem() (BareItem, error) {
	if p.eof() {
		return BareItem{}, p.fail("missing bare item")
	}
	switch c := p.peek(); {
	case c == '-' || isDigit(c):
		return p.number()
	case c == '"':
		return p.str()
	case c == '*' || isAlpha(c):
		return p.token(), nil
	case c == ':':
		return p.byteSequence()
	case c == '?':
		return p.boolean()
	case c == '@':
		p.pos++
		v, err := p.number()
		if err != nil {
			return BareItem{}, err
		}
		if v.kind != KindInteger {
			return BareItem{}, p.fail("date is not an integer")
		}
		return Date(v.num), nil
	case c == '%':
		return p.displayString()
	default:
		return BareItem{}, p.fail("unexpected %q at the start of a bare item", c)
	}
}

// number parses an Integer or Decimal (RFC 9651 section 4.2.4).
func (p *parser) number() (BareItem, error) {
	start := p.pos
	if !p.eof() && p.peek() == '-' {
		p.pos++
	}
	if p.eof() || !isDigit(p.peek()) {
		return BareItem{}, p.fail("number without digits")
	}
	digits, dot := 0, -1 // dot: integer digits before ".", or -1
scan:
	for ; !p.eof(); p.pos++ {
		switch c := p.peek(); {
		case isDigit(c):
			digits++
		case c == '.' && dot < 0:
			if digits > 12 {
				return BareItem{}, p.fail("decimal has more than 12 integer digits")
			}
			dot = digits
		default:
			break scan
		}
		// An integer has at most 15 digits; a decimal at most 16
		// characters, the "." included.
		if digits > 15 {
			return BareItem{}, p.fail("number has more than 15 digits")
		}
	}
	text := p.s[start:p.pos]
	if dot < 0 {
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return BareItem{}, p.fail("integer %q: %v", text, err)
		}
		return Integer(n), nil
	}
	switch frac := digits - dot; {
	case frac == 0:
		return BareItem{}, p.fail("decimal ends with \".\"")
	case frac > 3:
		return BareItem{}, p.fail("decimal has more than 3 fractional digits")
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return BareItem{}, p.fail("decimal %q: %v", text, err)
	}
	return Decimal(f), nil
}

// str parses a String (RFC 9651 section 4.2.5). An unescaped String
// shares the input's bytes.
func (p *parser) str() (BareItem, error) {
	p.pos++ // DQUOTE
	var b []byte
	escaped := false
	start := p.pos
	for !p.eof() {
		switch c := p.peek(); {
		case c == '\\':
			b = append(b, p.s[start:p.pos]...)
			escaped = true
			p.pos++
			if p.eof() {
				return BareItem{}, p.fail("string ends inside an escape")
			}
			if n := p.peek(); n != '"' && n != '\\' {
				return BareItem{}, p.fail("invalid escape \\%c", n)
			}
			start = p.pos // the escaped character starts the next run
			p.pos++
		case c == '"':
			s := p.s[start:p.pos]
			if escaped {
				s = string(append(b, s...))
			}
			p.pos++
			return String(s), nil
		case c < 0x20 || c > 0x7e:
			return BareItem{}, p.fail("string has byte %q", c)
		default:
			p.pos++
		}
	}
	return BareItem{}, p.fail("string is not closed")
}

// token parses a Token (RFC 9651 section 4.2.6); the caller checked the
// first character.
func (p *parser) token() BareItem {
	start := p.pos
	p.pos++
	for !p.eof() && isTokenChar(p.peek()) {
		p.pos++
	}
	return Token(p.s[start:p.pos])
}

// byteSequence parses a Byte Sequence (RFC 9651 section 4.2.7). Missing
// "=" padding and non-zero pad bits are accepted, as the RFC recommends.
func (p *parser) byteSequence() (BareItem, error) {
	p.pos++ // ":"
	end := strings.IndexByte(p.s[p.pos:], ':')
	if end < 0 {
		return BareItem{}, p.fail("byte sequence is not closed")
	}
	b64 := p.s[p.pos : p.pos+end]
	for i := range len(b64) {
		if c := b64[i]; !isAlpha(c) && !isDigit(c) && c != '+' && c != '/' && c != '=' {
			return BareItem{}, p.fail("byte sequence has byte %q", c)
		}
	}
	b, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(b64, "="))
	if err != nil {
		return BareItem{}, p.fail("byte sequence: %v", err)
	}
	p.pos += end + 1
	return ByteSequence(b), nil
}

// boolean parses a Boolean (RFC 9651 section 4.2.8).
func (p *parser) boolean() (BareItem, error) {
	p.pos++ // "?"
	if p.eof() {
		return BareItem{}, p.fail("boolean without a value")
	}
	switch p.peek() {
	case '1':
		p.pos++
		return Boolean(true), nil
	case '0':
		p.pos++
		return Boolean(false), nil
	default:
		return BareItem{}, p.fail("boolean must be ?0 or ?1")
	}
}

// displayString parses a Display String (RFC 9651 section 4.2.10).
func (p *parser) displayString() (BareItem, error) {
	if p.pos+1 >= len(p.s) || p.s[p.pos+1] != '"' {
		return BareItem{}, p.fail("display string must start with %%\"")
	}
	p.pos += 2
	var b []byte
	for !p.eof() {
		c := p.peek()
		switch {
		case c < 0x20 || c > 0x7e:
			return BareItem{}, p.fail("display string has byte %q", c)
		case c == '%':
			if p.pos+2 >= len(p.s) || !isLowerHex(p.s[p.pos+1]) || !isLowerHex(p.s[p.pos+2]) {
				return BareItem{}, p.fail("display string has an invalid percent escape")
			}
			b = append(b, lowerHexVal(p.s[p.pos+1])<<4|lowerHexVal(p.s[p.pos+2]))
			p.pos += 3
		case c == '"':
			p.pos++
			if !utf8.Valid(b) {
				return BareItem{}, p.fail("display string is not valid UTF-8")
			}
			return DisplayString(string(b)), nil
		default:
			b = append(b, c)
			p.pos++
		}
	}
	return BareItem{}, p.fail("display string is not closed")
}

func isLowerHex(c byte) bool { return isDigit(c) || ('a' <= c && c <= 'f') }

func lowerHexVal(c byte) byte {
	if isDigit(c) {
		return c - '0'
	}
	return c - 'a' + 10
}
