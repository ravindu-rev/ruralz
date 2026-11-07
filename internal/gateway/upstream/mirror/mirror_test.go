// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// epoch is the fake clock's start.
func epoch() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }

// counter is a recording emit.Counter.
type counter struct{ n atomic.Uint64 }

func (c *counter) Add(_ emit.Stripe, n uint64) { c.n.Add(n) }

// gauge is a recording emit.Gauge.
type gauge struct{ v atomic.Int64 }

func (g *gauge) Add(_ emit.Stripe, d int64) { g.v.Add(d) }
func (g *gauge) Set(v int64)                { g.v.Store(v) }

// testMirror is a Route mirror with recording handles.
type testMirror struct {
	*snapshot.Mirror
	results [emit.NumMirrorResults]*counter
}

// newMirror returns a mirror to upstream sampling percent with
// maxInFlight slots.
func newMirror(upstream string, percent uint32, maxInFlight int64) *testMirror {
	m := &testMirror{Mirror: &snapshot.Mirror{
		Upstream: upstream, Percent: percent, MaxInFlight: maxInFlight, InFlight: new(atomic.Int64), Metrics: &emit.MirrorMetrics{},
	}}
	for i := range m.results {
		m.results[i] = &counter{}
		m.Metrics.Results[i] = m.results[i]
	}
	return m
}

// counts returns the mirror's sent, dropped and failed counts.
func (m *testMirror) counts() [emit.NumMirrorResults]uint64 {
	var out [emit.NumMirrorResults]uint64
	for i, c := range m.results {
		out[i] = c.n.Load()
	}
	return out
}

// pinner is a Pinner recording the pinned legs.
type pinner struct {
	refuse atomic.Bool
	pinned atomic.Int64
	mu     sync.Mutex
	recs   []*snapshot.PinnedRequest
}

func (p *pinner) PinLeg(_ *snapshot.Snapshot, _ emit.Stripe, rec *snapshot.PinnedRequest) bool {
	if p.refuse.Load() {
		return false
	}
	p.pinned.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recs = append(p.recs, rec)
	return true
}

func (p *pinner) Unpin(_ *snapshot.Snapshot, _ emit.Stripe, rec *snapshot.PinnedRequest) {
	p.pinned.Add(-1)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recs = slices.DeleteFunc(p.recs, func(r *snapshot.PinnedRequest) bool { return r == rec })
}

// endAll ends every pinned leg as the ending protocol does at the Drain
// deadline.
func (p *pinner) endAll() {
	p.mu.Lock()
	recs := slices.Clone(p.recs)
	p.mu.Unlock()
	for _, r := range recs {
		r.Cancel()
	}
}

// sentCopy is what one leg sent, read as a transport reads it.
type sentCopy struct {
	upstream                     string
	method, path, rawPath, query string
	header, trailer              http.Header
	body                         string
	length                       int64
	// replay is what GetBody returns; "" without GetBody.
	replay   string
	key      snapshot.MirrorKey
	deadline time.Time
	stripe   emit.Stripe
}

// keyCall is one MirrorKey call.
type keyCall struct {
	upstream string
	vars     *expr.Vars
}

// legs is a snapshot.MirrorLegs that reads and records every leg, then
// runs send.
type legs struct {
	key  snapshot.MirrorKey
	send func(ctx context.Context, c *snapshot.MirrorCopy) error

	mu   sync.Mutex
	keys []keyCall
	sent []sentCopy
}

func (l *legs) MirrorKey(upstream string, vars *expr.Vars, _ emit.Stripe) snapshot.MirrorKey {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys = append(l.keys, keyCall{upstream: upstream, vars: vars})
	return l.key
}

func (l *legs) Mirror(ctx context.Context, upstream string, c *snapshot.MirrorCopy) error {
	r := c.Request
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	s := sentCopy{
		upstream: upstream, method: r.Method, path: r.URL.Path, rawPath: r.URL.RawPath, query: r.URL.RawQuery,
		header: r.Header, trailer: r.Trailer, body: string(b), length: r.ContentLength,
		key: c.Key, deadline: c.Deadline, stripe: c.Stripe,
	}
	if r.GetBody != nil {
		rc, err := r.GetBody()
		if err != nil {
			return err
		}
		rb, _ := io.ReadAll(rc)
		s.replay = string(rb)
	}
	l.mu.Lock()
	l.sent = append(l.sent, s)
	l.mu.Unlock()
	if l.send != nil {
		return l.send(ctx, c)
	}
	return nil
}

