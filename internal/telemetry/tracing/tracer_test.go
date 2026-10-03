// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

var update = flag.Bool("update", false, "rewrite golden files")

// TestDecide is spec 09 test 8 (reqs 28 to 30).
func TestDecide(t *testing.T) {
	for _, tc := range []struct {
		name        string
		h           http.Header
		ratio       float64
		wantRemote  bool
		wantSampled bool
	}{
		{"parent sampled", header(HeaderTraceparent, tpSampled), 0, true, true},
		{"parent unsampled at ratio 1", header(HeaderTraceparent, tpUnsampled), 1, true, false},
		{"no parent ratio 0", http.Header{}, 0, false, false},
		{"no parent ratio 1", http.Header{}, 1, false, true},
		{"invalid parent ratio 1", header(HeaderTraceparent, "garbage"), 1, false, true},
		{"nil header ratio 1", nil, 1, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tc2 testCounters
			tr := newTracer(t, Options{Counters: tc2.wire()})
			d := emit.Decision{Sampled: true, TraceState: "stale", Remote: true}
			tr.Decide(tc.h, tc.ratio, &d)
			if d.Remote != tc.wantRemote || d.Sampled != tc.wantSampled {
				t.Fatalf("Remote %v Sampled %v, want %v %v", d.Remote, d.Sampled, tc.wantRemote, tc.wantSampled)
			}
			if d.TraceID == ([16]byte{}) || d.ServerSpanID == ([8]byte{}) {
				t.Fatal("IDs not drawn")
			}
			if tc.wantRemote {
				if FormatTraceID(d.TraceID) != testTraceIDHex || d.ParentSpanID != mustHex8(t, testParentIDHex) {
					t.Fatalf("parent not kept: %x %x", d.TraceID, d.ParentSpanID)
				}
			} else if d.ParentSpanID != ([8]byte{}) || d.TraceState != "" {
				t.Fatalf("root decision kept stale fields: %+v", d)
			}
			if tc2.root.Load()+tc2.parent.Load() != 0 {
				t.Fatal("unsampled counter moved without a cap")
			}
		})
	}
}

// TestDecideRatio1PercentFraction checks the default ratio end to end
// (spec 09 req 28). The IDs come from a seeded source, so the count is
// reproducible.
func TestDecideRatio1PercentFraction(t *testing.T) {
	tr := newTracer(t, Options{RootPerSecond: 1 << 30})
	rng := rand.NewChaCha8([32]byte{'r', 'u', 'r', 'a', 'l', 'z'})
	tr.src.fill = func(b []byte) { _, _ = rng.Read(b) }
	var d emit.Decision
	hits := 0
	const n = 200_000
	for range n {
		tr.Decide(nil, DefaultTraceSampling, &d)
		if d.Sampled {
			hits++
		}
	}
	// 4σ of a binomial(200000, 0.01) is about 178.
	if hits < 2000-180 || hits > 2000+180 {
		t.Fatalf("sampled %d of %d at 0.01", hits, n)
	}
}

// TestDecideCaps covers the root and parent caps and the unsampled counter
// (spec 09 req 29): client-forced flags never consume root tokens, the
// ratio applies per call, and caps refill with time.
func TestDecideCaps(t *testing.T) {
	fake := clocktest.New(time.Unix(1_700_000_000, 0))
	var c testCounters
	tr := newTracer(t, Options{Clock: fake, Counters: c.wire(), RootPerSecond: 10, ParentPerSecond: 5})
	sampled := func(h http.Header, ratio float64, n int) int {
		got := 0
		var d emit.Decision
		for range n {
			tr.Decide(h, ratio, &d)
			if d.Sampled {
				got++
			}
		}
		return got
	}
	parent := header(HeaderTraceparent, tpSampled)
	if got := sampled(parent, 0, 8); got != 5 {
		t.Fatalf("parent cap admitted %d of 8, want 5", got)
	}
	if got := c.parent.Load(); got != 3 {
		t.Fatalf("rate_cap_parent = %d, want 3", got)
	}
	// The parent cap is exhausted; root tokens are untouched.
	if got := sampled(nil, 1, 12); got != 10 {
		t.Fatalf("root cap admitted %d of 12, want 10", got)
	}
	if got := c.root.Load(); got != 2 {
		t.Fatalf("rate_cap_root = %d, want 2", got)
	}
	// Ratio 0 decides unsampled without touching a cap or a counter.
	if got := sampled(nil, 0, 5); got != 0 || c.root.Load() != 2 {
		t.Fatalf("ratio 0 sampled %d, root counter %d", got, c.root.Load())
	}
	// An unsampled parent consumes nothing and counts nothing.
	if got := sampled(header(HeaderTraceparent, tpUnsampled), 1, 5); got != 0 || c.parent.Load() != 3 {
		t.Fatalf("unsampled parent sampled %d, parent counter %d", got, c.parent.Load())
	}
	fake.Advance(time.Second)
	if got := sampled(parent, 0, 5); got != 5 {
		t.Fatalf("after 1 s parent cap admitted %d, want 5", got)
	}
	if got := sampled(nil, 1, 10); got != 10 {
		t.Fatalf("after 1 s root cap admitted %d, want 10", got)
	}
}

