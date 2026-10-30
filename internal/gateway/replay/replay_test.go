// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/admission"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/composition"
	"github.com/ravindu-rev/ruralz/internal/gateway/exchange"
	"github.com/ravindu-rev/ruralz/internal/gateway/executor"
	"github.com/ravindu-rev/ruralz/internal/gateway/handler/handlertest"
	"github.com/ravindu-rev/ruralz/internal/gateway/retire"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// epoch is the fake clock's start.
func epoch() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }

// fixture is a Replayer over real Node parts on a fake clock.
type fixture struct {
	t      *testing.T
	clk    *clocktest.Fake
	holder *retire.Holder
	units  *admission.Units
	budget *body.Budget
	tracer *handlertest.Tracer
	p      *Replayer
}

// newFixture returns a fixture with units in-flight units.
func newFixture(t *testing.T, units int64) *fixture {
	t.Helper()
	f := &fixture{t: t, clk: clocktest.New(epoch()), tracer: &handlertest.Tracer{}}
	f.holder = retire.New(retire.Config{Clock: f.clk, Stripes: 1})
	f.units = admission.NewUnits(units)
	f.budget = body.NewBudget(64<<20, &handlertest.Gauge{})
	p, err := New(Deps{
		Snapshots: f.holder, Units: f.units, Exchanges: exchange.NewPool(exchange.Deps{Budget: f.budget}),
		Executor: executor.New(executor.Deps{Clock: f.clk, Tracer: f.tracer}), Tracer: f.tracer, Clock: f.clk,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.p = p
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = f.holder.Close(ctx)
	})
	return f
}

// publish builds and publishes the snapshot spec describes.
func (f *fixture) publish(spec handlertest.Spec) *snapshot.Snapshot {
	f.t.Helper()
	s := handlertest.Snapshot(f.t, spec)
	if _, err := f.holder.Publish(s); err != nil {
		f.t.Fatalf("Publish: %v", err)
	}
	return s
}

// saved returns a saved request as the Response Cache keeps it: absolute
// target, no body, its own header map.
func saved(t *testing.T, target string, h http.Header) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header = h
	return r
}

// legCall is what a leg saw of the replay.
type legCall struct {
	upstream       string
	replay         bool
	via            string
	trace          emit.Decision
	deadline       time.Time
	method, path   string
	host, scheme   string
	query          string
	header         http.Header
	source         filter.Source
	route          string
	consumer, auth bool
	hooks          bool
	hinted         bool
}

// legRunner is a snapshot.LegRunner recording each leg.
type legRunner struct {
	mu    sync.Mutex
	calls []legCall
	fn    func(ctx context.Context, upstream string, o *snapshot.Outbound) (*snapshot.UpstreamResponse, error)
}

// RunLeg implements snapshot.LegRunner.
func (l *legRunner) RunLeg(ctx context.Context, name string, o *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
	x := o.X
	c := legCall{
		upstream: name, replay: o.Replay, via: o.Via, deadline: o.Deadline,
		method: x.Method(), path: x.Path(), host: x.Host(), scheme: x.Scheme(), query: x.Received().RawQuery,
		header: x.Header().Clone(), source: x.Source(), hooks: o.Hooks != nil, hinted: o.Hints != nil,
	}
	if o.Trace != nil {
		c.trace = *o.Trace
	}
	if r := x.Route(); r != nil {
		c.route = r.Name
	}
	if v := x.Vars(); v != nil {
		c.consumer, c.auth = v.Consumer != nil, v.Auth != nil
	}
	l.mu.Lock()
	l.calls = append(l.calls, c)
	l.mu.Unlock()
	if l.fn == nil {
		return &snapshot.UpstreamResponse{Status: http.StatusNotModified, Header: http.Header{"Etag": {`"v1"`}}, Upstream: name}, nil
	}
	return l.fn(ctx, name, o)
}

func (l *legRunner) list() []legCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// plainRoute is a Route with upstreams named name.
func plainRoute(name string, timeout time.Duration) handlertest.Route {
	return handlertest.Route{Name: name, Timeout: timeout, Forward: handlertest.Respond("orders", http.StatusOK, http.Header{}, "")}
}

