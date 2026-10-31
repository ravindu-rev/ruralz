// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"reflect"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for the attempt context (the attempt record, armed on the leg's
// reused deadline timer, answering httptrace's key) and the copy-free
// forwarding writer (the pooled outgoing header sharing the client's
// values), the Upstream layer row of
// docs/architecture/12-performance-budgets-and-benchmarking.md "Per-stage
// latency budget".

// funcTransport is a RoundTripper over a function.
type funcTransport func(*http.Request) (*http.Response, error)

func (f funcTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// okResponse answers r with 200 and body.
func okResponse(r *http.Request, body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

// sendUpstream compiles the Upstream u (default: orders on one Endpoint)
// on a fake clock, its pools sending on tr.
func sendUpstream(t *testing.T, tr http.RoundTripper, u ...*v1alpha1.Upstream) (*Upstream, *clocktest.Fake) {
	t.Helper()
	clk := clocktest.New(epoch())
	m := NewManager(Options{Clock: clk, Rand: lowSource{}, Dial: newDial().dial, Send: tr})
	cfg := newUpstream("orders", "10.0.0.1:8080")
	if len(u) > 0 {
		cfg = u[0]
	}
	s, err := m.Compile(t.Context(), Input{Upstreams: []Config{{Upstream: cfg}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.Activate()
	up, _ := s.Upstream("orders")
	return up, clk
}

// liveOutbound is an outbound whose 15 s deadline lies ahead on the fake
// clock (outbound).
func liveOutbound(x filter.Exchange, h snapshot.LegHooks) *snapshot.Outbound { return outbound(x, h) }

// mapID identifies a header map.
func mapID(h http.Header) uintptr { return reflect.ValueOf(h).Pointer() }

// TestOutgoingHeaderSharesClientValues checks the copy-free forwarding
// writer: the outgoing request carries the client's fields without copying
// their values, capped, so a Filter appending to a field, and the Via entry
// the Node adds, never write into the client's arrays even where they have
// spare capacity; the forwarding fields and the Via entry are set, and
// hop-by-hop fields are gone.
func TestOutgoingHeaderSharesClientValues(t *testing.T) {
	var sent http.Header
	u, _ := sendUpstream(t, funcTransport(func(r *http.Request) (*http.Response, error) {
		sent = r.Header.Clone()
		return okResponse(r, "ok"), nil
	}))
	x := exchange(http.MethodGet, "/v1/orders")
	// The client's arrays: one value each, with spare capacity.
	var xa, via [4]string
	xa[0], via[0] = "a", "1.1 edge"
	x.Hdr["X-A"], x.Hdr["Via"], x.Hdr["Keep-Alive"] = xa[:1], via[:1], []string{"timeout=5"}
	h := &hooks{onRequest: func(leg *filter.Leg) *filter.Response {
		leg.Request.Header["X-A"] = append(leg.Request.Header["X-A"], "b")
		return nil
	}}
	o := liveOutbound(x, h)
	o.Via = "1.1 ruralz"
	r, err := u.Leg(t.Context(), o, Target{})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()
	if got := sent["X-A"]; !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("sent X-A = %v, want [a b]", got)
	}
	if got := sent["Via"]; !slices.Equal(got, []string{"1.1 edge", "1.1 ruralz"}) {
		t.Errorf("sent Via = %v", got)
	}
	if xa[1] != "" || via[1] != "" {
		t.Errorf("the client's arrays were written: X-A %q, Via %q", xa[:2], via[:2])
	}
	if len(x.Hdr["X-A"]) != 1 || len(x.Hdr["Via"]) != 1 {
		t.Errorf("client header changed: %v", x.Hdr)
	}
	if _, ok := sent["Keep-Alive"]; ok {
		t.Error("a hop-by-hop field was forwarded")
	}
	if got := sent["X-Forwarded-For"]; !slices.Equal(got, []string{"203.0.113.7"}) {
		t.Errorf("X-Forwarded-For = %v", got)
	}
	if sent.Get("X-Forwarded-Proto") != "https" || sent.Get("X-Forwarded-Host") != "api.shop.example" {
		t.Errorf("forwarding fields = %v", sent)
	}
}

// TestOutgoingHeaderAddsViaAndUserAgent checks the fields the writer adds
// from its own storage: the Via entry for a client without one, and an
// empty User-Agent for a client without one, so net/http adds none.
func TestOutgoingHeaderAddsViaAndUserAgent(t *testing.T) {
	var sent http.Header
	u, _ := sendUpstream(t, funcTransport(func(r *http.Request) (*http.Response, error) {
		sent = r.Header.Clone()
		return okResponse(r, ""), nil
	}))
	x := exchange(http.MethodGet, "/v1/orders")
	delete(x.Hdr, "User-Agent")
	o := liveOutbound(x, nil)
	o.Via = "1.1 ruralz"
	r, err := u.Leg(t.Context(), o, Target{})
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	if got := sent["Via"]; !slices.Equal(got, []string{"1.1 ruralz"}) {
		t.Errorf("Via = %v", got)
	}
	if got, ok := sent["User-Agent"]; !ok || !slices.Equal(got, []string{""}) {
		t.Errorf("User-Agent = %v (present %v), want one empty line", got, ok)
	}
}

// TestOutgoingHeaderRecycledAfterClose checks when a header is reused: not
// while the response of its request is open, since net/http may still read
// the request until the body is closed.
func TestOutgoingHeaderRecycledAfterClose(t *testing.T) {
	var mu sync.Mutex
	var maps []uintptr
	u, _ := sendUpstream(t, funcTransport(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		maps = append(maps, mapID(r.Header))
		mu.Unlock()
		return okResponse(r, "ok"), nil
	}))
	first, err := u.Leg(t.Context(), liveOutbound(exchange(http.MethodGet, "/a"), nil), Target{})
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		r, err := u.Leg(t.Context(), liveOutbound(exchange(http.MethodGet, "/b"), nil), Target{})
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	for i, id := range maps[1:] {
		if id == maps[0] {
			t.Fatalf("leg %d reused the header of a request whose response is still open", i+2)
		}
	}
	_ = first.Body.Close()
}

// TestOutgoingHeaderNotRecycledAfterFailedAttempt checks that a header
// whose attempt failed is never reused: the transport may still be writing
// it after RoundTrip returned (here, a goroutine reading it while later
// legs run). Under -race any reuse is a reported data race.
func TestOutgoingHeaderNotRecycledAfterFailedAttempt(t *testing.T) {
	stop := make(chan struct{})
	var readers sync.WaitGroup
	defer readers.Wait()
	defer close(stop)
	fail := true
	u, _ := sendUpstream(t, funcTransport(func(r *http.Request) (*http.Response, error) {
		if fail {
			h := r.Header
			readers.Go(func() {
				for {
					select {
					case <-stop:
						return
					default:
					}
					for k, vs := range h {
						_, _ = k, len(vs)
					}
					goruntime.Gosched()
				}
			})
			return nil, errors.New("connection reset by peer")
		}
		return okResponse(r, "ok"), nil
	}))
	o := liveOutbound(exchange(http.MethodGet, "/a"), nil)
	if _, err := u.Leg(t.Context(), o, Target{}); err == nil {
		t.Fatal("failed attempt returned no error")
	}
	fail = false
	for range 50 {
		r, err := u.Leg(t.Context(), liveOutbound(exchange(http.MethodGet, "/b"), nil), Target{})
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
	}
}

// TestAttemptContextAnswersTraceKey checks the attempt context: the request
// context answers httptrace's key with the attempt's hooks without a value
// context of its own, other keys and the cancellation reach the embedded
// cancel context, and net/http's own contexts register with it, so the
// attempt's end cancels them.
func TestAttemptContextAnswersTraceKey(t *testing.T) {
	var reqCtx context.Context
	u, _ := sendUpstream(t, funcTransport(func(r *http.Request) (*http.Response, error) {
		reqCtx = r.Context()
		return okResponse(r, "ok"), nil
	}))
	type key struct{}
	parent := context.WithValue(t.Context(), key{}, "v")
	r, err := u.Leg(parent, liveOutbound(exchange(http.MethodGet, "/a"), nil), Target{})
	if err != nil {
		t.Fatal(err)
	}
	trace := httptrace.ContextClientTrace(reqCtx)
	if trace == nil || trace.GotConn == nil {
		t.Fatal("the request context carries no ClientTrace with GotConn")
	}
	if _, isRecord := reqCtx.(*attemptRecord); !isRecord {
		t.Errorf("request context is %T, want the attempt record itself", reqCtx)
	}
	if reqCtx.Value(key{}) != "v" {
		t.Error("a parent value is lost")
	}
	child, cancelChild := context.WithCancel(reqCtx)
	defer cancelChild()
	if reqCtx.Err() != nil || child.Err() != nil {
		t.Fatal("attempt context ended before its response was closed")
	}
	_, _ = io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()
	if !errors.Is(reqCtx.Err(), context.Canceled) {
		t.Errorf("attempt context after close: %v, want canceled", reqCtx.Err())
	}
	if !errors.Is(child.Err(), context.Canceled) {
		t.Errorf("a context derived from the attempt context: %v, want canceled with it", child.Err())
	}
}

// TestAttemptContextComposesExistingTrace checks that a leg context that
// already carries a ClientTrace gets the attempt's hooks composed into it,
// on a hook set of the attempt's own: both GotConn hooks run.
func TestAttemptContextComposesExistingTrace(t *testing.T) {
	var outer int
	u, _ := sendUpstream(t, funcTransport(func(r *http.Request) (*http.Response, error) {
		if tr := httptrace.ContextClientTrace(r.Context()); tr != nil && tr.GotConn != nil {
			tr.GotConn(httptrace.GotConnInfo{})
		}
		return okResponse(r, "ok"), nil
	}))
	ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { outer++ }})
	for range 2 {
		r, err := u.Leg(ctx, liveOutbound(exchange(http.MethodGet, "/a"), nil), Target{})
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
	}
	if outer != 2 {
		t.Errorf("outer GotConn ran %d times, want 2 (once per attempt, never composed twice)", outer)
	}
}

// TestAttemptDeadlineOnReusedTimer checks that every attempt gets its own
// deadline on the leg state's reused timer: legs run one after the other
// (reusing the pooled state and its timer), each with one attempt that
// never answers, are each cut exactly at their per-try timeout, never
// before, with the attempt timeout as the context's cause.
func TestAttemptDeadlineOnReusedTimer(t *testing.T) {
	var mu sync.Mutex
	var causes []error
	started := make(chan struct{}, 8)
	cfg := newUpstream("orders", "10.0.0.1:8080")
	cfg.Spec.Retries = &v1alpha1.Retries{Attempts: ptr[int32](0), PerTryTimeout: dur(time.Second)}
	u, clk := sendUpstream(t, funcTransport(func(r *http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-r.Context().Done()
		mu.Lock()
		causes = append(causes, context.Cause(r.Context()))
		mu.Unlock()
		return nil, context.Cause(r.Context())
	}), cfg)
	cut := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(causes)
	}
	for leg := 1; leg <= 3; leg++ {
		o := outbound(exchange(http.MethodGet, "/slow"), nil)
		o.Deadline = clk.Now().Add(time.Minute)
		ch := goLeg(t.Context(), u, o)
		select {
		case <-started:
		case res := <-ch:
			t.Fatalf("leg %d returned before its attempt: %v", leg, res.err)
		case <-time.After(10 * time.Second):
			t.Fatalf("leg %d: the attempt did not start", leg)
		}
		clk.Advance(time.Second - time.Nanosecond)
		if n := cut(); n != leg-1 {
			t.Fatalf("leg %d was cut before its per-try timeout", leg)
		}
		clk.Advance(time.Nanosecond)
		res := await(t, ch)
		if codeOf(res.err) != "RZ-UP-003" {
			t.Fatalf("leg %d = %v, want RZ-UP-003", leg, res.err)
		}
		if n := cut(); n != leg {
			t.Fatalf("after leg %d, %d attempts were cut", leg, n)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for i, c := range causes {
		if !errors.Is(c, resilience.ErrAttemptTimeout) {
			t.Errorf("leg %d cause = %v, want the attempt timeout", i+1, c)
		}
	}
}
