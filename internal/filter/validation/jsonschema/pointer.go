// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// escapeToken returns tok escaped for an RFC 6901 JSON Pointer: "~" as
// "~0" and "/" as "~1".
func escapeToken(tok string) string {
	if !strings.ContainsAny(tok, "~/") {
		return tok
	}
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(tok)
}

// unescapeToken reverses escapeToken; ok is false for a "~" not followed
// by "0" or "1".
func unescapeToken(tok string) (string, bool) {
	if !strings.Contains(tok, "~") {
		return tok, true
	}
	var b strings.Builder
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		if c != '~' {
			b.WriteByte(c)
			continue
		}
		if i+1 >= len(tok) {
			return "", false
		}
		switch tok[i+1] {
		case '0':
			b.WriteByte('~')
		case '1':
			b.WriteByte('/')
		default:
			return "", false
		}
		i++
	}
	return b.String(), true
}

// appendToken returns the pointer p extended by the unescaped token tok.
func appendToken(p, tok string) string { return p + "/" + escapeToken(tok) }

// splitPointer returns the escaped tokens of a JSON Pointer; "" (the
// document root) has none. ok is false when p does not start with "/".
func splitPointer(p string) (toks []string, ok bool) {
	if p == "" {
		return nil, true
	}
	if p[0] != '/' {
		return nil, false
	}
	return strings.Split(p[1:], "/"), true
}

// encodeFragment returns p as a URI fragment the way the jsonschema
// library writes schema locations: each token percent-encoded as a path
// segment.
func encodeFragment(p string) string {
	toks, _ := splitPointer(p)
	var b strings.Builder
	for _, t := range toks {
		b.WriteByte('/')
		b.WriteString(url.PathEscape(t))
	}
	return b.String()
}

// pointerOf returns the JSON Pointer of a schema location URL compiled
// under baseURL; ok is false for a location outside the document.
func pointerOf(location string) (string, bool) {
	frag, ok := strings.CutPrefix(location, baseURL+"#")
	if !ok {
		return "", false
	}
	p, err := url.PathUnescape(frag)
	if err != nil || (p != "" && p[0] != '/') {
		return "", false
	}
	return p, true
}

// lookup resolves the JSON Pointer p in doc as the jsonschema library
// does: an object member by name, an array element by a decimal index.
func lookup(doc any, p string) (any, bool) {
	toks, ok := splitPointer(p)
	if !ok {
		return nil, false
	}
	v := doc
	for _, raw := range toks {
		tok, ok := unescapeToken(raw)
		if !ok {
			return nil, false
		}
		switch t := v.(type) {
		case map[string]any:
			if v, ok = t[tok]; !ok {
				return nil, false
			}
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(t) {
				return nil, false
			}
			v = t[i]
		default:
			return nil, false
		}
	}
	return v, true
}

// comparePointers orders JSON Pointers token by token: two decimal tokens
// by numeric value (so "/allOf/2" precedes "/allOf/10"), others bytewise,
// and a pointer before every pointer it prefixes.
func comparePointers(a, b string) int {
	ta, _ := splitPointer(a)
	tb, _ := splitPointer(b)
	for i := 0; i < len(ta) && i < len(tb); i++ {
		if c := compareTokens(ta[i], tb[i]); c != 0 {
			return c
		}
	}
	return len(ta) - len(tb)
}

func compareTokens(a, b string) int {
	if isDecimal(a) && isDecimal(b) {
		a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
		if len(a) != len(b) {
			return len(a) - len(b)
		}
	}
	return strings.Compare(a, b)
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// schemaPath returns the key-aware path of the JSON Pointer p inside the
// schema document doc, under prefix: an object member is a field, an
// array element an index. Tokens past the end of the document stay fields,
// so a finding about a missing member still names it.
func schemaPath(prefix diag.Path, doc any, p string) diag.Path {
	out := prefix.Append()
	toks, _ := splitPointer(p)
	v, inDoc := doc, true
	for _, raw := range toks {
		tok, ok := unescapeToken(raw)
		if !ok {
			tok = raw
		}
		if arr, isArr := v.([]any); inDoc && isArr {
			if i, err := strconv.Atoi(tok); err == nil && i >= 0 && i < len(arr) {
				out = append(out, diag.Index(i))
				v = arr[i]
				continue
			}
		}
		out = append(out, diag.Field(tok))
		if m, isMap := v.(map[string]any); inDoc && isMap {
			if v, ok = m[tok]; ok {
				continue
			}
		}
		inDoc = false
	}
	return out
}