// TestNewRequiresDeps checks that New refuses a Deps without a required
// part.
func TestNewRequiresDeps(t *testing.T) {
	f := newFixture(t, 4)
	full := Deps{Snapshots: f.holder, Units: f.units, Exchanges: f.p.pool, Executor: f.p.exec, Tracer: f.tracer}
	if _, err := New(full); err != nil {
		t.Fatalf("New(full) = %v", err)
	}
	for name, mod := range map[string]func(*Deps){
		"Snapshots": func(d *Deps) { d.Snapshots = nil },
		"Units":     func(d *Deps) { d.Units = nil },
		"Exchanges": func(d *Deps) { d.Exchanges = nil },
		"Executor":  func(d *Deps) { d.Executor = nil },
		"Tracer":    func(d *Deps) { d.Tracer = nil },
	} {
		d := full
		mod(&d)
		if _, err := New(d); !errors.Is(err, ErrDeps) {
			t.Errorf("New without %s = %v, want ErrDeps", name, err)
		}
	}
}

// TestReplayRunsUpstreamLeg checks a replay on a Route with upstreams: one
// leg to the named Upstream on a synthetic exchange (the saved request,
// the named Route, no source, no consumer or auth), marked as a replay
// without a Via entry or 103 relay, on a new unsampled root trace, with
// the deadline at the call's start plus the Route timeout; the response
// holds the pin and the in-flight unit until its body closes.
func TestReplayRunsUpstreamLeg(t *testing.T) {
	f := newFixture(t, 4)
	legs := &legRunner{}
	snap := f.publish(handlertest.Spec{Legs: legs, Routes: []handlertest.Route{plainRoute("orders", 5*time.Second)}})
	h := http.Header{
		"If-None-Match": {`"v1"`},
		"Traceparent":   {"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"},
		"Accept":        {"application/json"},
	}
	req := saved(t, "https://API.example/v1/./orders/../orders?id=7", h)
	resp, err := f.p.Replay(t.Context(), "orders", "orders", req)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if resp.StatusCode != http.StatusNotModified || resp.Header.Get("Etag") != `"v1"` || resp.ContentLength != -1 || resp.Request != req {
		t.Errorf("response = %d %v length %d, want the leg's 304", resp.StatusCode, resp.Header, resp.ContentLength)
	}
	if pins(snap) != 1 || f.units.InUse() != 1 {
		t.Errorf("before Close: pins %d, units %d, want 1 and 1", pins(snap), f.units.InUse())
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
	if pins(snap) != 0 || f.units.InUse() != 0 {
		t.Errorf("after Close: pins %d, units %d, want 0 and 0", pins(snap), f.units.InUse())
	}
	if _, err := resp.Body.Read(make([]byte, 1)); !errors.Is(err, http.ErrBodyReadAfterClose) {
		t.Errorf("Read after Close = %v, want http.ErrBodyReadAfterClose", err)
	}

	calls := legs.list()
	if len(calls) != 1 {
		t.Fatalf("legs = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.upstream != "orders" || !c.replay || c.via != "" || c.hinted || !c.hooks {
		t.Errorf("leg = upstream %q replay %t via %q hints %t hooks %t, want orders, a replay with hooks and nothing else", c.upstream, c.replay, c.via, c.hinted, c.hooks)
	}
	if c.method != http.MethodGet || c.path != "/v1/orders" || c.host != "api.example" || c.scheme != "https" || c.query != "id=7" || c.route != "orders" {
		t.Errorf("exchange = %s %s://%s%s?%s on %s, want the saved request on route orders", c.method, c.scheme, c.host, c.path, c.query, c.route)
	}
	if c.header.Get("If-None-Match") != `"v1"` || c.source.IP.IsValid() || c.consumer || c.auth {
		t.Errorf("exchange header %v, source %v, consumer %t, auth %t; want the saved fields, no source, consumer or auth", c.header, c.source, c.consumer, c.auth)
	}
	if c.trace.Sampled || c.trace.Remote || c.trace.TraceID == ([16]byte{}) || fmt.Sprintf("%x", c.trace.TraceID) == "0af7651916cd43dd8448eb211c80319c" {
		t.Errorf("trace = %+v, want a new unsampled root", c.trace)
	}
	if !c.deadline.Equal(epoch().Add(5 * time.Second)) {
		t.Errorf("deadline = %v, want start + 5s", c.deadline)
	}
	if req.Header.Get("Traceparent") == "" || len(req.Header) != 3 {
		t.Errorf("saved request header = %v, want it untouched", req.Header)
	}
}

// pins returns the requests pinned to s.
func pins(s *snapshot.Snapshot) int64 { return s.Pins.Count() }

// TestReplayNotAvailable checks the replays that cannot run: no snapshot,
// a Route or Upstream the snapshot no longer holds, a Route whose kind no
// longer fits the entry, a full in-flight ceiling and an ending snapshot;
// each is filter.ErrNotAvailable and holds nothing afterwards.
func TestReplayNotAvailable(t *testing.T) {
	unknown := &legRunner{fn: func(_ context.Context, name string, _ *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		return nil, fmt.Errorf("upstream %q: %w", name, filter.ErrNotAvailable)
	}}
	comp := handlertest.Route{Name: "composed", Forward: &composition.Composition{}}
	tests := []struct {
		name     string
		units    int64
		spec     *handlertest.Spec
		route    string
		upstream string
		drain    bool
	}{
		{name: "no snapshot", units: 4, route: "orders", upstream: "orders"},
		{name: "unknown route", units: 4, spec: &handlertest.Spec{Legs: &legRunner{}, Routes: []handlertest.Route{plainRoute("orders", 0)}}, route: "gone", upstream: "orders"},
		{name: "unknown upstream", units: 4, spec: &handlertest.Spec{Legs: unknown, Routes: []handlertest.Route{plainRoute("orders", 0)}}, route: "orders", upstream: "gone"},
		{name: "no leg runner", units: 4, spec: &handlertest.Spec{Routes: []handlertest.Route{plainRoute("orders", 0)}}, route: "orders", upstream: "orders"},
		{name: "composition entry on a plain route", units: 4, spec: &handlertest.Spec{Legs: &legRunner{}, Routes: []handlertest.Route{plainRoute("orders", 0)}}, route: "orders"},
		{name: "upstream entry on a composition route", units: 4, spec: &handlertest.Spec{Legs: &legRunner{}, Routes: []handlertest.Route{comp}}, route: "composed", upstream: "orders"},
		{name: "full ceiling", units: 0, spec: &handlertest.Spec{Legs: &legRunner{}, Routes: []handlertest.Route{plainRoute("orders", 0)}}, route: "orders", upstream: "orders"},
		{name: "draining", units: 4, spec: &handlertest.Spec{Legs: &legRunner{}, Routes: []handlertest.Route{plainRoute("orders", 0)}}, route: "orders", upstream: "orders", drain: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, tt.units)
			var snap *snapshot.Snapshot
			if tt.spec != nil {
				snap = f.publish(*tt.spec)
			}
			if tt.drain {
				if _, err := f.holder.EndAll(t.Context(), snapshot.EndDrain); err != nil {
					t.Fatal(err)
				}
			}
			resp, err := f.p.Replay(t.Context(), tt.route, tt.upstream, saved(t, "http://api.example/v1", http.Header{}))
			if !errors.Is(err, filter.ErrNotAvailable) || resp != nil {
				t.Fatalf("Replay = %v, %v; want filter.ErrNotAvailable", resp, err)
			}
			if f.units.InUse() != 0 || (snap != nil && pins(snap) != 0) {
				t.Errorf("units %d, pins %v after the refusal, want none", f.units.InUse(), snap)
			}
		})
	}
}

