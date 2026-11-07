// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/balance"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/mirror"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// offered is one attempt a leg offered to its mirrors, as it was at the
// offer.
type offered struct {
	req    *http.Request
	host   string
	signed string
}

// offers is a snapshot.MirrorSink recording each offered attempt, then
// handing it to next.
type offers struct {
	mu   sync.Mutex
	got  []offered
	next func(r *http.Request)
}

func (o *offers) MirrorAttempt(r *http.Request) {
	o.mu.Lock()
	o.got = append(o.got, offered{req: r, host: r.URL.Host, signed: r.Header.Get("X-Signed")})
	o.mu.Unlock()
	if o.next != nil {
		o.next(r)
	}
}

func (o *offers) list() []offered {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]offered(nil), o.got...)
}

// legPins pins every mirror leg.
type legPins struct{ n atomic.Int64 }

func (p *legPins) PinLeg(*snapshot.Snapshot, emit.Stripe, *snapshot.PinnedRequest) bool {
	p.n.Add(1)
	return true
}

func (p *legPins) Unpin(*snapshot.Snapshot, emit.Stripe, *snapshot.PinnedRequest) { p.n.Add(-1) }

// startRunner returns a running mirror Runner on the fixture's clock; the
// test's end stops it before the fixture's Sets close.
func startRunner(t *testing.T, f *fixture) *mirror.Runner {
	t.Helper()
	r, err := mirror.New(mirror.Deps{Snapshots: &legPins{}, Budget: body.NewBudget(1<<20, &gauge{}), Clock: f.clk})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("mirror Run: %v", err)
		}
	})
	return r
}

// mirrorCounts are a mirror's ruralz_upstream_mirrors_total handles.
type mirrorCounts [emit.NumMirrorResults]*counter

// newRouteMirror returns a mirror to upstream copying every request.
func newRouteMirror(upstream string) (*snapshot.Mirror, *mirrorCounts) {
	c := &mirrorCounts{}
	m := &snapshot.Mirror{Upstream: upstream, Percent: 100, MaxInFlight: 10, InFlight: new(atomic.Int64), Metrics: &emit.MirrorMetrics{}}
	for i := range c {
		c[i] = &counter{}
		m.Metrics.Results[i] = c[i]
	}
	return m, c
}

// mirrorRequest returns a mirror leg's request as the Runner builds it,
// with body b replayable; "" has no body.
func mirrorRequest(method, b string) *http.Request {
	r := &http.Request{
		Method: method, URL: &url.URL{Path: "/v1/a/b", RawPath: "/v1/a%2Fb", RawQuery: "x=1&y=%20"},
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"X-Signed": {"1"}, "Traceparent": {"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}},
		Body:   http.NoBody,
	}
	if b != "" {
		r.Body, r.ContentLength = io.NopCloser(strings.NewReader(b)), int64(len(b))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(b)), nil }
	}
	return r
}

// bodyServer is a loopback Upstream recording the body of each request
// before answering with h.
func bodyServer(t *testing.T, bodies *atomic.Value, h http.HandlerFunc) *server {
	t.Helper()
	return newServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies.Store(string(b))
		h(w, r)
	})
}

