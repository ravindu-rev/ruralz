// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package transform

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestBodyField covers config.body: a top-level string written as its
// UTF-8 bytes, bytes as they are and any other value as JSON with sorted
// keys and decoded numbers verbatim, Content-Type set to contentType; a
// string or bytes result under a JSON contentType must be one JSON value,
// a result without a JSON form is result_type, and a body over the output
// cap output_cap.
func TestBodyField(t *testing.T) {
	deep := strings.Repeat(`{"a": `, 70) + "1" + strings.Repeat("}", 70)
	tests := []struct {
		name, body, contentType string
		in                      string
		want, wantType          string
		kind                    string
	}{
		{name: "SOAP envelope", body: `"<soap:Envelope>" + request.body.id + "</soap:Envelope>"`, contentType: "text/xml",
			in: `{"id": "7"}`, want: "<soap:Envelope>7</soap:Envelope>", wantType: "text/xml"},
		{name: "bytes", body: `b"raw\x00\xff"`, contentType: "application/octet-stream", in: `{}`, want: "raw\x00\xff", wantType: "application/octet-stream"},
		{name: "map", body: `{"b": 1, "a": [1.5, "x<&>", null, true]}`, in: `{}`,
			want: `{"a":[1.5,"x<&>",null,true],"b":1}`, wantType: "application/json"},
		{name: "decoded body", body: "request.body", in: `{"z": 1.0e2, "a": 12345678901234567890, "m": {"y": 0.50}}`,
			want: `{"a":12345678901234567890,"m":{"y":0.50},"z":1.0e2}`, wantType: "application/json"},
		{name: "JSON string", body: `"{\"a\": 1}"`, in: `{}`, want: `{"a": 1}`, wantType: "application/json"},
		{name: "JSON string under a +json type", body: `"[1]"`, contentType: "application/vnd.api+json", in: `{}`, want: `[1]`, wantType: "application/vnd.api+json"},
		{name: "deep map", body: deep, in: `{}`, want: strings.Repeat(`{"a":`, 70) + "1" + strings.Repeat("}", 70), wantType: "application/json"},
		{name: "empty string", body: `""`, contentType: "text/plain", in: `{"a": 1}`, want: "", wantType: "text/plain"},
		{name: "string not JSON", body: `"not json"`, in: `{}`, kind: errTypeInvalidValue},
		{name: "string with duplicate names", body: `"{\"a\": 1, \"a\": 2}"`, in: `{}`, kind: errTypeInvalidValue},
		{name: "bytes not JSON", body: `b"\x01"`, contentType: "application/problem+json", in: `{}`, kind: errTypeInvalidValue},
		{name: "NaN", body: `{"x": double("NaN")}`, in: `{}`, kind: errTypeResultType},
		{name: "duplicate keys", body: `{1: "a", "1": "b"}`, in: `{}`, kind: errTypeResultType},
		{name: "reads a null body", body: `{"id": request.body.id}`, in: "id=7", kind: errTypeCEL},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &v1alpha1.TransformRequestConfig{Body: tc.body}
			if tc.contentType != "" {
				cfg.ContentType = ptr(tc.contentType)
			}
			f := build(t, cfg, phase.ScopeRoute)
			h := jsonHeader()
			if !strings.HasPrefix(tc.in, "{") {
				h = http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}
			}
			h.Set("Content-Encoding", "identity")
			h.Set("Digest", "sha-256=x")
			x, m := onRequest(t, h, tc.in, "")
			res := f.Handle(context.Background(), phase.OnRequestBody, x)
			if tc.kind != "" {
				wantUndecided(t, tc.name, res, tc.kind)
				if string(m.Raw) != tc.in || m.setBodies != 0 {
					t.Errorf("body %q after a failure", m.Raw)
				}
				return
			}
			wantNext(t, tc.name, res)
			if string(m.Raw) != tc.want || m.Hdr.Get("Content-Type") != tc.wantType {
				t.Errorf("body %q, Content-Type %q; want %q, %q", m.Raw, m.Hdr.Get("Content-Type"), tc.want, tc.wantType)
			}
			if m.Hdr.Get("Content-Length") != strconv.Itoa(len(tc.want)) || m.Hdr.Get("Content-Encoding") != "" || m.Hdr.Get("Digest") != "" {
				t.Errorf("header %v after a rewrite", m.Hdr)
			}
		})
	}
}