// TestReplayNoRequest checks that a Replay without a request fails without
// taking anything.
func TestReplayNoRequest(t *testing.T) {
	f := newFixture(t, 4)
	if _, err := f.p.Replay(t.Context(), "orders", "orders", nil); !errors.Is(err, ErrNoRequest) {
		t.Errorf("Replay(nil) = %v, want ErrNoRequest", err)
	}
	if f.units.InUse() != 0 {
		t.Errorf("units = %d, want 0", f.units.InUse())
	}
}

// TestReplayFailures checks the replays that end without an Upstream
// response: a leg error returns with its code, and a response the Node
// generated (a leg Filter's 401, a closed failure's 502) returns as
// ErrGenerated with its code; either releases everything.
func TestReplayFailures(t *testing.T) {
	tests := []struct {
		name string
		fn   func(context.Context, string, *snapshot.Outbound) (*snapshot.UpstreamResponse, error)
		gen  bool
		code string
	}{
		{"leg error", func(context.Context, string, *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
			return nil, &upstream.LegError{Upstream: "orders", Err: errcode.Wrap("RZ-UP-001", errors.New("refused"))}
		}, false, "RZ-UP-001"},
		{"leg Filter response", func(context.Context, string, *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
			return &snapshot.UpstreamResponse{Status: 401, Generated: &filter.Response{Status: 401, Code: "RZ-AUTH-020"}}, nil
		}, true, "RZ-AUTH-020"},
		{"closed failure", func(context.Context, string, *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
			return &snapshot.UpstreamResponse{Status: 502, Generated: &filter.Response{Status: 502, Code: "RZ-RT-012"}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}, true, "RZ-RT-012"},
		{"generated without a code", func(context.Context, string, *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
			return &snapshot.UpstreamResponse{Status: 403, Generated: &filter.Response{Status: 403}}, nil
		}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, 4)
			snap := f.publish(handlertest.Spec{Legs: &legRunner{fn: tt.fn}, Routes: []handlertest.Route{plainRoute("orders", 0)}})
			resp, err := f.p.Replay(t.Context(), "orders", "orders", saved(t, "http://api.example/v1", http.Header{}))
			if err == nil || resp != nil {
				t.Fatalf("Replay = %v, %v; want an error", resp, err)
			}
			if errors.Is(err, ErrGenerated) != tt.gen {
				t.Errorf("Replay error %v: ErrGenerated %t, want %t", err, errors.Is(err, ErrGenerated), tt.gen)
			}
			if code, _ := errcode.CodeOf(err); code != tt.code {
				t.Errorf("code = %q, want %q", code, tt.code)
			}
			if f.units.InUse() != 0 || pins(snap) != 0 {
				t.Errorf("units %d, pins %d, want 0 and 0", f.units.InUse(), pins(snap))
			}
		})
	}
}

