// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cors

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Origins of the fixture: the cors-default Policy of the configuration
// model's example Bundle allows shop, cors-partner allows partner.
const (
	shop    = "https://shop.example"
	partner = "https://portal.acme.example"
	other   = "https://evil.example"
)

// build compiles cors-default: allowOrigins [shop], allowMethods [GET,
// POST].
func build(t *testing.T) *Filter {
	t.Helper()
	return buildWith(t, &v1alpha1.CORSConfig{AllowOrigins: []string{shop}, AllowMethods: []string{"GET", "POST"}})
}

// buildWith compiles cfg.
func buildWith(t testing.TB, cfg *v1alpha1.CORSConfig) *Filter {
	t.Helper()
	f, err := Factory{}.Build(context.Background(), filter.BuildEnv{Config: cfg})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return f.(*Filter)
}

// request returns an Exchange for method with the request fields h.
func request(method string, h http.Header) *filtertest.Exchange {
	if h == nil {
		h = http.Header{}
	}
	return &filtertest.Exchange{MethodV: method, Hdr: h}
}

// preflight returns a CORS-preflight request from origin for method.
func preflight(origin, method string) *filtertest.Exchange {
	return request(http.MethodOptions, http.Header{fieldOrigin: {origin}, fieldRequestMethod: {method}})
}

// respond runs onResponse on x over the response fields h and returns
// them.
func respond(t *testing.T, f *Filter, x *filtertest.Exchange, h http.Header) http.Header {
	t.Helper()
	if h == nil {
		h = http.Header{}
	}
	x.Msg = &filtertest.Message{Hdr: h}
	if r := f.Handle(t.Context(), phase.OnResponse, x); r.Outcome != filter.Continue {
		t.Fatalf("onResponse = %+v, want Continue", r)
	}
	return h
}

// answer runs onRequestHeaders on a preflight and returns the 204's
// fields, failing unless the preflight is allowed.
func answer(t *testing.T, f *Filter, x *filtertest.Exchange) http.Header {
	t.Helper()
	r := f.Handle(t.Context(), phase.OnRequestHeaders, x)
	if r.Outcome != filter.Respond || r.Response == nil || r.Response.Status != http.StatusNoContent {
		t.Fatalf("preflight = %+v, want a 204", r)
	}
	if resp := r.Response; resp.Code != "" || resp.Body != nil || resp.Detail != "" {
		t.Fatalf("preflight answer = %+v, want a plain 204", resp)
	}
	return r.Response.Header
}

// refused checks that onRequestHeaders refuses a preflight with 403
// RZ-RT-008 and no CORS field.
func refused(t *testing.T, f *Filter, x *filtertest.Exchange) {
	t.Helper()
	r := f.Handle(t.Context(), phase.OnRequestHeaders, x)
	if r.Outcome != filter.Respond || r.Response == nil {
		t.Fatalf("preflight = %+v, want Respond", r)
	}
	if resp := r.Response; resp.Status != 403 || resp.Code != CodeRejected || len(resp.Header) != 0 || resp.Body != nil {
		t.Fatalf("preflight answer = %+v, want 403 %s without fields", resp, CodeRejected)
	}
}

// fields compares h with want, field by field, line by line.
func fields(t *testing.T, what string, h, want http.Header) {
	t.Helper()
	if len(h) != len(want) {
		t.Errorf("%s fields = %v, want %v", what, h, want)
		return
	}
	for k, v := range want {
		if !slices.Equal(h[k], v) {
			t.Errorf("%s %s = %q, want %q", what, k, h[k], v)
		}
	}
}

