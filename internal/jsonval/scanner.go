// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import (
	"bytes"
	"errors"
	"hash/maphash"
	"io"
	"unicode/utf16"
	"unicode/utf8"
)

// Depth limits.
const (
	// DefaultMaxDepth is the nesting limit when options leave it zero: the
	// configuration loader's default (OQ-configuration-model-18 (a)) and the
	// transform and validation limit (07 req 58, 80).
	DefaultMaxDepth = 64
	// MaxDepthLimit caps any configured nesting limit; it equals the
	// jsontext default.
	MaxDepthLimit = 10000
)

// Kind is the kind of a token.
type Kind uint8

// Token kinds.
const (
	// KindInvalid is the zero Kind; no token has it.
	KindInvalid Kind = iota
	// KindNull is the literal null.
	KindNull
	// KindFalse is the literal false.
	KindFalse
	// KindTrue is the literal true.
	KindTrue
	// KindNumber is a number.
	KindNumber
	// KindString is a string value.
	KindString
	// KindName is an object member name.
	KindName
	// KindObjectStart is '{'.
	KindObjectStart
	// KindObjectEnd is '}'.
	KindObjectEnd
	// KindArrayStart is '['.
	KindArrayStart
	// KindArrayEnd is ']'.
	KindArrayEnd
)

// String returns the kind's name, such as "string" or "object start".
func (k Kind) String() string {
	switch k {
	case KindNull:
		return "null"
	case KindFalse:
		return "false"
	case KindTrue:
		return "true"
	case KindNumber:
		return "number"
	case KindString:
		return "string"
	case KindName:
		return "name"
	case KindObjectStart:
		return "object start"
	case KindObjectEnd:
		return "object end"
	case KindArrayStart:
		return "array start"
	case KindArrayEnd:
		return "array end"
	default:
		return "invalid"
	}
}

// Token is one token of the scanned input: data[Offset:End] is its text,
// quotes included for strings and names.
type Token struct {
	// Kind is the token kind.
	Kind Kind
	// Escaped reports a string or name holding at least one escape
	// sequence; without one, the text between the quotes is the value.
	Escaped bool
	// Offset is the byte offset of the token's first byte.
	Offset int
	// End is the byte offset just past the token.
	End int
}

// ScanOptions configures a Scanner. The zero value is the strict default.
type ScanOptions struct {
	// MaxDepth limits nesting: `[]` has depth 1. Zero means
	// DefaultMaxDepth; values above MaxDepthLimit mean MaxDepthLimit.
	MaxDepth int
	// AllowDuplicateNames turns off the duplicate member name check. The
	// strict default rejects a duplicate with ErrDuplicateName, naming both
	// offsets (01 req 15, 06 req 25, 07 req 80).
	AllowDuplicateNames bool
}

// depth returns the effective nesting limit.
func (o ScanOptions) depth() int {
	switch {
	case o.MaxDepth <= 0:
		return DefaultMaxDepth
	case o.MaxDepth > MaxDepthLimit:
		return MaxDepthLimit
	default:
		return o.MaxDepth
	}
}

// Scanner states: what the next token may be.
const (
	stValue      uint8 = iota // a value (top level, after ':' or after ',' in an array)
	stValueOrEnd              // a value or ']' (after '[')
	stNameOrEnd               // a name or '}' (after '{')
	stColon                   // ':' then a value (after a name)
	stCommaOrEnd              // ',' or the container end (after a member or element)
	stEnd                     // only whitespace (after the top-level value)
)

// Scanner reads the tokens of one JSON text held in memory. It checks the
// full RFC 8259 grammar (one top-level value, whitespace around it only),
// UTF-8 validity, surrogate pairing, the nesting limit and, by default,
// duplicate member names. Create one with NewScanner, or call Reset on a
// zero Scanner, before Next. A Scanner is reusable through Reset; after
// warm-up it allocates nothing per token. Reset drops the input and every
// scratch structure over about 64 KiB (unescape buffers, duplicate-name
// records and indexes), so a Scanner kept for reuse, for example in a
// sync.Pool after Reset(nil, ScanOptions{}), never pins a large body (07 req
// 73).
type Scanner struct {
	data     []byte
	pos      int
	depth    int
	maxDepth int
	state    uint8
	checkDup bool
	err      error
	// stack holds one bit per open container: set for an object. The depth
	// limit bounds it to MaxDepthLimit bits (about 1.2 KiB), so Reset keeps
	// it.
	stack []uint64
	names nameSet
}