// TestInject is spec 09 test 9 (req 31).
func TestInject(t *testing.T) {
	tr, mem := newMemTracer(t, Options{})
	n := newOrdersNames()
	client := header(HeaderTraceparent, tpSampled, HeaderTracestate, "congo=t61rcWkgMzE", "Baggage", "k=v")
	var d emit.Decision
	tr.Decide(client, 0, &d)
	if !d.Sampled {
		t.Fatal("not sampled")
	}
	ctx := context.Background()

	t.Run("no spans uses the server span ID", func(t *testing.T) {
		out := client.Clone()
		tr.Inject(ctx, &d, out)
		want := string(AppendTraceparent(nil, d.TraceID, d.ServerSpanID, true))
		if got := out.Values(HeaderTraceparent); len(got) != 1 || got[0] != want {
			t.Fatalf("traceparent = %q, want %q", got, want)
		}
		if got := out.Values(HeaderTracestate); len(got) != 1 || got[0] != "congo=t61rcWkgMzE" {
			t.Fatalf("tracestate = %q", got)
		}
		if out.Get("Baggage") != "k=v" || len(out) != 3 {
			t.Fatalf("other fields changed: %v", out)
		}
	})
	t.Run("attempt span is the parent", func(t *testing.T) {
		sctx, server := tr.StartServer(ctx, &d, http.MethodGet, emit.ServerAttrs{})
		lctx, up := tr.StartUpstream(sctx, n.orders, emit.UpstreamAttrs{Attempt: 1})
		out := http.Header{}
		tr.Inject(lctx, &d, out)
		want := string(AppendTraceparent(nil, d.TraceID, up.SpanID(), true))
		if got := out.Get(HeaderTraceparent); got != want || up.SpanID() == d.ServerSpanID {
			t.Fatalf("traceparent = %q, want %q", got, want)
		}
		// With the server context the server span is the parent.
		tr.Inject(sctx, &d, out)
		if got := out.Get(HeaderTraceparent); got != string(AppendTraceparent(nil, d.TraceID, d.ServerSpanID, true)) {
			t.Fatalf("server-context traceparent = %q", got)
		}
		up.End(http.StatusOK, "", "")
		server.End(http.StatusOK, "", "")
		spans := flushed(ctx, t, tr, mem)
		if len(spans) != 2 {
			t.Fatalf("%d spans", len(spans))
		}
	})
	t.Run("unsampled flags 00 and no tracestate", func(t *testing.T) {
		var u emit.Decision
		tr.Decide(header(HeaderTraceparent, tpUnsampled), 0, &u)
		out := header(HeaderTraceparent, "x", HeaderTracestate, "y=1", KeyTraceparent, "raw", KeyTracestate, "raw")
		tr.Inject(ctx, &u, out)
		if got := out.Get(HeaderTraceparent); got != string(AppendTraceparent(nil, u.TraceID, u.ServerSpanID, false)) || !strings.HasSuffix(got, "-00") {
			t.Fatalf("traceparent = %q", got)
		}
		if _, ok := out[HeaderTracestate]; ok {
			t.Fatal("client tracestate forwarded")
		}
		if _, ok := map[string][]string(out)[KeyTraceparent]; ok {
			t.Fatal("non-canonical traceparent left")
		}
	})
	t.Run("requestId is the trace ID", func(t *testing.T) {
		out := http.Header{}
		tr.Inject(ctx, &d, out)
		if got := out.Get(HeaderTraceparent)[3:35]; got != FormatTraceID(d.TraceID) || got != testTraceIDHex {
			t.Fatalf("trace ID %q, requestId %q", got, FormatTraceID(d.TraceID))
		}
	})
	t.Run("nil decision removes the client values", func(t *testing.T) {
		out := client.Clone()
		tr.Inject(ctx, nil, out)
		if out.Get(HeaderTraceparent) != "" || out.Get(HeaderTracestate) != "" {
			t.Fatalf("client values forwarded: %v", out)
		}
	})
	t.Run("nil header is left alone", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Inject into a nil header panicked: %v", r)
			}
		}()
		for _, dec := range []*emit.Decision{&d, nil} {
			tr.Inject(ctx, dec, nil)
		}
	})
}

// mapCarrier is a Carrier for the M3 extension point.
type mapCarrier map[string]string

func (m mapCarrier) Get(k string) string { return m[k] }
func (m mapCarrier) Set(k, v string)     { m[k] = v }
func (m mapCarrier) Delete(k string)     { delete(m, k) }