// TestRequestWithoutOriginIsNotCORS checks that a request without Origin
// passes on untouched and its response gets no CORS field, only Origin in
// Vary, since the answer depends on Origin (Fetch, "CORS protocol and
// HTTP caches"); OPTIONS without Origin is no preflight.
func TestRequestWithoutOriginIsNotCORS(t *testing.T) {
	f := build(t)
	for _, m := range []string{http.MethodGet, http.MethodOptions} {
		x := request(m, http.Header{fieldRequestMethod: {"GET"}})
		if r := f.Handle(t.Context(), phase.OnRequestHeaders, x); r.Outcome != filter.Continue || x.State != nil {
			t.Fatalf("%s without Origin = %+v, state %v; want Continue and no state", m, r, x.State)
		}
		h := respond(t, f, x, http.Header{"Content-Type": {"application/json"}})
		fields(t, m+" response", h, http.Header{"Content-Type": {"application/json"}, fieldVary: {fieldOrigin}})
	}
}

// TestPreflightAllowed checks that a preflight from an allowed origin for
// an allowed method, asking for no header or only allowed ones (ignoring
// case), is answered in onRequestHeaders, so it never reaches
// authentication: 204 with the request's origin, allowMethods,
// allowHeaders when set and Vary over the three request fields; with the
// strict defaults, no credentials, max-age or allow-headers field.
func TestPreflightAllowed(t *testing.T) {
	strict := build(t)
	fields(t, "strict answer", answer(t, strict, preflight(shop, "POST")), http.Header{
		fieldAllowOrigin:  {shop},
		fieldAllowMethods: {"GET, POST"},
		fieldVary:         {varyPreflight},
	})
	x := preflight(shop, "POST")
	x.Hdr[fieldRequestHeaders] = []string{" , "}
	fields(t, "answer to empty elements", answer(t, strict, x), http.Header{
		fieldAllowOrigin:  {shop},
		fieldAllowMethods: {"GET, POST"},
		fieldVary:         {varyPreflight},
	})

	f := buildWith(t, &v1alpha1.CORSConfig{
		AllowOrigins: []string{shop}, AllowMethods: []string{"GET", "POST"},
		AllowHeaders: []string{"Authorization", "Content-Type", "x-api-key"},
	})
	for _, requested := range [][]string{
		{"authorization, content-type"},
		{"X-Api-Key"},
		{" x-api-key ,, ", "CONTENT-TYPE"},
	} {
		x := preflight(shop, "POST")
		x.Hdr[fieldRequestHeaders] = requested
		h := answer(t, f, x)
		fields(t, "answer", h, http.Header{
			fieldAllowOrigin:  {shop},
			fieldAllowMethods: {"GET, POST"},
			fieldAllowHeaders: {"Authorization, Content-Type, x-api-key"},
			fieldVary:         {varyPreflight},
		})
		if x.State != nil {
			t.Errorf("preflight recorded state %v", x.State)
		}
		// onResponse runs on the generated answer and keeps it.
		after := respond(t, f, x, h.Clone())
		fields(t, "answer after onResponse", after, h)
	}
}

