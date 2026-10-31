// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package transform

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/cel/celtypes"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// testLimit is the body limit of the tests' Node, request and response.
const testLimit = 1 << 20

// reqSet returns a transform.request set entry.
func reqSet(target, name, src string) v1alpha1.TransformRequestSet {
	return v1alpha1.TransformRequestSet{Target: v1alpha1.TransformRequestTarget(target), Name: name, ValueExpression: src}
}

// reqRemove returns a transform.request remove entry.
func reqRemove(target, name string) v1alpha1.TransformRequestRemove {
	return v1alpha1.TransformRequestRemove{Target: v1alpha1.TransformRequestTarget(target), Name: name}
}

// respSet returns a transform.response set entry.
func respSet(target, name, src string) v1alpha1.TransformResponseSet {
	return v1alpha1.TransformResponseSet{Target: v1alpha1.TransformResponseTarget(target), Name: name, ValueExpression: src}
}

// respRemove returns a transform.response remove entry.
func respRemove(target, name string) v1alpha1.TransformResponseRemove {
	return v1alpha1.TransformResponseRemove{Target: v1alpha1.TransformResponseTarget(target), Name: name}
}

// arrayOp returns an arrayOps entry.
func arrayOp(op, from, to string) v1alpha1.ArrayOp {
	return v1alpha1.ArrayOp{Op: v1alpha1.ArrayOpKind(op), From: from, To: to}
}

// pattern returns a replace entry with an RE2 pattern; path "" rewrites
// the raw body.
func pattern(path, re, replacement string) v1alpha1.Replace {
	return v1alpha1.Replace{Path: path, Pattern: re, Replacement: replacement}
}

// literally returns a literal replace entry.
func literally(path, old, replacement string) v1alpha1.Replace {
	t := true
	return v1alpha1.Replace{Path: path, Pattern: old, Replacement: replacement, Literal: &t}
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }

// specOf returns the Spec of a transform.request or transform.response
// config.
func specOf(tb testing.TB, cfg any) Spec {
	tb.Helper()
	switch c := cfg.(type) {
	case *v1alpha1.TransformRequestConfig:
		return RequestSpec(c)
	case *v1alpha1.TransformResponseConfig:
		return ResponseSpec(c)
	default:
		tb.Fatalf("config %T", cfg)
		return Spec{}
	}
}

// buildEnv returns the BuildEnv of the transform Policy transform-test
// with config cfg at scope, compiling through a real CEL builder under the
// default Gateway's caps, with body limits of testLimit.
func buildEnv(tb testing.TB, cfg any, scope phase.Scope) filter.BuildEnv {
	tb.Helper()
	typ := specOf(tb, cfg).Side.PolicyType()
	caps := celtest.Caps([]*expr.Route{{Name: "orders"}}, &expr.Consumer{Name: "acme"})
	caps.AddedHeaderBytes = -1
	return filter.BuildEnv{
		Policy: &v1alpha1.Policy{
			Metadata: v1alpha1.ObjectMeta{Name: "transform-test"},
			Spec:     v1alpha1.PolicySpec{Type: typ},
		},
		Config: cfg,
		Scope:  scope,
		CEL:    celtest.Compiler(tb).NewBuilder(nil, caps),
		Site: func(p expr.PlaceID) expr.Site {
			return expr.Site{Place: p, Scope: scope, PolicyType: typ}
		},
		Limits: filter.Limits{MaxRequestBodyBytes: testLimit, MaxResponseBodyBytes: testLimit, MaxBufferedBytes: 64 << 20},
	}
}

// build compiles cfg at scope, or stops the test.
func build(tb testing.TB, cfg any, scope phase.Scope) *Filter {
	tb.Helper()
	f, err := Build(context.Background(), buildEnv(tb, cfg, scope), specOf(tb, cfg))
	if err != nil {
		tb.Fatalf("Build: %v", err)
	}
	return f
}

// jsonHeader returns a header map with Content-Type: application/json.
func jsonHeader() http.Header { return http.Header{"Content-Type": {"application/json"}} }

// decoder is the CEL compiler that decodes the tests' bodies, built once.
var decoder = sync.OnceValues(cel.New)

// bodyValue returns the CEL value of body under h, as the data plane
// decodes it: the decoded document for a JSON body, null otherwise.
func bodyValue(tb testing.TB, h http.Header, body string) expr.Value {
	tb.Helper()
	if body == "" || !jsonContent(h) {
		return celtypes.Null()
	}
	c, err := decoder()
	if err != nil {
		tb.Fatalf("cel.New: %v", err)
	}
	v, _, err := c.DecodeJSON([]byte(body), 0)
	if err != nil {
		return celtypes.Null()
	}
	return v
}