func (l *legs) list() []sentCopy {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.sent)
}

// lockedBuffer is a log destination safe for the legs' goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// fixture is a running Runner over a recording Pinner, a snapshot whose
// MirrorLegs is legs, a fake clock and a buffer budget.
type fixture struct {
	t      *testing.T
	clk    *clocktest.Fake
	pins   *pinner
	budget *body.Budget
	legs   *legs
	snap   *snapshot.Snapshot
	logs   *lockedBuffer
	r      *Runner
	stop   func()
}

// newFixture starts a Runner drawing from src with a budget of total
// bytes; the test's end stops it.
func newFixture(t *testing.T, src rand.Source, total int64) *fixture {
	t.Helper()
	f := &fixture{
		t: t, clk: clocktest.New(epoch()), pins: &pinner{}, budget: body.NewBudget(total, &gauge{}), legs: &legs{},
		logs: &lockedBuffer{},
	}
	f.snap = &snapshot.Snapshot{MirrorLegs: f.legs}
	r, err := New(Deps{Snapshots: f.pins, Budget: f.budget, Rand: src, Clock: f.clk, Logger: slog.New(slog.NewTextHandler(f.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	f.r = r
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	var once sync.Once
	f.stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("Run: %v", err)
			}
		})
	}
	t.Cleanup(f.stop)
	return f
}

// offer offers req to ms with a Route deadline 15 s after epoch on
// stripe 3.
func (f *fixture) offer(req *http.Request, ms ...*testMirror) *Offer {
	o := &Offer{Snapshot: f.snap, Request: req, Vars: &expr.Vars{}, Deadline: epoch().Add(15 * time.Second), Stripe: 3}
	for _, m := range ms {
		o.Mirrors = append(o.Mirrors, m.Mirror)
	}
	f.r.Offer(o)
	return o
}

// attempt returns an attempt as the Upstream layer sends it, with body b:
// replayable through GetBody when gated, else streamed; "" sends none.
func attempt(method, b string, gated bool) *http.Request {
	r := &http.Request{
		Method: method,
		URL:    &url.URL{Scheme: "http", Host: "10.0.0.1:80", Path: "/v1/a/b", RawPath: "/v1/a%2Fb", RawQuery: "x=1&y=%20"},
		Header: http.Header{"X-Signed": {"1"}, "Traceparent": {"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}},
		Body:   http.NoBody,
	}
	if b != "" {
		r.Body, r.ContentLength = io.NopCloser(strings.NewReader(b)), int64(len(b))
		if gated {
			r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(b)), nil }
		}
	}
	return r
}

// waitFor polls cond with a real-time bound; it only waits for goroutines
// the test started, never for fake time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(time.Millisecond):
		}
	}
}

// failSource fails the test on every draw.
type failSource struct{ t *testing.T }

func (s failSource) Uint64() uint64 {
	s.t.Error("a mirror at percent 0 or 100 drew a random number")
	return 0
}

var (
	sent    = [emit.NumMirrorResults]uint64{1, 0, 0}
	dropped = [emit.NumMirrorResults]uint64{0, 1, 0}
	failed  = [emit.NumMirrorResults]uint64{0, 0, 1}
)

// TestNewRequiresDeps: New refuses a Deps without Snapshots or Budget.
func TestNewRequiresDeps(t *testing.T) {
	b := body.NewBudget(1<<20, &gauge{})
	for _, d := range []Deps{{Budget: b}, {Snapshots: &pinner{}}} {
		if _, err := New(d); !errors.Is(err, ErrDeps) {
			t.Errorf("New(%+v) = %v, want ErrDeps", d, err)
		}
	}
}

// TestRunOnce: a Runner runs once; a second Run returns ErrRunning.
func TestRunOnce(t *testing.T) {
	f := newFixture(t, nil, 1<<20)
	f.stop()
	if err := f.r.Run(t.Context()); !errors.Is(err, ErrRunning) {
		t.Errorf("second Run = %v, want ErrRunning", err)
	}
}