// TestPreflightRejected checks that a preflight the Policy cannot allow
// is refused in onRequestHeaders with 403 RZ-RT-008 and no CORS field: an
// origin outside allowOrigins, a method outside allowMethods (safelisted
// methods included), two Origin or Access-Control-Request-Method lines, a
// requested header outside allowHeaders (any, with the strict default),
// and a requested element that is not a field name.
func TestPreflightRejected(t *testing.T) {
	tests := []struct {
		name    string
		headers []string // allowHeaders
		h       http.Header
	}{
		{"other origin", nil, http.Header{fieldOrigin: {other}, fieldRequestMethod: {"GET"}}},
		{"method not allowed", nil, http.Header{fieldOrigin: {shop}, fieldRequestMethod: {"DELETE"}}},
		{"safelisted method not allowed", nil, http.Header{fieldOrigin: {shop}, fieldRequestMethod: {"HEAD"}}},
		{"method case differs", nil, http.Header{fieldOrigin: {shop}, fieldRequestMethod: {"get"}}},
		{"empty method", nil, http.Header{fieldOrigin: {shop}, fieldRequestMethod: {""}}},
		{"two methods", nil, http.Header{fieldOrigin: {shop}, fieldRequestMethod: {"GET", "POST"}}},
		{"two origins", nil, http.Header{fieldOrigin: {shop, shop}, fieldRequestMethod: {"GET"}}},
		{"any header by default", nil, http.Header{fieldOrigin: {shop}, fieldRequestMethod: {"GET"}, fieldRequestHeaders: {"content-type"}}},
		{"header not allowed", []string{"x-api-key"}, http.Header{fieldOrigin: {shop}, fieldRequestMethod: {"GET"}, fieldRequestHeaders: {"x-api-key, authorization"}}},
		{"header name not a token", []string{"x-ok"}, http.Header{fieldOrigin: {shop}, fieldRequestMethod: {"GET"}, fieldRequestHeaders: {"x-ok, bad name"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := buildWith(t, &v1alpha1.CORSConfig{AllowOrigins: []string{shop}, AllowMethods: []string{"GET", "POST"}, AllowHeaders: tt.headers})
			x := request(http.MethodOptions, tt.h)
			refused(t, f, x)
			h := respond(t, f, x, http.Header{})
			fields(t, "rejection after onResponse", h, http.Header{fieldVary: {fieldOrigin}})
		})
	}
	if got := errcode.Status(CodeRejected); got != 403 {
		t.Errorf("registered status of %s = %d, want 403", CodeRejected, got)
	}
}

// TestActualRequestAllowed checks that an actual CORS request whose origin
// is allowed and method in allowMethods passes on, and that its response,
// an Upstream one or one an auth Policy generated, gets
// Access-Control-Allow-Origin with the request's origin, replacing the
// Upstream's value, and Origin in Vary.
func TestActualRequestAllowed(t *testing.T) {
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		f := build(t)
		x := request(m, http.Header{fieldOrigin: {shop}})
		if r := f.Handle(t.Context(), phase.OnRequestHeaders, x); r.Outcome != filter.Continue {
			t.Fatalf("%s from %s = %+v, want Continue", m, shop, r)
		}
		h := respond(t, f, x, http.Header{fieldAllowOrigin: {"*"}, "Content-Type": {"application/json"}})
		fields(t, m+" response", h, http.Header{
			fieldAllowOrigin: {shop}, "Content-Type": {"application/json"}, fieldVary: {fieldOrigin},
		})
	}
	// A 401 generated by an auth Policy after cors gets the fields too.
	f := build(t)
	x := request(http.MethodGet, http.Header{fieldOrigin: {shop}})
	f.Handle(t.Context(), phase.OnRequestHeaders, x)
	x.Msg = &filtertest.Message{Hdr: http.Header{"Www-Authenticate": {`Bearer realm="ruralz"`}}, Gen: true, Code: 401}
	f.Handle(t.Context(), phase.OnResponse, x)
	fields(t, "generated 401", x.Msg.Header(), http.Header{
		"Www-Authenticate": {`Bearer realm="ruralz"`}, fieldAllowOrigin: {shop}, fieldVary: {fieldOrigin},
	})
}

// TestActualRequestNotAllowedPasses checks that an actual CORS request
// from an origin outside allowOrigins, or with a method outside
// allowMethods, is not rejected: it passes on and its response gets no
// CORS field, so the browser withholds it from the page; an Upstream's own
// CORS field is left as it is.
func TestActualRequestNotAllowedPasses(t *testing.T) {
	tests := []struct {
		name   string
		method string
		origin []string
	}{
		{"other origin", http.MethodGet, []string{other}},
		{"method not allowed", http.MethodDelete, []string{shop}},
		{"two origins", http.MethodGet, []string{shop, shop}},
		{"OPTIONS without request method", http.MethodOptions, []string{shop}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := build(t)
			x := request(tt.method, http.Header{fieldOrigin: tt.origin})
			if r := f.Handle(t.Context(), phase.OnRequestHeaders, x); r.Outcome != filter.Continue || x.State != nil {
				t.Fatalf("request = %+v, state %v; want Continue and no state", r, x.State)
			}
			h := respond(t, f, x, http.Header{})
			fields(t, "response", h, http.Header{fieldVary: {fieldOrigin}})
			h = respond(t, f, x, http.Header{fieldAllowOrigin: {"https://upstream.example"}})
			fields(t, "response with an Upstream field", h, http.Header{
				fieldAllowOrigin: {"https://upstream.example"}, fieldVary: {fieldOrigin},
			})
		})
	}
}

