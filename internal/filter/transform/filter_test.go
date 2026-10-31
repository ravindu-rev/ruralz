// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package transform

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestHeaderEdits covers header entries: names compare ignoring case and
// are written canonical, the lines of differently cased keys fold into the
// canonical key first, set leaves one line and remove none, a computed
// value is trimmed and an empty one sets an empty field, and the entries
// apply in list order, set before remove.
func TestHeaderEdits(t *testing.T) {
	cfg := &v1alpha1.TransformRequestConfig{
		Set: []v1alpha1.TransformRequestSet{
			reqSet("header", "x-a", `request.headers["x-src"]`),
			reqSet("header", "X-B", `"  spaced \t"`),
			reqSet("header", "x-empty", `" "`),
			reqSet("header", "x-twice", `"1"`),
			reqSet("header", "X-TWICE", `"2"`),
			reqSet("header", "x-del", `"v"`),
		},
		Remove: []v1alpha1.TransformRequestRemove{reqRemove("header", "x-gone"), reqRemove("header", "X-Del")},
	}
	f := build(t, cfg, phase.ScopeRoute)
	h := http.Header{"X-Src": {"abc"}, "X-A": {"old"}, "x-a": {"older"}, "X-Gone": {"1"}, "x-gone": {"2"}, "X-Keep": {"k"}}
	x, m := onRequest(t, h, "", "")
	m.Unavailable = true // the body streams: header entries do not read it
	wantNext(t, "Handle", f.Handle(context.Background(), phase.OnRequestBody, x))
	want := http.Header{
		"X-Src": {"abc"}, "X-A": {"abc"}, "X-B": {"spaced"}, "X-Empty": {""}, "X-Twice": {"2"}, "X-Keep": {"k"},
	}
	if len(h) != len(want) {
		t.Errorf("header = %v, want %v", h, want)
	}
	for k, v := range want {
		if !slices.Equal(h[k], v) {
			t.Errorf("header %s = %q, want %q (header %v)", k, h[k], v, h)
		}
	}
	if m.bodyReads != 0 || m.setBodies != 0 {
		t.Errorf("body reads %d, writes %d; want none", m.bodyReads, m.setBodies)
	}
}

// TestHeaderValueInvalid covers a computed header value with a control
// byte other than a tab, or over 8 KiB after trimming: cannot decide
// (invalid_value), leaving every header as it was.
func TestHeaderValueInvalid(t *testing.T) {
	for _, src := range []string{`"a\nb"`, `"a\x7fb"`, `request.headers["x-long"]`} {
		cfg := &v1alpha1.TransformRequestConfig{
			Remove: []v1alpha1.TransformRequestRemove{reqRemove("header", "x-gone")},
			Set:    []v1alpha1.TransformRequestSet{reqSet("header", "x-a", src)},
		}
		f := build(t, cfg, phase.ScopeRoute)
		h := http.Header{"X-Long": {strings.Repeat("a", 8<<10+1)}, "X-Gone": {"1"}}
		x, _ := onRequest(t, h, "", "")
		wantUndecided(t, src, f.Handle(context.Background(), phase.OnRequestBody, x), errTypeInvalidValue)
		if _, ok := h["X-A"]; ok || h.Get("X-Gone") != "1" {
			t.Errorf("%s: header edited after a failure: %v", src, h)
		}
	}
	tabs := build(t, &v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{reqSet("header", "x-a", `"a\tb"`)}}, phase.ScopeRoute)
	h := http.Header{}
	x, _ := onRequest(t, h, "", "")
	wantNext(t, "tab", tabs.Handle(context.Background(), phase.OnRequestBody, x))
	if got := h.Get("X-A"); got != "a\tb" {
		t.Errorf("X-A = %q, want a tab kept", got)
	}
}

