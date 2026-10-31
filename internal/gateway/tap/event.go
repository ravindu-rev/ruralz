// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tap

import (
	"encoding/hex"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/redact"
)

// Event is one finished exchange as the request handler reports it. The
// handler fills it from values it already holds; Publish encodes it before
// returning and keeps no reference to it or to its headers.
type Event struct {
	// Time is the exchange start, written in RFC 3339 UTC.
	Time time.Time
	// TraceID is the request's trace ID, the requestId of its errors.
	TraceID [16]byte
	// Listener, Protocol and Route are the listener name, its protocol
	// label (http1, http2) and the matched Route, or _unmatched.
	Listener, Protocol, Route string
	// Method and Host are the request method and host.
	Method, Host string
	// Path is the normalized request path; a query string or fragment is
	// cut off before encoding.
	Path string
	// Status is the final status and Code its RZ code, "" for none.
	Status int
	Code   string
	// Duration is the exchange's total time.
	Duration time.Duration
	// RequestBytes and ResponseBytes count body bytes.
	RequestBytes, ResponseBytes int64
	// Upstream and Endpoint describe the last upstream leg, Consumer the
	// authenticated Consumer; "" for none.
	Upstream, Endpoint, Consumer string
	// RequestHeaders and ResponseHeaders are the client-side headers.
	RequestHeaders, ResponseHeaders http.Header
	// Credentials is the credential-header set of the snapshot that served
	// the exchange; nil redacts the fixed credential headers and the
	// sensitive names only.
	Credentials *redact.Set
}

// redactedValue is a redacted header value as a JSON string.
const redactedValue = `"` + redact.Marker + `"`

// stackNames is the number of header names sorted without allocating.
const stackNames = 32

// appendLine appends ev as one NDJSON line: the adminapi.TapEvent members
// in their documented order, empty optional members omitted, then '\n'.
func appendLine(dst []byte, ev *Event) []byte {
	dst = append(dst, `{"time":"`...)
	dst = ev.Time.UTC().AppendFormat(dst, time.RFC3339Nano)
	dst = append(dst, `","traceId":"`...)
	dst = hex.AppendEncode(dst, ev.TraceID[:])
	dst = append(dst, `","listener":`...)
	dst = appendString(dst, ev.Listener, false)
	dst = append(dst, `,"protocol":`...)
	dst = appendString(dst, ev.Protocol, false)
	dst = append(dst, `,"route":`...)
	dst = appendString(dst, ev.Route, false)
	dst = append(dst, `,"method":`...)
	dst = appendString(dst, ev.Method, false)
	dst = append(dst, `,"host":`...)
	dst = appendString(dst, ev.Host, false)
	dst = append(dst, `,"path":`...)
	dst = appendString(dst, redact.Path(ev.Path), false)
	dst = append(dst, `,"status":`...)
	dst = strconv.AppendInt(dst, int64(ev.Status), 10)
	dst = appendOptional(dst, `,"code":`, ev.Code)
	dst = append(dst, `,"durationMs":`...)
	dst = strconv.AppendFloat(dst, float64(ev.Duration)/float64(time.Millisecond), 'f', -1, 64)
	dst = append(dst, `,"requestBytes":`...)
	dst = strconv.AppendInt(dst, ev.RequestBytes, 10)
	dst = append(dst, `,"responseBytes":`...)
	dst = strconv.AppendInt(dst, ev.ResponseBytes, 10)
	dst = appendOptional(dst, `,"upstream":`, ev.Upstream)
	dst = appendOptional(dst, `,"endpoint":`, ev.Endpoint)
	dst = appendOptional(dst, `,"consumer":`, ev.Consumer)
	if len(ev.RequestHeaders) > 0 {
		dst = append(dst, `,"requestHeaders":`...)
		dst = appendHeaders(dst, ev.RequestHeaders, ev.Credentials)
	}
	if len(ev.ResponseHeaders) > 0 {
		dst = append(dst, `,"responseHeaders":`...)
		dst = appendHeaders(dst, ev.ResponseHeaders, ev.Credentials)
	}
	return append(dst, '}', '\n')
}

// appendOptional appends member and the JSON string s unless s is empty.
func appendOptional(dst []byte, member, s string) []byte {
	if s == "" {
		return dst
	}
	return appendString(append(dst, member...), s, false)
}

// appendHeaders appends the /tap view of h: names in lowercase and in byte
// order, each value of a name set redacts replaced by "[REDACTED]". Names
// that differ only in case (a hand-built header) merge into one member,
// their values in the byte order of the spellings.
func appendHeaders(dst []byte, h http.Header, set *redact.Set) []byte {
	var stack [stackNames]string
	names := stack[:0]
	for name := range h {
		names = append(names, name)
	}
	slices.SortFunc(names, compareNames)
	dst = append(dst, '{')
	for i := 0; i < len(names); {
		j := i + 1
		for j < len(names) && compareLower(names[i], names[j]) == 0 {
			j++
		}
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = appendString(dst, names[i], true)
		dst = append(dst, ':', '[')
		n := 0
		for _, name := range names[i:j] {
			redacted := set.Redacts(name)
			for _, v := range h[name] {
				if n > 0 {
					dst = append(dst, ',')
				}
				n++
				if redacted {
					dst = append(dst, redactedValue...)
				} else {
					dst = appendString(dst, v, false)
				}
			}
		}
		dst = append(dst, ']')
		i = j
	}
	return append(dst, '}')
}

// compareNames orders header names by their ASCII lowercase form, then by
// their bytes, so spellings of one name are adjacent and in a fixed order.
func compareNames(a, b string) int {
	if c := compareLower(a, b); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

// compareLower compares a and b with ASCII letters lowercased.
func compareLower(a, b string) int {
	n := min(len(a), len(b))
	for i := range n {
		ca, cb := lower(a[i]), lower(b[i])
		if ca != cb {
			if ca < cb {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// lower returns c with an ASCII uppercase letter lowercased.
func lower(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// hexDigits are the digits of \u00xx escapes.
const hexDigits = "0123456789abcdef"

// appendString appends s as a JSON string: '"', '\\' and characters below
// U+0020 escaped, invalid UTF-8 replaced by U+FFFD, and with toLower the
// ASCII uppercase letters lowercased. It never fails, so no header value
// can stop an event.
func appendString(dst []byte, s string, toLower bool) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c >= utf8.RuneSelf {
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				dst = append(dst, s[start:i]...)
				dst = append(dst, "�"...)
				i++
				start = i
				continue
			}
			i += size
			continue
		}
		if c >= 0x20 && c != '"' && c != '\\' && (!toLower || c < 'A' || c > 'Z') {
			i++
			continue
		}
		dst = append(dst, s[start:i]...)
		switch {
		case c == '"' || c == '\\':
			dst = append(dst, '\\', c)
		case c == '\n':
			dst = append(dst, '\\', 'n')
		case c == '\r':
			dst = append(dst, '\\', 'r')
		case c == '\t':
			dst = append(dst, '\\', 't')
		case c < 0x20:
			dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
		default:
			dst = append(dst, lower(c))
		}
		i++
		start = i
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

// appendDropped appends the {"dropped":n} line.
func appendDropped(dst []byte, n uint64) []byte {
	dst = append(dst, `{"dropped":`...)
	dst = strconv.AppendUint(dst, n, 10)
	return append(dst, '}', '\n')
}