// TestOriginsMatchExactly checks that listed origins match the Origin
// field byte for byte: another case, an explicit default port, a
// trailing slash or another scheme allow nothing, and a listed "null"
// allows the null origin.
func TestOriginsMatchExactly(t *testing.T) {
	f := buildWith(t, &v1alpha1.CORSConfig{AllowOrigins: []string{shop, "null"}, AllowMethods: []string{"GET"}})
	tests := []struct {
		origin string
		want   bool
	}{
		{shop, true},
		{"null", true},
		{"https://SHOP.example", false},
		{"https://shop.example:443", false},
		{"https://shop.example/", false},
		{"http://shop.example", false},
		{"*", false},
		{other, false},
	}
	for _, tt := range tests {
		r := f.Handle(t.Context(), phase.OnRequestHeaders, preflight(tt.origin, "GET"))
		if got := r.Response != nil && r.Response.Status == http.StatusNoContent; got != tt.want {
			t.Errorf("preflight from %q allowed = %v, want %v", tt.origin, got, tt.want)
		}
	}
}

// TestWildcardOrigin checks that "*" in allowOrigins allows any origin
// and is answered with Access-Control-Allow-Origin: *, for a preflight and
// an actual request, whatever other entries the list holds; the method
// rule still applies.
func TestWildcardOrigin(t *testing.T) {
	f := buildWith(t, &v1alpha1.CORSConfig{AllowOrigins: []string{shop, "*"}, AllowMethods: []string{"GET"}})
	for _, o := range []string{shop, other, "null", "http://localhost:3000"} {
		fields(t, "answer to "+o, answer(t, f, preflight(o, "GET")), http.Header{
			fieldAllowOrigin: {"*"}, fieldAllowMethods: {"GET"}, fieldVary: {varyPreflight},
		})
		x := request(http.MethodGet, http.Header{fieldOrigin: {o}})
		f.Handle(t.Context(), phase.OnRequestHeaders, x)
		fields(t, "response to "+o, respond(t, f, x, http.Header{}), http.Header{fieldAllowOrigin: {"*"}, fieldVary: {fieldOrigin}})
	}
	refused(t, f, preflight(other, "POST"))
	x := request(http.MethodPost, http.Header{fieldOrigin: {other}})
	f.Handle(t.Context(), phase.OnRequestHeaders, x)
	fields(t, "response to a method not allowed", respond(t, f, x, http.Header{}), http.Header{fieldVary: {fieldOrigin}})
}

// TestWildcardHeaders checks "*" in allowHeaders: a preflight asking for
// any field names passes and the answer sends "*", followed by the
// Authorization name as requested when the preflight asks for it, since a
// browser never lets "*" cover Authorization; an element that is no field
// name is still refused.
func TestWildcardHeaders(t *testing.T) {
	f := buildWith(t, &v1alpha1.CORSConfig{AllowOrigins: []string{shop}, AllowMethods: []string{"POST"}, AllowHeaders: []string{"*", "x-listed"}})
	tests := []struct {
		requested []string
		want      string
	}{
		{nil, "*"},
		{[]string{"x-anything, content-type"}, "*"},
		{[]string{"x-trace-id", "authorization"}, "*, authorization"},
		{[]string{"Authorization, AUTHORIZATION"}, "*, Authorization"},
	}
	for _, tt := range tests {
		x := preflight(shop, "POST")
		if tt.requested != nil {
			x.Hdr[fieldRequestHeaders] = tt.requested
		}
		fields(t, "answer to "+strings.Join(tt.requested, "|"), answer(t, f, x), http.Header{
			fieldAllowOrigin:  {shop},
			fieldAllowMethods: {"POST"},
			fieldAllowHeaders: {tt.want},
			fieldVary:         {varyPreflight},
		})
	}
	x := preflight(shop, "POST")
	x.Hdr[fieldRequestHeaders] = []string{"x-ok, bad name"}
	refused(t, f, x)
}