// TestSamplingBounds: percent 100 copies every request and 0 none, both
// without a draw; a request a mirror does not sample counts nothing.
func TestSamplingBounds(t *testing.T) {
	f := newFixture(t, failSource{t}, 1<<20)
	never, always := newMirror("never", 0, 1000), newMirror("always", 100, 1000)
	for range 5 {
		f.offer(attempt(http.MethodGet, "", false), never, always)
	}
	f.stop()
	if got := always.counts(); got != [emit.NumMirrorResults]uint64{5, 0, 0} {
		t.Errorf("percent 100 counts = %v, want 5 sent", got)
	}
	if got := never.counts(); got != [emit.NumMirrorResults]uint64{} {
		t.Errorf("percent 0 counts = %v, want none", got)
	}
	for _, s := range f.legs.list() {
		if s.upstream != "always" {
			t.Errorf("a leg went to %s", s.upstream)
		}
	}
}

// TestSamplingDraws: a mirror below 100 percent copies a request when a
// draw of the injected source in [0, 100) is below its percent.
func TestSamplingDraws(t *testing.T) {
	f := newFixture(t, rand.NewPCG(7, 11), 1<<20)
	twin := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // G404: replays the test's sampling source.
	m := newMirror("shadow", 30, 1000)
	want := uint64(0)
	for range 200 {
		if twin.IntN(100) < 30 {
			want++
		}
		f.offer(attempt(http.MethodGet, "", false), m)
	}
	f.stop()
	if want == 0 || want == 200 {
		t.Fatalf("the seed samples %d of 200; pick a seed that tells sampling apart", want)
	}
	if got := m.counts(); got != [emit.NumMirrorResults]uint64{want, 0, 0} {
		t.Errorf("counts = %v, want %d sent", got, want)
	}
	if n := uint64(len(f.legs.list())); n != want {
		t.Errorf("legs = %d, want %d", n, want)
	}
}

// TestInFlightBound: a mirror runs at most maxInFlight legs on the Node;
// a sampled copy past the bound is dropped and counted, and a slot freed
// by a finished leg takes the next copy.
func TestInFlightBound(t *testing.T) {
	f := newFixture(t, nil, 1<<20)
	release, started := make(chan struct{}), make(chan struct{}, 4)
	f.legs.send = func(context.Context, *snapshot.MirrorCopy) error {
		started <- struct{}{}
		<-release
		return nil
	}
	m := newMirror("shadow", 100, 2)
	for range 3 {
		f.offer(attempt(http.MethodGet, "", false), m)
	}
	<-started
	<-started
	if n, got := m.InFlight.Load(), m.counts(); n != 2 || got != dropped {
		t.Errorf("in flight %d, counts %v; want 2 legs and 1 dropped", n, got)
	}
	close(release)
	waitFor(t, "the legs' end", func() bool { return m.InFlight.Load() == 0 })
	f.offer(attempt(http.MethodGet, "", false), m)
	<-started
	f.stop()
	if got := m.counts(); got != [emit.NumMirrorResults]uint64{3, 1, 0} {
		t.Errorf("counts = %v, want 3 sent and 1 dropped", got)
	}
	if m.InFlight.Load() != 0 || f.pins.pinned.Load() != 0 {
		t.Errorf("in flight %d, pins %d after the legs, want 0", m.InFlight.Load(), f.pins.pinned.Load())
	}
}

// TestSlowMirrorNeverBlocksOffer: Offer returns while a mirror's leg is
// still running, so the next request offers at once; the slow legs end
// with the Runner, counted failed.
func TestSlowMirrorNeverBlocksOffer(t *testing.T) {
	f := newFixture(t, nil, 1<<20)
	started := make(chan struct{}, 2)
	f.legs.send = func(ctx context.Context, _ *snapshot.MirrorCopy) error {
		started <- struct{}{}
		<-ctx.Done()
		return context.Cause(ctx)
	}
	m := newMirror("shadow", 100, 1000)
	f.offer(attempt(http.MethodGet, "", false), m)
	<-started
	f.offer(attempt(http.MethodGet, "", false), m)
	<-started
	if n := m.InFlight.Load(); n != 2 {
		t.Errorf("in flight = %d, want both slow legs", n)
	}
	f.stop()
	if got := m.counts(); got != [emit.NumMirrorResults]uint64{0, 0, 2} {
		t.Errorf("counts = %v, want 2 failed", got)
	}
}

