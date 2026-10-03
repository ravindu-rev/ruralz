// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// MethodOther is http.request.method for a method outside RFC 9110 and
// PATCH (spec 09 req 33).
const MethodOther = "_OTHER"

// UnmatchedRoute is the ruralz.route value of a request no Route matched;
// such a server span keeps the name "<method>" (spec 09 reqs 33 and 34).
const UnmatchedRoute = "_unmatched"

// onLogPhase is the Phase name that creates no span (spec 09 req 35).
const onLogPhase = "onLog"

// Attribute buffer sizes per span kind: the most attributes each kind
// sets (spec 09 reqs 33 to 36). Every attribute is collected in the
// buffer and handed to the SDK in one call at End, so the SDK allocates
// the span's attribute slice once; a full buffer is flushed early.
const (
	serverAttrs   = 17
	matchAttrs    = 4
	filterAttrs   = 10
	upstreamAttrs = 7
)

// spanKind selects the attribute and status rules of a span.
type spanKind uint8

const (
	kindServer spanKind = iota
	kindMatch
	kindFilter
	kindUpstream
)

// span is the emit.Span of a sampled request: the SDK span plus a buffer
// of its attributes. It is used by one goroutine at a time (the request's,
// or one leg's).
type span struct {
	sdk    trace.Span
	id     [8]byte
	kind   spanKind
	method string // server: http.request.method, the first word of the name
	route  string // server: ruralz.route, the second word of the name
	attrs  []attribute.KeyValue
}

// Spans with their attribute buffer in the same allocation.
type (
	serverSpan struct {
		span
		buf [serverAttrs]attribute.KeyValue
	}
	matchSpan struct {
		span
		buf [matchAttrs]attribute.KeyValue
	}
	filterSpan struct {
		span
		buf [filterAttrs]attribute.KeyValue
	}
	upstreamSpan struct {
		span
		buf [upstreamAttrs]attribute.KeyValue
	}
)

// newSpan returns a span of kind k over sdk with an empty buffer: one
// allocation.
func newSpan(k spanKind, sdk trace.Span, id [8]byte) *span {
	var s *span
	switch k {
	case kindServer:
		b := &serverSpan{}
		b.attrs = b.buf[:0]
		s = &b.span
	case kindMatch:
		b := &matchSpan{}
		b.attrs = b.buf[:0]
		s = &b.span
	case kindFilter:
		b := &filterSpan{}
		b.attrs = b.buf[:0]
		s = &b.span
	case kindUpstream:
		b := &upstreamSpan{}
		b.attrs = b.buf[:0]
		s = &b.span
	}
	s.sdk, s.kind, s.id = sdk, k, id
	return s
}

var _ emit.Span = (*span)(nil)

// SetAttr sets one attribute; slog kinds map to OTLP types (durations as
// seconds, spec 09 req 35 ruralz.state.duration). On the server span,
// ruralz.route also names the span "<method> <route>" at End (req 34).
func (s *span) SetAttr(key string, v slog.Value) {
	kv, ok := attrOf(key, v)
	if !ok {
		return
	}
	if s.kind == kindServer && key == catalog.AttrRoute {
		s.route = kv.Value.AsString()
	}
	s.put(kv)
}

// SpanID returns the span's ID.
func (s *span) SpanID() [8]byte { return s.id }

// End sets the end attributes and status and ends the span: status is the
// HTTP status (0 when none), code the RZ code ("" when none) and errorType
// the error.type value ("" when none).
func (s *span) End(status int, code, errorType string) {
	if status > 0 {
		s.put(semconv.HTTPResponseStatusCodeKey.Int(status))
	}
	if code != "" {
		s.put(attribute.String(catalog.AttrCode, code))
	}
	failed := errorType != ""
	switch s.kind {
	case kindServer:
		// Status Error for 5xx only (req 33); semconv sets error.type to
		// the status code when nothing more specific is known.
		failed = status >= 500
		if errorType == "" && failed {
			errorType = strconv.Itoa(status)
		}
	case kindUpstream:
		// Status Error for 4xx, 5xx or an error (req 36).
		failed = failed || status >= 400
	case kindMatch, kindFilter:
	}
	if errorType != "" {
		s.put(semconv.ErrorTypeKey.String(errorType))
	}
	s.flush()
	if s.kind == kindServer && s.route != "" && s.route != UnmatchedRoute {
		s.sdk.SetName(s.method + " " + s.route)
	}
	if failed {
		s.sdk.SetStatus(codes.Error, "")
	}
	s.sdk.End()
}