// TestLegOffersFirstSentAttempt checks the mirror offer: a leg offers the
// first attempt it sends, as its onUpstreamRequest Policies left it (a
// replaced request included), once, never its retries.
func TestLegOffersFirstSentAttempt(t *testing.T) {
	srv := newServer(t, status(http.StatusOK, "ok"))
	const refused = "127.0.0.1:1"
	f := newFixture(t, refused)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", refused, srv.addr)})
	h := &hooks{onRequest: func(leg *filter.Leg) *filter.Response {
		r := leg.Request.Clone(leg.Request.Context())
		r.Header.Set("X-Signed", "attempt-"+strconv.Itoa(leg.Attempt))
		leg.Request = r
		return nil
	}}
	sink := &offers{}
	o := outbound(exchange(http.MethodPost, "/"), h)
	o.Mirror = sink
	r, err := f.upstream(s, "orders").Leg(t.Context(), o, Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	if got := readAll(t, r); got != "ok" || r.Attempts != 2 {
		t.Fatalf("leg %q after %d attempts, want ok after a retry", got, r.Attempts)
	}
	got := sink.list()
	if len(got) != 1 || got[0].signed != "attempt-1" || got[0].host != refused {
		t.Errorf("offers = %+v, want the first attempt to %s as onUpstreamRequest replaced it, once", got, refused)
	}
}

// TestLegOffersNothingUnsent checks that a leg offers no attempt it does
// not send to the Upstream: one an onUpstreamRequest Policy answered, and
// no attempt of a composition step.
func TestLegOffersNothingUnsent(t *testing.T) {
	srv := newServer(t, status(http.StatusOK, "ok"))
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	up := f.upstream(s, "orders")
	sink := &offers{}
	h := &hooks{onRequest: func(*filter.Leg) *filter.Response {
		return &filter.Response{Status: http.StatusUnauthorized, Code: "RZ-AUTH-020"}
	}}
	o := outbound(exchange(http.MethodGet, "/"), h)
	o.Mirror = sink
	if r, err := up.Leg(t.Context(), o, Target{}); err != nil || r.Generated == nil {
		t.Fatalf("Leg = %+v, %v; want the generated response", r, err)
	}
	o = outbound(exchange(http.MethodGet, "/"), nil)
	o.Mirror = sink
	r, err := up.Leg(t.Context(), o, Target{Step: "inventory"})
	if err != nil {
		t.Fatalf("step Leg: %v", err)
	}
	readAll(t, r)
	if got := sink.list(); len(got) != 0 {
		t.Errorf("offers = %+v, want none", got)
	}
}

// TestSetMirrorSendsCopy checks a mirror leg's one attempt: the copy goes
// to the mirror Upstream's Endpoint with its authority as Host, the path,
// raw path, query, header fields and body as given, counts in its
// attempts, starts no span, is never retried, and any response is sent.
func TestSetMirrorSendsCopy(t *testing.T) {
	var bodies atomic.Value
	shadow := bodyServer(t, &bodies, status(http.StatusServiceUnavailable, "busy"))
	f := newFixture(t)
	met := newMetrics()
	u := newUpstream("shadow", shadow.addr)
	u.Spec.Retries = &v1alpha1.Retries{Attempts: ptr[int32](2)}
	s := f.compile(nil, Config{Upstream: u, Metrics: met.m})
	err := s.Mirror(t.Context(), "shadow", &snapshot.MirrorCopy{Request: mirrorRequest(http.MethodPut, "order=42"), Deadline: epoch().Add(15 * time.Second)})
	if err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	got := shadow.last()
	if shadow.hits.Load() != 1 || got.Method != http.MethodPut || got.RequestURI != "/v1/a%2Fb?x=1&y=%20" || got.Host != shadow.addr {
		t.Errorf("%d requests, last %s %s Host %s; want one PUT /v1/a%%2Fb?x=1&y=%%20 Host %s", shadow.hits.Load(), got.Method, got.RequestURI, got.Host, shadow.addr)
	}
	if got.Header.Get("X-Signed") != "1" || got.Header.Get("Traceparent") == "" {
		t.Errorf("header = %v, want the copy's fields", got.Header)
	}
	if b, _ := bodies.Load().(string); b != "order=42" {
		t.Errorf("body = %q, want order=42", b)
	}
	if a := met.attempts.list(); len(a) != 1 || a[0] != "503" {
		t.Errorf("attempts = %v, want [503]", a)
	}
	if spans := f.tracer.list(); len(spans) != 0 || met.retries.get() != 0 {
		t.Errorf("spans %v, retries %d; want none", spans, met.retries.get())
	}
	if n := f.upstream(s, "shadow").rt.res.Bulkhead().InFlight(); n != 0 {
		t.Errorf("bulkhead slots held = %d, want 0", n)
	}
}

// TestSetMirrorGates checks the gates a mirror leg passes before any
// attempt: an Upstream the snapshot does not hold, no Endpoint
// (RZ-UP-008), a passed Route deadline, an open breaker (RZ-UP-005) and a
// bulkhead without a free slot (RZ-UP-006), which a mirror never queues
// for.
func TestSetMirrorGates(t *testing.T) {
	copyOf := func(deadline time.Time) *snapshot.MirrorCopy {
		return &snapshot.MirrorCopy{Request: mirrorRequest(http.MethodGet, ""), Deadline: deadline}
	}
	later := epoch().Add(15 * time.Second)
	t.Run("unknown Upstream", func(t *testing.T) {
		f := newFixture(t)
		s := f.compile(nil)
		if err := s.Mirror(t.Context(), "shadow", copyOf(later)); !errors.Is(err, filter.ErrNotAvailable) {
			t.Errorf("Mirror = %v, want ErrNotAvailable", err)
		}
	})
	t.Run("no Endpoint", func(t *testing.T) {
		f := newFixture(t)
		s := f.compile(nil, Config{Upstream: newUpstream("shadow")})
		if err := s.Mirror(t.Context(), "shadow", copyOf(later)); codeOf(err) != "RZ-UP-008" {
			t.Errorf("Mirror = %v, want RZ-UP-008", err)
		}
	})
	t.Run("Route deadline passed", func(t *testing.T) {
		srv := newServer(t, status(http.StatusOK, ""))
		f := newFixture(t)
		s := f.compile(nil, Config{Upstream: newUpstream("shadow", srv.addr)})
		if err := s.Mirror(t.Context(), "shadow", copyOf(epoch())); !errors.Is(err, errMirrorExpired) || srv.hits.Load() != 0 {
			t.Errorf("Mirror = %v after %d requests, want the passed deadline and none", err, srv.hits.Load())
		}
	})
	t.Run("breaker open", func(t *testing.T) {
		srv := newServer(t, status(http.StatusServiceUnavailable, ""))
		f := newFixture(t)
		u := newUpstream("shadow", srv.addr)
		u.Spec.Retries = &v1alpha1.Retries{Attempts: ptr[int32](0)}
		u.Spec.CircuitBreaker = &v1alpha1.CircuitBreaker{ConsecutiveFailures: ptr[int32](1)}
		s := f.compile(nil, Config{Upstream: u})
		up := f.upstream(s, "shadow")
		runLegs(t, f, up, 20)
		hits := srv.hits.Load()
		if err := s.Mirror(t.Context(), "shadow", copyOf(f.clk.Now().Add(15*time.Second))); codeOf(err) != "RZ-UP-005" || srv.hits.Load() != hits {
			t.Errorf("Mirror = %v with %d more requests, want RZ-UP-005 and none", err, srv.hits.Load()-hits)
		}
	})
	t.Run("bulkhead full", func(t *testing.T) {
		release := make(chan struct{})
		srv := newServer(t, blocking(release))
		f := newFixture(t)
		u := newUpstream("shadow", srv.addr)
		u.Spec.Retries = &v1alpha1.Retries{Attempts: ptr[int32](0)}
		u.Spec.CircuitBreaker = &v1alpha1.CircuitBreaker{MaxConnections: ptr[int32](1), MaxPendingRequests: ptr[int32](4)}
		s := f.compile(nil, Config{Upstream: u})
		up := f.upstream(s, "shadow")
		first := goLeg(t.Context(), up, outbound(exchange(http.MethodGet, "/"), nil))
		waitFor(t, "the leg's attempt", func() bool { return srv.hits.Load() == 1 })
		if err := s.Mirror(t.Context(), "shadow", copyOf(later)); codeOf(err) != "RZ-UP-006" || up.rt.res.Bulkhead().Pending() != 0 {
			t.Errorf("Mirror = %v with %d waiting, want RZ-UP-006 at once, never queued", err, up.rt.res.Bulkhead().Pending())
		}
		close(release)
		res := await(t, first)
		if res.err != nil {
			t.Fatalf("Leg: %v", res.err)
		}
		readAll(t, res.r)
	})
}

// TestSetMirrorTimeout checks that a mirror leg's attempt is bounded by
// retries.perTryTimeout on the Upstream layer's clock, as a leg's only
// attempt is.
func TestSetMirrorTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := newServer(t, blocking(release))
	f := newFixture(t)
	met := newMetrics()
	u := newUpstream("shadow", srv.addr)
	u.Spec.Retries = &v1alpha1.Retries{Attempts: ptr[int32](2), PerTryTimeout: dur(time.Second)}
	s := f.compile(nil, Config{Upstream: u, Metrics: met.m})
	done := make(chan error, 1)
	go func() {
		done <- s.Mirror(t.Context(), "shadow", &snapshot.MirrorCopy{Request: mirrorRequest(http.MethodGet, ""), Deadline: epoch().Add(15 * time.Second)})
	}()
	waitFor(t, "the attempt", func() bool { return srv.hits.Load() == 1 })
	f.clk.Advance(time.Second)
	select {
	case err := <-done:
		if err == nil {
			t.Error("Mirror = nil after the per-try timeout")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Mirror did not return")
	}
	if a := met.attempts.list(); len(a) != 1 || a[0] != "timeout" || srv.hits.Load() != 1 {
		t.Errorf("attempts = %v after %d requests, want [timeout] and no retry", a, srv.hits.Load())
	}
}

// TestSetMirrorConnectFailure checks that a mirror leg whose Endpoint
// refuses the connection fails with its one attempt, retried nowhere.
func TestSetMirrorConnectFailure(t *testing.T) {
	const refused = "127.0.0.1:2"
	srv := newServer(t, status(http.StatusOK, ""))
	f := newFixture(t, refused)
	met := newMetrics()
	s := f.compile(nil, Config{Upstream: newUpstream("shadow", refused, srv.addr), Metrics: met.m})
	err := s.Mirror(t.Context(), "shadow", &snapshot.MirrorCopy{Request: mirrorRequest(http.MethodPost, "x"), Deadline: epoch().Add(15 * time.Second)})
	if err == nil {
		t.Fatal("Mirror = nil to a refused Endpoint")
	}
	if a := met.attempts.list(); len(a) != 1 || a[0] != "connect" || srv.hits.Load() != 0 || f.dial.count(refused) != 1 {
		t.Errorf("attempts %v, %d requests elsewhere, %d dials; want one connect attempt", a, srv.hits.Load(), f.dial.count(refused))
	}
}

// TestMirrorKey checks a mirror leg's ring-hash key: its Upstream's
// hashKey over the request's variables, picking the Endpoint a leg of the
// same key picks; a runtime error picks at random and counts in
// ruralz_upstream_cel_errors_total{field="hashKey"}; another algorithm or
// an Upstream the snapshot does not hold has no key.
func TestMirrorKey(t *testing.T) {
	srvs, _ := orderedServers(t, 3)
	f := newFixture(t)
	met := newMetrics()
	u := newUpstream("shadow", srvs[0].addr, srvs[1].addr, srvs[2].addr)
	u.Spec.LoadBalancing = &v1alpha1.LoadBalancing{Algorithm: ptr(v1alpha1.LoadBalancingRingHash), HashKey: `request.headers["x-user"]`}
	prg := celtest.Program(t, celtest.At(expr.PlaceHashKey), u.Spec.LoadBalancing.HashKey)
	s := f.compile(nil, Config{Upstream: u, HashKey: prg, Metrics: met.m}, Config{Upstream: newUpstream("plain", srvs[0].addr)})
	vars := func(user string) *expr.Vars {
		h := http.Header{}
		if user != "" {
			h.Set("X-User", user)
		}
		return &expr.Vars{Request: &expr.Request{Method: http.MethodGet, Path: "/", Header: h}}
	}
	if k := s.MirrorKey("shadow", vars("alice"), 0); k != (snapshot.MirrorKey{Key: balance.HashKey("alice")}) {
		t.Errorf("MirrorKey(alice) = %+v, want the hash of alice", k)
	}
	if k := s.MirrorKey("shadow", vars(""), 0); k != (snapshot.MirrorKey{Random: true}) || met.cel[emit.CELHashKey].get() != 1 {
		t.Errorf("MirrorKey without the header = %+v with %d errors, want Random and 1", k, met.cel[emit.CELHashKey].get())
	}
	if k := s.MirrorKey("plain", vars("alice"), 0); k != (snapshot.MirrorKey{}) {
		t.Errorf("MirrorKey(plain) = %+v, want none", k)
	}
	if k := s.MirrorKey("nope", vars("alice"), 0); k != (snapshot.MirrorKey{}) {
		t.Errorf("MirrorKey(nope) = %+v, want none", k)
	}
	x := exchange(http.MethodGet, "/")
	x.Hdr.Set("X-User", "alice")
	x.VarsV.Request.Header = x.Hdr
	r, err := f.upstream(s, "shadow").Leg(t.Context(), outbound(x, nil), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	readAll(t, r)
	err = s.Mirror(t.Context(), "shadow", &snapshot.MirrorCopy{Request: mirrorRequest(http.MethodGet, ""), Key: s.MirrorKey("shadow", vars("alice"), 0), Deadline: epoch().Add(15 * time.Second)})
	if err != nil {
		t.Fatalf("Mirror: %v", err)
	}
	for _, srv := range srvs {
		if want := int64(0); srv.addr == r.Endpoint {
			if srv.hits.Load() != 2 {
				t.Errorf("%s got %d requests, want the leg's and the mirror's", srv.addr, srv.hits.Load())
			}
		} else if srv.hits.Load() != want {
			t.Errorf("%s got %d requests, want none", srv.addr, srv.hits.Load())
		}
	}
}

// TestMirrorNeverDelaysOrFailsPrimary drives the Upstream layer with the
// mirror Runner: the mirror Upstream gets what the primary's first
// attempt sent after onUpstreamRequest, with a buffered or a streamed
// body, while the primary's response comes back without waiting for a
// mirror that has not answered; a mirror that cannot connect fails alone.
func TestMirrorNeverDelaysOrFailsPrimary(t *testing.T) {
	for _, tc := range []struct {
		name string
		body func(t *testing.T) *body.Request
	}{
		{"buffered body", func(t *testing.T) *body.Request { return gatedBody(t, "order=42") }},
		{"streamed body", func(*testing.T) *body.Request { return streamBody("order=42") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			var shadowBody atomic.Value
			shadow := bodyServer(t, &shadowBody, blocking(release))
			orders := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				w.Header().Set("X-Got", string(b))
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "ok")
			})
			f := newFixture(t)
			s := f.compile(nil, Config{Upstream: newUpstream("orders", orders.addr)}, Config{Upstream: newUpstream("shadow", shadow.addr)})
			runner := startRunner(t, f)
			m, counts := newRouteMirror("shadow")
			snap := &snapshot.Snapshot{MirrorLegs: s}
			x := exchange(http.MethodPost, "/v1/orders")
			o := outbound(x, &hooks{onRequest: func(leg *filter.Leg) *filter.Response {
				leg.Request.Header.Set("X-Signed", "1")
				return nil
			}})
			o.Body = tc.body(t)
			o.Mirror = &offers{next: func(r *http.Request) {
				runner.Offer(&mirror.Offer{Snapshot: snap, Mirrors: []*snapshot.Mirror{m}, Request: r, Vars: x.VarsV, Deadline: o.Deadline})
			}}
			r, err := f.upstream(s, "orders").Leg(t.Context(), o, Target{})
			if err != nil {
				t.Fatalf("Leg: %v", err)
			}
			if got := readAll(t, r); got != "ok" || r.Header.Get("X-Got") != "order=42" {
				t.Errorf("primary %q with X-Got %q, want ok and the whole body", got, r.Header.Get("X-Got"))
			}
			waitFor(t, "the mirror's request", func() bool { return shadow.hits.Load() == 1 })
			if n := counts[emit.MirrorSent].get() + counts[emit.MirrorFailed].get(); n != 0 {
				t.Fatalf("the mirror ended before its Upstream answered: %d", n)
			}
			got := shadow.last()
			if got.Method != http.MethodPost || got.RequestURI != "/v1/orders" || got.Host != shadow.addr || got.Header.Get("X-Signed") != "1" {
				t.Errorf("mirror request %s %s Host %s X-Signed %q, want the primary's attempt to %s", got.Method, got.RequestURI, got.Host, got.Header.Get("X-Signed"), shadow.addr)
			}
			if b, _ := shadowBody.Load().(string); b != "order=42" {
				t.Errorf("mirror body = %q, want order=42", b)
			}
			close(release)
			waitFor(t, "the mirror's end", func() bool { return m.InFlight.Load() == 0 })
			if counts[emit.MirrorSent].get() != 1 {
				t.Errorf("mirror counts = sent %d dropped %d failed %d, want 1 sent", counts[0].get(), counts[1].get(), counts[2].get())
			}
		})
	}
	t.Run("mirror cannot connect", func(t *testing.T) {
		const refused = "127.0.0.1:1"
		orders := newServer(t, status(http.StatusOK, "ok"))
		f := newFixture(t, refused)
		s := f.compile(nil, Config{Upstream: newUpstream("orders", orders.addr)}, Config{Upstream: newUpstream("shadow", refused)})
		runner := startRunner(t, f)
		m, counts := newRouteMirror("shadow")
		snap := &snapshot.Snapshot{MirrorLegs: s}
		x := exchange(http.MethodGet, "/")
		o := outbound(x, nil)
		o.Mirror = &offers{next: func(r *http.Request) {
			runner.Offer(&mirror.Offer{Snapshot: snap, Mirrors: []*snapshot.Mirror{m}, Request: r, Vars: x.VarsV, Deadline: o.Deadline})
		}}
		r, err := f.upstream(s, "orders").Leg(t.Context(), o, Target{})
		if err != nil {
			t.Fatalf("Leg: %v", err)
		}
		if got := readAll(t, r); got != "ok" || r.Status != http.StatusOK {
			t.Errorf("primary = %d %q, want 200 ok", r.Status, got)
		}
		waitFor(t, "the mirror's end", func() bool { return m.InFlight.Load() == 0 })
		if counts[emit.MirrorFailed].get() != 1 || counts[emit.MirrorSent].get() != 0 {
			t.Errorf("mirror counts = sent %d dropped %d failed %d, want 1 failed", counts[0].get(), counts[1].get(), counts[2].get())
		}
	})
}