// TestReplayDeadline checks the replay's deadline: the Route timeout from
// the call's start on the Replayer's clock cancels the leg with
// context.DeadlineExceeded.
func TestReplayDeadline(t *testing.T) {
	f := newFixture(t, 4)
	entered := make(chan struct{})
	var cause error
	legs := &legRunner{fn: func(ctx context.Context, _ string, _ *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		close(entered)
		<-ctx.Done()
		cause = context.Cause(ctx)
		return nil, errcode.Wrap("RZ-UP-003", cause)
	}}
	f.publish(handlertest.Spec{Legs: legs, Routes: []handlertest.Route{plainRoute("orders", 2*time.Second)}})
	done := make(chan error, 1)
	go func() {
		_, err := f.p.Replay(context.Background(), "orders", "orders", saved(t, "http://api.example/v1", http.Header{}))
		done <- err
	}()
	<-entered
	f.clk.Advance(2 * time.Second)
	if err := <-done; err == nil || !errors.Is(cause, context.DeadlineExceeded) {
		t.Errorf("Replay = %v with cause %v, want the Route deadline", err, cause)
	}
}

// TestReplayEndedWhileRunning checks the ending protocol on a running
// replay: Drain cancels it, and it releases its pin.
func TestReplayEndedWhileRunning(t *testing.T) {
	f := newFixture(t, 4)
	var cause error
	legs := &legRunner{}
	legs.fn = func(ctx context.Context, _ string, _ *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		if _, err := f.holder.EndAll(context.Background(), snapshot.EndDrain); err != nil {
			return nil, err
		}
		<-ctx.Done()
		cause = context.Cause(ctx)
		return nil, errcode.Wrap("RZ-UP-004", cause)
	}
	snap := f.publish(handlertest.Spec{Legs: legs, Routes: []handlertest.Route{plainRoute("orders", 0)}})
	if _, err := f.p.Replay(t.Context(), "orders", "orders", saved(t, "http://api.example/v1", http.Header{})); err == nil || !errors.Is(cause, errEnded) {
		t.Errorf("Replay = %v with cause %v, want an end by the ending protocol", err, cause)
	}
	if pins(snap) != 0 || f.units.InUse() != 0 {
		t.Errorf("pins %d, units %d, want 0 and 0", pins(snap), f.units.InUse())
	}
}