// put buffers kv, replacing an earlier value of the same key. A string
// value is cut to MaxAttributeValueLen bytes first (spec 09 req 22).
func (s *span) put(kv attribute.KeyValue) {
	if kv.Value.Type() == attribute.STRING {
		if v := kv.Value.AsString(); len(v) > MaxAttributeValueLen {
			kv.Value = attribute.StringValue(cutUTF8(v, MaxAttributeValueLen))
		}
	}
	for i := range s.attrs {
		if s.attrs[i].Key == kv.Key {
			s.attrs[i] = kv
			return
		}
	}
	if len(s.attrs) == cap(s.attrs) {
		s.flush()
	}
	s.attrs = append(s.attrs, kv)
}

// flush hands the buffered attributes to the SDK, which copies them.
func (s *span) flush() {
	if len(s.attrs) == 0 {
		return
	}
	s.sdk.SetAttributes(s.attrs...)
	clear(s.attrs)
	s.attrs = s.attrs[:0]
}

// serverStart buffers the SERVER span attributes known at start (spec 09
// req 33): semconv HTTP server attributes minus url.full, url.query and
// every header attribute, plus ruralz.listener.
func (s *span) serverStart(original string, a *emit.ServerAttrs) {
	s.put(semconv.HTTPRequestMethodKey.String(s.method))
	if original != "" {
		s.put(semconv.HTTPRequestMethodOriginalKey.String(original))
	}
	if a.Scheme != "" {
		s.put(semconv.URLSchemeKey.String(a.Scheme))
	}
	if p := pathOnly(a.Path); p != "" {
		s.put(semconv.URLPathKey.String(p))
	}
	if a.Host != "" {
		s.put(semconv.ServerAddressKey.String(a.Host))
	}
	if a.Port > 0 {
		s.put(semconv.ServerPortKey.Int(a.Port))
	}
	if a.ClientAddress != "" {
		s.put(semconv.ClientAddressKey.String(a.ClientAddress))
	}
	if v := protocolVersion(a.Protocol); v != "" {
		s.put(semconv.NetworkProtocolVersionKey.String(v))
	}
	if a.UserAgent != "" {
		s.put(semconv.UserAgentOriginalKey.String(a.UserAgent))
	}
	if a.Listener != "" {
		s.put(attribute.String(catalog.AttrListener, a.Listener))
	}
}

// filterStart buffers the ruralz.filter.<name> start attributes (req 35).
func (s *span) filterStart(a *emit.FilterAttrs) {
	if a.PolicyType != "" {
		s.put(attribute.String(catalog.AttrPolicyType, a.PolicyType))
	}
	if ph := a.Phase.String(); ph != "" {
		s.put(attribute.String(catalog.AttrPhase, ph))
	}
}

// upstreamStart buffers the ruralz.upstream.<name> start attributes
// (req 36): the attempt, the Endpoint's server.address and server.port,
// and the composition step when the leg is one.
func (s *span) upstreamStart(a *emit.UpstreamAttrs) {
	if a.Attempt > 0 {
		s.put(attribute.Int(catalog.AttrAttempt, a.Attempt))
	}
	if a.Endpoint != "" {
		host, port := splitEndpoint(a.Endpoint)
		s.put(semconv.ServerAddressKey.String(host))
		if port > 0 {
			s.put(semconv.ServerPortKey.Int(port))
		}
	}
	if a.Step != "" {
		s.put(attribute.String(catalog.AttrCompStep, a.Step))
	}
}

