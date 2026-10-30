// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/gateway/handler/handlertest"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// counted is a Forwarder answering 200 that counts its calls.
func counted(n *atomic.Int64) snapshot.Forwarder {
	return handlertest.ForwardFunc(func(context.Context, *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		n.Add(1)
		return &snapshot.UpstreamResponse{Status: http.StatusOK, Header: http.Header{}, Body: http.NoBody}, nil
	})
}

// TestConnectAndAsteriskMatchNoRoute checks that CONNECT and OPTIONS *
// get 404 RZ-RT-001 even where a catch-all Route exists, logged under
// _unmatched.
func TestConnectAndAsteriskMatchNoRoute(t *testing.T) {
	for name, r := range map[string]*http.Request{
		"CONNECT": func() *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Method = http.MethodConnect
			return r
		}(),
		"OPTIONS *": httptest.NewRequest(http.MethodOptions, "*", nil),
	} {
		t.Run(name, func(t *testing.T) {
			k := newKit(t)
			var n atomic.Int64
			k.publish(handlertest.Spec{Routes: []handlertest.Route{route("all", counted(&n))}})
			wantProblem(t, k.do(r), http.StatusNotFound, codeNoRoute)
			if n.Load() != 0 {
				t.Error("the request reached a Route")
			}
			if rec := k.record(); rec.Route != unmatchedRoute || rec.Code != codeNoRoute {
				t.Errorf("record = %s %s, want _unmatched RZ-RT-001", rec.Route, rec.Code)
			}
		})
	}
}

// TestHeaderBlockLimits checks RZ-RT-002: a header block, request target
// included, over limits.maxRequestHeaderBytes, or more than 256 field
// lines, is 431 before routing; a block within both passes.
func TestHeaderBlockLimits(t *testing.T) {
	big := func(r *http.Request) { r.Header.Set("X-Big", strings.Repeat("v", 900)) }
	lines := func(n int) func(*http.Request) {
		return func(r *http.Request) {
			for i := range n {
				r.Header.Add("X-L", strconv.Itoa(i))
			}
		}
	}
	tests := []struct {
		name   string
		limit  int64
		target string
		mod    func(*http.Request)
		want   int
	}{
		{"within", 1024, "/", func(*http.Request) {}, http.StatusOK},
		{"header over the limit", 1024, "/", big, http.StatusRequestHeaderFieldsTooLarge},
		{"target counts", 1024, "/?q=" + strings.Repeat("a", 900), func(*http.Request) {}, http.StatusRequestHeaderFieldsTooLarge},
		{"256 field lines", 64 << 10, "/", lines(MaxFieldLines), http.StatusOK},
		{"257 field lines", 64 << 10, "/", lines(MaxFieldLines + 1), http.StatusRequestHeaderFieldsTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newKit(t)
			var n atomic.Int64
			k.publish(handlertest.Spec{Limits: snapshot.Limits{MaxRequestHeaderBytes: tt.limit}, Routes: []handlertest.Route{route("all", counted(&n))}})
			r := httptest.NewRequest(http.MethodGet, tt.target, nil)
			tt.mod(r)
			res := k.do(r)
			if tt.want == http.StatusOK {
				if res.Code != http.StatusOK || n.Load() != 1 {
					t.Fatalf("status %d, forwarded %d, want 200 forwarded", res.Code, n.Load())
				}
				return
			}
			wantProblem(t, res, tt.want, codeHeaderTooLarge)
			if n.Load() != 0 {
				t.Error("a refused request was forwarded")
			}
		})
	}
}

// TestFieldListSize checks the size request hardening counts: RFC 9113's
// field-list size, each field line's name and value plus 32, with the four
// pseudo-header fields counted for every protocol, and the field lines
// without them.
func TestFieldListSize(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://api.example/v1/orders?id=7", nil)
	r.Header = http.Header{"Accept": {"*/*"}, "Cookie": {"a=1; b=2"}}
	size, lines := fieldListSize(r, "https")
	want := int64(len(":method")+len("GET")+32) + int64(len(":scheme")+len("https")+32) +
		int64(len(":authority")+len("api.example")+32) + int64(len(":path")+len("/v1/orders?id=7")+32) +
		int64(len("Accept")+len("*/*")+32) + int64(len("Cookie")+len("a=1; b=2")+32)
	if size != want || lines != 2 {
		t.Errorf("fieldListSize = %d, %d; want %d, 2", size, lines, want)
	}
}

