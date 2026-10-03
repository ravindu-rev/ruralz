// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package logsink

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"strconv"
	"time"
	"unicode/utf8"
)

// timeLayout is RFC 3339 in UTC with microseconds (09 req 63); UTC
// renders the zone as "Z".
const timeLayout = "2006-01-02T15:04:05.000000Z07:00"

const hexDigits = "0123456789abcdef"

// safeASCII reports whether b (below utf8.RuneSelf) stays unescaped in a
// JSON string: printable ASCII except '"' and '\' (encoding/json's safe set).
func safeASCII(b byte) bool { return b >= 0x20 && b != '"' && b != '\\' }

// appendString appends s as a quoted JSON string. Invalid UTF-8 becomes
// \ufffd, control characters and U+2028/U+2029 are escaped, HTML
// characters are not (as slog.JSONHandler does).
func appendString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	dst = appendEscaped(dst, s)
	return append(dst, '"')
}

// appendEscaped appends the escaped body of a JSON string without quotes.
func appendEscaped(dst []byte, s string) []byte {
	start := 0
	for i := 0; i < len(s); {
		b := s[i]
		if b < utf8.RuneSelf {
			if safeASCII(b) {
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			switch b {
			case '\\', '"':
				dst = append(dst, '\\', b)
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[b>>4], hexDigits[b&0xF])
			}
			i++
			start = i
			continue
		}
		c, size := utf8.DecodeRuneInString(s[i:])
		if c == utf8.RuneError && size == 1 {
			dst = append(dst, s[start:i]...)
			dst = append(dst, `\ufffd`...)
			i++
			start = i
			continue
		}
		if c == '\u2028' || c == '\u2029' {
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\\', 'u', '2', '0', '2', hexDigits[c&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	return append(dst, s[start:]...)
}

// appendKey appends a member name, preceded by a comma unless it opens the
// enclosing object.
func appendKey(dst []byte, key string) []byte {
	if n := len(dst); n > 0 && dst[n-1] != '{' {
		dst = append(dst, ',')
	}
	dst = appendString(dst, key)
	return append(dst, ':')
}

// appendTime appends t as a quoted RFC 3339 UTC time with microseconds.
func appendTime(dst []byte, t time.Time) []byte {
	dst = append(dst, '"')
	dst = t.UTC().AppendFormat(dst, timeLayout)
	return append(dst, '"')
}

// appendHex appends b as a quoted lower-case hex string.
func appendHex(dst []byte, b []byte) []byte {
	dst = append(dst, '"')
	for _, c := range b {
		dst = append(dst, hexDigits[c>>4], hexDigits[c&0xF])
	}
	return append(dst, '"')
}

// appendFloat appends f the way encoding/json does; NaN and infinities,
// which JSON cannot represent, become the strings "NaN", "+Inf", "-Inf".
func appendFloat(dst []byte, f float64) []byte {
	switch {
	case math.IsNaN(f):
		return append(dst, `"NaN"`...)
	case math.IsInf(f, 1):
		return append(dst, `"+Inf"`...)
	case math.IsInf(f, -1):
		return append(dst, `"-Inf"`...)
	}
	format := byte('f')
	if abs := math.Abs(f); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	dst = strconv.AppendFloat(dst, f, format, -1, 64)
	if format == 'e' {
		// Clean up e-09 to e-9, as encoding/json does.
		if n := len(dst); n >= 4 && dst[n-4] == 'e' && dst[n-3] == '-' && dst[n-2] == '0' {
			dst[n-2] = dst[n-1]
			dst = dst[:n-1]
		}
	}
	return dst
}

// appendScalar appends a resolved, non-group value as JSON. Durations are
// integer nanoseconds and errors their Error text, as slog.JSONHandler
// writes them; any other value is encoded with encoding/json.
func appendScalar(dst []byte, v slog.Value) []byte {
	switch v.Kind() {
	case slog.KindString:
		return appendString(dst, v.String())
	case slog.KindInt64:
		return strconv.AppendInt(dst, v.Int64(), 10)
	case slog.KindUint64:
		return strconv.AppendUint(dst, v.Uint64(), 10)
	case slog.KindFloat64:
		return appendFloat(dst, v.Float64())
	case slog.KindBool:
		return strconv.AppendBool(dst, v.Bool())
	case slog.KindDuration:
		return strconv.AppendInt(dst, int64(v.Duration()), 10)
	case slog.KindTime:
		return appendTime(dst, v.Time())
	default:
		return appendAny(dst, v.Any())
	}
}

// appendAny encodes an arbitrary value; a panic from a nil receiver prints
// "<nil>" and any other failure is written as an "!ERROR:" string, so
// encoding never fails a record.
func appendAny(dst []byte, a any) (out []byte) {
	mark := len(dst)
	defer func() {
		if r := recover(); r != nil {
			out = dst[:mark]
			if rv := reflect.ValueOf(a); rv.Kind() == reflect.Pointer && rv.IsNil() {
				out = appendString(out, "<nil>")
				return
			}
			out = appendString(out, fmt.Sprintf("!PANIC: %v", r))
		}
	}()
	if a == nil {
		return append(dst, "null"...)
	}
	if _, marshaler := a.(json.Marshaler); !marshaler {
		if err, ok := a.(error); ok {
			return appendString(dst, err.Error())
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(a); err != nil {
		return appendString(dst, "!ERROR:"+err.Error())
	}
	return append(dst, bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})...)
}
