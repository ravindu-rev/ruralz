// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package composition

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/gateway/admission"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// epoch is the fake clock's start.
func epoch() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }

// call is one leg call a fakeLeg saw.
type call struct {
	target upstream.Target
	// sendsBody is set when the step's Outbound carried the client body.
	sendsBody bool
	body      string
}

// fakeLeg is a scripted Leg recording its calls. fn answers a call; nil
// answers 200 with an empty JSON object.
type fakeLeg struct {
	name  string
	mu    sync.Mutex
	calls []call
	fn    func(ctx context.Context, t upstream.Target) (*snapshot.UpstreamResponse, error)
}

// Leg implements Leg.
func (l *fakeLeg) Leg(ctx context.Context, o *snapshot.Outbound, t upstream.Target) (*snapshot.UpstreamResponse, error) {
	c := call{target: t, sendsBody: o.Body != nil}
	if o.Body != nil {
		rc, err := o.Body.Open()
		if err == nil {
			b, _ := io.ReadAll(rc)
			_ = rc.Close()
			c.body = string(b)
		}
	}
	l.mu.Lock()
	l.calls = append(l.calls, c)
	l.mu.Unlock()
	if l.fn == nil {
		return jsonResp(200, `{}`), nil
	}
	resp, err := l.fn(ctx, t)
	if resp != nil && resp.Upstream == "" {
		resp.Upstream = l.name
	}
	return resp, err
}