// TestQueryEdits covers query entries: names compare after decoding, set
// replaces every occurrence with one parameter at the first one's place
// or appends it, remove drops every occurrence, written names and values
// are percent-encoded per RFC 3986, untouched parameters keep their bytes,
// and the query is written only when it changed.
func TestQueryEdits(t *testing.T) {
	cfg := &v1alpha1.TransformRequestConfig{
		Set: []v1alpha1.TransformRequestSet{
			reqSet("query", "a", `"x y"`),
			reqSet("query", "a+b", `"z"`),
			reqSet("query", "new", `"é&="`),
		},
		Remove: []v1alpha1.TransformRequestRemove{reqRemove("query", "c.d"), reqRemove("query", "missing")},
	}
	f := build(t, cfg, phase.ScopeRoute)
	// a%2Bb decodes to the name a+b; a+b decodes to "a b", another name.
	x, m := onRequest(t, nil, "", "a=1&b=2&a=3&a%2Bb=4&a+b=5&c%2Ed=6&keep=%7e&e=7")
	wantNext(t, "Handle", f.Handle(context.Background(), phase.OnRequestBody, x))
	if want := "a=x%20y&b=2&a%2Bb=z&a+b=5&keep=%7e&e=7&new=%C3%A9%26%3D"; m.Query != want || m.setQueries != 1 {
		t.Errorf("query = %q after %d writes, want %q once", m.Query, m.setQueries, want)
	}
	unchanged := build(t, &v1alpha1.TransformRequestConfig{Remove: []v1alpha1.TransformRequestRemove{reqRemove("query", "zzz")}}, phase.ScopeRoute)
	x, m = onRequest(t, nil, "", "a=1;b&c")
	wantNext(t, "unchanged", unchanged.Handle(context.Background(), phase.OnRequestBody, x))
	if m.Query != "a=1;b&c" || m.setQueries != 0 {
		t.Errorf("query = %q after %d writes, want it untouched", m.Query, m.setQueries)
	}
}

// TestQueryValueCap covers a computed query value over 8 KiB: cannot
// decide (invalid_value); one at the cap is written.
func TestQueryValueCap(t *testing.T) {
	f := build(t, &v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{reqSet("query", "q", `request.headers["x-v"]`)}}, phase.ScopeRoute)
	x, m := onRequest(t, http.Header{"X-V": {strings.Repeat("v", 8<<10+1)}}, "", "q=1")
	wantUndecided(t, "over the cap", f.Handle(context.Background(), phase.OnRequestBody, x), errTypeInvalidValue)
	if m.Query != "q=1" {
		t.Errorf("query = %q after a failure", m.Query)
	}
	x, m = onRequest(t, http.Header{"X-V": {strings.Repeat("v", 8<<10)}}, "", "q=1")
	wantNext(t, "at the cap", f.Handle(context.Background(), phase.OnRequestBody, x))
	if len(m.Query) != len("q=")+8<<10 {
		t.Errorf("query length %d", len(m.Query))
	}
}

// TestPathTarget covers a set entry with target path: the forwarded path
// becomes the result normalized as the Router normalizes a client path,
// and a result that does not start with /, holds ? or #, has a dot
// segment, also written with %2E, fails the hardening of the client path
// or is over 8 KiB, computed or forwarded, cannot decide (invalid_value),
// leaving the path as it was.
func TestPathTarget(t *testing.T) {
	ok := []struct{ result, want string }{
		{"/v2/orders", "/v2/orders"},
		{"/a/%7euser/b%2fc", "/a/~user/b%2Fc"},
		{"/café/x y", "/caf%C3%A9/x%20y"},
		{"//double//slash/", "//double//slash/"},
		{"/a/.../b", "/a/.../b"},
		{"/" + strings.Repeat("a", MaxPathBytes-1), "/" + strings.Repeat("a", MaxPathBytes-1)},
	}
	for _, tc := range ok {
		f := build(t, &v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{reqSet("path", "", strconv.Quote(tc.result))}}, phase.ScopeRoute)
		x, m := onRequest(t, nil, "", "")
		wantNext(t, tc.result, f.Handle(context.Background(), phase.OnRequestBody, x))
		if m.PathV != tc.want || m.setPaths != 1 {
			t.Errorf("path %q: forwarded %q after %d writes, want %q", tc.result, m.PathV, m.setPaths, tc.want)
		}
	}
	bad := []string{
		"", "v2", "/a?b", "/a#b", "/a/./b", "/a/../b", "/.", "/a/..", "/a/%2e/b", "/a/%2E%2e/b", "/a/.%2E",
		"/a%00", `/a\b`, "/a%5cb", "/a%5Cb", "/a\x01", "/a%1f", "/a%7F", "/a%zz", "/a%2",
		"/" + strings.Repeat("a", MaxPathBytes),
		"/" + strings.Repeat("é", 2000),
	}
	for _, result := range bad {
		f := build(t, &v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{reqSet("path", "", strconv.Quote(result))}}, phase.ScopeRoute)
		x, m := onRequest(t, nil, "", "")
		wantUndecided(t, strconv.Quote(result), f.Handle(context.Background(), phase.OnRequestBody, x), errTypeInvalidValue)
		if m.PathV != "/v1/orders/42" || m.setPaths != 0 {
			t.Errorf("path %q: forwarded %q after a failure", result, m.PathV)
		}
	}
}