// TestFailingMirrorLeavesAttempt: a mirror whose leg fails counts failed,
// and the attempt it copied keeps its header and its whole buffered body.
func TestFailingMirrorLeavesAttempt(t *testing.T) {
	f := newFixture(t, nil, 1<<20)
	f.legs.send = func(context.Context, *snapshot.MirrorCopy) error { return errors.New("connection reset") }
	m := newMirror("shadow", 100, 1000)
	req := attempt(http.MethodPost, "order=42", true)
	orig := req.Body
	f.offer(req, m)
	f.stop()
	if got := m.counts(); got != failed {
		t.Errorf("counts = %v, want 1 failed", got)
	}
	if req.Body != orig || req.Header.Get("X-Signed") != "1" {
		t.Errorf("the attempt changed: body %T, header %v", req.Body, req.Header)
	}
	if b, _ := io.ReadAll(req.Body); string(b) != "order=42" {
		t.Errorf("attempt body = %q, want order=42", b)
	}
}

// TestLegRequest: a leg sends the attempt's method, path, raw path, query
// and header fields, cloned at Offer, with the ring-hash key evaluated at
// Offer over the request's variables, the Route deadline and the stripe;
// mirrors of one offer share the body copy, in mirror order.
func TestLegRequest(t *testing.T) {
	f := newFixture(t, nil, 1<<20)
	f.legs.key = snapshot.MirrorKey{Key: 42}
	a, b := newMirror("audit", 100, 1000), newMirror("shadow", 100, 1000)
	req := attempt(http.MethodPost, "order=42", true)
	o := f.offer(req, a, b)
	req.Header.Set("X-Signed", "changed after the offer")
	f.stop()
	got := f.legs.list()
	slices.SortFunc(got, func(x, y sentCopy) int { return strings.Compare(x.upstream, y.upstream) })
	if len(got) != 2 || got[0].upstream != "audit" || got[1].upstream != "shadow" {
		t.Fatalf("legs = %+v, want audit and shadow", got)
	}
	for _, s := range got {
		if s.method != http.MethodPost || s.path != "/v1/a/b" || s.rawPath != "/v1/a%2Fb" || s.query != "x=1&y=%20" {
			t.Errorf("%s: request line %s %s (%s) ?%s", s.upstream, s.method, s.path, s.rawPath, s.query)
		}
		if s.header.Get("X-Signed") != "1" || s.header.Get("Traceparent") == "" {
			t.Errorf("%s: header = %v, want the attempt's as offered", s.upstream, s.header)
		}
		if s.body != "order=42" || s.length != 8 || s.replay != "order=42" {
			t.Errorf("%s: body %q length %d replay %q, want order=42", s.upstream, s.body, s.length, s.replay)
		}
		if s.key != (snapshot.MirrorKey{Key: 42}) || !s.deadline.Equal(o.Deadline) || s.stripe != 3 {
			t.Errorf("%s: key %+v deadline %v stripe %d", s.upstream, s.key, s.deadline, s.stripe)
		}
	}
	f.legs.mu.Lock()
	keys := slices.Clone(f.legs.keys)
	f.legs.mu.Unlock()
	if len(keys) != 2 || keys[0].upstream != "audit" || keys[1].upstream != "shadow" || keys[0].vars != o.Vars || keys[1].vars != o.Vars {
		t.Errorf("MirrorKey calls = %+v, want audit then shadow over the offer's variables", keys)
	}
	if u := f.budget.Used(); u != 0 {
		t.Errorf("budget used = %d after the legs, want 0", u)
	}
}

// TestBufferedBodyCopy: a replayable body is copied at Offer through
// GetBody, reserved from the budget while a leg holds it, and the attempt
// keeps its own body.
func TestBufferedBodyCopy(t *testing.T) {
	f := newFixture(t, nil, 1<<20)
	var held atomic.Int64
	f.legs.send = func(context.Context, *snapshot.MirrorCopy) error {
		held.Store(f.budget.Used())
		return nil
	}
	m := newMirror("shadow", 100, 1000)
	req := attempt(http.MethodPut, "hello mirror", true)
	orig := req.Body
	f.offer(req, m)
	if req.Body != orig {
		t.Error("Offer replaced a replayable body")
	}
	f.stop()
	if got := m.counts(); got != sent {
		t.Fatalf("counts = %v, want 1 sent", got)
	}
	if s := f.legs.list()[0]; s.body != "hello mirror" || s.length != 12 || s.replay != "hello mirror" {
		t.Errorf("leg body %q length %d replay %q", s.body, s.length, s.replay)
	}
	if held.Load() != body.Increment || f.budget.Used() != 0 {
		t.Errorf("budget used %d during the leg and %d after, want %d and 0", held.Load(), f.budget.Used(), body.Increment)
	}
}