// TestWildcardExposeHeaders checks "*" in exposeHeaders: an allowed
// actual response sends Access-Control-Expose-Headers: *, whatever other
// names the list holds.
func TestWildcardExposeHeaders(t *testing.T) {
	f := buildWith(t, &v1alpha1.CORSConfig{AllowOrigins: []string{shop}, AllowMethods: []string{"GET"}, ExposeHeaders: []string{"RateLimit", "*"}})
	x := request(http.MethodGet, http.Header{fieldOrigin: {shop}})
	f.Handle(t.Context(), phase.OnRequestHeaders, x)
	fields(t, "response", respond(t, f, x, http.Header{}), http.Header{
		fieldAllowOrigin: {shop}, fieldExposeHeaders: {"*"}, fieldVary: {fieldOrigin},
	})
}

// TestConfiguredAnswers checks the fields beyond allowOrigins and
// allowMethods: allowCredentials adds Access-Control-Allow-Credentials:
// true to the preflight answer and the actual response, maxAge is sent in
// whole seconds (truncated) on the preflight answer only, and
// exposeHeaders reaches the actual response only.
func TestConfiguredAnswers(t *testing.T) {
	yes := true
	for _, tc := range []struct {
		maxAge time.Duration
		want   string
	}{
		{10*time.Minute + 30*time.Second + 900*time.Millisecond, "630"},
		{500 * time.Millisecond, "0"},
		{0, "0"},
	} {
		age := v1alpha1.Duration(tc.maxAge)
		f := buildWith(t, &v1alpha1.CORSConfig{
			AllowOrigins: []string{shop}, AllowMethods: []string{"GET"}, AllowCredentials: &yes,
			MaxAge: &age, ExposeHeaders: []string{"RateLimit", "RateLimit-Policy"},
		})
		fields(t, "answer", answer(t, f, preflight(shop, "GET")), http.Header{
			fieldAllowOrigin:      {shop},
			fieldAllowMethods:     {"GET"},
			fieldAllowCredentials: {"true"},
			fieldMaxAge:           {tc.want},
			fieldVary:             {varyPreflight},
		})
		x := request(http.MethodGet, http.Header{fieldOrigin: {shop}})
		f.Handle(t.Context(), phase.OnRequestHeaders, x)
		fields(t, "response", respond(t, f, x, http.Header{}), http.Header{
			fieldAllowOrigin:      {shop},
			fieldAllowCredentials: {"true"},
			fieldExposeHeaders:    {"RateLimit, RateLimit-Policy"},
			fieldVary:             {fieldOrigin},
		})
	}
	no := false
	f := buildWith(t, &v1alpha1.CORSConfig{AllowOrigins: []string{shop}, AllowMethods: []string{"GET"}, AllowCredentials: &no})
	fields(t, "answer without credentials", answer(t, f, preflight(shop, "GET")), http.Header{
		fieldAllowOrigin: {shop}, fieldAllowMethods: {"GET"}, fieldVary: {varyPreflight},
	})
}

