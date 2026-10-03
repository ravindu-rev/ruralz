// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package accesslog

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Record limits (09 req 67, 68; targets).
const (
	// MaxRecordBytes caps the escaped bytes of a record's string values;
	// fields past it are cut and named in truncated.
	MaxRecordBytes = 4096
	// MaxHostBytes caps host.
	MaxHostBytes = 256
	// MaxPathBytes caps path.
	MaxPathBytes = 1024
	// MaxUserAgentBytes caps user_agent.
	MaxUserAgentBytes = 256
	// MaxFailureModes caps failure_modes.
	MaxFailureModes = 8
)

// field indexes the string fields of a record, in encoding order; the
// order is also the order of the names in truncated and the order in
// which fields draw on the MaxRecordBytes budget. fieldFailureModes has
// no span of its own.
type field uint8

const (
	fieldRevision field = iota
	fieldListener
	fieldProtocol
	fieldRoute
	fieldMethod
	fieldHost
	fieldPath
	fieldCode
	fieldClientAddress
	fieldUserAgent
	fieldTLSVersion
	fieldConsumer
	fieldTier
	fieldAuthMethod
	fieldUpstream
	fieldEndpoint
	fieldShortCircuit
	fieldFailureModes
	fieldCache
	numFields
)

// name returns a field's JSON member name.
func (f field) name() string {
	switch f {
	case fieldRevision:
		return "revision"
	case fieldListener:
		return "listener"
	case fieldProtocol:
		return "protocol"
	case fieldRoute:
		return "route"
	case fieldMethod:
		return "method"
	case fieldHost:
		return "host"
	case fieldPath:
		return "path"
	case fieldCode:
		return "code"
	case fieldClientAddress:
		return "client_address"
	case fieldUserAgent:
		return "user_agent"
	case fieldTLSVersion:
		return "tls_version"
	case fieldConsumer:
		return "consumer"
	case fieldTier:
		return "tier"
	case fieldAuthMethod:
		return "auth_method"
	case fieldUpstream:
		return "upstream"
	case fieldEndpoint:
		return "endpoint"
	case fieldShortCircuit:
		return "short_circuit"
	case fieldFailureModes:
		return "failure_modes"
	case fieldCache:
		return "cache"
	default:
		return ""
	}
}

// span locates a copied value in an entry's arena.
type span struct{ off, n uint16 }

// failureMode is one copied failure_modes element.
type failureMode struct {
	policy, mode span
	phase        string // constant Phase spelling
}

// Arena size classes, 256 bytes to 4 KiB: an entry takes the smallest
// class that holds its copied strings.
const numClasses = 5

func classSize(c int) int { return 256 << c }

// entryOverhead is the fixed part of a record's actual size (09 req 67):
// its start time, trace and span IDs, status, durations, byte counts and
// attempts. Submit adds the bytes copied into the arena.
const entryOverhead = 128

// entry is a queued record: fixed fields by value and every string copied,
// truncated, into its own arena, so it never references request memory
// (09 req 67).
type entry struct {
	class         uint8
	start         time.Time
	traceID       [16]byte
	spanID        [8]byte
	sampled       bool
	status        int
	duration      time.Duration
	gateway       time.Duration
	gatewaySkip   bool
	upstreamDur   time.Duration
	stateStoreDur time.Duration
	requestBytes  int64
	responseBytes int64
	attempts      int
	scPhase       string // constant Phase spelling of the short circuit
	present       uint32 // bit per field: the source value was not empty
	truncated     uint32 // bit per field: the value was cut
	spans         [numFields]span
	fms           [MaxFailureModes]failureMode
	nfm           int
	budget        int   // escaped bytes left of MaxRecordBytes
	size          int64 // bytes reserved from the queue budget
	arena         []byte
}

// reset clears e for reuse, keeping its arena.
func (e *entry) reset() {
	arena, class := e.arena[:0], e.class
	*e = entry{arena: arena, class: class}
}

// str returns a copied value.
func (e *entry) str(s span) []byte { return e.arena[s.off : s.off+s.n] }

// need returns the arena bytes r's strings need after the per-field caps,
// at most MaxRecordBytes.
func need(r *emit.AccessRecord) int {
	n := len(r.Revision) + len(r.Listener) + len(r.Protocol) + len(r.Route) + len(r.Method) +
		min(len(r.Host), MaxHostBytes) + min(len(stripQuery(r.Path)), MaxPathBytes) + len(r.Code) +
		len(r.ClientAddress) + min(len(r.UserAgent), MaxUserAgentBytes) + len(r.TLSVersion) +
		len(r.Consumer) + len(r.Tier) + len(r.AuthMethod) + len(r.Upstream) + len(r.Endpoint) +
		len(r.ShortCircuitPolicy) + len(r.Cache)
	for i, fm := range r.FailureModes {
		if i == MaxFailureModes {
			break
		}
		n += len(fm.Policy) + len(fm.Mode)
	}
	return min(n, MaxRecordBytes)
}