// TestInjectCarrier covers spec 09 req 31 for the Carrier extension point
// of section 8 (Get, Set, Delete).
func TestInjectCarrier(t *testing.T) {
	tr := newTracer(t, Options{})
	var d emit.Decision
	tr.Decide(header(HeaderTraceparent, tpSampled, HeaderTracestate, "a=1"), 0, &d)
	c := mapCarrier{KeyTraceparent: "client", KeyTracestate: "client"}
	tr.InjectCarrier(context.Background(), &d, c)
	var cr Carrier = c
	if cr.Get(KeyTraceparent) != string(AppendTraceparent(nil, d.TraceID, d.ServerSpanID, true)) || cr.Get(KeyTracestate) != "a=1" {
		t.Fatalf("carrier = %v", c)
	}
	tr.InjectCarrier(context.Background(), &d, nil) // a nil carrier is left alone
	tr.InjectCarrier(context.Background(), nil, c)
	if len(c) != 0 {
		t.Fatalf("nil decision left %v", c)
	}
	var root emit.Decision
	tr.Decide(nil, 0, &root)
	tr.InjectCarrier(context.Background(), &root, c)
	if _, ok := c[KeyTracestate]; ok || !strings.HasSuffix(c[KeyTraceparent], "-00") {
		t.Fatalf("root carrier = %v", c)
	}
}

// attrs indexes a span's attributes.
func attrs(s tracetest.SpanStub) map[attribute.Key]attribute.Value {
	m := map[attribute.Key]attribute.Value{}
	for _, kv := range s.Attributes {
		m[kv.Key] = kv.Value
	}
	return m
}

func byName(spans tracetest.SpanStubs, name string) (tracetest.SpanStub, bool) {
	for _, s := range spans {
		if s.Name == name {
			return s, true
		}
	}
	return tracetest.SpanStub{}, false
}

// TestSpanModel is spec 09 test 10 (reqs 33 to 36).
func TestSpanModel(t *testing.T) {
	tr, mem := newMemTracer(t, Options{})
	nm := newOrdersNames()
	ctx := context.Background()
	var d emit.Decision
	tr.Decide(header(HeaderTraceparent, tpSampled), 0, &d)
	ordersOut, inventoryOut := ordersSummary(ctx, tr, &d, &nm)
	spans := flushed(ctx, t, tr, mem)
	if len(spans) != 15 {
		t.Fatalf("%d spans, want 15", len(spans))
	}

	server, ok := byName(spans, "GET orders-summary")
	if !ok {
		t.Fatalf("no server span named after the Route: %v", names(spans))
	}
	if server.SpanKind != trace.SpanKindServer || server.SpanContext.SpanID() != d.ServerSpanID ||
		server.SpanContext.TraceID() != d.TraceID || server.Parent.SpanID() != d.ParentSpanID || !server.Parent.IsRemote() {
		t.Fatalf("server span identity: kind %v ctx %v parent %v", server.SpanKind, server.SpanContext, server.Parent)
	}
	wantServer := map[attribute.Key]attribute.Value{
		semconv.HTTPRequestMethodKey:      attribute.StringValue("GET"),
		semconv.URLSchemeKey:              attribute.StringValue("https"),
		semconv.URLPathKey:                attribute.StringValue("/orders/summary"),
		semconv.ServerAddressKey:          attribute.StringValue("api.example.com"),
		semconv.ServerPortKey:             attribute.IntValue(443),
		semconv.ClientAddressKey:          attribute.StringValue("203.0.113.7"),
		semconv.NetworkProtocolVersionKey: attribute.StringValue("2"),
		semconv.UserAgentOriginalKey:      attribute.StringValue("curl/8.9"),
		semconv.HTTPResponseStatusCodeKey: attribute.IntValue(200),
		catalog.AttrListener:              attribute.StringValue("public"),
		catalog.AttrRoute:                 attribute.StringValue("orders-summary"),
		catalog.AttrRevision:              attribute.StringValue("rev-0123456789ab"),
		catalog.AttrConsumer:              attribute.StringValue("partner-a"),
		catalog.AttrTier:                  attribute.StringValue("gold"),
	}
	if got := attrs(server); !equalAttrs(got, wantServer) {
		t.Fatalf("server attributes\n got %v\nwant %v", got, wantServer)
	}
	if server.Status.Code != codes.Unset {
		t.Fatalf("server status %v on 200", server.Status)
	}

	match, _ := byName(spans, catalog.SpanRouteMatch)
	if match.SpanKind != trace.SpanKindInternal || match.Parent.SpanID() != d.ServerSpanID ||
		attrs(match)[catalog.AttrRoute] != attribute.StringValue("orders-summary") {
		t.Fatalf("route match span: %+v", match)
	}

	jwt, _ := byName(spans, nm.jwt)
	if jwt.SpanKind != trace.SpanKindInternal || jwt.Parent.SpanID() != d.ServerSpanID || !equalAttrs(attrs(jwt), map[attribute.Key]attribute.Value{
		catalog.AttrPolicyType: attribute.StringValue("auth.jwt"),
		catalog.AttrPhase:      attribute.StringValue("onRequestHeaders"),
		catalog.AttrOutcome:    attribute.StringValue("continue"),
	}) {
		t.Fatalf("filter span: kind %v parent %v attrs %v", jwt.SpanKind, jwt.Parent, attrs(jwt))
	}

	orders, _ := byName(spans, nm.orders)
	if orders.SpanKind != trace.SpanKindClient || orders.Parent.SpanID() != d.ServerSpanID || !equalAttrs(attrs(orders), map[attribute.Key]attribute.Value{
		catalog.AttrAttempt:               attribute.IntValue(1),
		semconv.ServerAddressKey:          attribute.StringValue("10.0.0.1"),
		semconv.ServerPortKey:             attribute.IntValue(8080),
		catalog.AttrCompStep:              attribute.StringValue("orders"),
		semconv.HTTPResponseStatusCodeKey: attribute.IntValue(200),
	}) {
		t.Fatalf("upstream span: kind %v parent %v attrs %v", orders.SpanKind, orders.Parent, attrs(orders))
	}
	inventory, _ := byName(spans, nm.inventory)
	if a := attrs(inventory); a[semconv.ServerAddressKey] != attribute.StringValue("2001:db8::2") || a[semconv.ServerPortKey] != attribute.IntValue(8443) {
		t.Fatalf("IPv6 endpoint attributes %v", a)
	}
	// Each leg's traceparent names its attempt span as the parent (req 31).
	for _, lc := range []struct {
		h    http.Header
		span tracetest.SpanStub
	}{{ordersOut, orders}, {inventoryOut, inventory}} {
		want := string(AppendTraceparent(nil, d.TraceID, lc.span.SpanContext.SpanID(), true))
		if got := lc.h.Get(HeaderTraceparent); got != want {
			t.Fatalf("leg %s traceparent %q, want %q", lc.span.Name, got, want)
		}
	}
	// Upstream-scoped Policies are children of their leg span.
	oauth, _ := byName(spans, nm.oauth)
	if oauth.Parent.SpanID() != orders.SpanContext.SpanID() {
		t.Fatal("upstream-oauth is not a child of the orders leg")
	}

	// No span carries url.full, url.query or any header attribute (req 33).
	for _, s := range spans {
		for _, kv := range s.Attributes {
			k := string(kv.Key)
			if k == "url.full" || k == "url.query" || strings.HasPrefix(k, "http.request.header.") || strings.HasPrefix(k, "http.response.header.") {
				t.Fatalf("span %s carries %s", s.Name, k)
			}
		}
		if s.SpanContext.TraceID() != d.TraceID || s.InstrumentationScope.Name != ScopeName {
			t.Fatalf("span %s: trace %s scope %q", s.Name, s.SpanContext.TraceID(), s.InstrumentationScope.Name)
		}
	}
}