// TestCheckPath covers the path rule directly: an accepted path is
// normalized, idempotently, and every dot segment spelling is refused as a
// composition step's path is.
func TestCheckPath(t *testing.T) {
	for _, p := range []string{"/", "/a", "/a/", "/%41%2f", "/a b", "/%2E%2E%2Ex", "/a./.b/..."} {
		got, err := checkPath(p)
		if err != nil {
			t.Errorf("checkPath(%q) = %v", p, err)
			continue
		}
		again, err := checkPath(got)
		if err != nil || again != got {
			t.Errorf("checkPath(%q) = %q, then %q, %v", p, got, again, err)
		}
	}
	for _, seg := range []string{".", "..", "%2e", "%2E", ".%2e", "%2E.", "%2e%2E"} {
		if _, err := checkPath("/a/" + seg + "/b"); !errors.Is(err, routematch.ErrRejected) {
			t.Errorf("checkPath with segment %q = %v, want a rejection", seg, err)
		}
	}
}

// TestStreamingTransform covers a transform without a body need: it never
// reads the body, which streams on, and edits headers, query and path.
func TestStreamingTransform(t *testing.T) {
	cfg := &v1alpha1.TransformRequestConfig{
		Set: []v1alpha1.TransformRequestSet{
			reqSet("header", "x-route", "route.name"), reqSet("query", "v", `"2"`), reqSet("path", "", `"/v2" + request.path`),
		},
	}
	f := build(t, cfg, phase.ScopeRoute)
	if f.needsBody {
		t.Fatal("needsBody without a body entry")
	}
	x, m := onRequest(t, nil, "", "")
	m.Unavailable = true
	wantNext(t, "Handle", f.Handle(context.Background(), phase.OnRequestBody, x))
	if m.bodyReads != 0 || m.Hdr.Get("X-Route") != "orders" || m.Query != "v=2" || m.PathV != "/v2/v1/orders/42" {
		t.Errorf("reads %d, header %v, query %q, path %q", m.bodyReads, m.Hdr, m.Query, m.PathV)
	}
}

// TestOtherPhases covers the Phases a Filter does not act in: it
// continues without touching the message.
func TestOtherPhases(t *testing.T) {
	f := build(t, &v1alpha1.TransformRequestConfig{Remove: []v1alpha1.TransformRequestRemove{reqRemove("header", "x-a")}}, phase.ScopeRoute)
	for _, p := range []phase.Phase{phase.OnRequestHeaders, phase.OnRoute, phase.OnUpstreamRequest, phase.OnResponse, phase.OnLog} {
		h := http.Header{"X-A": {"1"}}
		x, _ := onRequest(t, h, "", "")
		wantNext(t, p.String(), f.Handle(context.Background(), p, x))
		if h.Get("X-A") != "1" {
			t.Errorf("%v: header edited", p)
		}
	}
}

// nilHeader is a message whose header map is nil.
type nilHeader struct{ filtertest.Message }

// Header returns nil.
func (*nilHeader) Header() http.Header { return nil }

// TestNoMessage covers a Phase without a message or header map: cannot
// decide.
func TestNoMessage(t *testing.T) {
	f := build(t, &v1alpha1.TransformRequestConfig{Remove: []v1alpha1.TransformRequestRemove{reqRemove("header", "x-a")}}, phase.ScopeRoute)
	for _, x := range []*filtertest.Exchange{{}, {Msg: &nilHeader{}}} {
		res := f.Handle(context.Background(), phase.OnRequestBody, x)
		if res.Outcome != filter.CannotDecide || !errors.Is(res.Err, errNoMessage) {
			t.Errorf("Handle = %+v, want CannotDecide with errNoMessage", res)
		}
	}
}