// bodyCase is one body operation over a JSON request body.
type bodyCase struct {
	name string
	cfg  *v1alpha1.TransformRequestConfig
	in   string
	// want is the body after the call; "" with kind set for a failure.
	want, kind string
}

// runBodyCases runs cases at Route scope.
func runBodyCases(t *testing.T, cases []bodyCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := build(t, tc.cfg, phase.ScopeRoute)
			x, m := onRequest(t, jsonHeader(), tc.in, "")
			res := f.Handle(context.Background(), phase.OnRequestBody, x)
			if tc.kind != "" {
				wantUndecided(t, tc.name, res, tc.kind)
				if string(m.Raw) != tc.in || m.setBodies != 0 {
					t.Errorf("body %s after a failure", m.Raw)
				}
				return
			}
			wantNext(t, tc.name, res)
			if string(m.Raw) != tc.want {
				t.Errorf("body\n%s\nwant\n%s", m.Raw, tc.want)
			}
		})
	}
}

// sets returns a config of body set entries name=src.
func sets(pairs ...string) *v1alpha1.TransformRequestConfig {
	c := &v1alpha1.TransformRequestConfig{}
	for i := 0; i+1 < len(pairs); i += 2 {
		c.Set = append(c.Set, reqSet("body", pairs[i], pairs[i+1]))
	}
	return c
}

// removes returns a config of body remove entries.
func removes(names ...string) *v1alpha1.TransformRequestConfig {
	c := &v1alpha1.TransformRequestConfig{}
	for _, n := range names {
		c.Remove = append(c.Remove, reqRemove("body", n))
	}
	return c
}

// ops returns a config of arrayOps entries.
func ops(a ...v1alpha1.ArrayOp) *v1alpha1.TransformRequestConfig {
	return &v1alpha1.TransformRequestConfig{ArrayOps: a}
}

// replaces returns a config of replace entries.
func replaces(r ...v1alpha1.Replace) *v1alpha1.TransformRequestConfig {
	return &v1alpha1.TransformRequestConfig{Replace: r}
}

// TestBodySet covers set entries with target body: the value is written
// at every match, replacing what it held, missing objects are created
// with a new member at the end of its object, nothing below a * on a
// missing array, and an index past an array's end or a path through a
// scalar is path_error.
func TestBodySet(t *testing.T) {
	runBodyCases(t, []bodyCase{
		{"new member at the end", sets("customer.tier", `"gold"`), `{"customer": {"name": "a"}, "z": 1}`, `{"customer":{"name":"a","tier":"gold"},"z":1}`, ""},
		{"missing objects", sets("a.b.c", "1"), `{"z": 1}`, `{"z":1,"a":{"b":{"c":1}}}`, ""},
		{"replaces in place", sets("customer", `{"id": 7, "tags": ["x"]}`), `{"customer": "a", "z": 1}`, `{"customer":{"id":7,"tags":["x"]},"z":1}`, ""},
		{"every match of *", sets("items.*.price", "0"), `{"items": [{"p": 1}, {"p": 2}]}`, `{"items":[{"p":1,"price":0},{"p":2,"price":0}]}`, ""},
		{"* over a missing array", sets("items.*.price", "0"), `{"z": 1}`, `{"z": 1}`, ""},
		{"array index", sets("items.1", `"b2"`), `{"items": ["a", "b"]}`, `{"items":["a","b2"]}`, ""},
		{"escaped segment", sets(`a\.b`, "1"), `{}`, `{"a.b":1}`, ""},
		{"numeric name on an object", sets("codes.200", `"ok"`), `{"codes": {}}`, `{"codes":{"200":"ok"}}`, ""},
		{"index past the end", sets("items.5", "1"), `{"items": [1]}`, "", errTypePath},
		{"through a scalar", sets("z.a", "1"), `{"z": 1}`, "", errTypePath},
		{"name on an array", sets("items.x", "1"), `{"items": []}`, "", errTypePath},
		{"keeps member order and number text", sets("y", "1"), `{ "n" : 1.50 ,
 "m" : 1e3, "h": "<a&b>" }`, `{"n":1.50,"m":1e3,"h":"<a&b>","y":1}`, ""},
		{"minimal escapes", sets("x", "1"), `{"u": "` + "\\" + `u00e9` + "\\" + `/"}`, `{"u":"é/","x":1}`, ""},
	})
}

