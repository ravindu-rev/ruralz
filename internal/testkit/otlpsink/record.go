// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package otlpsink

import (
	"cmp"
	"context"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Request is one recorded export request. The protobuf messages are shared
// with the sink and must not be modified.
type Request struct {
	// Seq numbers recorded requests from 1 in arrival order.
	Seq uint64
	// Signal is the service the request was sent to.
	Signal Signal
	// Received is when the sink recorded it.
	Received time.Time
	// Peer is the client address.
	Peer string
	// Conn numbers the connection that carried the request: connections
	// are numbered from 1 in the order they opened, across Down, so two
	// requests share a connection exactly when their Conn values are
	// equal.
	Conn uint64
	// Metadata is the incoming gRPC metadata (HTTP/2 headers), keys in
	// lower case.
	Metadata map[string][]string
	// TLS is the connection state over TLS, nil over h2c.
	TLS *tls.ConnectionState
	// Raw is the request's protobuf encoding as the client sent it (after
	// gRPC decompression), for canary scans (11 req 39).
	Raw []byte
	// Traces, Metrics and Logs hold the decoded request; exactly the one
	// matching Signal is set.
	Traces  *coltrace.ExportTraceServiceRequest
	Metrics *colmetrics.ExportMetricsServiceRequest
	Logs    *collogs.ExportLogsServiceRequest
}

// transportKey reports whether a metadata key is set by gRPC or HTTP/2
// itself rather than by the exporter's configuration.
func transportKey(k string) bool {
	switch k {
	case ":authority", "content-type", "user-agent", "te",
		"grpc-accept-encoding", "grpc-encoding", "grpc-timeout":
		return true
	}
	return false
}

// ExtraMetadata returns the metadata without the keys gRPC and HTTP/2 set
// themselves (":authority", "content-type", "user-agent", "te",
// "grpc-accept-encoding", "grpc-encoding", "grpc-timeout"): what an
// exporter adds, such as OTEL_EXPORTER_OTLP_HEADERS (09 test 36).
func (r Request) ExtraMetadata() map[string][]string {
	out := map[string][]string{}
	for k, v := range r.Metadata {
		if !transportKey(k) {
			out[k] = slices.Clone(v)
		}
	}
	return out
}

// Span is one received span with the resource and scope it was sent
// under. Proto is shared with the sink and must not be modified.
type Span struct {
	// Request is the Seq of the export request that carried the span.
	Request  uint64
	Resource *resourcepb.Resource
	Scope    *commonpb.InstrumentationScope
	Proto    *tracepb.Span
}

// Name returns the span name.
func (s Span) Name() string { return s.Proto.GetName() }

// TraceID returns the trace ID in lower-case hex.
func (s Span) TraceID() string { return hex.EncodeToString(s.Proto.GetTraceId()) }

// SpanID returns the span ID in lower-case hex.
func (s Span) SpanID() string { return hex.EncodeToString(s.Proto.GetSpanId()) }

// ParentSpanID returns the parent span ID in lower-case hex, "" for a
// span without a parent.
func (s Span) ParentSpanID() string { return hex.EncodeToString(s.Proto.GetParentSpanId()) }

// Kind returns the span kind.
func (s Span) Kind() tracepb.Span_SpanKind { return s.Proto.GetKind() }

// Start returns the start time.
func (s Span) Start() time.Time { return unixNano(s.Proto.GetStartTimeUnixNano()) }

// End returns the end time.
func (s Span) End() time.Time { return unixNano(s.Proto.GetEndTimeUnixNano()) }

// Attr returns the value of the span attribute key.
func (s Span) Attr(key string) (*commonpb.AnyValue, bool) { return Attr(s.Proto.GetAttributes(), key) }

// AttrString returns the span attribute key in its ValueString form, ""
// when absent.
func (s Span) AttrString(key string) string {
	v, _ := s.Attr(key)
	return ValueString(v)
}

// LogRecord is one received log record with the resource and scope it was
// sent under. Proto is shared with the sink and must not be modified.
type LogRecord struct {
	// Request is the Seq of the export request that carried the record.
	Request  uint64
	Resource *resourcepb.Resource
	Scope    *commonpb.InstrumentationScope
	Proto    *logspb.LogRecord
}

// Body returns the record body in its ValueString form.
func (l LogRecord) Body() string { return ValueString(l.Proto.GetBody()) }

// EventName returns the record's event name (ruralz.access for access
// log records).
func (l LogRecord) EventName() string { return l.Proto.GetEventName() }

// TraceID returns the trace ID in lower-case hex, "" when unset.
func (l LogRecord) TraceID() string { return hex.EncodeToString(l.Proto.GetTraceId()) }

// SpanID returns the span ID in lower-case hex, "" when unset.
func (l LogRecord) SpanID() string { return hex.EncodeToString(l.Proto.GetSpanId()) }

// Time returns the record timestamp.
func (l LogRecord) Time() time.Time { return unixNano(l.Proto.GetTimeUnixNano()) }

// Attr returns the value of the record attribute key.
func (l LogRecord) Attr(key string) (*commonpb.AnyValue, bool) {
	return Attr(l.Proto.GetAttributes(), key)
}

// AttrString returns the record attribute key in its ValueString form, ""
// when absent.
func (l LogRecord) AttrString(key string) string {
	v, _ := l.Attr(key)
	return ValueString(v)
}

// Metric is one received metric with the resource and scope it was sent
// under. Proto is shared with the sink and must not be modified.
type Metric struct {
	// Request is the Seq of the export request that carried the metric.
	Request  uint64
	Resource *resourcepb.Resource
	Scope    *commonpb.InstrumentationScope
	Proto    *metricspb.Metric
}

// Name returns the metric name.
func (m Metric) Name() string { return m.Proto.GetName() }

// Attr returns the value of key among attrs.
func Attr(attrs []*commonpb.KeyValue, key string) (*commonpb.AnyValue, bool) {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return kv.GetValue(), true
		}
	}
	return nil, false
}