// stepCall is what a composition step's leg saw of its Outbound.
type stepCall struct {
	replay bool
	via    string
}

// stepLeg is a composition leg runner for one Upstream.
type stepLeg struct {
	name  string
	mu    sync.Mutex
	calls []stepCall
	fn    func() (*snapshot.UpstreamResponse, error)
}

// Leg implements composition.Leg.
func (l *stepLeg) Leg(_ context.Context, o *snapshot.Outbound, _ upstream.Target) (*snapshot.UpstreamResponse, error) {
	l.mu.Lock()
	l.calls = append(l.calls, stepCall{replay: o.Replay, via: o.Via})
	l.mu.Unlock()
	resp, err := l.fn()
	if resp != nil {
		resp.Upstream = l.name
	}
	return resp, err
}

// TestReplayComposition checks a composition entry's replay: the Route's
// whole composition runs, every step on the replay's Outbound, and an
// optional step left out marks the response partial, as a store would see
// it.
func TestReplayComposition(t *testing.T) {
	f := newFixture(t, 8)
	jsonBody := func(s string) *snapshot.UpstreamResponse {
		return &snapshot.UpstreamResponse{Status: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(s))}
	}
	a := &stepLeg{name: "a", fn: func() (*snapshot.UpstreamResponse, error) { return jsonBody(`{"a":1}`), nil }}
	b := &stepLeg{name: "b", fn: func() (*snapshot.UpstreamResponse, error) {
		return nil, &upstream.LegError{Upstream: "b", Err: errcode.Wrap("RZ-UP-001", errors.New("refused"))}
	}}
	legs := map[string]*stepLeg{"a": a, "b": b}
	yes := true
	e := composition.New(composition.Deps{Clock: f.clk, Budget: f.budget, Units: f.units, Decoder: celtest.Compiler(t)})
	comp, err := e.Compile(composition.Input{
		Route: "composed",
		Composition: &v1alpha1.Composition{Mode: v1alpha1.CompositionModeAggregate, Steps: []v1alpha1.CompositionStep{
			{Name: "a", Upstream: "a", Path: "/a"}, {Name: "b", Upstream: "b", Path: "/b", Optional: &yes},
		}},
		Upstream: func(name string) (composition.Leg, bool) {
			l, ok := legs[name]
			return l, ok
		},
		Builder: celtest.Compiler(t).NewBuilder(nil, expr.InputCaps{}),
		Limits:  handlertest.DefaultLimits(),
	})
	if err != nil {
		t.Fatal(err)
	}
	snap := f.publish(handlertest.Spec{Routes: []handlertest.Route{{Name: "composed", Forward: comp}}})
	resp, err := f.p.Replay(t.Context(), "composed", "", saved(t, "http://api.example/v1/summary", http.Header{}))
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	b2, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b2), `"a":1`) || resp.Header.Get("Ruralz-Partial") != "true" {
		t.Errorf("response = %d %q %v, want the merged body marked partial", resp.StatusCode, b2, resp.Header)
	}
	for _, l := range []*stepLeg{a, b} {
		if len(l.calls) != 1 || !l.calls[0].replay || l.calls[0].via != "" {
			t.Errorf("step %s legs = %+v, want one replay without Via", l.name, l.calls)
		}
	}
	if pins(snap) != 0 || f.units.InUse() != 0 {
		t.Errorf("pins %d, units %d after Close, want 0 and 0", pins(snap), f.units.InUse())
	}
}

