// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"net/http"
	"strings"
)

// W3C Trace Context header names in net/http canonical form, so a header
// map is indexed without canonicalizing a key per request.
const (
	HeaderTraceparent = "Traceparent"
	HeaderTracestate  = "Tracestate"
)

// Carrier keys of the W3C fields for non-HTTP legs (lower case).
const (
	KeyTraceparent = "traceparent"
	KeyTracestate  = "tracestate"
)

// TraceparentLen is the length of a version 00 traceparent value, and the
// prefix of a higher version that this parser reads (spec 09 req 27).
const TraceparentLen = 55

// Limits of a tracestate a Node keeps and re-injects; a longer value or
// one with more list members is dropped (spec 09 req 27, proposed).
const (
	MaxTracestateBytes   = 512
	MaxTracestateMembers = 32
)

// FlagSampled is bit 0 of trace-flags.
const FlagSampled byte = 0x01

// Key and value length limits of W3C Trace Context tracestate list members.
const (
	maxSimpleKey = 256
	maxTenantID  = 241
	maxSystemID  = 14
	maxValue     = 256
)

const hexDigits = "0123456789abcdef"

// ParseTraceparent parses a traceparent value per W3C Trace Context (spec
// 09 req 27): leading and trailing spaces and tabs are ignored; version 00
// is exactly 55 bytes "00-<32 lowercase hex>-<16 lowercase hex>-<2
// lowercase hex>"; version ff is invalid; a higher version is read on its
// first 55 bytes when byte 55 is "-" or absent; the trace ID and the
// parent ID are not all zeros. On any failure ok is false and the other
// results are zero. It never allocates.
func ParseTraceparent(v string) (traceID [16]byte, parentID [8]byte, flags byte, ok bool) {
	v = trimOWS(v)
	if len(v) < TraceparentLen {
		return traceID, parentID, 0, false
	}
	ver, vok := hexByte(v[0], v[1])
	if !vok || ver == 0xff {
		return traceID, parentID, 0, false
	}
	switch {
	case ver == 0 && len(v) != TraceparentLen:
		return traceID, parentID, 0, false
	case len(v) > TraceparentLen && v[TraceparentLen] != '-':
		return traceID, parentID, 0, false
	}
	if v[2] != '-' || v[35] != '-' || v[52] != '-' {
		return traceID, parentID, 0, false
	}
	var tid [16]byte
	var pid [8]byte
	if !decodeHex(tid[:], v[3:35]) || !decodeHex(pid[:], v[36:52]) {
		return traceID, parentID, 0, false
	}
	fl, fok := hexByte(v[53], v[54])
	if !fok || tid == ([16]byte{}) || pid == ([8]byte{}) {
		return traceID, parentID, 0, false
	}
	return tid, pid, fl, true
}

// AppendTraceparent appends a version 00 traceparent value with flags 01
// when sampled and 00 otherwise (spec 09 req 31).
func AppendTraceparent(dst []byte, traceID [16]byte, spanID [8]byte, sampled bool) []byte {
	dst = append(dst, '0', '0', '-')
	dst = appendHex(dst, traceID[:])
	dst = append(dst, '-')
	dst = appendHex(dst, spanID[:])
	if sampled {
		return append(dst, '-', '0', '1')
	}
	return append(dst, '-', '0', '0')
}

// AppendTraceID appends the 32 lowercase hex characters of id: the
// requestId of every problem document and the access log trace_id (spec
// 09 req 32).
func AppendTraceID(dst []byte, id [16]byte) []byte { return appendHex(dst, id[:]) }

// AppendSpanID appends the 16 lowercase hex characters of id.
func AppendSpanID(dst []byte, id [8]byte) []byte { return appendHex(dst, id[:]) }

// FormatTraceID returns the 32 lowercase hex characters of id.
func FormatTraceID(id [16]byte) string {
	var b [32]byte
	return string(appendHex(b[:0], id[:]))
}

// ValidTracestate reports whether v is a tracestate this Node keeps: at
// most MaxTracestateBytes bytes and MaxTracestateMembers list members, each
// member "key=value" with a W3C simple or multi-tenant key and a W3C
// value, spaces and tabs allowed around members, empty members allowed,
// keys unique. An empty v is not valid (there is nothing to keep). It
// never allocates.
func ValidTracestate(v string) bool {
	if v == "" || len(v) > MaxTracestateBytes {
		return false
	}
	var m tracestateMembers
	return m.add(v) && m.n > 0
}