// classFor returns the smallest arena class holding n bytes.
func classFor(n int) int {
	for c := range numClasses {
		if n <= classSize(c) {
			return c
		}
	}
	return numClasses - 1
}

// stripQuery removes a query string from a request path: the access log
// never carries one (09 req 68).
func stripQuery(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

// fill copies r into e: fixed fields by value, strings truncated into the
// arena in field order. e must be reset.
func (e *entry) fill(r *emit.AccessRecord) {
	e.budget = MaxRecordBytes
	e.start = r.Start
	e.traceID = r.TraceID
	e.spanID = r.SpanID
	e.sampled = r.Sampled
	e.status = r.Status
	e.duration = r.Duration
	e.gateway = r.GatewayDuration
	e.gatewaySkip = r.GatewayDurationSkipped
	e.upstreamDur = r.UpstreamDuration
	e.stateStoreDur = r.StateStoreDuration
	e.requestBytes = r.RequestBytes
	e.responseBytes = r.ResponseBytes
	e.attempts = r.Attempts

	e.put(fieldRevision, r.Revision, MaxRecordBytes)
	e.put(fieldListener, r.Listener, MaxRecordBytes)
	e.put(fieldProtocol, r.Protocol, MaxRecordBytes)
	e.put(fieldRoute, r.Route, MaxRecordBytes)
	e.put(fieldMethod, r.Method, MaxRecordBytes)
	e.put(fieldHost, r.Host, MaxHostBytes)
	e.put(fieldPath, stripQuery(r.Path), MaxPathBytes)
	e.put(fieldCode, r.Code, MaxRecordBytes)
	e.put(fieldClientAddress, r.ClientAddress, MaxRecordBytes)
	e.put(fieldUserAgent, r.UserAgent, MaxUserAgentBytes)
	e.put(fieldTLSVersion, r.TLSVersion, MaxRecordBytes)
	e.put(fieldConsumer, r.Consumer, MaxRecordBytes)
	e.put(fieldTier, r.Tier, MaxRecordBytes)
	e.put(fieldAuthMethod, r.AuthMethod, MaxRecordBytes)
	e.put(fieldUpstream, r.Upstream, MaxRecordBytes)
	e.put(fieldEndpoint, r.Endpoint, MaxRecordBytes)
	if r.ShortCircuitPolicy != "" {
		e.put(fieldShortCircuit, r.ShortCircuitPolicy, MaxRecordBytes)
		e.scPhase = r.ShortCircuitPhase.String()
	}
	for i, fm := range r.FailureModes {
		if i == MaxFailureModes {
			e.truncated |= 1 << fieldFailureModes
			break
		}
		p, pcut := e.copyStr(fm.Policy, MaxRecordBytes)
		m, mcut := e.copyStr(fm.Mode, MaxRecordBytes)
		if pcut || mcut {
			e.truncated |= 1 << fieldFailureModes
		}
		e.fms[i] = failureMode{policy: p, mode: m, phase: fm.Phase.String()}
		e.nfm++
	}
	if e.nfm > 0 {
		e.present |= 1 << fieldFailureModes
	}
	e.put(fieldCache, r.Cache, MaxRecordBytes)
}

// put copies a string field; an empty source leaves the field absent.
func (e *entry) put(f field, s string, maxRaw int) {
	if s == "" {
		return
	}
	e.present |= 1 << f
	sp, cut := e.copyStr(s, maxRaw)
	e.spans[f] = sp
	if cut {
		e.truncated |= 1 << f
	}
}

// copyStr appends the longest prefix of s that fits maxRaw raw bytes and
// the remaining escaped budget, cut at a UTF-8 boundary.
func (e *entry) copyStr(s string, maxRaw int) (span, bool) {
	n, enc := fit(s, maxRaw, e.budget)
	off := len(e.arena)
	e.arena = append(e.arena, s[:n]...)
	e.budget -= enc
	return span{off: uint16(off), n: uint16(n)}, n < len(s) //nolint:gosec // G115: offsets and lengths are at most MaxRecordBytes.
}

// fit returns the length of the longest prefix of s, ending on a UTF-8
// boundary (an invalid byte counts as one character), with at most maxRaw
// bytes and at most maxEnc bytes once JSON-escaped, and its escaped size.
func fit(s string, maxRaw, maxEnc int) (n, enc int) {
	for n < len(s) {
		b := s[n]
		size, cost := 1, 1
		if b < utf8.RuneSelf {
			if !safeASCII(b) {
				cost = 6
				if b == '"' || b == '\\' || b == '\n' || b == '\r' || b == '\t' {
					cost = 2
				}
			}
		} else {
			var c rune
			c, size = utf8.DecodeRuneInString(s[n:])
			switch {
			case c == utf8.RuneError && size == 1, c == '\u2028', c == '\u2029':
				cost = 6
			default:
				cost = size
			}
		}
		if n+size > maxRaw || enc+cost > maxEnc {
			break
		}
		n += size
		enc += cost
	}
	return n, enc
}