// TestConflictingFraming checks RZ-RT-017 for framing net/http let
// through: a coding other than exactly chunked, Transfer-Encoding beside
// Content-Length, and Content-Length field lines that disagree with the
// length net/http framed the request by, as the Go 1.26 HTTP/2 server
// lets them through: differing values, a list or a signed value, which it
// reads as 0, and a length on a stream that ended with its HEADERS.
func TestConflictingFraming(t *testing.T) {
	tests := map[string]func(*http.Request){
		"two codings":     func(r *http.Request) { r.TransferEncoding = []string{"gzip", "chunked"} },
		"not chunked":     func(r *http.Request) { r.TransferEncoding = []string{"gzip"} },
		"beside a length": func(r *http.Request) { r.TransferEncoding = []string{"chunked"}; r.Header.Set("Content-Length", "5") },
		"left in header":  func(r *http.Request) { r.Header.Set("Transfer-Encoding", "chunked") },
		"differing lengths": func(r *http.Request) {
			r.ProtoMajor, r.Header["Content-Length"] = 2, []string{"5", "6"}
		},
		"a list of lengths": func(r *http.Request) {
			r.ProtoMajor, r.Header["Content-Length"], r.ContentLength = 2, []string{"5, 5"}, 0
		},
		"a signed length": func(r *http.Request) {
			r.ProtoMajor, r.Header["Content-Length"], r.ContentLength = 2, []string{"+5"}, 0
		},
		"a length without content": func(r *http.Request) {
			r.ProtoMajor, r.Body, r.ContentLength = 2, http.NoBody, 0
			r.Header.Set("Content-Length", "5")
		},
	}
	for name, mod := range tests {
		t.Run(name, func(t *testing.T) {
			k := newKit(t)
			var n atomic.Int64
			k.publish(handlertest.Spec{Routes: []handlertest.Route{route("all", counted(&n))}})
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("hello"))
			mod(r)
			wantProblem(t, k.do(r), http.StatusBadRequest, codeRejected)
			if n.Load() != 0 {
				t.Error("conflicting framing was forwarded")
			}
		})
	}
}

// TestAgreeingContentLength checks that Content-Length field lines that
// agree with the request's framing pass: one value, equal repeated lines,
// which count as one, and 0 on a request without content.
func TestAgreeingContentLength(t *testing.T) {
	tests := map[string]func(*http.Request){
		"one value":      func(r *http.Request) { r.Header.Set("Content-Length", "5") },
		"repeated lines": func(r *http.Request) { r.ProtoMajor, r.Header["Content-Length"] = 2, []string{"5", "5"} },
		"zero without content": func(r *http.Request) {
			r.Body, r.ContentLength = http.NoBody, 0
			r.Header.Set("Content-Length", "0")
		},
	}
	for name, mod := range tests {
		t.Run(name, func(t *testing.T) {
			k := newKit(t)
			var n atomic.Int64
			k.publish(handlertest.Spec{Routes: []handlertest.Route{route("all", counted(&n))}})
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("hello"))
			mod(r)
			if res := k.do(r); res.Code != http.StatusOK || n.Load() != 1 {
				t.Errorf("response %d, forwarded %d; want 200 forwarded once", res.Code, n.Load())
			}
		})
	}
}

// TestAbsoluteFormTarget checks the absolute-form target on HTTP/1.1: its
// authority routes the request, and a scheme other than the listener's is
// 400 RZ-RT-017.
func TestAbsoluteFormTarget(t *testing.T) {
	k := newKit(t)
	var n atomic.Int64
	k.publish(handlertest.Spec{Routes: []handlertest.Route{{Name: "api", Match: &v1alpha1.RouteMatch{Hosts: []string{"api.example"}}, Forward: counted(&n)}}})
	if res := k.do(httptest.NewRequest(http.MethodGet, "http://api.example/v1", nil)); res.Code != http.StatusOK || n.Load() != 1 {
		t.Fatalf("http absolute-form on an http listener = %d, forwarded %d; want 200 by its authority", res.Code, n.Load())
	}
	r := httptest.NewRequest(http.MethodGet, "https://api.example/v1", nil)
	r.TLS = nil
	wantProblem(t, k.do(r), http.StatusBadRequest, codeRejected)
}