// NewScanner returns a Scanner over data.
func NewScanner(data []byte, o ScanOptions) *Scanner {
	s := new(Scanner)
	s.Reset(data, o)
	return s
}

// Reset starts scanning data with options o. It keeps internal buffers up
// to the scratch bounds and drops larger ones (07 req 73).
func (s *Scanner) Reset(data []byte, o ScanOptions) {
	s.data = data
	s.pos = 0
	s.depth = 0
	s.maxDepth = o.depth()
	s.state = stValue
	s.checkDup = !o.AllowDuplicateNames
	s.err = nil
	s.stack = s.stack[:0]
	s.names.reset()
}

// Depth returns the number of open objects and arrays.
func (s *Scanner) Depth() int { return s.depth }

// Offset returns the byte offset just past the last token.
func (s *Scanner) Offset() int { return s.pos }

// Bytes returns the token's text, quotes included, aliasing the input.
func (s *Scanner) Bytes(t Token) []byte { return s.data[t.Offset:t.End] }

// AppendText appends the decoded value of a string or name token to dst.
// For other tokens it appends the token text.
func (s *Scanner) AppendText(dst []byte, t Token) []byte {
	if t.Kind != KindString && t.Kind != KindName {
		return append(dst, s.data[t.Offset:t.End]...)
	}
	raw := s.data[t.Offset+1 : t.End-1]
	if !t.Escaped {
		return append(dst, raw...)
	}
	return appendUnescaped(dst, raw)
}

// Text returns the decoded value of a string or name token, or the text of
// any other token.
func (s *Scanner) Text(t Token) string {
	if (t.Kind == KindString || t.Kind == KindName) && !t.Escaped {
		return string(s.data[t.Offset+1 : t.End-1])
	}
	var buf [64]byte
	return string(s.AppendText(buf[:0], t))
}

// Next returns the next token. After the top-level value and its trailing
// whitespace it returns io.EOF. A failure is an *Error and is returned by
// every later call.
func (s *Scanner) Next() (Token, error) {
	if s.err != nil {
		return Token{}, s.err
	}
	i := skipSpace(s.data, s.pos)
	switch s.state {
	case stValueOrEnd:
		if i < len(s.data) && s.data[i] == ']' {
			return s.close(i, false)
		}
		return s.value(i)
	case stNameOrEnd:
		if i < len(s.data) && s.data[i] == '}' {
			return s.close(i, true)
		}
		return s.name(i)
	case stColon:
		if i >= len(s.data) {
			return s.fail(ErrSyntax, msgEOF, i)
		}
		if s.data[i] != ':' {
			return s.fail(ErrSyntax, msgColon, i)
		}
		return s.value(skipSpace(s.data, i+1))
	case stCommaOrEnd:
		return s.commaOrEnd(i)
	case stEnd:
		if i < len(s.data) {
			return s.fail(ErrSyntax, msgAfterTop, i)
		}
		s.pos = i
		return Token{}, io.EOF
	default: // stValue
		return s.value(i)
	}
}

// commaOrEnd handles the separator or end after a member or element.
func (s *Scanner) commaOrEnd(i int) (Token, error) {
	obj := s.inObject()
	if i >= len(s.data) {
		return s.fail(ErrSyntax, msgEOF, i)
	}
	switch s.data[i] {
	case ',':
		i = skipSpace(s.data, i+1)
		if obj {
			return s.name(i)
		}
		return s.value(i)
	case '}':
		if obj {
			return s.close(i, true)
		}
	case ']':
		if !obj {
			return s.close(i, false)
		}
	}
	if obj {
		return s.fail(ErrSyntax, msgAfterMember, i)
	}
	return s.fail(ErrSyntax, msgAfterElement, i)
}