func equalAttrs(got, want map[attribute.Key]attribute.Value) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func names(spans tracetest.SpanStubs) []string {
	var out []string
	for _, s := range spans {
		out = append(out, s.Name)
	}
	return out
}

// TestSpanStatusAndErrors covers the status rules: server Error on 5xx
// only, client Error on 4xx, 5xx and every error.type, internal spans
// Error when they carry an error.type (spec 09 reqs 33 to 36).
func TestSpanStatusAndErrors(t *testing.T) {
	tr, mem := newMemTracer(t, Options{})
	ctx := context.Background()
	type end struct {
		status    int
		code, typ string
	}
	upstreamStart := func(ctx context.Context) emit.Span {
		_, s := tr.StartUpstream(ctx, catalog.UpstreamSpanName("u"), emit.UpstreamAttrs{Attempt: 2, Endpoint: "no-port"})
		return s
	}
	filterStart := func(ctx context.Context) emit.Span {
		_, s := tr.StartFilter(ctx, catalog.FilterSpanName("p"), emit.FilterAttrs{PolicyType: "auth.jwt", Phase: phase.OnRequestHeaders})
		return s
	}
	matchStart := func(ctx context.Context) emit.Span {
		_, s := tr.StartRouteMatch(ctx)
		return s
	}
	for _, tc := range []struct {
		name     string
		start    func(context.Context) emit.Span
		end      end
		wantErr  bool
		wantType string
	}{
		{"server 404", nil, end{404, "RZ-RT-001", ""}, false, ""},
		{"server 499 client abort", nil, end{499, "", "client_abort"}, false, "client_abort"},
		{"server 502", nil, end{502, "RZ-UP-001", ""}, true, "502"},
		{"server 503 typed", nil, end{503, "RZ-RT-005", "admission"}, true, "admission"},
		{"upstream 404", upstreamStart, end{404, "", ""}, true, ""},
		{"upstream 200", upstreamStart, end{200, "", ""}, false, ""},
		{"upstream connect", upstreamStart, end{0, "RZ-UP-001", "connect"}, true, "connect"},
		{"upstream timeout", upstreamStart, end{0, "", "timeout"}, true, "timeout"},
		{"upstream reset", upstreamStart, end{0, "", "reset"}, true, "reset"},
		{"upstream tls", upstreamStart, end{0, "", "tls"}, true, "tls"},
		{"filter respond 401", filterStart, end{401, "RZ-AUTH-001", ""}, false, ""},
		{"filter cannot decide", filterStart, end{0, "RZ-STS-001", "state_store"}, true, "state_store"},
		{"route match cel error", matchStart, end{500, "RZ-RT-006", "cel"}, true, "cel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem.Reset()
			var d emit.Decision
			tr.Decide(nil, 1, &d)
			sctx, server := tr.StartServer(ctx, &d, "PROPFIND", emit.ServerAttrs{Path: "/a?secret=1"})
			target := server
			if tc.start != nil {
				target = tc.start(sctx)
			}
			target.End(tc.end.status, tc.end.code, tc.end.typ)
			if tc.start != nil {
				server.End(200, "", "")
			}
			spans := flushed(ctx, t, tr, mem)
			var s tracetest.SpanStub
			for _, sp := range spans {
				if sp.SpanContext.SpanID() == target.SpanID() {
					s = sp
				}
			}
			a := attrs(s)
			if (s.Status.Code == codes.Error) != tc.wantErr {
				t.Fatalf("status %v, want error %v", s.Status, tc.wantErr)
			}
			if got := a[semconv.ErrorTypeKey].AsString(); got != tc.wantType {
				t.Fatalf("error.type = %q, want %q", got, tc.wantType)
			}
			if tc.end.code != "" && a[catalog.AttrCode].AsString() != tc.end.code {
				t.Fatalf("ruralz.code = %v", a[catalog.AttrCode])
			}
			if tc.end.status > 0 && a[semconv.HTTPResponseStatusCodeKey].AsInt64() != int64(tc.end.status) {
				t.Fatalf("status code attribute %v", a[semconv.HTTPResponseStatusCodeKey])
			}
			if tc.start == nil {
				// PROPFIND is outside RFC 9110: _OTHER with the original.
				if s.Name != MethodOther || a[semconv.HTTPRequestMethodKey].AsString() != MethodOther ||
					a[semconv.HTTPRequestMethodOriginalKey].AsString() != "PROPFIND" {
					t.Fatalf("method: name %q attrs %v", s.Name, a)
				}
				if a[semconv.URLPathKey].AsString() != "/a" {
					t.Fatalf("url.path %v carries a query", a[semconv.URLPathKey])
				}
			}
		})
	}
}