// ResourceAttr returns the value of the resource attribute key.
func ResourceAttr(r *resourcepb.Resource, key string) (*commonpb.AnyValue, bool) {
	return Attr(r.GetAttributes(), key)
}

// ValueString renders a value: strings as is, booleans and numbers in
// strconv form, bytes in hex, arrays as [a,b] and key-value lists as
// {k=v,...}; nil renders as "".
func ValueString(v *commonpb.AnyValue) string {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(x.BoolValue)
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(x.IntValue, 10)
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(x.DoubleValue, 'g', -1, 64)
	case *commonpb.AnyValue_BytesValue:
		return hex.EncodeToString(x.BytesValue)
	case *commonpb.AnyValue_ArrayValue:
		parts := make([]string, 0, len(x.ArrayValue.GetValues()))
		for _, e := range x.ArrayValue.GetValues() {
			parts = append(parts, ValueString(e))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case *commonpb.AnyValue_KvlistValue:
		parts := make([]string, 0, len(x.KvlistValue.GetValues()))
		for _, kv := range x.KvlistValue.GetValues() {
			parts = append(parts, kv.GetKey()+"="+ValueString(kv.GetValue()))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	return ""
}

func unixNano(ns uint64) time.Time {
	if ns > math.MaxInt64 {
		ns = math.MaxInt64
	}
	return time.Unix(0, int64(ns))
}

// recorder keeps the recorded requests and their flattened items.
type recorder struct {
	maxRequests int
	maxBytes    int64

	mu       sync.Mutex
	notify   chan struct{} // closed and replaced on every change
	seq      uint64
	bytes    int64
	requests []Request
	spans    []Span
	logs     []LogRecord
	metrics  []Metric
	answered [3]int64 // per Signal-1: answered OK
	dropped  int64
}

func newRecorder(maxRequests int, maxBytes int64) *recorder {
	return &recorder{maxRequests: maxRequests, maxBytes: maxBytes, notify: make(chan struct{})}
}

func (r *recorder) add(req Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.answered[req.Signal-1]++
	if len(r.requests) >= r.maxRequests || r.bytes+int64(len(req.Raw)) > r.maxBytes {
		r.dropped++
		return
	}
	r.seq++
	req.Seq = r.seq
	r.bytes += int64(len(req.Raw))
	r.requests = append(r.requests, req)
	for _, rs := range req.Traces.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				r.spans = append(r.spans, Span{Request: req.Seq, Resource: rs.GetResource(), Scope: ss.GetScope(), Proto: sp})
			}
		}
	}
	for _, rl := range req.Logs.GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				r.logs = append(r.logs, LogRecord{Request: req.Seq, Resource: rl.GetResource(), Scope: sl.GetScope(), Proto: lr})
			}
		}
	}
	for _, rm := range req.Metrics.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				r.metrics = append(r.metrics, Metric{Request: req.Seq, Resource: rm.GetResource(), Scope: sm.GetScope(), Proto: m})
			}
		}
	}
	r.changedLocked()
}