// TestExampleBundlePartner checks the configuration model's example
// Bundle: cors-partner on orders-summary (GET only) answers the partner
// portal's preflight for GET, which the Router now routes to that Route,
// and refuses POST and other origins.
func TestExampleBundlePartner(t *testing.T) {
	f := buildWith(t, &v1alpha1.CORSConfig{AllowOrigins: []string{partner}, AllowMethods: []string{"GET"}})
	fields(t, "partner answer", answer(t, f, preflight(partner, "GET")), http.Header{
		fieldAllowOrigin: {partner}, fieldAllowMethods: {"GET"}, fieldVary: {varyPreflight},
	})
	refused(t, f, preflight(partner, "POST"))
	refused(t, f, preflight(shop, "GET"))
}

// TestVaryKeepsExistingMembers checks that Origin is added to a Vary
// field the response already has as one more line, and not added when
// Vary already lists Origin, in any case, or "*".
func TestVaryKeepsExistingMembers(t *testing.T) {
	tests := []struct {
		name string
		vary []string
		want []string
	}{
		{"other member", []string{"Accept-Encoding"}, []string{"Accept-Encoding", fieldOrigin}},
		{"origin listed", []string{"Accept-Encoding, Origin"}, []string{"Accept-Encoding, Origin"}},
		{"origin lower case", []string{"accept-encoding", " origin "}, []string{"accept-encoding", " origin "}},
		{"star", []string{"*"}, []string{"*"}},
		{"star in a list", []string{"Accept, *"}, []string{"Accept, *"}},
		{"originator is not origin", []string{"Originator"}, []string{"Originator", fieldOrigin}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := build(t)
			x := request(http.MethodGet, http.Header{fieldOrigin: {shop}})
			f.Handle(t.Context(), phase.OnRequestHeaders, x)
			h := respond(t, f, x, http.Header{fieldVary: slices.Clone(tt.vary)})
			if !slices.Equal(h[fieldVary], tt.want) || !slices.Equal(h[fieldAllowOrigin], []string{shop}) {
				t.Errorf("Vary %q became %q (Access-Control-Allow-Origin %q), want %q", tt.vary, h[fieldVary], h[fieldAllowOrigin], tt.want)
			}
		})
	}
}

// TestSharedValuesStayIntact checks the sharing rule of the response
// side: a later Filter appending to the fields it set, as http.Header.Add
// does, never changes what the next response gets.
func TestSharedValuesStayIntact(t *testing.T) {
	f := build(t)
	for range 2 {
		x := request(http.MethodGet, http.Header{fieldOrigin: {shop}})
		f.Handle(t.Context(), phase.OnRequestHeaders, x)
		h := respond(t, f, x, http.Header{})
		fields(t, "response", h, http.Header{fieldAllowOrigin: {shop}, fieldVary: {fieldOrigin}})
		h.Add(fieldVary, "Accept-Encoding")
		h.Add(fieldAllowOrigin, other)
	}
	resp := f.Handle(t.Context(), phase.OnRequestHeaders, preflight(shop, "GET")).Response
	resp.Header.Add(fieldAllowMethods, "PUT")
	resp.Header.Add(fieldVary, "Accept")
	fields(t, "second preflight answer", answer(t, f, preflight(shop, "GET")), http.Header{
		fieldAllowOrigin: {shop}, fieldAllowMethods: {"GET, POST"}, fieldVary: {varyPreflight},
	})
}

// TestEmptyListsAllowNothing checks that an empty allowOrigins or
// allowMethods allows no origin or method: every preflight is refused and
// no response gets Access-Control-Allow-Origin.
func TestEmptyListsAllowNothing(t *testing.T) {
	for _, cfg := range []*v1alpha1.CORSConfig{
		{AllowMethods: []string{"GET"}},
		{AllowOrigins: []string{shop}},
	} {
		f := buildWith(t, cfg)
		refused(t, f, preflight(shop, "GET"))
		x := request(http.MethodGet, http.Header{fieldOrigin: {shop}})
		f.Handle(t.Context(), phase.OnRequestHeaders, x)
		if h := respond(t, f, x, http.Header{}); h[fieldAllowOrigin] != nil {
			t.Errorf("config %+v: response allows %q", cfg, h[fieldAllowOrigin])
		}
	}
}