// value scans a value starting at i.
func (s *Scanner) value(i int) (Token, error) {
	data := s.data
	if i >= len(data) {
		return s.fail(ErrSyntax, msgEOF, i)
	}
	switch c := data[i]; {
	case c == '{':
		return s.open(i, true)
	case c == '[':
		return s.open(i, false)
	case c == '"':
		end, esc, err := s.scanString(i)
		if err != nil {
			return Token{}, err
		}
		return s.scalar(Token{Kind: KindString, Escaped: esc, Offset: i, End: end}), nil
	case c == 'n':
		return s.literal(i, "null", KindNull)
	case c == 't':
		return s.literal(i, "true", KindTrue)
	case c == 'f':
		return s.literal(i, "false", KindFalse)
	case c == '-' || isDigit(c):
		end, bad := scanNumber(data, i)
		if bad >= 0 {
			if bad >= len(data) {
				return s.fail(ErrSyntax, msgEOF, bad)
			}
			return s.fail(ErrSyntax, msgNumber, bad)
		}
		return s.scalar(Token{Kind: KindNumber, Offset: i, End: end}), nil
	default:
		return s.fail(ErrSyntax, msgValueStart, i)
	}
}

// literal scans null, true or false.
func (s *Scanner) literal(i int, lit string, k Kind) (Token, error) {
	data := s.data
	for j := 0; j < len(lit); j++ {
		if i+j >= len(data) {
			return s.fail(ErrSyntax, msgEOF, i+j)
		}
		if data[i+j] != lit[j] {
			return s.fail(ErrSyntax, msgLiteral, i+j)
		}
	}
	return s.scalar(Token{Kind: k, Offset: i, End: i + len(lit)}), nil
}

// scalar records a finished scalar value.
func (s *Scanner) scalar(t Token) Token {
	s.pos = t.End
	s.afterValue()
	return t
}

// afterValue sets the state after a complete value.
func (s *Scanner) afterValue() {
	if s.depth == 0 {
		s.state = stEnd
	} else {
		s.state = stCommaOrEnd
	}
}

// name scans an object member name starting at i.
func (s *Scanner) name(i int) (Token, error) {
	if i >= len(s.data) {
		return s.fail(ErrSyntax, msgEOF, i)
	}
	if s.data[i] != '"' {
		return s.fail(ErrSyntax, msgName, i)
	}
	end, esc, err := s.scanString(i)
	if err != nil {
		return Token{}, err
	}
	t := Token{Kind: KindName, Escaped: esc, Offset: i, End: end}
	if s.checkDup {
		if first := s.names.add(s.data, t); first >= 0 {
			s.err = &Error{Kind: ErrDuplicateName, Msg: msgDuplicate, Offset: i, Other: first}
			return Token{}, s.err
		}
	}
	s.pos = end
	s.state = stColon
	return t, nil
}

// open starts an object or array at i.
func (s *Scanner) open(i int, obj bool) (Token, error) {
	if s.depth >= s.maxDepth {
		return s.fail(ErrDepth, msgDepth, i)
	}
	w, bit := s.depth>>6, uint64(1)<<(s.depth&63)
	if w == len(s.stack) {
		s.stack = append(s.stack, 0)
	}
	if obj {
		s.stack[w] |= bit
		if s.checkDup {
			s.names.open()
		}
		s.state = stNameOrEnd
	} else {
		s.stack[w] &^= bit
		s.state = stValueOrEnd
	}
	s.depth++
	s.pos = i + 1
	if obj {
		return Token{Kind: KindObjectStart, Offset: i, End: i + 1}, nil
	}
	return Token{Kind: KindArrayStart, Offset: i, End: i + 1}, nil
}

// close ends the innermost container at i.
func (s *Scanner) close(i int, obj bool) (Token, error) {
	s.depth--
	if obj && s.checkDup {
		s.names.close()
	}
	s.pos = i + 1
	s.afterValue()
	if obj {
		return Token{Kind: KindObjectEnd, Offset: i, End: i + 1}, nil
	}
	return Token{Kind: KindArrayEnd, Offset: i, End: i + 1}, nil
}

// inObject reports whether the innermost open container is an object.
func (s *Scanner) inObject() bool {
	d := s.depth - 1
	return s.stack[d>>6]&(uint64(1)<<(d&63)) != 0
}