func (r *recorder) changedLocked() {
	close(r.notify)
	r.notify = make(chan struct{})
}

// changed returns a channel closed at the next change.
func (r *recorder) changed() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.notify
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bytes = 0
	r.requests, r.spans, r.logs, r.metrics = nil, nil, nil, nil
	r.changedLocked()
}

func (r *recorder) stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Stats{
		TraceRequests:  r.answered[Traces-1],
		MetricRequests: r.answered[Metrics-1],
		LogRequests:    r.answered[Logs-1],
		Dropped:        r.dropped,
	}
}

func (r *recorder) snapshotSpans() []Span {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.spans)
}

func (r *recorder) snapshotLogs() []LogRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.logs)
}

func (r *recorder) snapshotMetrics() []Metric {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.metrics)
}

// Requests returns the recorded export requests of every signal in
// arrival order.
func (s *Sink) Requests() []Request {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	return slices.Clone(s.rec.requests)
}

// RequestsOf returns the recorded export requests of one signal in
// arrival order.
func (s *Sink) RequestsOf(sig Signal) []Request {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	var out []Request
	for _, r := range s.rec.requests {
		if r.Signal == sig {
			out = append(out, r)
		}
	}
	return out
}

// Reset forgets every recorded request, span, log record and metric; the
// Stats counters keep counting.
func (s *Sink) Reset() { s.rec.reset() }

// Spans returns every recorded span in arrival order.
func (s *Sink) Spans() []Span { return s.rec.snapshotSpans() }

// FindSpans returns the recorded spans pred accepts, in arrival order.
func (s *Sink) FindSpans(pred func(Span) bool) []Span {
	return slices.DeleteFunc(s.rec.snapshotSpans(), func(sp Span) bool { return !pred(sp) })
}