// tracestateMembers collects the keys of one tracestate, possibly spread
// over several field lines, to check the member count and key uniqueness.
type tracestateMembers struct {
	keys [MaxTracestateMembers]string
	n    int
}

// add validates the list members of v and records their keys; empty
// members are allowed. It reports false on an invalid member, a repeated
// key or more than MaxTracestateMembers members in all.
func (m *tracestateMembers) add(v string) bool {
	for rest := v; ; {
		member, tail, more := strings.Cut(rest, ",")
		member = trimOWS(member)
		if member != "" {
			if m.n == MaxTracestateMembers {
				return false
			}
			key, value, found := strings.Cut(member, "=")
			if !found || !validKey(key) || !validValue(value) {
				return false
			}
			for _, k := range m.keys[:m.n] {
				if k == key {
					return false
				}
			}
			m.keys[m.n] = key
			m.n++
		}
		if !more {
			return true
		}
		rest = tail
	}
}

// tracestate returns the validated tracestate of h, "" when absent or
// invalid. Several field lines are one list combined with commas (RFC
// 9110 section 5.3): they are validated in place, and only a valid
// combination allocates its combined value.
func tracestate(h http.Header) string {
	vs := h[HeaderTracestate]
	switch len(vs) {
	case 0:
		return ""
	case 1:
		if v := trimOWS(vs[0]); ValidTracestate(v) {
			return v
		}
		return ""
	}
	total := len(vs) - 1
	for _, s := range vs {
		total += len(s)
	}
	if total > MaxTracestateBytes {
		return ""
	}
	var m tracestateMembers
	for _, s := range vs {
		if !m.add(s) {
			return ""
		}
	}
	if m.n == 0 {
		return ""
	}
	return trimOWS(strings.Join(vs, ","))
}

// validKey reports a W3C tracestate key: simple-key or tenant-id@system-id.
func validKey(k string) bool {
	tenant, system, multi := strings.Cut(k, "@")
	if !multi {
		return len(k) <= maxSimpleKey && k != "" && isLower(k[0]) && keyChars(k[1:])
	}
	if tenant == "" || len(tenant) > maxTenantID || (!isLower(tenant[0]) && !isDigit(tenant[0])) || !keyChars(tenant[1:]) {
		return false
	}
	return system != "" && len(system) <= maxSystemID && isLower(system[0]) && keyChars(system[1:])
}

// keyChars reports whether every byte of s is lcalpha, DIGIT, "_", "-",
// "*" or "/".
func keyChars(s string) bool {
	for i := range len(s) {
		c := s[i]
		if !isLower(c) && !isDigit(c) && c != '_' && c != '-' && c != '*' && c != '/' {
			return false
		}
	}
	return true
}

// validValue reports a W3C tracestate value: 1 to 256 printable ASCII
// characters other than "," and "=", spaces allowed except at the end.
func validValue(v string) bool {
	if v == "" || len(v) > maxValue || v[len(v)-1] == ' ' {
		return false
	}
	for i := range len(v) {
		c := v[i]
		if c < 0x20 || c > 0x7e || c == ',' || c == '=' {
			return false
		}
	}
	return true
}

func isLower(c byte) bool { return c >= 'a' && c <= 'z' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// trimOWS removes leading and trailing spaces and tabs.
func trimOWS(s string) string {
	for s != "" && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for s != "" && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// hexNibble decodes one lowercase hex digit.
func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	default:
		return 0, false
	}
}

// hexByte decodes two lowercase hex digits.
func hexByte(hi, lo byte) (byte, bool) {
	h, ok1 := hexNibble(hi)
	l, ok2 := hexNibble(lo)
	return h<<4 | l, ok1 && ok2
}

// decodeHex decodes the len(dst)*2 lowercase hex digits of s into dst.
func decodeHex(dst []byte, s string) bool {
	for i := range dst {
		b, ok := hexByte(s[2*i], s[2*i+1])
		if !ok {
			return false
		}
		dst[i] = b
	}
	return true
}

// appendHex appends src as lowercase hex.
func appendHex(dst, src []byte) []byte {
	for _, b := range src {
		dst = append(dst, hexDigits[b>>4], hexDigits[b&0x0f])
	}
	return dst
}