// TestEvalErrors covers CEL runtime errors: cel_error, and the SPI
// sentinel when the body an expression read could not be decoded for lack
// of buffer budget or past its decoded limit.
func TestEvalErrors(t *testing.T) {
	cfg := &v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{reqSet("header", "x-a", "string(request.body.id)")}}
	f := build(t, cfg, phase.ScopeRoute)
	x, _ := onRequest(t, jsonHeader(), `{"other": 1}`, "")
	wantUndecided(t, "missing key", f.Handle(context.Background(), phase.OnRequestBody, x), errTypeCEL)
	for _, sentinel := range []error{filter.ErrBudget, filter.ErrTooLarge} {
		x, m := onRequest(t, jsonHeader(), `{"id": 1}`, "")
		x.VarsV.Request.Body = nil // the decoded view failed
		m.decodedErr = sentinel
		res := f.Handle(context.Background(), phase.OnRequestBody, x)
		if res.Outcome != filter.CannotDecide || !errors.Is(res.Err, sentinel) {
			t.Errorf("Handle = %+v, want CannotDecide with %v", res, sentinel)
		}
	}
	x, _ = onRequest(t, jsonHeader(), `{"id": 1}`, "")
	x.VarsV.Request.Body = nil
	wantUndecided(t, "body unavailable", f.Handle(context.Background(), phase.OnRequestBody, x), errTypeCEL)
}

// TestBodyReadErrors covers a body the Filter needs but cannot read: the
// SPI sentinels pass through, and a body that is not available cannot
// decide.
func TestBodyReadErrors(t *testing.T) {
	f := build(t, &v1alpha1.TransformRequestConfig{Remove: []v1alpha1.TransformRequestRemove{reqRemove("body", "a"), reqRemove("header", "x-a")}}, phase.ScopeRoute)
	for _, err := range []error{filter.ErrBudget, filter.ErrTooLarge, filter.ErrNotAvailable} {
		h := http.Header{"X-A": {"1"}}
		x, m := onRequest(t, h, `{"a": 1}`, "")
		m.bodyErr = err
		res := f.Handle(context.Background(), phase.OnRequestBody, x)
		if res.Outcome != filter.CannotDecide || !errors.Is(res.Err, err) || h.Get("X-A") != "1" {
			t.Errorf("Body error %v: Handle = %+v, header %v", err, res, h)
		}
	}
}

// TestSetBodyBudget covers a spent buffer budget when the rewritten body
// is reserved: the SPI sentinel, and no other edit.
func TestSetBodyBudget(t *testing.T) {
	cfg := &v1alpha1.TransformRequestConfig{
		Set:    []v1alpha1.TransformRequestSet{reqSet("header", "x-a", `"1"`), reqSet("query", "q", `"2"`), reqSet("path", "", `"/p"`)},
		Remove: []v1alpha1.TransformRequestRemove{reqRemove("body", "a")},
	}
	f := build(t, cfg, phase.ScopeRoute)
	x, m := onRequest(t, jsonHeader(), `{"a": 1}`, "q=1")
	m.setBodyErr = filter.ErrBudget
	res := f.Handle(context.Background(), phase.OnRequestBody, x)
	if res.Outcome != filter.CannotDecide || !errors.Is(res.Err, filter.ErrBudget) {
		t.Fatalf("Handle = %+v, want CannotDecide with ErrBudget", res)
	}
	if m.Hdr.Get("X-A") != "" || m.Query != "q=1" || m.PathV != "/v1/orders/42" || string(m.Raw) != `{"a": 1}` {
		t.Errorf("edited after a refused reservation: header %v, query %q, path %q, body %s", m.Hdr, m.Query, m.PathV, m.Raw)
	}
}