// TraceIDs returns the trace IDs seen, in first-seen order.
func (s *Sink) TraceIDs() []string {
	seen := map[string]bool{}
	var out []string
	for _, sp := range s.rec.snapshotSpans() {
		if id := sp.TraceID(); !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// Trace returns the spans of one trace (hex trace ID) ordered by start
// time, ties in arrival order.
func (s *Sink) Trace(traceID string) []Span {
	out := s.FindSpans(func(sp Span) bool { return sp.TraceID() == traceID })
	sortByStart(out)
	return out
}

// SpanNames returns the names of Trace(traceID) in the same order.
func (s *Sink) SpanNames(traceID string) []string {
	spans := s.Trace(traceID)
	out := make([]string, len(spans))
	for i, sp := range spans {
		out[i] = sp.Name()
	}
	return out
}

// Children returns the spans of the trace whose parent is parentSpanID
// (hex), ordered by start time, ties in arrival order.
func (s *Sink) Children(traceID, parentSpanID string) []Span {
	return slices.DeleteFunc(s.Trace(traceID), func(sp Span) bool { return sp.ParentSpanID() != parentSpanID })
}

// Roots returns the spans of the trace without a parent among the
// trace's recorded spans (no parent, or a remote parent from an incoming
// traceparent), ordered by start time.
func (s *Sink) Roots(traceID string) []Span {
	spans := s.Trace(traceID)
	ids := map[string]bool{}
	for _, sp := range spans {
		ids[sp.SpanID()] = true
	}
	return slices.DeleteFunc(spans, func(sp Span) bool { return ids[sp.ParentSpanID()] })
}

// FormatTree renders the trace as an indented tree, one span per line as
// "<name> [<KIND>]", children under their parent in start order (ties in
// arrival order), two spaces per level: a stable form for span tree
// goldens (09 test 34). Roots are the spans Roots returns; spans reachable
// from no root (a parent cycle) are left out.
func (s *Sink) FormatTree(traceID string) string {
	spans := s.Trace(traceID)
	children := map[string][]Span{}
	ids := map[string]bool{}
	for _, sp := range spans {
		ids[sp.SpanID()] = true
	}
	var roots []Span
	for _, sp := range spans {
		if p := sp.ParentSpanID(); ids[p] {
			children[p] = append(children[p], sp)
		} else {
			roots = append(roots, sp)
		}
	}
	var b strings.Builder
	visited := map[string]bool{}
	var walk func(sp Span, depth int)
	walk = func(sp Span, depth int) {
		if visited[sp.SpanID()] {
			return
		}
		visited[sp.SpanID()] = true
		fmt.Fprintf(&b, "%s%s [%s]\n", strings.Repeat("  ", depth), sp.Name(), kindName(sp.Kind()))
		for _, c := range children[sp.SpanID()] {
			walk(c, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	return b.String()
}

func kindName(k tracepb.Span_SpanKind) string {
	return strings.TrimPrefix(k.String(), "SPAN_KIND_")
}

func sortByStart(spans []Span) {
	slices.SortStableFunc(spans, func(a, b Span) int {
		return cmp.Compare(a.Proto.GetStartTimeUnixNano(), b.Proto.GetStartTimeUnixNano())
	})
}

// Logs returns every recorded log record in arrival order.
func (s *Sink) Logs() []LogRecord { return s.rec.snapshotLogs() }

// FindLogs returns the recorded log records pred accepts, in arrival
// order.
func (s *Sink) FindLogs(pred func(LogRecord) bool) []LogRecord {
	return slices.DeleteFunc(s.rec.snapshotLogs(), func(l LogRecord) bool { return !pred(l) })
}

// Metrics returns every recorded metric in arrival order.
func (s *Sink) Metrics() []Metric { return s.rec.snapshotMetrics() }

// MetricNames returns the distinct recorded metric names, sorted (the
// OTLP side of the name identity gate, 09 req 76).
func (s *Sink) MetricNames() []string {
	var out []string
	for _, m := range s.rec.snapshotMetrics() {
		out = append(out, m.Name())
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// LastMetric returns the most recently received metric named name.
func (s *Sink) LastMetric(name string) (Metric, bool) {
	ms := s.rec.snapshotMetrics()
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i].Name() == name {
			return ms[i], true
		}
	}
	return Metric{}, false
}

// Wait blocks until cond returns true or ctx ends. cond runs at once and
// again after every recorded request and Reset; it may call the Sink's
// query methods.
func (s *Sink) Wait(ctx context.Context, cond func() bool) error {
	for {
		ch := s.rec.changed()
		if cond() {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return fmt.Errorf("otlpsink: wait: %w", ctx.Err())
		}
	}
}

// WaitSpans waits until at least n recorded spans satisfy pred (nil
// accepts every span) and returns them in arrival order.
func (s *Sink) WaitSpans(ctx context.Context, n int, pred func(Span) bool) ([]Span, error) {
	if pred == nil {
		pred = func(Span) bool { return true }
	}
	var got []Span
	err := s.Wait(ctx, func() bool {
		got = s.FindSpans(pred)
		return len(got) >= n
	})
	return got, err
}

// WaitLogs waits until at least n recorded log records satisfy pred (nil
// accepts every record) and returns them in arrival order.
func (s *Sink) WaitLogs(ctx context.Context, n int, pred func(LogRecord) bool) ([]LogRecord, error) {
	if pred == nil {
		pred = func(LogRecord) bool { return true }
	}
	var got []LogRecord
	err := s.Wait(ctx, func() bool {
		got = s.FindLogs(pred)
		return len(got) >= n
	})
	return got, err
}

// WaitMetric waits until a metric named name is recorded and returns the
// most recent one.
func (s *Sink) WaitMetric(ctx context.Context, name string) (Metric, error) {
	var got Metric
	err := s.Wait(ctx, func() bool {
		var ok bool
		got, ok = s.LastMetric(name)
		return ok
	})
	return got, err
}