// fail records a sticky error.
func (s *Scanner) fail(kind error, msg string, off int) (Token, error) {
	s.err = &Error{Kind: kind, Msg: msg, Offset: off, Other: -1}
	return Token{}, s.err
}

// scanString scans a string starting at the opening quote at i and returns
// the offset past the closing quote.
func (s *Scanner) scanString(i int) (end int, escaped bool, err error) {
	data := s.data
	n := len(data)
	j := i + 1
	for {
		for j < n {
			c := data[j]
			if c < 0x20 || c == '"' || c == '\\' || c >= utf8.RuneSelf {
				break
			}
			j++
		}
		if j >= n {
			_, err = s.fail(ErrSyntax, msgEOF, n)
			return 0, false, err
		}
		switch c := data[j]; {
		case c == '"':
			return j + 1, escaped, nil
		case c == '\\':
			escaped = true
			var bad int
			j, bad = scanEscape(data, j)
			if bad >= 0 {
				switch {
				case j < 0:
					_, err = s.fail(ErrSurrogate, msgSurrogate, bad)
				case bad >= n:
					_, err = s.fail(ErrSyntax, msgEOF, n)
				default:
					_, err = s.fail(ErrSyntax, msgEscape, bad)
				}
				return 0, false, err
			}
		case c < 0x20:
			_, err = s.fail(ErrSyntax, msgControl, j)
			return 0, false, err
		default:
			r, size := utf8.DecodeRune(data[j:])
			if r == utf8.RuneError && size == 1 {
				_, err = s.fail(ErrInvalidUTF8, msgUTF8, j)
				return 0, false, err
			}
			j += size
		}
	}
}

// scanEscape checks the escape sequence at data[j] == '\\'. It returns the
// offset past it and bad = -1, or bad >= 0 at the failure; next is -1 for an
// unpaired surrogate.
func scanEscape(data []byte, j int) (next, bad int) {
	if j+1 >= len(data) {
		return j, len(data)
	}
	switch data[j+1] {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		return j + 2, -1
	case 'u':
	default:
		return j, j
	}
	r, badHex := hex4(data, j+2)
	if badHex >= 0 {
		return j, badHex
	}
	if !utf16.IsSurrogate(r) {
		return j + 6, -1
	}
	if r >= 0xDC00 {
		return -1, j
	}
	// A high surrogate needs a low surrogate escape right after it. Input
	// that ends there, or a malformed escape there, is reported at its own
	// offset as the end of input or an invalid escape (01 req 15), not as an
	// unpaired surrogate.
	k := j + 6
	if k >= len(data) || data[k] == '\\' && k+1 >= len(data) {
		return j, len(data)
	}
	if data[k] == '\\' {
		switch data[k+1] {
		case 'u':
			r2, b := hex4(data, k+2)
			if b >= 0 {
				return j, b
			}
			if r2 >= 0xDC00 && r2 <= 0xDFFF {
				return j + 12, -1
			}
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			// A valid escape that is not a low surrogate.
		default:
			return j, k
		}
	}
	return -1, j
}

// hex4 decodes four hex digits at data[i:]. bad is -1 on success, else the
// offset of the first non-hex byte (len(data) when input ends).
func hex4(data []byte, i int) (r rune, bad int) {
	for k := i; k < i+4; k++ {
		if k >= len(data) {
			return 0, len(data)
		}
		c := data[k]
		var v byte
		switch {
		case c >= '0' && c <= '9':
			v = c - '0'
		case c >= 'a' && c <= 'f':
			v = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			v = c - 'A' + 10
		default:
			return 0, k
		}
		r = r<<4 | rune(v)
	}
	return r, -1
}

// scanNumber scans the RFC 8259 number at data[i]. bad is -1 on success,
// else the offset of the first byte that breaks the grammar.
func scanNumber[T ~string | ~[]byte](data T, i int) (end, bad int) {
	n := len(data)
	j := i
	if data[j] == '-' {
		j++
	}
	switch {
	case j >= n:
		return 0, j
	case data[j] == '0':
		j++
	case isDigit(data[j]):
		j++
		for j < n && isDigit(data[j]) {
			j++
		}
	default:
		return 0, j
	}
	if j < n && data[j] == '.' {
		j++
		if j >= n || !isDigit(data[j]) {
			return 0, j
		}
		for j < n && isDigit(data[j]) {
			j++
		}
	}
	if j < n && (data[j] == 'e' || data[j] == 'E') {
		j++
		if j < n && (data[j] == '+' || data[j] == '-') {
			j++
		}
		if j >= n || !isDigit(data[j]) {
			return 0, j
		}
		for j < n && isDigit(data[j]) {
			j++
		}
	}
	return j, -1
}

