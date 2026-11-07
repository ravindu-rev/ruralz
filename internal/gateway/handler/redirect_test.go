// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/handler/handlertest"
	"github.com/ravindu-rev/ruralz/internal/gateway/redirect"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// compileRedirect compiles the redirect r of an exact Route.
func compileRedirect(t *testing.T, r v1alpha1.RouteRedirect) snapshot.Redirector {
	t.Helper()
	rt := &v1alpha1.Route{}
	rt.Metadata.Name = "moved"
	rt.Spec.Match.Path = &v1alpha1.PathMatch{Exact: "/v1/a"}
	rt.Spec.Redirect = &r
	rd, err := redirect.Compile(rt, nil, expr.Site{Place: expr.PlaceRedirectLocation})
	if err != nil {
		t.Fatalf("redirect.Compile: %v", err)
	}
	return rd
}

// fakeRedirect is a Redirector recording the requests it built a
// Location for.
type fakeRedirect struct {
	status int
	loc    string
	err    error
	calls  atomic.Int32
	last   snapshot.RedirectRequest
	path   string
}

func (r *fakeRedirect) Status() int { return r.status }

func (r *fakeRedirect) Location(in *snapshot.RedirectRequest) (string, error) {
	r.calls.Add(1)
	r.last = *in
	if in.Vars != nil && in.Vars.Request != nil {
		r.path = in.Vars.Request.Path
	}
	return r.loc, r.err
}

// phaseLog returns a Filter that records "<name> <Phase>" for each Phase
// it runs in.
func phaseLog(seen *[]string, name string) filter.Filter {
	return filterFunc(func(_ context.Context, p phase.Phase, _ filter.Exchange) filter.Result {
		*seen = append(*seen, name+" "+p.String())
		return filter.Next()
	})
}

// TestRedirectAnswers checks a redirect Route
// (docs/architecture/09-traffic-management-and-resilience.md
// "Redirects"): after onRequestHeaders it answers with its status and the
// Location built from the request, no content and the Node's Date; the
// response Phases run on it; onRequestBody, onRoute and the Forwarder never
// run, and the request body is never read.
func TestRedirectAnswers(t *testing.T) {
	k := newKit(t)
	var seen []string
	var forwarded atomic.Bool
	fwd := handlertest.ForwardFunc(func(context.Context, *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		forwarded.Store(true)
		return nil, errors.New("unreachable")
	})
	k.publish(handlertest.Spec{Routes: []handlertest.Route{{
		Name: "moved", Forward: fwd,
		Redirect: compileRedirect(t, v1alpha1.RouteRedirect{Status: ptrTo[int32](301), Scheme: v1alpha1.RedirectSchemeHTTPS}),
		Policies: []*snapshot.Policy{
			handlertest.Policy("cors", phase.ClassCORS, v1alpha1.PolicyTypeCORS, header("X-Resp", "1"), phase.OnResponse),
			handlertest.Policy("jwt", phase.ClassAuth, v1alpha1.PolicyTypeAuthJWT, phaseLog(&seen, "jwt"), phase.OnRequestHeaders),
			handlertest.Policy("tr", phase.ClassTransform, v1alpha1.PolicyTypeTransformRequest, phaseLog(&seen, "tr"), phase.OnRequestBody, phase.OnRoute),
		},
	}}})
	body := newTracked("payload")
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://api.shop.example:8080/v1/a?q=1&r=%20", body)
	res := k.do(req)
	if res.Code != http.StatusMovedPermanently || res.Header().Get("Location") != "https://api.shop.example/v1/a?q=1&r=%20" {
		t.Fatalf("response = %d Location %q, want 301 to https://api.shop.example/v1/a?q=1&r=%%20", res.Code, res.Header().Get("Location"))
	}
	if res.Body.Len() != 0 || res.Header().Get("Content-Length") != "0" || res.Header().Get("Date") == "" || res.Header().Get("X-Resp") != "1" {
		t.Errorf("body %q, fields %v; want no content, Content-Length 0, the Node's Date and onResponse's X-Resp", res.Body.String(), res.Header())
	}
	if !slices.Equal(seen, []string{"jwt onRequestHeaders"}) || forwarded.Load() {
		t.Errorf("Phases %q, forwarded %t; want onRequestHeaders alone and no Forwarder", seen, forwarded.Load())
	}
	rec := k.record()
	if rec.Status != http.StatusMovedPermanently || rec.Code != "" || rec.ShortCircuitPolicy != "" || rec.RequestBytes != 0 {
		t.Errorf("record = status %d code %q policy %q request bytes %d; want 301 without code or Policy, no body read", rec.Status, rec.Code, rec.ShortCircuitPolicy, rec.RequestBytes)
	}
	if c := k.node.Responses.Codes(); len(c) != 0 {
		t.Errorf("node responses = %v, want none", c)
	}
	if ev := k.plan.ListenerHandles(handlertest.ListenerName).Requests.Events(); len(ev) != 1 || ev[0].Origin != emit.OriginNode {
		t.Errorf("listener requests = %+v, want origin node", ev)
	}
}