// TestAtomicity covers a failure after earlier entries succeeded: the
// message keeps its headers, query, path and body.
func TestAtomicity(t *testing.T) {
	cfg := &v1alpha1.TransformRequestConfig{
		Set: []v1alpha1.TransformRequestSet{
			reqSet("header", "x-a", `"1"`), reqSet("query", "q", `"2"`), reqSet("path", "", `"/p"`), reqSet("body", "added", "true"),
		},
		Remove:  []v1alpha1.TransformRequestRemove{reqRemove("body", "a"), reqRemove("header", "x-keep")},
		Replace: []v1alpha1.Replace{pattern("num", "1", "2")},
	}
	f := build(t, cfg, phase.ScopeRoute)
	h := jsonHeader()
	h.Set("X-Keep", "k")
	x, m := onRequest(t, h, `{"a": 1, "num": 1}`, "q=1")
	wantUndecided(t, "Handle", f.Handle(context.Background(), phase.OnRequestBody, x), errTypePath)
	if h.Get("X-A") != "" || h.Get("X-Keep") != "k" || m.Query != "q=1" || m.PathV != "/v1/orders/42" ||
		string(m.Raw) != `{"a": 1, "num": 1}` || m.setBodies+m.setQueries+m.setPaths != 0 {
		t.Errorf("edited after a failure: header %v, query %q, path %q, body %s", h, m.Query, m.PathV, m.Raw)
	}
}

// TestErrorsHoldNoRequestData covers the text of a failure: it names the
// Policy type, the entry and the rule, never the body or a value.
func TestErrorsHoldNoRequestData(t *testing.T) {
	const secret = "s3cr3t-value"
	cases := []struct {
		cfg  *v1alpha1.TransformRequestConfig
		body string
		kind string
	}{
		{&v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{reqSet("header", "x-a", `request.body.v + "\n"`)}}, `{"v": "` + secret + `"}`, errTypeInvalidValue},
		{&v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{reqSet("body", "v.x", "1")}}, `{"v": "` + secret + `"}`, errTypePath},
		{&v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{reqSet("header", "x-a", `string(request.body.v + 1)`)}}, `{"v": "` + secret + `"}`, errTypeCEL},
		{&v1alpha1.TransformRequestConfig{Remove: []v1alpha1.TransformRequestRemove{reqRemove("body", "v")}}, `{"v": "` + secret + `"`, errTypeNullBody},
	}
	for _, tc := range cases {
		f := build(t, tc.cfg, phase.ScopeRoute)
		x, _ := onRequest(t, jsonHeader(), tc.body, "")
		res := f.Handle(context.Background(), phase.OnRequestBody, x)
		wantUndecided(t, tc.kind, res, tc.kind)
		if text := res.Err.Error(); strings.Contains(text, secret) || !strings.HasPrefix(text, "transform.request: config.") {
			t.Errorf("%s: error text %q", tc.kind, text)
		}
	}
}