// TestHTTP2HostMatchesAuthority checks that an HTTP/2 request whose Host
// names another host than :authority is malformed, 400 RZ-RT-017, compared
// lowercased and without a default port.
func TestHTTP2HostMatchesAuthority(t *testing.T) {
	tests := []struct {
		host string
		want int
	}{
		{"other.example", http.StatusBadRequest},
		{"API.example:80", http.StatusOK},
		{"api.example", http.StatusOK},
		{"api.example:8080", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			k := newKit(t)
			var n atomic.Int64
			k.publish(handlertest.Spec{Routes: []handlertest.Route{route("all", counted(&n))}})
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Proto, r.ProtoMajor, r.ProtoMinor = "HTTP/2.0", 2, 0
			r.Host = "api.example"
			r.Header["Host"] = []string{tt.host}
			res := k.do(r)
			if tt.want == http.StatusOK {
				if res.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200", res.Code)
				}
				return
			}
			wantProblem(t, res, tt.want, codeRejected)
		})
	}
}

// TestPathHardening checks the one normalized path: %00 and encoded
// backslashes are 400 RZ-RT-017, dot segments are removed before the
// Router and the Forwarder see the path.
func TestPathHardening(t *testing.T) {
	k := newKit(t)
	var path string
	fwd := handlertest.ForwardFunc(func(_ context.Context, o *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		path = o.X.Path()
		return &snapshot.UpstreamResponse{Status: http.StatusOK, Header: http.Header{}, Body: http.NoBody}, nil
	})
	k.publish(handlertest.Spec{Routes: []handlertest.Route{{Name: "b", Match: &v1alpha1.RouteMatch{Path: &v1alpha1.PathMatch{Exact: "/b"}}, Forward: fwd}}})
	for _, target := range []string{"/a%00b", "/a%5Cb", "/a%5cb"} {
		wantProblem(t, k.get(target), http.StatusBadRequest, codeRejected)
	}
	if res := k.get("/a/../b"); res.Code != http.StatusOK || path != "/b" {
		t.Errorf("GET /a/../b = %d with path %q, want 200 on /b", res.Code, path)
	}
}

// TestNoRouteAndWhenError checks the Router's answers: none left is 404
// RZ-RT-001; a match.when runtime error is 500 RZ-RT-006 without
// falling through to a less specific Route.
func TestNoRouteAndWhenError(t *testing.T) {
	k := newKit(t)
	var fallback atomic.Int64
	src := `request.headers["x-tenant"] == "a"`
	when := celtest.Program(t, expr.Site{Place: expr.PlaceRouteMatchWhen}, src)
	k.publish(handlertest.Spec{Routes: []handlertest.Route{
		{Name: "guarded", Match: &v1alpha1.RouteMatch{Path: &v1alpha1.PathMatch{Prefix: "/admin"}, When: src}, When: when, Forward: ok()},
		{Name: "orders", Match: &v1alpha1.RouteMatch{Path: &v1alpha1.PathMatch{Prefix: "/orders"}}, Forward: counted(&fallback)},
		{Name: "fallback", Match: &v1alpha1.RouteMatch{Path: &v1alpha1.PathMatch{Exact: "/admin/x"}, Methods: []string{http.MethodPost}}, Forward: counted(&fallback)},
	}})
	wantProblem(t, k.get("/nothing"), http.StatusNotFound, codeNoRoute)
	wantProblem(t, k.get("/admin/x"), http.StatusInternalServerError, codeWhen)
	if fallback.Load() != 0 {
		t.Error("an erroring match.when fell through")
	}
	for _, rec := range k.alog.Records() {
		if rec.Route != unmatchedRoute {
			t.Errorf("record route = %s, want _unmatched", rec.Route)
		}
	}
	if spans := k.tracer.Spans("match"); len(spans) != 2 || spans[1].Attr("ruralz.route") != unmatchedRoute {
		t.Errorf("route match spans = %d, want 2 naming _unmatched", len(spans))
	}
}

// TestPreRoutingWriteLimit checks the pre-routing bound: past it the
// handler aborts with http.ErrAbortHandler without writing.
func TestPreRoutingWriteLimit(t *testing.T) {
	k := newKit(t, func(o *kitOptions) { o.preRouting = 0 })
	k.publish(handlertest.Spec{})
	res := k.get("/")
	if !res.aborted || res.Body.Len() != 0 {
		t.Errorf("aborted %t, body %q, want an abort without writing", res.aborted, res.Body.String())
	}
	if k.units.InUse() != 0 {
		t.Errorf("in-flight units = %d after the abort, want 0", k.units.InUse())
	}
}