// TestRedirectRequestParts checks what a redirect builds its Location
// from: https on a TLS connection, the Host the client sent, the one
// normalized path, the raw query, and the request's Base variables.
func TestRedirectRequestParts(t *testing.T) {
	k := newKit(t)
	rd := &fakeRedirect{status: http.StatusFound, loc: "/elsewhere"}
	k.publish(handlertest.Spec{Routes: []handlertest.Route{{Name: "moved", Redirect: rd}}})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://Shop.Example:8443/v1/%7Euser?b=2&a=%41", nil)
	req.TLS = &tls.ConnectionState{}
	res := k.do(req)
	if res.Code != http.StatusFound || res.Header().Get("Location") != "/elsewhere" {
		t.Fatalf("response = %d Location %q, want 302 /elsewhere", res.Code, res.Header().Get("Location"))
	}
	got := rd.last
	if got.Scheme != "https" || got.Host != "Shop.Example:8443" || got.Path != "/v1/~user" || got.RawQuery != "b=2&a=%41" || rd.path != "/v1/~user" {
		t.Errorf("redirect request = %+v (variables path %q), want https Shop.Example:8443 /v1/~user b=2&a=%%41", got, rd.path)
	}
}

// TestRedirectAfterRequestHeaders checks that a redirect answers only
// after onRequestHeaders: a request an authentication or Rate Limit
// Policy refuses there gets that Policy's response and no redirect.
func TestRedirectAfterRequestHeaders(t *testing.T) {
	for _, tc := range []struct {
		name   string
		class  phase.Class
		typ    v1alpha1.PolicyType
		status int
		code   string
	}{
		{"authentication", phase.ClassAuth, v1alpha1.PolicyTypeAuthJWT, http.StatusUnauthorized, "RZ-AUTH-001"},
		{"rate limit", phase.ClassAdmission, v1alpha1.PolicyTypeRateLimit, http.StatusTooManyRequests, "RZ-RL-001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newKit(t)
			rd := &fakeRedirect{status: http.StatusFound, loc: "https://elsewhere.example/"}
			deny := filterFunc(func(context.Context, phase.Phase, filter.Exchange) filter.Result {
				return filter.Deny(tc.status, tc.code, nil)
			})
			k.publish(handlertest.Spec{Routes: []handlertest.Route{{
				Name: "moved", Redirect: rd,
				Policies: []*snapshot.Policy{handlertest.Policy("deny", tc.class, tc.typ, deny, phase.OnRequestHeaders)},
			}}})
			res := k.get("/v1/a")
			wantProblem(t, res, tc.status, tc.code)
			if res.Header().Get("Location") != "" || rd.calls.Load() != 0 {
				t.Errorf("Location %q after %d Location calls, want no redirect", res.Header().Get("Location"), rd.calls.Load())
			}
		})
	}
}

// TestRedirectHEAD checks a HEAD request to a redirect Route: the
// redirect's status and Location, no body.
func TestRedirectHEAD(t *testing.T) {
	k := newKit(t)
	k.publish(handlertest.Spec{Routes: []handlertest.Route{{Name: "moved", Redirect: &fakeRedirect{status: http.StatusPermanentRedirect, loc: "https://a.example/"}}}})
	res := k.do(httptest.NewRequestWithContext(t.Context(), http.MethodHead, "/v1/a", nil))
	if res.Code != http.StatusPermanentRedirect || res.Header().Get("Location") != "https://a.example/" || res.Body.Len() != 0 {
		t.Errorf("HEAD = %d Location %q body %q, want 308 https://a.example/ without a body", res.Code, res.Header().Get("Location"), res.Body.String())
	}
}

// TestRedirectLocationFault checks a Location the redirect cannot build:
// only a locationExpression fails, which the serve check refuses, so the
// snapshot holding it is a fault of the release, 500 RZ-RT-025.
func TestRedirectLocationFault(t *testing.T) {
	k := newKit(t)
	k.publish(handlertest.Spec{Routes: []handlertest.Route{{Name: "moved", Redirect: &fakeRedirect{status: http.StatusFound, err: redirect.ErrLocation}}}})
	res := k.get("/v1/a")
	wantProblem(t, res, http.StatusInternalServerError, codeFault)
	if res.Header().Get("Location") != "" {
		t.Errorf("Location = %q, want none", res.Header().Get("Location"))
	}
}

// ptrTo returns a pointer to v.
func ptrTo[T any](v T) *T { return &v }

// mirrorLegs is a snapshot.MirrorLegs whose legs block until release is
// closed, recording each copy.
type mirrorLegs struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	keys    []string
	copies  []snapshot.MirrorCopy
}

func newMirrorLegs() *mirrorLegs {
	return &mirrorLegs{started: make(chan struct{}, 4), release: make(chan struct{})}
}

func (l *mirrorLegs) MirrorKey(upstream string, vars *expr.Vars, _ emit.Stripe) snapshot.MirrorKey {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys = append(l.keys, upstream+" "+vars.Request.Path)
	return snapshot.MirrorKey{}
}