// TestStreamedBodyCopy: a streamed body is copied as the attempt reads it,
// through a reader Offer puts in its place, and the leg is sent once the
// attempt read it to its end, with the trailer values the attempt sent.
func TestStreamedBodyCopy(t *testing.T) {
	f := newFixture(t, nil, 1<<20)
	m := newMirror("shadow", 100, 1000)
	req := attempt(http.MethodPost, "hello stream", false)
	req.Trailer, req.ContentLength = http.Header{"X-Checksum": nil}, -1
	src := req.Body
	req.Body = &trailerFill{ReadCloser: src, trailer: req.Trailer}
	f.offer(req, m)
	b, err := io.ReadAll(req.Body)
	if err != nil || string(b) != "hello stream" {
		t.Fatalf("attempt read %q, %v; want hello stream", b, err)
	}
	_ = req.Body.Close()
	waitFor(t, "the leg", func() bool { return m.counts() == sent })
	s := f.legs.list()[0]
	if s.body != "hello stream" || s.length != -1 || s.replay != "hello stream" {
		t.Errorf("leg body %q length %d replay %q", s.body, s.length, s.replay)
	}
	if got := s.trailer.Get("X-Checksum"); got != "c0ffee" {
		t.Errorf("leg trailer = %v, want X-Checksum c0ffee", s.trailer)
	}
	f.stop()
	if f.budget.Used() != 0 {
		t.Errorf("budget used = %d after the leg, want 0", f.budget.Used())
	}
}

// trailerFill fills the trailer values at the body's end, as the Upstream
// layer's trailer reader does.
type trailerFill struct {
	io.ReadCloser
	trailer http.Header
}

func (r *trailerFill) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		r.trailer.Set("X-Checksum", "c0ffee")
	}
	return n, err
}

// errBody returns data then fails.
type errBody struct{ data []byte }

func (b *errBody) Read(p []byte) (int, error) {
	if len(b.data) > 0 {
		n := copy(p, b.data)
		b.data = b.data[n:]
		return n, nil
	}
	return 0, errors.New("client went away")
}

func (*errBody) Close() error { return nil }

// TestStreamedBodyIncomplete: a streamed body the attempt did not send to
// its end, closed early or failed, drops every mirror of the copy, and
// its memory goes back to the budget.
func TestStreamedBodyIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name string
		send func(req *http.Request)
	}{
		{"closed early", func(req *http.Request) {
			_, _ = io.ReadFull(req.Body, make([]byte, 5))
			_ = req.Body.Close()
		}},
		{"failed", func(req *http.Request) {
			_, _ = io.ReadAll(req.Body)
			_ = req.Body.Close()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil, 1<<20)
			a, b := newMirror("audit", 100, 1000), newMirror("shadow", 100, 1000)
			req := attempt(http.MethodPost, "hello stream", false)
			if tc.name == "failed" {
				req.Body = &errBody{data: []byte("hello")}
			}
			f.offer(req, a, b)
			tc.send(req)
			f.stop()
			if a.counts() != dropped || b.counts() != dropped || len(f.legs.list()) != 0 {
				t.Errorf("counts %v %v, legs %d; want both dropped, none sent", a.counts(), b.counts(), len(f.legs.list()))
			}
			if f.budget.Used() != 0 || a.InFlight.Load() != 0 || f.pins.pinned.Load() != 0 {
				t.Errorf("budget %d, in flight %d, pins %d; want 0", f.budget.Used(), a.InFlight.Load(), f.pins.pinned.Load())
			}
		})
	}
}