// list returns the calls so far.
func (l *fakeLeg) list() []call {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// trackedBody is a response body that records its Close.
type trackedBody struct {
	io.Reader
	closed atomic.Bool
}

// Close records the close.
func (b *trackedBody) Close() error {
	b.closed.Store(true)
	return nil
}

// newBody returns a tracked body of s.
func newBody(s string) *trackedBody { return &trackedBody{Reader: strings.NewReader(s)} }

// jsonResp returns an Upstream response of status with a JSON body and
// its Content-Length.
func jsonResp(status int, s string) *snapshot.UpstreamResponse {
	return &snapshot.UpstreamResponse{
		Status: status,
		Header: http.Header{"Content-Type": {"application/json"}, "Content-Length": {strconv.Itoa(len(s))}},
		Body:   newBody(s),
	}
}

// respWith returns an Upstream response of status whose header is h and
// body s.
func respWith(status int, h http.Header, s string) *snapshot.UpstreamResponse {
	return &snapshot.UpstreamResponse{Status: status, Header: h, Body: newBody(s)}
}

// answer returns a leg function answering resp.
func answer(resp func() *snapshot.UpstreamResponse) func(context.Context, upstream.Target) (*snapshot.UpstreamResponse, error) {
	return func(context.Context, upstream.Target) (*snapshot.UpstreamResponse, error) { return resp(), nil }
}

// fixture is an Engine on a fake clock with fake legs by Upstream name.
type fixture struct {
	t      *testing.T
	clk    *clocktest.Fake
	budget *body.Budget
	units  *admission.Units
	legs   map[string]*fakeLeg
	e      *Engine
	limits snapshot.Limits
}

// newFixture returns a fixture with legs for each name, a 1 MiB buffer
// budget, 100 in-flight units and the default limits.
func newFixture(t *testing.T, names ...string) *fixture {
	t.Helper()
	f := &fixture{
		t:      t,
		clk:    clocktest.New(epoch()),
		budget: body.NewBudget(1<<20, nil),
		units:  admission.NewUnits(100),
		legs:   map[string]*fakeLeg{},
		limits: snapshot.Limits{MaxRequestBodyBytes: 1 << 20, MaxResponseBodyBytes: 64 << 10, MaxCompositionSteps: 16},
	}
	for _, n := range names {
		f.legs[n] = &fakeLeg{name: n}
	}
	f.e = New(Deps{Clock: f.clk, Budget: f.budget, Units: f.units, Decoder: celtest.Compiler(t)})
	return f
}

// leg returns the fake leg of name.
func (f *fixture) leg(name string) *fakeLeg { return f.legs[name] }

// input returns the compile input of c on the fixture's legs.
func (f *fixture) input(c *v1alpha1.Composition) Input {
	return Input{
		Route:       "r",
		Composition: c,
		Upstream: func(name string) (Leg, bool) {
			l, ok := f.legs[name]
			return l, ok
		},
		Builder: celtest.Compiler(f.t).NewBuilder(nil, expr.InputCaps{}),
		Limits:  f.limits,
	}
}

// compile compiles c, failing the test on an error.
func (f *fixture) compile(c *v1alpha1.Composition) *Composition {
	f.t.Helper()
	comp, err := f.e.Compile(f.input(c))
	if err != nil {
		f.t.Fatalf("Compile: %v", err)
	}
	return comp
}

// composition returns a composition of mode over steps.
func composition(mode v1alpha1.CompositionMode, steps ...v1alpha1.CompositionStep) *v1alpha1.Composition {
	return &v1alpha1.Composition{Mode: mode, Steps: steps}
}

// stepTo returns a step named name to upstream with a literal path.
func stepTo(name, upstreamName, path string) v1alpha1.CompositionStep {
	return v1alpha1.CompositionStep{Name: name, Upstream: upstreamName, Path: path}
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }

// optional returns s marked optional.
func optional(s v1alpha1.CompositionStep) v1alpha1.CompositionStep {
	s.Optional = ptr(true)
	return s
}

// exchange returns a client exchange for method, path and raw query, with
// the template captures params.
func exchange(method, path, query string, params ...expr.Param) *filtertest.Exchange {
	req := &expr.Request{Method: method, Scheme: "https", Host: "api.shop.example", Path: path, PathParams: params, Header: http.Header{}}
	req.SetRawQuery(query)
	return &filtertest.Exchange{
		MethodV: method,
		SchemeV: "https",
		HostV:   "api.shop.example",
		PathV:   path,
		Hdr:     http.Header{},
		SourceV: filter.Source{IP: netip.MustParseAddr("203.0.113.7")},
		Msg:     &filtertest.Message{Query: query},
		VarsV:   &expr.Vars{Request: req},
	}
}

// outbound returns the Outbound of x with a 15 s deadline and the gated
// client body b ("" for an empty body).
func (f *fixture) outbound(x filter.Exchange, b string) *snapshot.Outbound {
	f.t.Helper()
	q := &body.Request{}
	if b != "" {
		if err := q.Prepare(f.t.Context(), strings.NewReader(b), int64(len(b)), true, 1<<20, f.budget, 0); err != nil {
			f.t.Fatal(err)
		}
		f.t.Cleanup(q.Reset)
	}
	return &snapshot.Outbound{X: x, Body: q, Deadline: epoch().Add(15 * time.Second)}
}

// forward runs comp for a GET of /v1 and returns its outcome.
func (f *fixture) forward(comp *Composition) (*snapshot.UpstreamResponse, error) {
	f.t.Helper()
	return comp.Forward(f.t.Context(), f.outbound(exchange(http.MethodGet, "/v1", ""), ""))
}

// read reads and closes the body of resp.
func read(t *testing.T, resp *snapshot.UpstreamResponse) string {
	t.Helper()
	if resp == nil || resp.Body == nil {
		return ""
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
	return string(b)
}

// codeOf returns the RZ code of err.
func codeOf(err error) string {
	c, _ := errcode.CodeOf(err)
	return c
}

// stepOf returns the deciding step of a composition error.
func stepOf(err error) string {
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Step
	}
	return ""
}

// lockedWriter serializes writes to w.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// Write writes p to w under the lock.
func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// newTextLogger returns a logger writing text records to w.
func newTextLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(&lockedWriter{w: w}, nil))
}

// codedErr returns an error carrying code.
func codedErr(code string) error { return errcode.Wrap(code, errors.New("cause")) }