// message is a filter.Message that counts its writes and can fail them.
type message struct {
	filtertest.Message
	// bodyErr fails Body, setBodyErr SetBody and decodedErr Decoded.
	bodyErr, setBodyErr, decodedErr error
	// bodyReads, setBodies, setQueries and setPaths count the calls.
	bodyReads, setBodies, setQueries, setPaths int
}

// Body counts the read and fails with bodyErr when set.
func (m *message) Body(ctx context.Context) ([]byte, error) {
	m.bodyReads++
	if m.bodyErr != nil {
		return nil, m.bodyErr
	}
	return m.Message.Body(ctx)
}

// SetBody counts the write and fails with setBodyErr when set.
func (m *message) SetBody(ctx context.Context, b []byte) error {
	m.setBodies++
	if m.setBodyErr != nil {
		return m.setBodyErr
	}
	return m.Message.SetBody(ctx, b)
}

// Decoded fails with decodedErr when set.
func (m *message) Decoded(ctx context.Context) (expr.Value, error) {
	if m.decodedErr != nil {
		return nil, m.decodedErr
	}
	return m.Message.Decoded(ctx)
}

// SetRawQuery counts the write.
func (m *message) SetRawQuery(q string) {
	m.setQueries++
	m.Message.SetRawQuery(q)
}

// SetPath counts the write.
func (m *message) SetPath(p string) {
	m.setPaths++
	m.Message.SetPath(p)
}

// baseVars returns the activation shared by the tests' requests and
// responses: a request to /v1/orders/42 by Consumer acme.
func baseVars(h http.Header, path, query string) *expr.Vars {
	req := &expr.Request{Method: "POST", Scheme: "https", Host: "api.shop.example", Path: path, Header: h}
	req.SetRawQuery(query)
	return &expr.Vars{
		Request:  req,
		Source:   &expr.Source{IP: netip.MustParseAddr("192.0.2.1"), Port: 44321},
		Route:    celtest.Route("orders", nil),
		Consumer: celtest.Consumer(expr.Consumer{Name: "acme"}),
		Now:      time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC),
	}
}

// onRequest returns an Exchange of a request Phase whose message has the
// header map h, body, raw query and path /v1/orders/42, with request.body
// decoded as the data plane decodes it.
func onRequest(tb testing.TB, h http.Header, body, query string) (*filtertest.Exchange, *message) {
	tb.Helper()
	if h == nil {
		h = http.Header{}
	}
	m := &message{Message: filtertest.Message{Hdr: h, Raw: rawBody(body), Query: query, PathV: "/v1/orders/42"}}
	v := baseVars(h, "/v1/orders/42", query)
	v.Request.Body = bodyValue(tb, h, body)
	return &filtertest.Exchange{MethodV: "POST", PathV: "/v1/orders/42", Hdr: h, Msg: m, VarsV: v}, m
}

// onResponse returns an Exchange of a response Phase whose message has
// status, the header map h and body, after a GET request, with
// response.body decoded as the data plane decodes it.
func onResponse(tb testing.TB, status int, h http.Header, body string) (*filtertest.Exchange, *message) {
	tb.Helper()
	if h == nil {
		h = http.Header{}
	}
	m := &message{Message: filtertest.Message{Hdr: h, Raw: rawBody(body), Code: status}}
	v := baseVars(http.Header{}, "/v1/orders/42", "")
	v.Request.Method = "GET"
	v.Response = &expr.Response{Status: status, Header: h, Body: bodyValue(tb, h, body)}
	return &filtertest.Exchange{MethodV: "GET", PathV: "/v1/orders/42", Msg: m, VarsV: v}, m
}

// rawBody returns the body bytes of s, nil for an empty body.
func rawBody(s string) []byte {
	if s == "" {
		return nil
	}
	return []byte(s)
}

// errorType returns the error.type a Result's cause names, "" for none.
func errorType(res filter.Result) string {
	var typed interface{ ErrorType() string }
	if errors.As(res.Err, &typed) {
		return typed.ErrorType()
	}
	return ""
}

// wantNext fails unless res continues.
func wantNext(tb testing.TB, what string, res filter.Result) {
	tb.Helper()
	if res.Outcome != filter.Continue {
		tb.Fatalf("%s: Handle = %+v, want Continue", what, res)
	}
}

// wantUndecided fails unless res cannot decide with the class default
// code and the error type kind.
func wantUndecided(tb testing.TB, what string, res filter.Result, kind string) {
	tb.Helper()
	if res.Outcome != filter.CannotDecide || res.Code != "" || errorType(res) != kind {
		tb.Fatalf("%s: Handle = %+v (error type %q), want CannotDecide with error type %q", what, res, errorType(res), kind)
	}
}