// TestOtherPhasesContinue checks that cors acts only in its registered
// Phases, onRequestHeaders and onResponse.
func TestOtherPhasesContinue(t *testing.T) {
	f := build(t)
	for p := phase.OnRequestHeaders; p < phase.Count; p++ {
		if p == phase.OnRequestHeaders || p == phase.OnResponse {
			continue
		}
		x := preflight(other, "GET")
		x.Msg = &filtertest.Message{}
		if r := f.Handle(t.Context(), p, x); r.Outcome != filter.Continue || len(x.Msg.Header()) != 0 {
			t.Errorf("Handle(%s) = %+v, fields %v; want Continue and no change", p, r, x.Msg.Header())
		}
	}
}

// TestResponseWithoutMessageCannotDecide checks that onResponse without a
// response message cannot decide with the class default, so failureMode
// applies (502 RZ-RT-012 under closed).
func TestResponseWithoutMessageCannotDecide(t *testing.T) {
	f := build(t)
	r := f.Handle(t.Context(), phase.OnResponse, request(http.MethodGet, nil))
	if r.Outcome != filter.CannotDecide || r.Code != "" || !errors.Is(r.Err, errNoMessage) {
		t.Errorf("onResponse without a message = %+v, want CannotDecide with errNoMessage", r)
	}
}

// TestActualRequestAllocatesNothing checks the per-stage budget of cors:
// an allowed actual request allocates nothing in onRequestHeaders, and
// neither does its response side on a response without Vary
// (docs/architecture/12-performance-budgets-and-benchmarking.md).
func TestActualRequestAllocatesNothing(t *testing.T) {
	yes := true
	f := buildWith(t, &v1alpha1.CORSConfig{
		AllowOrigins: []string{shop}, AllowMethods: []string{"GET"}, AllowCredentials: &yes, ExposeHeaders: []string{"RateLimit"},
	})
	ctx := t.Context()
	x := request(http.MethodGet, http.Header{fieldOrigin: {shop}})
	if n := testing.AllocsPerRun(100, func() {
		x.State = nil
		f.Handle(ctx, phase.OnRequestHeaders, x)
	}); n != 0 {
		t.Errorf("onRequestHeaders allocates %v times per run, want 0", n)
	}
	msg := &filtertest.Message{Hdr: http.Header{}}
	x.Msg = msg
	if n := testing.AllocsPerRun(100, func() {
		delete(msg.Hdr, fieldVary)
		f.Handle(ctx, phase.OnResponse, x)
	}); n != 0 {
		t.Errorf("onResponse allocates %v times per run, want 0", n)
	}
	plain := request(http.MethodGet, nil)
	if n := testing.AllocsPerRun(100, func() { f.Handle(ctx, phase.OnRequestHeaders, plain) }); n != 0 {
		t.Errorf("onRequestHeaders without Origin allocates %v times per run, want 0", n)
	}
}

// BenchmarkActualRequest measures both sides of an allowed actual request
// (budget: 2 µs at p50, 0 allocations in onRequestHeaders).
func BenchmarkActualRequest(b *testing.B) {
	f := buildWith(b, &v1alpha1.CORSConfig{AllowOrigins: []string{shop, other}, AllowMethods: []string{"GET", "POST"}})
	x := request(http.MethodGet, http.Header{fieldOrigin: {shop}})
	msg := &filtertest.Message{Hdr: http.Header{}}
	x.Msg = msg
	ctx := b.Context()
	b.ReportAllocs()
	for b.Loop() {
		x.State = nil
		f.Handle(ctx, phase.OnRequestHeaders, x)
		delete(msg.Hdr, fieldVary)
		f.Handle(ctx, phase.OnResponse, x)
	}
}