func (l *mirrorLegs) Mirror(ctx context.Context, _ string, c *snapshot.MirrorCopy) error {
	l.mu.Lock()
	l.copies = append(l.copies, *c)
	l.mu.Unlock()
	l.started <- struct{}{}
	select {
	case <-l.release:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// offering is a Forwarder that offers one attempt to the Outbound's
// mirror sink, when it has one, then answers 200, recording whether it
// had one.
func offering(sinks *[]bool) snapshot.Forwarder {
	inner := ok()
	return handlertest.ForwardFunc(func(ctx context.Context, o *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		*sinks = append(*sinks, o.Mirror != nil)
		if o.Mirror != nil {
			o.Mirror.MirrorAttempt(&http.Request{Method: http.MethodGet, URL: &url.URL{Path: "/m"}, Header: http.Header{}, Body: http.NoBody})
		}
		return inner.Forward(ctx, o)
	})
}

// routeMirror returns the mirror of route to shadow with the kit's handles.
func routeMirror(k *kit, route string) *snapshot.Mirror {
	return &snapshot.Mirror{Upstream: "shadow", Percent: 100, MaxInFlight: 10, InFlight: new(atomic.Int64), Metrics: k.plan.Mirror(route, "shadow")}
}

// TestMirrorSink checks which requests the Upstream layer offers to
// mirrors: those of a Route with mirrors on a Node that runs them, no
// other.
func TestMirrorSink(t *testing.T) {
	prefix := func(p string) *v1alpha1.RouteMatch { return &v1alpha1.RouteMatch{Path: &v1alpha1.PathMatch{Prefix: p}} }
	k := newKit(t, func(o *kitOptions) { o.mirrors = true })
	var sinks []bool
	legs := newMirrorLegs()
	close(legs.release)
	k.publish(handlertest.Spec{MirrorLegs: legs, Routes: []handlertest.Route{
		{Name: "mirrored", Match: prefix("/m"), Forward: offering(&sinks), Mirrors: []*snapshot.Mirror{routeMirror(k, "mirrored")}},
		{Name: "plain", Match: prefix("/p"), Forward: offering(&sinks)},
	}})
	k.get("/m")
	k.get("/p")
	plain := newKit(t)
	plain.publish(handlertest.Spec{MirrorLegs: legs, Routes: []handlertest.Route{
		{Name: "mirrored", Forward: offering(&sinks), Mirrors: []*snapshot.Mirror{routeMirror(plain, "mirrored")}},
	}})
	plain.get("/m")
	if !slices.Equal(sinks, []bool{true, false, false}) {
		t.Errorf("mirror sinks = %v, want the mirrored Route's alone on the Node with mirrors", sinks)
	}
}

// TestMirrorAttemptOffers checks the offer of a request's attempt: the
// mirror Runner gets the request's snapshot, its Route's mirrors, its
// variables (the ring-hash key), its Route deadline and stripe; the leg
// takes no in-flight unit, pins the snapshot past the request's end and
// counts once it ended.
func TestMirrorAttemptOffers(t *testing.T) {
	k := newKit(t, func(o *kitOptions) { o.mirrors = true })
	legs := newMirrorLegs()
	var sinks []bool
	s := k.publish(handlertest.Spec{MirrorLegs: legs, Routes: []handlertest.Route{
		{Name: "mirrored", Forward: offering(&sinks), Mirrors: []*snapshot.Mirror{routeMirror(k, "mirrored")}},
	}})
	if res := k.get("/m/x"); res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	<-legs.started
	if n := pins(s); n != 1 || k.units.InUse() != 0 {
		t.Errorf("pins %d, in-flight units %d after the request; want the leg's pin and no unit", n, k.units.InUse())
	}
	legs.mu.Lock()
	keys, copies := slices.Clone(legs.keys), slices.Clone(legs.copies)
	legs.mu.Unlock()
	if !slices.Equal(keys, []string{"shadow /m/x"}) || len(copies) != 1 {
		t.Fatalf("keys %q, copies %d; want shadow's key over the request's variables and one copy", keys, len(copies))
	}
	if c := copies[0]; !c.Deadline.Equal(epoch().Add(handlertest.DefaultTimeout)) || c.Stripe != k.conn.StripeV || c.Request.URL.Path != "/m" {
		t.Errorf("copy = deadline %v stripe %d path %q, want the Route deadline, the request's stripe and the attempt", c.Deadline, c.Stripe, c.Request.URL.Path)
	}
	close(legs.release)
	waitUntil(t, "the leg's end", func() bool { return pins(s) == 0 })
	if n := k.plan.MirrorHandles("mirrored", "shadow").Results[emit.MirrorSent].Value(); n != 1 {
		t.Errorf("mirrors sent = %d, want 1", n)
	}
}

// waitUntil polls cond with a real-time bound; it only waits for
// goroutines the test started, never for fake time.
func waitUntil(t *testing.T, what string, cond func() bool) {
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