// TestBodyRemove covers remove entries with target body and delete: what
// the path matches is dropped, array elements letting later ones move up,
// and a missing path removes nothing.
func TestBodyRemove(t *testing.T) {
	runBodyCases(t, []bodyCase{
		{"member", removes("internal"), `{"a": 1, "internal": {"x": 1}, "b": 2}`, `{"a":1,"b":2}`, ""},
		{"every match of *", removes("items.*.secret"), `{"items": [{"secret": 1, "k": 1}, {"k": 2}]}`, `{"items":[{"k":1},{"k":2}]}`, ""},
		{"several", removes("a", "b.c"), `{"a": 1, "b": {"c": 2, "d": 3}}`, `{"b":{"d":3}}`, ""},
		{"through a scalar", removes("a.b"), `{"a": 1}`, "", errTypePath},
		{"* on an object", removes("a.*"), `{"a": {"b": 1}}`, "", errTypePath},
		{"delete an element", ops(arrayOp("delete", "items.0", "")), `{"items": ["a", "b", "c"]}`, `{"items":["b","c"]}`, ""},
		{"delete every element", ops(arrayOp("delete", "items.*", "")), `{"items": ["a", "b"], "z": 1}`, `{"items":[],"z":1}`, ""},
		{"delete a member", ops(arrayOp("delete", "customer.email", "")), `{"customer": {"email": "a", "id": 1}}`, `{"customer":{"id":1}}`, ""},
		{"delete past the end", ops(arrayOp("delete", "items.9", "")), `{"items": [1]}`, `{"items": [1]}`, ""},
	})
}

// TestBodyMoveAppend covers move and append: move takes the value at
// from, or the array of a * path's matches, out of the body and writes it
// at to, replacing what to held; append adds the value or each match to
// the end of the array at to, creating it and keeping from; a from that
// matches nothing moves or appends nothing; a to that is not an array is
// path_error for append.
func TestBodyMoveAppend(t *testing.T) {
	runBodyCases(t, []bodyCase{
		{"move flattens", ops(arrayOp("move", "items.*.sku", "skus")), `{"items": [{"sku": "A", "q": 1}, {"sku": "B"}, {"q": 2}]}`,
			`{"items":[{"q":1},{},{"q":2}],"skus":["A","B"]}`, ""},
		{"move a value", ops(arrayOp("move", "a", "b.c")), `{"a": {"x": 1}, "z": 1}`, `{"z":1,"b":{"c":{"x":1}}}`, ""},
		{"move replaces to", ops(arrayOp("move", "a", "b")), `{"a": [1], "b": "old"}`, `{"b":[1]}`, ""},
		{"move a missing value", ops(arrayOp("move", "missing", "b")), `{"a": 1}`, `{"a": 1}`, ""},
		{"move over an empty array", ops(arrayOp("move", "items.*.sku", "skus")), `{"items": []}`, `{"items": []}`, ""},
		{"move into a scalar", ops(arrayOp("move", "a", "z.b")), `{"a": 1, "z": 1}`, "", errTypePath},
		{"append matches", ops(arrayOp("append", "items.*.sku", "all")), `{"items": [{"sku": "A"}, {"sku": "B"}], "all": ["Z"]}`,
			`{"items":[{"sku":"A"},{"sku":"B"}],"all":["Z","A","B"]}`, ""},
		{"append creates the array", ops(arrayOp("append", "x", "out.list")), `{"x": [1, 2]}`, `{"x":[1,2],"out":{"list":[[1,2]]}}`, ""},
		{"append a missing value", ops(arrayOp("append", "missing", "list")), `{"list": []}`, `{"list": []}`, ""},
		{"append to an object", ops(arrayOp("append", "x", "o")), `{"x": 1, "o": {}}`, "", errTypePath},
		{"in order", ops(arrayOp("move", "a", "b"), arrayOp("append", "b", "list"), arrayOp("delete", "list.0", "")), `{"a": 1, "list": [0]}`,
			`{"list":[1],"b":1}`, ""},
	})
}