// TestConcurrentHandle covers one Filter serving many requests at once:
// each call has its own pooled scratch.
func TestConcurrentHandle(t *testing.T) {
	cfg := &v1alpha1.TransformRequestConfig{
		Set:      []v1alpha1.TransformRequestSet{reqSet("header", "x-n", "string(request.body.n)"), reqSet("body", "copy", "request.body.n")},
		ArrayOps: []v1alpha1.ArrayOp{arrayOp("move", "items.*.sku", "skus")},
	}
	f := build(t, cfg, phase.ScopeRoute)
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 50 {
				n := strconv.Itoa(i*100 + j)
				x, m := onRequest(t, jsonHeader(), `{"n": `+n+`, "items": [{"sku": "a"}, {"sku": "b"}]}`, "")
				if res := f.Handle(context.Background(), phase.OnRequestBody, x); res.Outcome != filter.Continue {
					errs <- "Handle: " + res.Err.Error()
					return
				}
				want := `{"n":` + n + `,"items":[{},{}],"skus":["a","b"],"copy":` + n + `}`
				if string(m.Raw) != want || m.Hdr.Get("X-N") != n {
					errs <- "body " + string(m.Raw) + ", want " + want
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestValueResultsNest covers a set entry with target body: its result is
// written as a nested JSON value, a string as a JSON string and bytes as
// base64, maps with sorted keys, the numbers of a map or list decoded from
// the body verbatim, and a number selected on its own in CEL's form.
func TestValueResultsNest(t *testing.T) {
	cfg := &v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{
		reqSet("body", "s", `"a\"b"`),
		reqSet("body", "b", `b"hi"`),
		reqSet("body", "m", `{"z": 1, "a": [1.5, null, true]}`),
		reqSet("body", "n", "request.body.n"),
		reqSet("body", "o", "request.body.o"),
		reqSet("body", "t", `timestamp("2026-10-29T09:00:00Z")`),
	}}
	f := build(t, cfg, phase.ScopeRoute)
	x, m := onRequest(t, jsonHeader(), `{"n": 1.50, "o": {"y": 1e3, "x": [0.10]}}`, "")
	wantNext(t, "Handle", f.Handle(context.Background(), phase.OnRequestBody, x))
	want := `{"n":1.5,"o":{"x":[0.10],"y":1e3},"s":"a\"b","b":"aGk=","m":{"a":[1.5,null,true],"z":1},"t":"2026-10-29T09:00:00Z"}`
	if string(m.Raw) != want {
		t.Errorf("body %s, want %s", m.Raw, want)
	}
	nan := build(t, &v1alpha1.TransformRequestConfig{Set: []v1alpha1.TransformRequestSet{reqSet("body", "x", `double("NaN")`)}}, phase.ScopeRoute)
	x, _ = onRequest(t, jsonHeader(), `{}`, "")
	wantUndecided(t, "NaN", nan.Handle(context.Background(), phase.OnRequestBody, x), errTypeResultType)
}

// TestDecodedBodyView covers what the CEL fields read: request.body as it
// reached the Policy, never an earlier entry's output.
func TestDecodedBodyView(t *testing.T) {
	cfg := &v1alpha1.TransformRequestConfig{
		Body:     `{"a": request.body.a, "k": request.body.k, "n": 1}`,
		ArrayOps: []v1alpha1.ArrayOp{arrayOp("move", "a", "moved")},
		Set:      []v1alpha1.TransformRequestSet{reqSet("body", "copy", "request.body.a"), reqSet("header", "x-k", "request.body.k")},
		Remove:   []v1alpha1.TransformRequestRemove{reqRemove("body", "n")},
		Replace:  []v1alpha1.Replace{pattern("moved", "v", "V")},
	}
	f := build(t, cfg, phase.ScopeRoute)
	x, m := onRequest(t, jsonHeader(), `{"a": "v", "k": "w"}`, "")
	wantNext(t, "Handle", f.Handle(context.Background(), phase.OnRequestBody, x))
	if want := `{"k":"w","moved":"V","copy":"v"}`; string(m.Raw) != want {
		t.Errorf("body %s, want %s", m.Raw, want)
	}
	if m.Hdr.Get("X-K") != "w" || m.Hdr.Get("Content-Type") != "application/json" || m.Hdr.Get("Content-Length") != strconv.Itoa(len(m.Raw)) {
		t.Errorf("header %v", m.Hdr)
	}
}

// TestUpstreamRequest covers transform.request at Upstream scope: it acts
// on the attempt's request in onUpstreamRequest, with upstream declared.
func TestUpstreamRequest(t *testing.T) {
	cfg := &v1alpha1.TransformRequestConfig{
		Set: []v1alpha1.TransformRequestSet{reqSet("header", "x-up", "upstream.name"), reqSet("body", "to", "upstream.endpoint")},
	}
	f := build(t, cfg, phase.ScopeUpstream)
	x, m := onRequest(t, jsonHeader(), `{"a": 1}`, "")
	x.LegV = &filter.Leg{Upstream: "orders", Attempt: 2}
	x.VarsV.Upstream = &expr.Upstream{Name: "orders", Endpoint: "10.0.0.7:8080"}
	wantNext(t, "onRequestBody", f.Handle(context.Background(), phase.OnRequestBody, x))
	if m.setBodies != 0 {
		t.Fatal("acted outside onUpstreamRequest")
	}
	wantNext(t, "onUpstreamRequest", f.Handle(context.Background(), phase.OnUpstreamRequest, x))
	if string(m.Raw) != `{"a":1,"to":"10.0.0.7:8080"}` || m.Hdr.Get("X-Up") != "orders" {
		t.Errorf("body %s, header %v", m.Raw, m.Hdr)
	}
}