// isDigit reports an ASCII digit.
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// skipSpace returns the offset of the first non-whitespace byte at or after
// i; JSON whitespace is space, tab, line feed and carriage return.
func skipSpace(data []byte, i int) int {
	for i < len(data) {
		switch data[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// appendUnescaped appends the decoded form of a validated string body.
func appendUnescaped(dst, raw []byte) []byte {
	for len(raw) > 0 {
		k := bytes.IndexByte(raw, '\\')
		if k < 0 {
			return append(dst, raw...)
		}
		dst = append(dst, raw[:k]...)
		raw = raw[k:]
		switch raw[1] {
		case 'b':
			dst = append(dst, '\b')
		case 'f':
			dst = append(dst, '\f')
		case 'n':
			dst = append(dst, '\n')
		case 'r':
			dst = append(dst, '\r')
		case 't':
			dst = append(dst, '\t')
		case 'u':
			r, _ := hex4(raw, 2)
			if utf16.IsSurrogate(r) {
				r2, _ := hex4(raw, 8)
				dst = utf8.AppendRune(dst, utf16.DecodeRune(r, r2))
				raw = raw[12:]
				continue
			}
			dst = utf8.AppendRune(dst, r)
			raw = raw[6:]
			continue
		default: // '"', '\\', '/'
			dst = append(dst, raw[1])
		}
		raw = raw[2:]
	}
	return dst
}

// nameSet detects duplicate member names in the open objects. Names of one
// object are compared linearly up to linearNames, then through a per-level
// hash index keyed by a randomly seeded maphash, so crafted names cannot
// force quadratic work.
type nameSet struct {
	seed   maphash.Seed
	seeded bool
	data   []byte
	recs   []nameRec
	objs   []int // per open object: index of its first record
	index  []levelIndex
	a, b   []byte
}

// nameRec is one member name of an open object.
type nameRec struct {
	hash    uint64
	tok     Token
	nextDup int // previous record of this object with the same hash, or -1
}

// levelIndex is the hash index of the open object at one nesting level.
type levelIndex struct {
	m map[uint64]int
	// room is the entries m has room for: its size hint or the most entries
	// it has held, since a cleared map keeps its room.
	room int
}

// indexHint is the size hint of a new level index.
const indexHint = 2 * linearNames

// linearNames is the member count up to which names are compared linearly.
const linearNames = 16

// Scratch bounds (07 req 73): a reused Scanner, Decoder or Encoder keeps at
// most about maxScratch bytes in each scratch structure and drops larger
// ones, so reuse never pins memory sized by a large body.
const (
	// maxScratch is the largest byte buffer kept for reuse.
	maxScratch = 64 << 10
	// maxScratchNames bounds the name records kept, and the index entries
	// kept over all nesting levels: a nameRec takes 40 bytes on 64-bit
	// platforms, an index entry about as much.
	maxScratchNames = maxScratch / 40
	// maxScratchLevels bounds the per-level slices kept (8 to 16 bytes a
	// level).
	maxScratchLevels = maxScratch / 16
)

// reset closes every open object and drops the input and the scratch over
// the bounds: name records, nesting-level slices, index maps beyond a total
// room of maxScratchNames entries, and unescape buffers.
func (ns *nameSet) reset() {
	for len(ns.objs) > 0 {
		ns.close()
	}
	ns.data = nil
	ns.recs = ns.recs[:0]
	if cap(ns.recs) > maxScratchNames {
		ns.recs = nil
	}
	if cap(ns.objs) > maxScratchLevels {
		ns.objs = nil
	}
	if cap(ns.index) > maxScratchLevels {
		ns.index = nil
	}
	kept := 0
	for i := range ns.index {
		if kept+ns.index[i].room > maxScratchNames {
			ns.index[i] = levelIndex{}
			continue
		}
		kept += ns.index[i].room
	}
	if cap(ns.a) > maxScratch {
		ns.a = nil
	}
	if cap(ns.b) > maxScratch {
		ns.b = nil
	}
}

func (ns *nameSet) open() { ns.objs = append(ns.objs, len(ns.recs)) }

// close ends the innermost object. Its index map is cleared for the next
// object at the level, or dropped when it grew past maxScratchNames entries,
// since a cleared map keeps its room.
func (ns *nameSet) close() {
	level := len(ns.objs) - 1
	base := ns.objs[level]
	if n := len(ns.recs) - base; n >= linearNames {
		li := &ns.index[level]
		if n > maxScratchNames {
			*li = levelIndex{}
		} else {
			clear(li.m)
			li.room = max(li.room, n)
		}
	}
	ns.recs = ns.recs[:base]
	ns.objs = ns.objs[:level]
}

// add records name token t of the innermost object and returns the offset
// of an earlier member with the same name, or -1. Names are hashed only once
// an object reaches linearNames members.
func (ns *nameSet) add(data []byte, t Token) int {
	ns.data = data
	level := len(ns.objs) - 1
	base := ns.objs[level]
	count := len(ns.recs) - base
	if count < linearNames {
		for k := base; k < len(ns.recs); k++ {
			if ns.equal(ns.recs[k].tok, t) {
				return ns.recs[k].tok.Offset
			}
		}
		ns.recs = append(ns.recs, nameRec{tok: t, nextDup: -1})
		if count+1 == linearNames {
			m := ns.levelIndex(level)
			for k := base; k < len(ns.recs); k++ {
				r := &ns.recs[k]
				r.hash = ns.hash(r.tok)
				if prev, ok := m[r.hash]; ok {
					r.nextDup = prev
				}
				m[r.hash] = k
			}
		}
		return -1
	}
	h := ns.hash(t)
	rec := nameRec{hash: h, tok: t, nextDup: -1}
	m := ns.index[level].m
	if k, ok := m[h]; ok {
		rec.nextDup = k
		for ; k >= 0; k = ns.recs[k].nextDup {
			if ns.equal(ns.recs[k].tok, t) {
				return ns.recs[k].tok.Offset
			}
		}
	}
	m[h] = len(ns.recs)
	ns.recs = append(ns.recs, rec)
	return -1
}

// levelIndex returns the cleared index map of an object nesting level.
func (ns *nameSet) levelIndex(level int) map[uint64]int {
	for len(ns.index) <= level {
		ns.index = append(ns.index, levelIndex{})
	}
	li := &ns.index[level]
	if li.m == nil {
		li.m = make(map[uint64]int, indexHint)
		li.room = indexHint
	}
	return li.m
}

// hash hashes the decoded name with the set's random seed.
func (ns *nameSet) hash(t Token) uint64 {
	if !ns.seeded {
		ns.seed = maphash.MakeSeed()
		ns.seeded = true
	}
	raw := ns.data[t.Offset+1 : t.End-1]
	if !t.Escaped {
		return maphash.Bytes(ns.seed, raw)
	}
	ns.a = appendUnescaped(ns.a[:0], raw)
	return maphash.Bytes(ns.seed, ns.a)
}

// equal compares two decoded names.
func (ns *nameSet) equal(x, y Token) bool {
	rx := ns.data[x.Offset+1 : x.End-1]
	ry := ns.data[y.Offset+1 : y.End-1]
	if !x.Escaped && !y.Escaped {
		return bytes.Equal(rx, ry)
	}
	if x.Escaped {
		ns.a = appendUnescaped(ns.a[:0], rx)
		rx = ns.a
	}
	if y.Escaped {
		ns.b = appendUnescaped(ns.b[:0], ry)
		ry = ns.b
	}
	return bytes.Equal(rx, ry)
}

// Validate reports whether data is one JSON text under o: nil, or the
// *Error of the first failure.
func Validate(data []byte, o ScanOptions) error {
	var s Scanner
	s.Reset(data, o)
	for {
		if _, err := s.Next(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}