// TestBodyReplace covers replace entries: with path, the strings it
// matches, by an RE2 pattern whose replacement expands groups or by
// literal text, a match that is not a string being path_error; without
// path, the raw body.
func TestBodyReplace(t *testing.T) {
	runBodyCases(t, []bodyCase{
		{"pattern with a group", replaces(pattern("customer.email", "^([^@]+)@", "<$1>@")), `{"customer": {"email": "jane@example.com"}}`,
			`{"customer":{"email":"<jane>@example.com"}}`, ""},
		{"named group", replaces(pattern("a", "(?P<x>b+)", "[${x}]")), `{"a": "abbc"}`, `{"a":"a[bb]c"}`, ""},
		{"literal", replaces(literally("a", "$1", "x")), `{"a": "p$1q$1"}`, `{"a":"pxqx"}`, ""},
		{"literal empty pattern", replaces(literally("a", "", "-")), `{"a": "ab"}`, `{"a":"-a-b-"}`, ""},
		{"every match of *", replaces(literally("tags.*", "a", "b")), `{"tags": ["a", "ca"]}`, `{"tags":["b","cb"]}`, ""},
		{"escapes the result", replaces(literally("a", "x", "\"\n")), `{"a": "x"}`, `{"a":"\"\n"}`, ""},
		{"no match", replaces(pattern("a", "z", "y")), `{"a": "x"}`, `{"a": "x"}`, ""},
		{"missing path", replaces(pattern("b", "z", "y")), `{"a": "x"}`, `{"a": "x"}`, ""},
		{"not a string", replaces(pattern("n", "1", "2")), `{"n": 1}`, "", errTypePath},
		{"raw body", replaces(pattern("", `"secret":"[^"]*"`, `"secret":"***"`)), `{"secret":"x","a":1}`, `{"secret":"***","a":1}`, ""},
		{"raw then path", replaces(literally("", "1", "2"), pattern("a", "x", "y")), `{"a": "x", "n": 1}`, `{"a":"y","n":2}`, ""},
		{"raw breaks the JSON", replaces(literally("", "}", ""), pattern("a", "x", "y")), `{"a": "x"}`, "", errTypeNullBody},
	})
}

// TestBodyUnchanged covers operations that change nothing: the body keeps
// its own bytes and is not written, so Content-Length and the validators
// stay.
func TestBodyUnchanged(t *testing.T) {
	cfg := &v1alpha1.TransformRequestConfig{
		Remove:   []v1alpha1.TransformRequestRemove{reqRemove("body", "missing")},
		ArrayOps: []v1alpha1.ArrayOp{arrayOp("move", "nothing.*", "x")},
		Replace:  []v1alpha1.Replace{pattern("a", "zzz", "y"), literally("", "zzz", "y")},
	}
	f := build(t, cfg, phase.ScopeRoute)
	in := "{\n  \"a\": \"x\"\n}"
	h := jsonHeader()
	h.Set("Content-Length", strconv.Itoa(len(in)))
	x, m := onRequest(t, h, in, "")
	wantNext(t, "Handle", f.Handle(context.Background(), phase.OnRequestBody, x))
	if string(m.Raw) != in || m.setBodies != 0 || h.Get("Content-Length") != strconv.Itoa(len(in)) {
		t.Errorf("body %q after %d writes, header %v", m.Raw, m.setBodies, h)
	}
}