// TestServerSpanName covers req 33 and 34: "<method> <route>" after the
// match, "<method>" alone when unmatched.
func TestServerSpanName(t *testing.T) {
	tr, mem := newMemTracer(t, Options{})
	for _, tc := range []struct{ method, route, want string }{
		{http.MethodPost, "orders", "POST orders"},
		{http.MethodGet, UnmatchedRoute, "GET"},
		{http.MethodDelete, "", "DELETE"},
		{"get", "r", MethodOther + " r"},
	} {
		mem.Reset()
		var d emit.Decision
		tr.Decide(nil, 1, &d)
		_, s := tr.StartServer(context.Background(), &d, tc.method, emit.ServerAttrs{})
		if tc.route != "" {
			s.SetAttr(catalog.AttrRoute, slog.StringValue(tc.route))
		}
		s.End(http.StatusOK, "", "")
		spans := flushed(t.Context(), t, tr, mem)
		if len(spans) != 1 || spans[0].Name != tc.want {
			t.Fatalf("%s %q: names %v, want %q", tc.method, tc.route, names(spans), tc.want)
		}
		if tc.route != "" && attrs(spans[0])[catalog.AttrRoute].AsString() != tc.route {
			t.Fatalf("ruralz.route = %v", attrs(spans[0])[catalog.AttrRoute])
		}
	}
}

// redacted is a LogValuer standing in for secret.Value.
type redacted struct{}

func (redacted) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