// TestBudgetRefusesCopy: a copy the buffer budget refuses drops its
// mirrors, at Offer for a declared length, mid-stream for a chunked body,
// whose attempt still sends every byte.
func TestBudgetRefusesCopy(t *testing.T) {
	t.Run("declared length", func(t *testing.T) {
		f := newFixture(t, nil, 2*body.Increment)
		m := newMirror("shadow", 100, 1000)
		req := attempt(http.MethodPost, strings.Repeat("a", 1<<20), true)
		orig := req.Body
		f.offer(req, m)
		if m.counts() != dropped || m.InFlight.Load() != 0 || req.Body != orig {
			t.Errorf("counts %v, in flight %d, body replaced %t; want dropped at Offer", m.counts(), m.InFlight.Load(), req.Body != orig)
		}
		f.stop()
		if len(f.legs.list()) != 0 || f.budget.Used() != 0 {
			t.Errorf("legs %d, budget %d; want none and 0", len(f.legs.list()), f.budget.Used())
		}
	})
	t.Run("chunked", func(t *testing.T) {
		f := newFixture(t, nil, 2*body.Increment)
		m := newMirror("shadow", 100, 1000)
		payload := strings.Repeat("b", 8*body.Increment)
		req := attempt(http.MethodPost, payload, false)
		req.ContentLength = -1
		f.offer(req, m)
		b, err := io.ReadAll(req.Body)
		if err != nil || string(b) != payload {
			t.Fatalf("attempt read %d bytes, %v; want all %d", len(b), err, len(payload))
		}
		_ = req.Body.Close()
		f.stop()
		if m.counts() != dropped || len(f.legs.list()) != 0 || f.budget.Used() != 0 {
			t.Errorf("counts %v, legs %d, budget %d; want dropped, none, 0", m.counts(), len(f.legs.list()), f.budget.Used())
		}
	})
}

// TestPinRefused: a leg past the Drain deadline pins nothing and is
// dropped, giving back its slot and its share of the copy.
func TestPinRefused(t *testing.T) {
	f := newFixture(t, nil, 1<<20)
	f.pins.refuse.Store(true)
	m := newMirror("shadow", 100, 1000)
	f.offer(attempt(http.MethodPost, "order=42", true), m)
	f.stop()
	if m.counts() != dropped || m.InFlight.Load() != 0 || f.budget.Used() != 0 || len(f.legs.list()) != 0 {
		t.Errorf("counts %v, in flight %d, budget %d, legs %d; want dropped and nothing held", m.counts(), m.InFlight.Load(), f.budget.Used(), len(f.legs.list()))
	}
}

// TestRunEndsLegs: the end of Run's context ends every running leg, which
// counts failed, and Offer starts no leg afterwards.
func TestRunEndsLegs(t *testing.T) {
	f := newFixture(t, nil, 1<<20)
	started := make(chan struct{}, 1)
	var cause atomic.Value
	f.legs.send = func(ctx context.Context, _ *snapshot.MirrorCopy) error {
		started <- struct{}{}
		<-ctx.Done()
		cause.Store(context.Cause(ctx))
		return context.Cause(ctx)
	}
	m := newMirror("shadow", 100, 1000)
	f.offer(attempt(http.MethodGet, "", false), m)
	<-started
	f.stop()
	if c, _ := cause.Load().(error); !errors.Is(c, errEnded) || m.counts() != failed {
		t.Errorf("cause %v, counts %v; want errEnded and 1 failed", c, m.counts())
	}
	f.offer(attempt(http.MethodGet, "", false), m)
	if got := m.counts(); got != [emit.NumMirrorResults]uint64{0, 1, 1} || len(f.legs.list()) != 1 || m.InFlight.Load() != 0 {
		t.Errorf("after Run: counts %v, legs %d, in flight %d; want 1 dropped and no new leg", got, len(f.legs.list()), m.InFlight.Load())
	}
}

// TestEndingProtocolEndsLeg: the ending protocol ends a leg through its
// pinned record, as it ends the request's other work.
func TestEndingProtocolEndsLeg(t *testing.T) {
	f := newFixture(t, nil, 1<<20)
	started := make(chan struct{}, 1)
	var cause atomic.Value
	f.legs.send = func(ctx context.Context, _ *snapshot.MirrorCopy) error {
		started <- struct{}{}
		<-ctx.Done()
		cause.Store(context.Cause(ctx))
		return context.Cause(ctx)
	}
	m := newMirror("shadow", 100, 1000)
	f.offer(attempt(http.MethodGet, "", false), m)
	<-started
	f.pins.endAll()
	waitFor(t, "the ended leg", func() bool { return f.pins.pinned.Load() == 0 })
	if c, _ := cause.Load().(error); !errors.Is(c, errEnded) || m.counts() != failed {
		t.Errorf("cause %v, counts %v; want errEnded and 1 failed", c, m.counts())
	}
}