// noopSpan is the span of an unsampled request below the server span. It
// is zero-sized, so returning it as an emit.Span allocates nothing.
type noopSpan struct{}

var _ emit.Span = noopSpan{}

// SetAttr does nothing.
func (noopSpan) SetAttr(string, slog.Value) {}

// SpanID returns the zero ID.
func (noopSpan) SpanID() [8]byte { return [8]byte{} }

// End does nothing.
func (noopSpan) End(int, string, string) {}

// noopServer is the server span of a request that creates no spans: a
// view of the caller's Decision, so SpanID returns the server span ID the
// access log and the injected traceparent use, and converting it to an
// emit.Span copies a pointer.
type noopServer emit.Decision

var _ emit.Span = (*noopServer)(nil)

// SetAttr does nothing.
func (*noopServer) SetAttr(string, slog.Value) {}

// SpanID returns the Decision's server span ID.
func (n *noopServer) SpanID() [8]byte { return n.ServerSpanID }

// End does nothing.
func (*noopServer) End(int, string, string) {}

// attrOf converts a slog attribute to an OTLP attribute. LogValuers are
// resolved first (a secret.Value becomes "[REDACTED]"); durations become
// float seconds; groups and empty keys are dropped.
func attrOf(key string, v slog.Value) (attribute.KeyValue, bool) {
	if key == "" {
		return attribute.KeyValue{}, false
	}
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return attribute.String(key, v.String()), true
	case slog.KindInt64:
		return attribute.Int64(key, v.Int64()), true
	case slog.KindUint64:
		u := v.Uint64()
		if u > math.MaxInt64 {
			u = math.MaxInt64
		}
		return attribute.Int64(key, int64(u)), true
	case slog.KindFloat64:
		return attribute.Float64(key, v.Float64()), true
	case slog.KindBool:
		return attribute.Bool(key, v.Bool()), true
	case slog.KindDuration:
		return attribute.Float64(key, v.Duration().Seconds()), true
	case slog.KindTime:
		return attribute.String(key, v.Time().UTC().Format(time.RFC3339Nano)), true
	case slog.KindGroup:
		return attribute.KeyValue{}, false
	default: // slog.KindAny, slog.KindLogValuer after Resolve
		return attribute.String(key, v.String()), true
	}
}

// cutUTF8 returns the longest prefix of v of at most n bytes that does not
// end inside a UTF-8 sequence; it backs up at most utf8.UTFMax-1 bytes, so
// invalid UTF-8 is cut near n too. Slicing allocates nothing.
func cutUTF8(v string, n int) string {
	if len(v) <= n {
		return v
	}
	i := n
	for range utf8.UTFMax - 1 {
		if i == 0 || utf8.RuneStart(v[i]) {
			break
		}
		i--
	}
	return v[:i]
}

// normalizeMethod returns http.request.method and, for a method outside
// RFC 9110 and PATCH (case-sensitive), the original value (req 33).
func normalizeMethod(m string) (name, original string) {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete,
		http.MethodConnect, http.MethodOptions, http.MethodTrace, http.MethodPatch:
		return m, ""
	default:
		return MethodOther, m
	}
}

// protocolVersion maps the listener protocol to network.protocol.version.
func protocolVersion(p string) string {
	switch p {
	case "http1", "HTTP/1.1", "1.1":
		return "1.1"
	case "HTTP/1.0", "1.0":
		return "1.0"
	case "http2", "HTTP/2.0", "HTTP/2", "2":
		return "2"
	default:
		return p
	}
}

// pathOnly cuts anything from "?" so no query string reaches a span, even
// when a caller passes a request URI (spec 09 req 4).
func pathOnly(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

// splitEndpoint splits "host:port" (IPv6 in brackets); a value without a
// valid port is all host.
func splitEndpoint(ep string) (host string, port int) {
	h, p, err := net.SplitHostPort(ep)
	if err != nil {
		return ep, 0
	}
	n, err := strconv.Atoi(p)
	if err != nil || n <= 0 || n > 65535 {
		return h, 0
	}
	return h, n
}