// TestNullBody covers a body that is not JSON but not empty: a JSON
// operation cannot decide (null_body), while replace without path, header
// entries and a body that ignores the body apply.
func TestNullBody(t *testing.T) {
	form := http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}
	twice := http.Header{"Content-Type": {"application/json", "application/json"}}
	jsonCT := jsonHeader()
	deep := strings.Repeat("[", 65) + strings.Repeat("]", 65)
	failing := []struct {
		name string
		h    http.Header
		in   string
	}{
		{"another media type", form, `{"a": 1}`},
		{"not valid JSON", jsonCT, `{"a": `},
		{"duplicate names", jsonCT, `{"a": 1, "a": 2}`},
		{"too deep", jsonCT, deep},
		{"two Content-Type lines", twice, `{"a": 1}`},
		{"invalid UTF-8", jsonCT, "{\"a\": \"\xff\"}"},
	}
	for _, tc := range failing {
		for _, cfg := range []*v1alpha1.TransformRequestConfig{
			removes("a"), sets("b", "1"), ops(arrayOp("delete", "a", "")), replaces(pattern("a", "x", "y")),
		} {
			cfg.Set = append(cfg.Set, reqSet("header", "x-a", `"1"`))
			f := build(t, cfg, phase.ScopeRoute)
			h := tc.h.Clone()
			x, m := onRequest(t, h, tc.in, "")
			wantUndecided(t, tc.name, f.Handle(context.Background(), phase.OnRequestBody, x), errTypeNullBody)
			if string(m.Raw) != tc.in || h.Get("X-A") != "" {
				t.Errorf("%s: edited after a failure: body %q, header %v", tc.name, m.Raw, h)
			}
		}
	}
	cfg := &v1alpha1.TransformRequestConfig{
		Set:     []v1alpha1.TransformRequestSet{reqSet("header", "x-a", `"1"`), reqSet("query", "q", `"2"`), reqSet("path", "", `"/p"`)},
		Replace: []v1alpha1.Replace{literally("", "a=1", "a=2")},
	}
	f := build(t, cfg, phase.ScopeRoute)
	x, m := onRequest(t, form.Clone(), "a=1&b=2", "")
	wantNext(t, "replace without path", f.Handle(context.Background(), phase.OnRequestBody, x))
	if string(m.Raw) != "a=2&b=2" || m.Hdr.Get("X-A") != "1" || m.Query != "q=2" || m.PathV != "/p" {
		t.Errorf("body %q, header %v, query %q, path %q", m.Raw, m.Hdr, m.Query, m.PathV)
	}
	wrap := build(t, &v1alpha1.TransformRequestConfig{Body: `{"wrapped": true}`, Remove: []v1alpha1.TransformRequestRemove{reqRemove("body", "x")}}, phase.ScopeRoute)
	x, m = onRequest(t, form.Clone(), "a=1", "")
	wantNext(t, "body ignoring the body", wrap.Handle(context.Background(), phase.OnRequestBody, x))
	if string(m.Raw) != `{"wrapped":true}` || m.Hdr.Get("Content-Type") != "application/json" {
		t.Errorf("body %q, header %v", m.Raw, m.Hdr)
	}
	textBody := build(t, &v1alpha1.TransformRequestConfig{Body: `"<a/>"`, ContentType: ptr("text/xml"), Remove: []v1alpha1.TransformRequestRemove{reqRemove("body", "x")}}, phase.ScopeRoute)
	x, _ = onRequest(t, jsonHeader(), `{"x": 1}`, "")
	wantUndecided(t, "JSON operation on a built text body", textBody.Handle(context.Background(), phase.OnRequestBody, x), errTypeNullBody)
}

// TestEmptyBody covers an empty body: remove, replace with path, move and
// delete do nothing; set starts from an empty object and the body it
// writes takes Content-Type application/json; append, whose from matches
// nothing there, does nothing.
func TestEmptyBody(t *testing.T) {
	text := func() http.Header { return http.Header{"Content-Type": {"text/plain"}} }
	for _, cfg := range []*v1alpha1.TransformRequestConfig{
		removes("a"), replaces(pattern("a", "x", "y")), ops(arrayOp("move", "a", "b")), ops(arrayOp("delete", "a.0", "")),
		ops(arrayOp("append", "a", "b")),
	} {
		f := build(t, cfg, phase.ScopeRoute)
		x, m := onRequest(t, text(), "", "")
		wantNext(t, "no-op", f.Handle(context.Background(), phase.OnRequestBody, x))
		if len(m.Raw) != 0 || m.setBodies != 0 || m.Hdr.Get("Content-Type") != "text/plain" {
			t.Errorf("%+v: body %q, header %v", cfg, m.Raw, m.Hdr)
		}
	}
	f := build(t, sets("a.b", "1", "c", `"d"`), phase.ScopeRoute)
	x, m := onRequest(t, text(), "", "")
	wantNext(t, "set", f.Handle(context.Background(), phase.OnRequestBody, x))
	if string(m.Raw) != `{"a":{"b":1},"c":"d"}` || m.Hdr.Get("Content-Type") != "application/json" {
		t.Errorf("body %q, header %v", m.Raw, m.Hdr)
	}
	empty := build(t, &v1alpha1.TransformRequestConfig{Body: `""`, ContentType: ptr("text/plain"), Set: []v1alpha1.TransformRequestSet{reqSet("body", "x", "1")}}, phase.ScopeRoute)
	x, m = onRequest(t, jsonHeader(), `{"a": 1}`, "")
	wantNext(t, "set after an empty body", empty.Handle(context.Background(), phase.OnRequestBody, x))
	if string(m.Raw) != `{"x":1}` || m.Hdr.Get("Content-Type") != "application/json" {
		t.Errorf("body %q, header %v", m.Raw, m.Hdr)
	}
}

