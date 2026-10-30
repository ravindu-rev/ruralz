// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package transform

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// ordersShape returns the orders-shape example of
// docs/architecture/03-data-plane.md "Transform Policies": its
// transform.response config, and for side Request the same operations as
// a transform.request config reading request.body.
func ordersShape(side Side) any {
	if side == Request {
		return &v1alpha1.TransformRequestConfig{
			ArrayOps: []v1alpha1.ArrayOp{arrayOp("move", "items.*.sku", "skus")},
			Set:      []v1alpha1.TransformRequestSet{reqSet("header", "x-order-total", "string(request.body.total)")},
			Remove:   []v1alpha1.TransformRequestRemove{reqRemove("body", "internal")},
			Replace:  []v1alpha1.Replace{pattern("customer.email", "^[^@]+", "***")},
		}
	}
	return &v1alpha1.TransformResponseConfig{
		ArrayOps: []v1alpha1.ArrayOp{arrayOp("move", "items.*.sku", "skus")},
		Set:      []v1alpha1.TransformResponseSet{respSet("header", "x-order-total", "string(response.body.total)")},
		Remove:   []v1alpha1.TransformResponseRemove{respRemove("body", "internal")},
		Replace:  []v1alpha1.Replace{pattern("customer.email", "^[^@]+", "***")},
	}
}

// ordersBody returns a 1 KiB order the orders-shape example acts on, and
// the body it rewrites it to.
func ordersBody() (in, out string) {
	var items, shaped, skus []string
	for i := range 12 {
		n := strconv.Itoa(i + 1)
		rest := `"qty":` + n + `,"price":1` + n + `.50,"tags":["sale","w` + n + `"]`
		items = append(items, `{"sku":"SKU-`+n+`",`+rest+`}`)
		shaped = append(shaped, `{`+rest+`}`)
		skus = append(skus, `"SKU-`+n+`"`)
	}
	head := `{"id":"ord-1001","status":"paid","total":129.90,"currency":"EUR",` +
		`"customer":{"id":"c-42","email":"jane.doe@example.com","name":"Jane Doe"},`
	shapedHead := strings.Replace(head, "jane.doe@", "***@", 1)
	internal := `"internal":{"trace":"4bf92f3577b34da6a3ce929d0e0e4736","shard":3,"notes":"reconciled by batch 2026-10-29"},`
	tail := `"shipping":{"method":"express","address":{"line1":"1 Main St","city":"Springfield","zip":"12345"}}`
	in = head + `"items":[` + strings.Join(items, ",") + `],` + internal + tail + `}`
	out = shapedHead + `"items":[` + strings.Join(shaped, ",") + `],` + tail + `,"skus":[` + strings.Join(skus, ",") + `]}`
	return in, out
}

// TestOrdersShape covers the orders-shape example on a 1 KiB order, in the
// four Phases a transform runs in.
func TestOrdersShape(t *testing.T) {
	in, want := ordersBody()
	if len(in) < 900 || len(in) > 1200 {
		t.Fatalf("order of %d bytes, want about 1 KiB", len(in))
	}
	for _, tc := range []struct {
		side  Side
		scope phase.Scope
		p     phase.Phase
	}{
		{Request, phase.ScopeRoute, phase.OnRequestBody},
		{Request, phase.ScopeUpstream, phase.OnUpstreamRequest},
		{Response, phase.ScopeUpstream, phase.OnUpstreamResponseBody},
		{Response, phase.ScopeRoute, phase.OnResponse},
	} {
		f := build(t, ordersShape(tc.side), tc.scope)
		var x *filtertest.Exchange
		var m *message
		if tc.side == Request {
			x, m = onRequest(t, jsonHeader(), in, "")
		} else {
			x, m = onResponse(t, http.StatusOK, jsonHeader(), in)
		}
		wantNext(t, tc.p.String(), f.Handle(context.Background(), tc.p, x))
		if string(m.Raw) != want || m.Hdr.Get("X-Order-Total") != "129.9" {
			t.Errorf("%v: body\n%s\nwant\n%s\nheader %v", tc.p, m.Raw, want, m.Hdr)
		}
	}
}

// TestResponseWithoutContent covers responses whose body is never
// rewritten, to a HEAD request, with status 1xx, 204, 304 or 206, or with
// Content-Range: config.body and every body entry are skipped, without
// reading the body, and header entries still apply.
func TestResponseWithoutContent(t *testing.T) {
	cfg := &v1alpha1.TransformResponseConfig{
		Body:    `{"x": response.body.missing}`,
		Set:     []v1alpha1.TransformResponseSet{respSet("header", "x-a", `"v"`), respSet("body", "y", "response.body.missing")},
		Remove:  []v1alpha1.TransformResponseRemove{respRemove("body", "z")},
		Replace: []v1alpha1.Replace{literally("", "a", "b")},
	}
	f := build(t, cfg, phase.ScopeRoute)
	cases := []struct {
		name   string
		method string
		status int
		h      http.Header
	}{
		{"HEAD", http.MethodHead, http.StatusOK, nil},
		{"103", http.MethodGet, http.StatusEarlyHints, nil},
		{"204", http.MethodGet, http.StatusNoContent, nil},
		{"304", http.MethodGet, http.StatusNotModified, nil},
		{"206", http.MethodGet, http.StatusPartialContent, nil},
		{"Content-Range", http.MethodGet, http.StatusOK, http.Header{"Content-Range": {"bytes 0-1/2"}}},
		{"content-range", http.MethodGet, http.StatusOK, http.Header{"content-range": {"bytes 0-1/2"}}},
	}
	for _, tc := range cases {
		h := tc.h
		if h == nil {
			h = http.Header{}
		}
		x, m := onResponse(t, tc.status, h, "")
		x.MethodV = tc.method
		m.Unavailable = true
		wantNext(t, tc.name, f.Handle(context.Background(), phase.OnResponse, x))
		if m.Hdr.Get("X-A") != "v" || m.bodyReads != 0 || m.setBodies != 0 {
			t.Errorf("%s: header %v, %d reads, %d writes", tc.name, m.Hdr, m.bodyReads, m.setBodies)
		}
	}
	reads := build(t, &v1alpha1.TransformResponseConfig{Set: []v1alpha1.TransformResponseSet{respSet("header", "x-n", `response.body == null ? "none" : "some"`)}}, phase.ScopeRoute)
	x, m := onResponse(t, http.StatusNoContent, http.Header{}, "")
	wantNext(t, "header reading the body", reads.Handle(context.Background(), phase.OnResponse, x))
	if m.bodyReads != 1 || m.Hdr.Get("X-N") != "none" {
		t.Errorf("reads %d, header %v", m.bodyReads, m.Hdr)
	}
}

