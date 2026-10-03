// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package otlpsink

import (
	"math"
	"slices"
	"testing"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Span-order queries (11 req 34: "the ordered ruralz.filter.<policy>
// spans ... equal the printed rows' Policies in Phase order"; 09 test 34:
// span tree names, kinds, parent links and order).

func TestSpanOrderQueries(t *testing.T) { // 11 req 34; 09 test 34
	s := start(t, Config{})
	cc := dial(t, s, nil)
	client := coltrace.NewTraceServiceClient(cc)
	ctx := ctxTimeout(t, 10*time.Second)
	const (
		server   = tracepb.Span_SPAN_KIND_SERVER
		internal = tracepb.Span_SPAN_KIND_INTERNAL
		clientK  = tracepb.Span_SPAN_KIND_CLIENT
	)
	// Trace 1 arrives in two exports, children before their parent and
	// out of start order, as a batch span processor may deliver them.
	first := traceReq("gw",
		span(1, 4, 1, "ruralz.filter.ratelimit", internal, 3),
		span(1, 3, 1, "ruralz.filter.auth-jwt", internal, 2),
		span(1, 6, 5, "ruralz.upstream.attempt", clientK, 5),
	)
	second := traceReq("gw",
		span(1, 2, 1, "ruralz.route.match", internal, 1, str("ruralz.route", "orders")),
		span(1, 5, 1, "ruralz.upstream.orders", internal, 4),
		span(1, 1, 0, "GET orders", server, 0, str("http.request.method", "GET")),
		// Same start as the auth span: ties keep arrival order.
		span(1, 7, 1, "ruralz.filter.cors", internal, 2),
	)
	// Trace 2 continues a remote parent (incoming traceparent).
	third := traceReq("gw",
		span(2, 9, 8, "POST pay", server, 0),
		span(2, 10, 9, "ruralz.filter.auth-jwt", internal, 1),
	)
	for _, r := range []*coltrace.ExportTraceServiceRequest{first, second, third} {
		if _, err := client.Export(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	tr1 := "ab00000000000000000000000000000" + "1"
	tr2 := "ab00000000000000000000000000000" + "2"
	if got := s.TraceIDs(); !slices.Equal(got, []string{tr1, tr2}) {
		t.Fatalf("TraceIDs = %v", got)
	}
	wantOrder := []string{
		"GET orders", "ruralz.route.match", "ruralz.filter.auth-jwt", "ruralz.filter.cors",
		"ruralz.filter.ratelimit", "ruralz.upstream.orders", "ruralz.upstream.attempt",
	}
	if got := s.SpanNames(tr1); !slices.Equal(got, wantOrder) {
		t.Fatalf("SpanNames = %v\nwant %v", got, wantOrder)
	}
	root := "cd0000000000000" + "1"
	var filters []string
	for _, c := range s.Children(tr1, root) {
		filters = append(filters, c.Name())
	}
	if want := []string{"ruralz.route.match", "ruralz.filter.auth-jwt", "ruralz.filter.cors", "ruralz.filter.ratelimit", "ruralz.upstream.orders"}; !slices.Equal(filters, want) {
		t.Fatalf("Children = %v\nwant %v", filters, want)
	}
	roots := s.Roots(tr1)
	if len(roots) != 1 || roots[0].Name() != "GET orders" || roots[0].ParentSpanID() != "" {
		t.Fatalf("Roots(tr1) = %v", roots)
	}
	roots = s.Roots(tr2)
	if len(roots) != 1 || roots[0].Name() != "POST pay" || roots[0].ParentSpanID() != "cd00000000000008" {
		t.Fatalf("Roots(tr2) = %v", roots)
	}
	wantTree := "GET orders [SERVER]\n" +
		"  ruralz.route.match [INTERNAL]\n" +
		"  ruralz.filter.auth-jwt [INTERNAL]\n" +
		"  ruralz.filter.cors [INTERNAL]\n" +
		"  ruralz.filter.ratelimit [INTERNAL]\n" +
		"  ruralz.upstream.orders [INTERNAL]\n" +
		"    ruralz.upstream.attempt [CLIENT]\n"
	if got := s.FormatTree(tr1); got != wantTree {
		t.Fatalf("FormatTree =\n%s\nwant\n%s", got, wantTree)
	}
	if got := s.FormatTree(tr2); got != "POST pay [SERVER]\n  ruralz.filter.auth-jwt [INTERNAL]\n" {
		t.Fatalf("FormatTree(tr2) =\n%s", got)
	}
	if got := s.FormatTree("ff"); got != "" {
		t.Fatalf("FormatTree(unknown) = %q", got)
	}

	// Span accessors.
	sp := s.Trace(tr1)[0]
	if sp.Kind() != server || sp.TraceID() != tr1 || sp.SpanID() != root {
		t.Fatalf("span = %s %s %s", sp.Kind(), sp.TraceID(), sp.SpanID())
	}
	if got := sp.End().Sub(sp.Start()); got != time.Millisecond {
		t.Fatalf("duration = %v", got)
	}
	if sp.AttrString("http.request.method") != "GET" || sp.AttrString("missing") != "" {
		t.Fatalf("attributes = %v", sp.Proto.GetAttributes())
	}
	if v, ok := ResourceAttr(sp.Resource, "service.name"); !ok || ValueString(v) != "gw" {
		t.Fatalf("resource = %v", sp.Resource)
	}
	if sp.Scope.GetName() != "github.com/ravindu-rev/ruralz" || sp.Request != 2 {
		t.Fatalf("scope %v request %d", sp.Scope, sp.Request)
	}
	got := s.FindSpans(func(sp Span) bool { return sp.Name() == "ruralz.filter.auth-jwt" })
	if len(got) != 2 || got[0].TraceID() != tr1 || got[1].TraceID() != tr2 {
		t.Fatalf("FindSpans = %v", got)
	}
	if n := len(s.Spans()); n != 9 {
		t.Fatalf("Spans = %d, want 9", n)
	}
}

func TestFormatTreeCycle(t *testing.T) {
	// Spans a and b parent each other (malformed): no root leads to them,
	// so FormatTree renders only the well-formed part and terminates.
	s := start(t, Config{})
	cc := dial(t, s, nil)
	req := traceReq("gw",
		span(3, 1, 2, "a", tracepb.Span_SPAN_KIND_INTERNAL, 0),
		span(3, 2, 1, "b", tracepb.Span_SPAN_KIND_INTERNAL, 1),
		span(3, 3, 0, "c", tracepb.Span_SPAN_KIND_INTERNAL, 2),
		span(3, 4, 3, "d", tracepb.Span_SPAN_KIND_INTERNAL, 3),
	)
	if _, err := coltrace.NewTraceServiceClient(cc).Export(ctxTimeout(t, 10*time.Second), req); err != nil {
		t.Fatal(err)
	}
	tr := s.TraceIDs()[0]
	if got := s.FormatTree(tr); got != "c [INTERNAL]\n  d [INTERNAL]\n" {
		t.Fatalf("FormatTree = %q", got)
	}
}

func TestLogAndMetricQueries(t *testing.T) {
	s := start(t, Config{})
	cc := dial(t, s, nil)
	ctx := ctxTimeout(t, 10*time.Second)
	if _, err := collogs.NewLogsServiceClient(cc).Export(ctx, logReq("gw",
		logRecord("request served", "ruralz.access", str("request_id", "01J")),
		logRecord("started", ""),
	)); err != nil {
		t.Fatal(err)
	}
	for _, v := range []int64{1, 5} {
		if _, err := colmetrics.NewMetricsServiceClient(cc).Export(ctx, metricReq("gw",
			counter("ruralz_http_requests_total", v), counter("ruralz_config_activations_total", 1))); err != nil {
			t.Fatal(err)
		}
	}
	logs := s.Logs()
	if len(logs) != 2 {
		t.Fatalf("Logs = %d", len(logs))
	}
	l := logs[0]
	if l.Body() != "request served" || l.EventName() != "ruralz.access" || l.AttrString("request_id") != "01J" || l.AttrString("x") != "" {
		t.Fatalf("log = %v", l.Proto)
	}
	if l.TraceID() != "ab000000000000000000000000000001" || l.SpanID() != "cd00000000000001" {
		t.Fatalf("log ids = %s %s", l.TraceID(), l.SpanID())
	}
	if !l.Time().Equal(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("log time = %v", l.Time())
	}
	access := s.FindLogs(func(l LogRecord) bool { return l.EventName() == "ruralz.access" })
	if len(access) != 1 {
		t.Fatalf("FindLogs = %v", access)
	}
	if got := s.MetricNames(); !slices.Equal(got, []string{"ruralz_config_activations_total", "ruralz_http_requests_total"}) {
		t.Fatalf("MetricNames = %v", got)
	}
	m, ok := s.LastMetric("ruralz_http_requests_total")
	if !ok || m.Proto.GetSum().GetDataPoints()[0].GetAsInt() != 5 || m.Request != 3 {
		t.Fatalf("LastMetric = %v %v", m, ok)
	}
	if _, ok := s.LastMetric("absent"); ok {
		t.Fatal("LastMetric(absent) found")
	}
	if n := len(s.Metrics()); n != 4 {
		t.Fatalf("Metrics = %d", n)
	}
}

func TestValueString(t *testing.T) {
	sv := func(s string) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
	}
	for _, tc := range []struct {
		v    *commonpb.AnyValue
		want string
	}{
		{nil, ""},
		{&commonpb.AnyValue{}, ""},
		{sv("x"), "x"},
		{&commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}, "true"},
		{&commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: -42}}, "-42"},
		{&commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 0.25}}, "0.25"},
		{&commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte{0xde, 0xad}}}, "dead"},
		{&commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{sv("a"), sv("b")}}}}, "[a,b]"},
		{&commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{str("k", "v"), str("n", "m")}}}}, "{k=v,n=m}"},
	} {
		if got := ValueString(tc.v); got != tc.want {
			t.Errorf("ValueString(%v) = %q, want %q", tc.v, got, tc.want)
		}
	}
	if _, ok := Attr(nil, "k"); ok {
		t.Error("Attr(nil) found a key")
	}
	if got := unixNano(math.MaxUint64); !got.Equal(time.Unix(0, math.MaxInt64)) {
		t.Errorf("unixNano(max) = %v", got)
	}
}