// TestContentTypeOrder covers the Content-Type writes in Policy order:
// config.body first, then the entries in order, so a set entry with target
// header after the body wins, and a body the set entries create after a
// header entry takes application/json.
func TestContentTypeOrder(t *testing.T) {
	tests := []struct {
		name string
		cfg  *v1alpha1.TransformRequestConfig
		in   string
		want string
	}{
		{"header after body", &v1alpha1.TransformRequestConfig{Body: `"<a/>"`, ContentType: ptr("text/xml"),
			Set: []v1alpha1.TransformRequestSet{reqSet("header", "content-type", `"application/xml"`)}}, `{}`, "application/xml"},
		{"created body after header", &v1alpha1.TransformRequestConfig{
			Set: []v1alpha1.TransformRequestSet{reqSet("header", "Content-Type", `"text/plain"`), reqSet("body", "x", "1")}}, "", "application/json"},
		{"header after created body", &v1alpha1.TransformRequestConfig{
			Set: []v1alpha1.TransformRequestSet{reqSet("body", "x", "1"), reqSet("header", "Content-Type", `"text/plain"`)}}, "", "text/plain"},
		{"existing body keeps its type", &v1alpha1.TransformRequestConfig{
			Set: []v1alpha1.TransformRequestSet{reqSet("body", "x", "1")}}, `{}`, "application/problem+json"},
	}
	for _, tc := range tests {
		f := build(t, tc.cfg, phase.ScopeRoute)
		h := http.Header{"Content-Type": {"application/problem+json"}, "content-type": {"text/old"}}
		x, m := onRequest(t, h, tc.in, "")
		wantNext(t, tc.name, f.Handle(context.Background(), phase.OnRequestBody, x))
		if got := m.Hdr["Content-Type"]; len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s: Content-Type %q, want %q (header %v)", tc.name, got, tc.want, m.Hdr)
		}
	}
}

// TestOutputCap covers the output cap: every operation's output and the
// body built or rewritten raw stay within limits.maxRequestBodyBytes, else
// output_cap.
func TestOutputCap(t *testing.T) {
	for _, cfg := range []*v1alpha1.TransformRequestConfig{
		sets("x", `"01234567890123456789"`),
		{Body: `"0123456789012345678901234567890"`, ContentType: ptr("text/plain")},
		replaces(literally("", "a", "aaaaaaaaaaaaaaaaaaaa")),
	} {
		env := buildEnv(t, cfg, phase.ScopeRoute)
		env.Limits.MaxRequestBodyBytes = 30
		f, err := Build(context.Background(), env, RequestSpec(cfg))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		x, m := onRequest(t, jsonHeader(), `{"a": "aaaa"}`, "")
		wantUndecided(t, "over the cap", f.Handle(context.Background(), phase.OnRequestBody, x), errTypeOutputCap)
		if string(m.Raw) != `{"a": "aaaa"}` {
			t.Errorf("body %q after a failure", m.Raw)
		}
	}
	env := buildEnv(t, sets("x", "1"), phase.ScopeRoute)
	env.Limits.MaxRequestBodyBytes = int64(len(`{"a":"aaaa","x":1}`))
	f, err := Build(context.Background(), env, RequestSpec(sets("x", "1")))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	x, m := onRequest(t, jsonHeader(), `{"a": "aaaa"}`, "")
	wantNext(t, "at the cap", f.Handle(context.Background(), phase.OnRequestBody, x))
	if string(m.Raw) != `{"a":"aaaa","x":1}` {
		t.Errorf("body %q", m.Raw)
	}
}