// legPolicy is an upstream-leg Filter recording what its onUpstreamRequest
// sees.
type legPolicy struct {
	mu     sync.Mutex
	route  string
	path   string
	source bool
	ran    int
}

func (p *legPolicy) Handle(_ context.Context, ph phase.Phase, x filter.Exchange) filter.Result {
	if ph != phase.OnUpstreamRequest {
		return filter.Next()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ran++
	if r := x.Route(); r != nil {
		p.route = r.Name
	}
	p.path, p.source = x.Path(), x.Source().IP.IsValid()
	return filter.Next()
}

// TestReplayThroughUpstreamLayer checks a replay through the real Upstream
// layer to a loopback Upstream: the Upstream's leg Policies run with CEL
// route the named Route, the saved request arrives with its validators and
// without forwarding fields or a Via entry, carrying the replay's new
// unsampled trace context, and the 304 comes back.
func TestReplayThroughUpstreamLayer(t *testing.T) {
	f := newFixture(t, 4)
	var mu sync.Mutex
	var got *http.Request
	srv := newUpstreamServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Clone(context.Background())
		mu.Unlock()
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.Header().Set("Etag", `"v1"`)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = io.WriteString(w, "full")
	})
	set := compileUpstreams(t, f, "orders", srv)
	lp := &legPolicy{}
	f.publish(handlertest.Spec{Legs: set, Routes: []handlertest.Route{{
		Name: "orders", Forward: handlertest.Respond("orders", http.StatusOK, http.Header{}, ""),
		Legs: map[string][]*snapshot.Policy{"orders": {handlertest.Policy("sign", phase.ClassUpstreamAuth, v1alpha1.PolicyTypeHeaders, lp, phase.OnUpstreamRequest)}},
	}}})
	req := saved(t, "http://api.example/v1/orders", http.Header{
		"If-None-Match":     {`"v1"`},
		"X-Forwarded-For":   {"203.0.113.9"},
		"X-Forwarded-Proto": {"https"},
		"X-Forwarded-Host":  {"api.example"},
		"Forwarded":         {"for=203.0.113.9"},
		"Via":               {"1.1 edge"},
		"Traceparent":       {"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"},
	})
	resp, err := f.p.Replay(t.Context(), "orders", "orders", req)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotModified || resp.Header.Get("Etag") != `"v1"` {
		t.Errorf("response = %d %v, want the Upstream's 304", resp.StatusCode, resp.Header)
	}
	mu.Lock()
	defer mu.Unlock()
	if got == nil {
		t.Fatal("the Upstream got no request")
	}
	for _, name := range []string{"X-Forwarded-For", "X-Forwarded-Proto", "X-Forwarded-Host", "Forwarded"} {
		if v := got.Header.Get(name); v != "" {
			t.Errorf("Upstream request %s = %q, want none", name, v)
		}
	}
	if v := got.Header.Values("Via"); len(v) != 1 || v[0] != "1.1 edge" {
		t.Errorf("Upstream request Via = %q, want the saved entry only", v)
	}
	if tp := got.Header.Get("Traceparent"); strings.Contains(tp, "0af7651916cd43dd8448eb211c80319c") || !strings.HasSuffix(tp, "-00") {
		t.Errorf("Upstream request traceparent = %q, want a new unsampled trace", tp)
	}
	if got.URL.Path != "/v1/orders" || got.Header.Get("If-None-Match") != `"v1"` {
		t.Errorf("Upstream request = %s with If-None-Match %q", got.URL.Path, got.Header.Get("If-None-Match"))
	}
	lp.mu.Lock()
	defer lp.mu.Unlock()
	if lp.ran != 1 || lp.route != "orders" || lp.path != "/v1/orders" || lp.source {
		t.Errorf("leg Policy ran %d times with route %q path %q source %t, want once on orders /v1/orders without a source", lp.ran, lp.route, lp.path, lp.source)
	}
}