// TestLegDeadline: the Route deadline bounds a leg on the Runner's clock:
// an attempt still running fails with the deadline, and a leg still
// waiting for a streamed copy is dropped.
func TestLegDeadline(t *testing.T) {
	t.Run("sending", func(t *testing.T) {
		f := newFixture(t, nil, 1<<20)
		var cause atomic.Value
		f.legs.send = func(ctx context.Context, _ *snapshot.MirrorCopy) error {
			<-ctx.Done()
			cause.Store(context.Cause(ctx))
			return context.Cause(ctx)
		}
		m := newMirror("shadow", 100, 1000)
		f.offer(attempt(http.MethodGet, "", false), m)
		waitFor(t, "the leg's request", func() bool { return len(f.legs.list()) == 1 })
		if err := f.clk.BlockUntil(t.Context(), 1); err != nil {
			t.Fatal(err)
		}
		f.clk.Advance(15 * time.Second)
		waitFor(t, "the leg's end", func() bool { return m.InFlight.Load() == 0 })
		if c, _ := cause.Load().(error); !errors.Is(c, context.DeadlineExceeded) || m.counts() != failed {
			t.Errorf("cause %v, counts %v; want the deadline and 1 failed", c, m.counts())
		}
	})
	t.Run("waiting for the copy", func(t *testing.T) {
		f := newFixture(t, nil, 1<<20)
		m := newMirror("shadow", 100, 1000)
		f.offer(attempt(http.MethodPost, "never read", false), m)
		if err := f.clk.BlockUntil(t.Context(), 1); err != nil {
			t.Fatal(err)
		}
		f.clk.Advance(15 * time.Second)
		waitFor(t, "the leg's end", func() bool { return m.InFlight.Load() == 0 })
		if m.counts() != dropped || len(f.legs.list()) != 0 {
			t.Errorf("counts %v, legs %d; want dropped, none sent", m.counts(), len(f.legs.list()))
		}
	})
}

// TestLegPanic: a leg that panics is recovered, logged with its stack at
// error and counted failed, and gives back what it held.
func TestLegPanic(t *testing.T) {
	f := newFixture(t, nil, 1<<20)
	f.legs.send = func(context.Context, *snapshot.MirrorCopy) error { panic("boom") }
	m := newMirror("shadow", 100, 1000)
	f.offer(attempt(http.MethodPost, "order=42", true), m)
	f.stop()
	if m.counts() != failed || m.InFlight.Load() != 0 || f.pins.pinned.Load() != 0 || f.budget.Used() != 0 {
		t.Errorf("counts %v, in flight %d, pins %d, budget %d", m.counts(), m.InFlight.Load(), f.pins.pinned.Load(), f.budget.Used())
	}
	if l := f.logs.String(); !strings.Contains(l, "level=ERROR") || !strings.Contains(l, "mirror leg panic recovered") ||
		!strings.Contains(l, "upstream=shadow") || !strings.Contains(l, "stack=") {
		t.Errorf("log = %q, want the recovered panic with its stack", l)
	}
}

// TestOfferWithoutLegs: an offer without a snapshot, legs or request
// copies nothing.
func TestOfferWithoutLegs(t *testing.T) {
	f := newFixture(t, nil, 1<<20)
	m := newMirror("shadow", 100, 1000)
	f.r.Offer(nil)
	f.r.Offer(&Offer{Mirrors: []*snapshot.Mirror{m.Mirror}, Request: attempt(http.MethodGet, "", false)})
	f.r.Offer(&Offer{Snapshot: &snapshot.Snapshot{}, Mirrors: []*snapshot.Mirror{m.Mirror}, Request: attempt(http.MethodGet, "", false)})
	f.r.Offer(&Offer{Snapshot: f.snap, Mirrors: []*snapshot.Mirror{m.Mirror}})
	f.stop()
	if m.counts() != ([emit.NumMirrorResults]uint64{}) || m.InFlight.Load() != 0 {
		t.Errorf("counts %v, in flight %d; want nothing", m.counts(), m.InFlight.Load())
	}
}