// TestValidators covers a response whose body was rewritten: each strong
// ETag turns weak and Accept-Ranges goes; nothing changes on a response
// whose body stayed, or on a request.
func TestValidators(t *testing.T) {
	cfg := &v1alpha1.TransformResponseConfig{Remove: []v1alpha1.TransformResponseRemove{respRemove("body", "x")}}
	f := build(t, cfg, phase.ScopeRoute)
	h := jsonHeader()
	h["Etag"] = []string{`"abc"`}
	h["etag"] = []string{`W/"def"`}
	h["Accept-Ranges"] = []string{"bytes"}
	x, m := onResponse(t, http.StatusOK, h, `{"x": 1, "y": 2}`)
	wantNext(t, "rewritten", f.Handle(context.Background(), phase.OnResponse, x))
	if string(m.Raw) != `{"y":2}` || !slices.Equal(h["Etag"], []string{`W/"abc"`, `W/"def"`}) || h["etag"] != nil || h["Accept-Ranges"] != nil {
		t.Errorf("body %s, header %v", m.Raw, h)
	}
	h = jsonHeader()
	h["Etag"] = []string{`"abc"`}
	h["Accept-Ranges"] = []string{"bytes"}
	x, _ = onResponse(t, http.StatusOK, h, `{"y": 2}`)
	wantNext(t, "unchanged", f.Handle(context.Background(), phase.OnResponse, x))
	if !slices.Equal(h["Etag"], []string{`"abc"`}) || h.Get("Accept-Ranges") != "bytes" {
		t.Errorf("header %v of an unchanged response", h)
	}
	req := build(t, removes("x"), phase.ScopeRoute)
	h = jsonHeader()
	h["Etag"] = []string{`"abc"`}
	h["Accept-Ranges"] = []string{"bytes"}
	rx, _ := onRequest(t, h, `{"x": 1}`, "")
	wantNext(t, "request", req.Handle(context.Background(), phase.OnRequestBody, rx))
	if !slices.Equal(h["Etag"], []string{`"abc"`}) || h.Get("Accept-Ranges") != "bytes" {
		t.Errorf("header %v of a request", h)
	}
}

// TestUpstreamResponse covers transform.response at Upstream scope: it
// acts on the leg's response in onUpstreamResponseBody, with upstream
// declared, under the leg's MaxBodyBytes when set, and leaves the body of
// a response to a HEAD attempt.
func TestUpstreamResponse(t *testing.T) {
	cfg := &v1alpha1.TransformResponseConfig{Set: []v1alpha1.TransformResponseSet{respSet("body", "from", "upstream.name"), respSet("header", "x-s", "string(response.status)")}}
	f := build(t, cfg, phase.ScopeUpstream)
	leg := func(method string, maxBody int64) *filter.Leg {
		return &filter.Leg{Upstream: "orders", Attempt: 1, Request: &http.Request{Method: method}, MaxBodyBytes: maxBody}
	}
	x, m := onResponse(t, http.StatusOK, jsonHeader(), `{"a": 1}`)
	x.LegV = leg(http.MethodGet, 0)
	x.VarsV.Upstream = &expr.Upstream{Name: "orders", Endpoint: "10.0.0.7:8080"}
	wantNext(t, "onResponse", f.Handle(context.Background(), phase.OnResponse, x))
	wantNext(t, "onUpstreamResponseBody", f.Handle(context.Background(), phase.OnUpstreamResponseBody, x))
	if string(m.Raw) != `{"a":1,"from":"orders"}` || m.Hdr.Get("X-S") != "200" {
		t.Errorf("body %s, header %v", m.Raw, m.Hdr)
	}
	x, m = onResponse(t, http.StatusOK, jsonHeader(), `{"a": 1}`)
	x.LegV = leg(http.MethodGet, 10)
	x.VarsV.Upstream = &expr.Upstream{Name: "orders"}
	wantUndecided(t, "step cap", f.Handle(context.Background(), phase.OnUpstreamResponseBody, x), errTypeOutputCap)
	x, m = onResponse(t, http.StatusOK, jsonHeader(), `{"a": 1}`)
	x.LegV = leg(http.MethodHead, 0)
	x.VarsV.Upstream = &expr.Upstream{Name: "orders"}
	wantNext(t, "HEAD attempt", f.Handle(context.Background(), phase.OnUpstreamResponseBody, x))
	if string(m.Raw) != `{"a": 1}` || m.Hdr.Get("X-S") != "200" {
		t.Errorf("body %s, header %v", m.Raw, m.Hdr)
	}
}
