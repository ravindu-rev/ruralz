// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resolver

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf16"
	"unicode/utf8"
)

// Fixed reasons of the JSON member extraction (spec 06 requirement 89:
// parse errors are replaced by fixed messages that never contain file
// content).
var (
	errJSONNotUTF8   = errors.New("file is not valid UTF-8")
	errJSONNotObject = errors.New("file is not a JSON object")
	errJSONMissing   = errors.New("the JSON object has no member named by key")
	errJSONDuplicate = errors.New("the JSON object names the key member more than once")
	errJSONNotString = errors.New("the key member is not a JSON string")
	errJSONSurrogate = errors.New("the key member escapes a lone UTF-16 surrogate, which is not Unicode")
)

// jsonMember returns the UTF-8 bytes of the string member key of the JSON
// object doc (spec 01 requirement 44). doc must be one object and nothing
// else; a duplicated key member is refused as ambiguous, and so is a
// member escaping a lone UTF-16 surrogate, which encoding/json would turn
// into U+FFFD, bytes the author never wrote. Every error is a fixed
// reason. The raw copies of members are cleared (best effort: the
// decoder's own buffer and the decoded string are not).
func jsonMember(doc []byte, key string) ([]byte, error) {
	if !utf8.Valid(doc) {
		return nil, errJSONNotUTF8
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errJSONNotObject
	}
	var found json.RawMessage
	defer func() { clear(found) }()
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, errJSONNotObject
		}
		name, ok := tok.(string)
		if !ok {
			return nil, errJSONNotObject
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			clear(raw)
			return nil, errJSONNotObject
		}
		if name != key {
			clear(raw)
			continue
		}
		if found != nil {
			clear(raw)
			return nil, errJSONDuplicate
		}
		found = raw
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, errJSONNotObject
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errJSONNotObject
	}
	if found == nil {
		return nil, errJSONMissing
	}
	if len(found) == 0 || found[0] != '"' {
		return nil, errJSONNotString
	}
	if loneSurrogate(found) {
		return nil, errJSONSurrogate
	}
	var s string
	if err := json.Unmarshal(found, &s); err != nil {
		return nil, errJSONNotString
	}
	return []byte(s), nil
}

// loneSurrogate reports whether the JSON string literal lit, already
// accepted by the decoder, escapes a UTF-16 surrogate that is not half of
// a high-low pair (\uD800 to \uDBFF followed at once by \uDC00 to \uDFFF).
func loneSurrogate(lit []byte) bool {
	for i := 0; i < len(lit); {
		if lit[i] != '\\' {
			i++
			continue
		}
		u, ok := escapedUnit(lit, i)
		if !ok {
			i += 2 // a two-byte escape such as \" or \\
			continue
		}
		i += 6
		if !utf16.IsSurrogate(u) {
			continue
		}
		if u >= 0xDC00 {
			return true // a low surrogate without a high one
		}
		lo, ok := escapedUnit(lit, i)
		if !ok || lo < 0xDC00 || lo > 0xDFFF {
			return true // a high surrogate without a low one
		}
		i += 6
	}
	return false
}

// escapedUnit returns the UTF-16 code unit of the \uXXXX escape at lit[i:],
// or false when there is none.
func escapedUnit(lit []byte, i int) (rune, bool) {
	if i+6 > len(lit) || lit[i] != '\\' || lit[i+1] != 'u' {
		return 0, false
	}
	var u rune
	for _, c := range lit[i+2 : i+6] {
		var d byte
		switch {
		case '0' <= c && c <= '9':
			d = c - '0'
		case 'a' <= c && c <= 'f':
			d = c - 'a' + 10
		case 'A' <= c && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, false
		}
		u = u<<4 | rune(d)
	}
	return u, true
}