// TestSetAttrKinds covers the slog to OTLP mapping, duplicate keys and
// the buffer overflow.
func TestSetAttrKinds(t *testing.T) {
	tr, mem := newMemTracer(t, Options{})
	var d emit.Decision
	tr.Decide(nil, 1, &d)
	sctx, server := tr.StartServer(context.Background(), &d, http.MethodGet, emit.ServerAttrs{})
	_, f := tr.StartFilter(sctx, catalog.FilterSpanName("p"), emit.FilterAttrs{PolicyType: "ratelimit", Phase: phase.OnRequestHeaders})
	stamp := time.Date(2026, 9, 26, 1, 2, 3, 4, time.UTC)
	for _, kv := range []struct {
		k string
		v slog.Value
	}{
		{catalog.AttrStateOp, slog.StringValue("gcra")},
		{catalog.AttrStateDuration, slog.DurationValue(1500 * time.Microsecond)},
		{"i", slog.Int64Value(-3)},
		{"u", slog.Uint64Value(1 << 63)},
		{"f", slog.Float64Value(0.5)},
		{"b", slog.BoolValue(true)},
		{"t", slog.TimeValue(stamp)},
		{"secret", slog.AnyValue(redacted{})},
		{"any", slog.AnyValue(errors.New("boom"))},
		{"group", slog.GroupValue(slog.Int("x", 1))},
		{"", slog.StringValue("empty key")},
		{catalog.AttrStateOp, slog.StringValue("quota")}, // replaces
	} {
		f.SetAttr(kv.k, kv.v)
	}
	f.End(0, "", "")
	// Overflow the server buffer: every attribute survives.
	for i := range 2 * serverAttrs {
		server.SetAttr(fmt.Sprintf("k%02d", i), slog.IntValue(i))
	}
	server.End(http.StatusOK, "", "")
	spans := flushed(t.Context(), t, tr, mem)
	fs, _ := byName(spans, catalog.FilterSpanName("p"))
	a := attrs(fs)
	want := map[attribute.Key]attribute.Value{
		catalog.AttrPolicyType:    attribute.StringValue("ratelimit"),
		catalog.AttrPhase:         attribute.StringValue("onRequestHeaders"),
		catalog.AttrStateOp:       attribute.StringValue("quota"),
		catalog.AttrStateDuration: attribute.Float64Value(0.0015),
		"i":                       attribute.Int64Value(-3),
		"u":                       attribute.Int64Value(1<<63 - 1),
		"f":                       attribute.Float64Value(0.5),
		"b":                       attribute.BoolValue(true),
		"t":                       attribute.StringValue("2026-09-26T01:02:03.000000004Z"),
		"secret":                  attribute.StringValue("[REDACTED]"),
		"any":                     attribute.StringValue("boom"),
	}
	if !equalAttrs(a, want) {
		t.Fatalf("filter attributes\n got %v\nwant %v", a, want)
	}
	ss, _ := byName(spans, http.MethodGet)
	sa := attrs(ss)
	for i := range 2 * serverAttrs {
		if sa[attribute.Key(fmt.Sprintf("k%02d", i))] != attribute.IntValue(i) && len(sa) < MaxSpanAttributes {
			t.Fatalf("server attribute k%02d lost: %v", i, sa)
		}
	}
	if len(ss.Attributes) != MaxSpanAttributes || ss.DroppedAttributes == 0 {
		t.Fatalf("span limit: %d attributes, %d dropped", len(ss.Attributes), ss.DroppedAttributes)
	}
}

// TestSpanLimits covers spec 09 req 22: string values are cut to at most
// 256 bytes (not characters) at a rune boundary, through the start
// attributes and SetAttr alike.
func TestSpanLimits(t *testing.T) {
	if l := SpanLimits(); l.AttributeCountLimit != 32 || l.AttributeValueLengthLimit != 256 || l.EventCountLimit != 8 || l.LinkCountLimit != 0 {
		t.Fatalf("limits %+v", l)
	}
	for _, tc := range []struct {
		name    string
		in      string
		wantLen int
	}{
		{"ascii", strings.Repeat("u", 1000), 256},
		{"exactly 256", strings.Repeat("u", 256), 256},
		{"short", "curl/8.9", 8},
		{"two-byte runes", strings.Repeat("é", 1000), 256},
		{"three-byte runes", strings.Repeat("€", 1000), 255},
		{"four-byte runes", strings.Repeat("😀", 1000), 256},
		{"rune across the limit", strings.Repeat("u", 255) + "€", 255},
		{"invalid UTF-8", strings.Repeat("\x80", 1000), 253},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, mem := newMemTracer(t, Options{})
			var d emit.Decision
			tr.Decide(nil, 1, &d)
			_, s := tr.StartServer(context.Background(), &d, http.MethodGet, emit.ServerAttrs{UserAgent: tc.in})
			s.SetAttr("custom", slog.StringValue(tc.in))
			s.End(http.StatusOK, "", "")
			spans := flushed(t.Context(), t, tr, mem)
			a := attrs(spans[0])
			for _, k := range []attribute.Key{semconv.UserAgentOriginalKey, "custom"} {
				got := a[k].AsString()
				if len(got) != tc.wantLen || !strings.HasPrefix(tc.in, got) {
					t.Fatalf("%s kept %d bytes, want %d", k, len(got), tc.wantLen)
				}
				if utf8.ValidString(tc.in) && !utf8.ValidString(got) {
					t.Fatalf("%s cut inside a rune: %q", k, got[len(got)-4:])
				}
			}
		})
	}
}

