// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package accesslog

import (
	"strconv"
	"time"
	"unicode/utf8"
)

// timeLayout is RFC 3339 in UTC with microseconds; UTC renders as "Z".
const timeLayout = "2006-01-02T15:04:05.000000Z07:00"

const hexDigits = "0123456789abcdef"

// Framing of every access line (09 req 68): slog-shaped, so stdout readers
// tell access lines from process lines by msg.
const (
	// Level is the level member of every access line.
	Level = "INFO"
	// Message is the msg member of every access line.
	Message = "access"
	// EventName is the OTLP event.name of access records.
	EventName = "ruralz.access"
)

// safeASCII reports whether b (below utf8.RuneSelf) stays unescaped in a
// JSON string.
func safeASCII(b byte) bool { return b >= 0x20 && b != '"' && b != '\\' }

// appendEscaped appends the escaped body of a JSON string without quotes:
// invalid UTF-8 becomes \ufffd, control characters and U+2028/U+2029 are
// escaped. fit computes the same sizes.
func appendEscaped(dst, s []byte) []byte {
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
		c, size := utf8.DecodeRune(s[i:])
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

func appendQuoted(dst, s []byte) []byte {
	dst = append(dst, '"')
	dst = appendEscaped(dst, s)
	return append(dst, '"')
}

// appendKey appends `,"key":` (keys are constant ASCII names).
func appendKey(dst []byte, key string) []byte {
	dst = append(dst, ',', '"')
	dst = append(dst, key...)
	return append(dst, '"', ':')
}

func appendHex(dst []byte, b []byte) []byte {
	dst = append(dst, '"')
	for _, c := range b {
		dst = append(dst, hexDigits[c>>4], hexDigits[c&0xF])
	}
	return append(dst, '"')
}

// appendSeconds appends d as seconds with exactly six decimals, a JSON
// number with microsecond precision (09 req 68, proposed).
func appendSeconds(dst []byte, d time.Duration) []byte {
	us := d.Microseconds()
	if us < 0 {
		dst = append(dst, '-')
		us = -us
	}
	dst = strconv.AppendInt(dst, us/1_000_000, 10)
	frac := us % 1_000_000
	dst = append(dst, '.',
		byte('0'+frac/100_000%10), byte('0'+frac/10_000%10), byte('0'+frac/1_000%10),
		byte('0'+frac/100%10), byte('0'+frac/10%10), byte('0'+frac%10))
	return dst
}

// appendField appends a string field when present.
func (e *entry) appendField(dst []byte, f field) []byte {
	if e.present&(1<<f) == 0 {
		return dst
	}
	dst = appendKey(dst, f.name())
	return appendQuoted(dst, e.str(e.spans[f]))
}

// appendAlways appends a string field, empty when absent.
func (e *entry) appendAlways(dst []byte, f field) []byte {
	dst = appendKey(dst, f.name())
	return appendQuoted(dst, e.str(e.spans[f]))
}

// appendLine encodes e as one JSON object (no newline) with the members
// of 09 req 68 in their fixed order. nodeMember is the preformatted
// `,"node_id":"..."` member.
func (e *entry) appendLine(dst, nodeMember []byte) []byte {
	dst = append(dst, `{"time":"`...)
	dst = e.start.UTC().AppendFormat(dst, timeLayout)
	dst = append(dst, `","level":"`+Level+`","msg":"`+Message+`"`...)
	dst = appendKey(dst, "trace_id")
	dst = appendHex(dst, e.traceID[:])
	dst = appendKey(dst, "span_id")
	dst = appendHex(dst, e.spanID[:])
	dst = appendKey(dst, "sampled")
	dst = strconv.AppendBool(dst, e.sampled)
	dst = append(dst, nodeMember...)
	dst = e.appendAlways(dst, fieldRevision)
	dst = e.appendAlways(dst, fieldListener)
	dst = e.appendAlways(dst, fieldProtocol)
	dst = e.appendAlways(dst, fieldRoute)
	dst = e.appendAlways(dst, fieldMethod)
	dst = e.appendAlways(dst, fieldHost)
	dst = e.appendAlways(dst, fieldPath)
	if e.truncated != 0 {
		dst = appendKey(dst, "truncated")
		dst = append(dst, '[')
		first := true
		for f := range numFields {
			if e.truncated&(1<<f) == 0 {
				continue
			}
			if !first {
				dst = append(dst, ',')
			}
			first = false
			dst = append(dst, '"')
			dst = append(dst, f.name()...)
			dst = append(dst, '"')
		}
		dst = append(dst, ']')
	}
	dst = appendKey(dst, "status")
	dst = strconv.AppendInt(dst, int64(e.status), 10)
	dst = e.appendField(dst, fieldCode)
	dst = appendKey(dst, "duration")
	dst = appendSeconds(dst, e.duration)
	dst = appendKey(dst, "request_bytes")
	dst = strconv.AppendInt(dst, e.requestBytes, 10)
	dst = appendKey(dst, "response_bytes")
	dst = strconv.AppendInt(dst, e.responseBytes, 10)
	if !e.gatewaySkip {
		dst = appendKey(dst, "gateway_duration")
		dst = appendSeconds(dst, e.gateway)
	}
	dst = appendKey(dst, "upstream_duration")
	dst = appendSeconds(dst, e.upstreamDur)
	dst = appendKey(dst, "state_store_duration")
	dst = appendSeconds(dst, e.stateStoreDur)
	dst = e.appendAlways(dst, fieldClientAddress)
	dst = e.appendAlways(dst, fieldUserAgent)
	dst = e.appendField(dst, fieldTLSVersion)
	dst = e.appendField(dst, fieldConsumer)
	dst = e.appendField(dst, fieldTier)
	dst = e.appendField(dst, fieldAuthMethod)
	if e.present&(1<<fieldUpstream) != 0 {
		dst = e.appendField(dst, fieldUpstream)
		dst = e.appendField(dst, fieldEndpoint)
		dst = appendKey(dst, "attempts")
		dst = strconv.AppendInt(dst, int64(e.attempts), 10)
	}
	if e.present&(1<<fieldShortCircuit) != 0 {
		dst = appendKey(dst, "short_circuit")
		dst = append(dst, `{"policy":`...)
		dst = appendQuoted(dst, e.str(e.spans[fieldShortCircuit]))
		dst = append(dst, `,"phase":"`...)
		dst = append(dst, e.scPhase...)
		dst = append(dst, '"', '}')
	}
	if e.nfm > 0 {
		dst = appendKey(dst, "failure_modes")
		dst = append(dst, '[')
		for i := range e.nfm {
			if i > 0 {
				dst = append(dst, ',')
			}
			fm := &e.fms[i]
			dst = append(dst, `{"policy":`...)
			dst = appendQuoted(dst, e.str(fm.policy))
			dst = append(dst, `,"phase":"`...)
			dst = append(dst, fm.phase...)
			dst = append(dst, `","mode":`...)
			dst = appendQuoted(dst, e.str(fm.mode))
			dst = append(dst, '}')
		}
		dst = append(dst, ']')
	}
	dst = e.appendField(dst, fieldCache)
	return append(dst, '}')
}

// nodeMember preformats the node_id member.
func nodeMember(nodeID string) []byte {
	dst := []byte(`,"node_id":"`)
	dst = appendEscaped(dst, []byte(nodeID))
	return append(dst, '"')
}