// TestCutUTF8 covers the byte cut of spec 09 req 22 at its edges.
func TestCutUTF8(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"", 4, ""},
		{"abc", 3, "abc"},
		{"abcd", 3, "abc"},
		{"ab€", 3, "ab"},
		{"ab€", 4, "ab"},
		{"ab€", 5, "ab€"},
		{"a😀b", 4, "a"},
		{"a😀b", 5, "a😀"},
		{"\x80\x80\x80\x80\x80", 4, "\x80"},
		{"€", 0, ""},
	} {
		if got := cutUTF8(tc.in, tc.n); got != tc.want {
			t.Errorf("cutUTF8(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

// TestSpansNeedExporterAndSample covers reqs 7 and 15: unsampled requests
// and requests without an OTLP endpoint create no span, onLog creates no
// span, and removing the exporter stops span creation.
func TestSpansNeedExporterAndSample(t *testing.T) {
	tr := newTracer(t, Options{})
	mem := tracetest.NewInMemoryExporter()
	ctx := context.Background()
	var sampled, unsampled emit.Decision
	tr.Decide(nil, 1, &sampled)
	tr.Decide(nil, 0, &unsampled)
	run := func(d *emit.Decision) emit.Span {
		sctx, s := tr.StartServer(ctx, d, http.MethodGet, emit.ServerAttrs{})
		_, f := tr.StartFilter(sctx, catalog.FilterSpanName("log"), emit.FilterAttrs{Phase: phase.OnLog})
		f.End(0, "", "")
		s.End(http.StatusOK, "", "")
		return s
	}
	if tr.SpansEnabled() {
		t.Fatal("enabled without exporter")
	}
	if s := run(&sampled); s.SpanID() != sampled.ServerSpanID {
		t.Fatal("no-op server span does not carry the server span ID")
	}
	if old := tr.SetExporter(mem); old != nil || !tr.SpansEnabled() {
		t.Fatal("SetExporter")
	}
	if s := run(&unsampled); s.SpanID() != unsampled.ServerSpanID {
		t.Fatal("unsampled server span ID")
	}
	if got := flushed(ctx, t, tr, mem); len(got) != 0 {
		t.Fatalf("spans without sampling or exporter: %v", names(got))
	}
	run(&sampled)
	if got := flushed(ctx, t, tr, mem); len(got) != 1 || got[0].Name != http.MethodGet {
		t.Fatalf("sampled request spans %v, want only the server span (onLog creates none)", names(got))
	}
	if old := tr.SetExporter(nil); old != mem || tr.SpansEnabled() {
		t.Fatal("SetExporter(nil)")
	}
	mem.Reset()
	run(&sampled)
	if got := flushed(ctx, t, tr, mem); len(got) != 0 {
		t.Fatalf("spans after the exporter was removed: %v", names(got))
	}
	// Nil decision and a no-op StartChunk.
	if _, s := tr.StartServer(ctx, nil, http.MethodGet, emit.ServerAttrs{}); s.SpanID() != ([8]byte{}) {
		t.Fatal("nil decision")
	}
	if c, s := tr.StartChunk(ctx, catalog.FilterSpanName("c"), emit.FilterAttrs{Phase: phase.OnChunk}); c != ctx || s.SpanID() != ([8]byte{}) {
		t.Fatal("StartChunk without a span")
	}
}

// TestStartChunk covers the M3 helper of req 37.
func TestStartChunk(t *testing.T) {
	tr, mem := newMemTracer(t, Options{})
	var d emit.Decision
	tr.Decide(nil, 1, &d)
	sctx, s := tr.StartServer(context.Background(), &d, http.MethodGet, emit.ServerAttrs{})
	_, c := tr.StartChunk(sctx, catalog.FilterSpanName("stream"), emit.FilterAttrs{PolicyType: "ai.guard", Phase: phase.OnChunk})
	c.End(0, "", "")
	s.End(http.StatusOK, "", "")
	cs, ok := byName(flushed(t.Context(), t, tr, mem), catalog.FilterSpanName("stream"))
	if !ok || attrs(cs)[catalog.AttrPhase].AsString() != "onChunk" || cs.Parent.SpanID() != d.ServerSpanID {
		t.Fatalf("chunk span %+v", cs)
	}
}

// TestIDGenerator covers req 30: only the server span takes the
// Decision's IDs; children and spans started after the server span ended
// draw fresh ones; the random source never yields zero IDs.
func TestIDGenerator(t *testing.T) {
	tr, mem := newMemTracer(t, Options{})
	var d emit.Decision
	tr.Decide(nil, 1, &d) // root
	sctx, s := tr.StartServer(context.Background(), &d, http.MethodGet, emit.ServerAttrs{})
	_, c1 := tr.StartFilter(sctx, catalog.FilterSpanName("a"), emit.FilterAttrs{Phase: phase.OnRequestHeaders})
	c1.End(0, "", "")
	s.End(http.StatusOK, "", "")
	spans := flushed(t.Context(), t, tr, mem)
	seen := map[trace.SpanID]bool{}
	for _, sp := range spans {
		if seen[sp.SpanContext.SpanID()] {
			t.Fatalf("duplicate span ID %s", sp.SpanContext.SpanID())
		}
		seen[sp.SpanContext.SpanID()] = true
	}
	root, _ := byName(spans, http.MethodGet)
	if root.SpanContext.SpanID() != d.ServerSpanID || root.SpanContext.TraceID() != d.TraceID || root.Parent.IsValid() {
		t.Fatalf("root server span %v parent %v", root.SpanContext, root.Parent)
	}

	// A source yielding zeros first is retried.
	calls := 0
	src := &source{fill: func(b []byte) {
		calls++
		v := byte(0)
		if calls >= 3 {
			v = 1
		}
		for i := range b {
			b[i] = v
		}
	}}
	var tid [16]byte
	var sid [8]byte
	src.ids(&tid, &sid)
	if tid == ([16]byte{}) || sid == ([8]byte{}) || calls != 3 {
		t.Fatalf("ids after %d reads: %x %x", calls, tid, sid)
	}
	calls = 0
	src.spanID(&sid)
	if sid == ([8]byte{}) {
		t.Fatal("zero span ID")
	}
	g := &IDGenerator{src: src}
	if tid2, sid2 := g.NewIDs(context.Background()); tid2 == (trace.TraceID{}) || sid2 == (trace.SpanID{}) {
		t.Fatal("NewIDs returned zero IDs")
	}
	if sid3 := g.NewSpanID(context.Background(), trace.TraceID(tid)); sid3 == (trace.SpanID{}) {
		t.Fatal("NewSpanID returned a zero ID")
	}
}

// TestSetExporterSwap covers req 24: a swap routes later batches to the
// new exporter and hands the old one back.
func TestSetExporterSwap(t *testing.T) {
	tr := newTracer(t, Options{})
	a, b := newRecordExporter(), newRecordExporter()
	tr.SetExporter(a)
	var d emit.Decision
	tr.Decide(nil, 1, &d)
	one := func() {
		_, s := tr.StartServer(context.Background(), &d, http.MethodGet, emit.ServerAttrs{})
		s.End(http.StatusOK, "", "")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tr.ForceFlush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	one()
	if old := tr.SetExporter(b); old != a {
		t.Fatalf("old exporter %v", old)
	}
	one()
	ab, _ := a.got()
	bb, _ := b.got()
	if !slices.Equal(ab, []int{1}) || !slices.Equal(bb, []int{1}) {
		t.Fatalf("batches a %v b %v", ab, bb)
	}
}

func TestNewOptions(t *testing.T) {
	for _, o := range []Options{
		{RootPerSecond: -1},
		{ParentPerSecond: -1},
		{QueueSize: -1},
		{BatchSize: -1},
		{BatchInterval: -time.Second},
		{ExportTimeout: -time.Second},
	} {
		if _, err := New(o); !errors.Is(err, ErrOptions) {
			t.Errorf("New(%+v) = %v, want ErrOptions", o, err)
		}
	}
	res := resource.NewSchemaless(semconv.ServiceName("ruralzd"))
	tr, mem := newMemTracer(t, Options{Resource: res})
	var d emit.Decision
	tr.Decide(nil, 1, &d)
	_, s := tr.StartServer(context.Background(), &d, http.MethodGet, emit.ServerAttrs{})
	s.End(http.StatusOK, "", "")
	spans := flushed(t.Context(), t, tr, mem)
	if len(spans) != 1 || spans[0].Resource.Equivalent() != res.Equivalent() {
		t.Fatalf("resource not used: %v", spans)
	}
}

// TestSpanTreeGolden is spec 09 golden test 34: the 15 Ruralz spans of
// OBS Figure 2 for the orders-summary Route (names, kinds, parent links,
// order).
func TestSpanTreeGolden(t *testing.T) {
	tr, mem := newMemTracer(t, Options{})
	nm := newOrdersNames()
	var d emit.Decision
	tr.Decide(header(HeaderTraceparent, tpSampled), 0, &d)
	ordersSummary(context.Background(), tr, &d, &nm)
	spans := flushed(t.Context(), t, tr, mem)
	children := map[trace.SpanID][]tracetest.SpanStub{}
	var roots []tracetest.SpanStub
	for _, s := range spans {
		if s.Parent.IsRemote() {
			roots = append(roots, s)
			continue
		}
		children[s.Parent.SpanID()] = append(children[s.Parent.SpanID()], s)
	}
	var b strings.Builder
	var walk func(s tracetest.SpanStub, depth int)
	walk = func(s tracetest.SpanStub, depth int) {
		a := attrs(s)
		extra := ""
		if ph, ok := a[catalog.AttrPhase]; ok {
			extra = " " + ph.AsString()
		}
		if at, ok := a[catalog.AttrAttempt]; ok {
			extra = fmt.Sprintf(" attempt %d", at.AsInt64())
		}
		fmt.Fprintf(&b, "%s%s [%s]%s\n", strings.Repeat("  ", depth), s.Name, s.SpanKind, extra)
		kids := children[s.SpanContext.SpanID()]
		slices.SortStableFunc(kids, func(x, y tracetest.SpanStub) int { return x.StartTime.Compare(y.StartTime) })
		for _, k := range kids {
			walk(k, depth+1)
		}
	}
	if len(roots) != 1 {
		t.Fatalf("roots %v", names(roots))
	}
	walk(roots[0], 0)
	got := b.String()
	if strings.Count(got, "\n") != 15 {
		t.Fatalf("tree has %d spans:\n%s", strings.Count(got, "\n"), got)
	}
	const path = "testdata/orders-summary.spans.txt"
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("span tree differs from %s:\n%s", path, got)
	}
}
