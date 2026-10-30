// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/handler/handlertest"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestNewRequiresDeps checks that New refuses a Deps without a required
// part.
func TestNewRequiresDeps(t *testing.T) {
	k := newKit(t)
	full := Deps{
		Snapshots: k.holder, Units: k.units, PreRouting: k.pre, Exchanges: k.h.pool, Executor: k.h.exec, Tracer: k.tracer,
	}
	if _, err := New(full); err != nil {
		t.Fatalf("New(full) = %v", err)
	}
	for name, mod := range map[string]func(*Deps){
		"Snapshots":  func(d *Deps) { d.Snapshots = nil },
		"Units":      func(d *Deps) { d.Units = nil },
		"PreRouting": func(d *Deps) { d.PreRouting = nil },
		"Exchanges":  func(d *Deps) { d.Exchanges = nil },
		"Executor":   func(d *Deps) { d.Executor = nil },
		"Tracer":     func(d *Deps) { d.Tracer = nil },
	} {
		d := full
		mod(&d)
		if _, err := New(d); !errors.Is(err, ErrDeps) {
			t.Errorf("New without %s = %v, want ErrDeps", name, err)
		}
	}
}

// TestServeForwardsUpstreamResponse checks the request path end to end on
// a plain upstreams Route: the Forwarder gets the Route deadline, the
// sampling decision, the leg hooks, the client hop's Via entry and the
// Early Hints relay; the client gets the Upstream's status, fields and
// body, the Node's Date, no sniffed Content-Type; the request is counted
// and logged, and the pin and in-flight unit are released.
func TestServeForwardsUpstreamResponse(t *testing.T) {
	k := newKit(t)
	var got snapshot.Outbound
	fwd := handlertest.ForwardFunc(func(_ context.Context, o *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		got = *o
		return &snapshot.UpstreamResponse{
			Status: http.StatusCreated, Header: http.Header{"X-Up": {"1"}},
			Body: io.NopCloser(strings.NewReader("created")), Upstream: "orders", Endpoint: "10.0.0.1:80", Attempts: 2,
		}, nil
	})
	snap := k.publish(handlertest.Spec{Routes: []handlertest.Route{route("orders", fwd)}})
	res := k.get("http://api.example/v1/orders?x=1")

	if res.Code != http.StatusCreated || res.Body.String() != "created" || res.Header().Get("X-Up") != "1" {
		t.Fatalf("response = %d %q %v, want 201 created with X-Up", res.Code, res.Body.String(), res.Header())
	}
	if d := res.Header().Get("Date"); d != epoch().Format(http.TimeFormat) {
		t.Errorf("Date = %q, want the Node's %q", d, epoch().Format(http.TimeFormat))
	}
	if v, ok := res.Header()["Content-Type"]; !ok || len(v) != 0 {
		t.Errorf("Content-Type = %q (present %t), want the no-sniff marker without a value", v, ok)
	}
	if got.Via != "1.1 ruralz" || !got.Deadline.Equal(epoch().Add(handlertest.DefaultTimeout)) || got.Hooks == nil || got.Trace == nil || got.Hints == nil || got.Excluder == nil {
		t.Errorf("Outbound = Via %q Deadline %v Hooks %v Trace %v Hints %v, want 1.1 ruralz, start+15s and every hook", got.Via, got.Deadline, got.Hooks, got.Trace, got.Hints)
	}
	rec := k.record()
	if rec.Route != "orders" || rec.Status != http.StatusCreated || rec.Code != "" || rec.Upstream != "orders" || rec.Endpoint != "10.0.0.1:80" || rec.Attempts != 2 {
		t.Errorf("record = %+v, want route orders 201 from orders 10.0.0.1:80 in 2 attempts", rec)
	}
	if rec.Protocol != protoHTTP1 || rec.Listener != handlertest.ListenerName || rec.Method != http.MethodGet || rec.Host != "api.example" || rec.Path != "/v1/orders" {
		t.Errorf("record request = %+v, want http1 public GET api.example /v1/orders", rec)
	}
	if rec.ClientAddress != "192.0.2.10" || rec.Revision != snap.Revision.Digest.Short() || rec.ResponseBytes != 7 || !rec.Start.Equal(epoch()) {
		t.Errorf("record = client %q revision %q bytes %d start %v, want 192.0.2.10 %s 7 %v", rec.ClientAddress, rec.Revision, rec.ResponseBytes, rec.Start, snap.Revision.Digest.Short(), epoch())
	}
	rh := k.plan.RouteHandles("orders")
	if s := rh.Requests.Statuses(); !slices.Equal(s, []int{http.StatusCreated}) || len(rh.Duration.Values()) != 1 {
		t.Errorf("route metrics = %v and %d durations, want [201] and 1", s, len(rh.Duration.Values()))
	}
	lh := k.plan.ListenerHandles(handlertest.ListenerName)
	if ev := lh.Requests.Events(); len(ev) != 1 || ev[0] != (handlertest.ListenerRequest{Protocol: emit.ProtoHTTP1, Status: http.StatusCreated, Origin: emit.OriginUpstream}) {
		t.Errorf("listener requests = %+v, want one http1 201 upstream", ev)
	}
	if v := lh.ResponseBody.Values(); !slices.Equal(v, []uint64{7}) || len(lh.Gateway.Values()) != 1 || lh.Active.Value() != 0 {
		t.Errorf("listener body sizes %v, gateway durations %d, active %d, want [7], 1, 0", v, len(lh.Gateway.Values()), lh.Active.Value())
	}
	if c := k.node.Responses.Codes(); len(c) != 0 {
		t.Errorf("node responses = %v, want none for an Upstream response", c)
	}
	if pins(snap) != 0 || k.units.InUse() != 0 {
		t.Errorf("pins %d, in-flight units %d after the request, want 0 and 0", pins(snap), k.units.InUse())
	}
}

// TestServeKeepsUpstreamDate checks that an Upstream's Date passes
// unchanged while a generated response gets the Node's.
func TestServeKeepsUpstreamDate(t *testing.T) {
	k := newKit(t)
	up := "Wed, 21 Oct 2015 07:28:00 GMT"
	k.publish(handlertest.Spec{Routes: []handlertest.Route{route("orders", handlertest.Respond("orders", 200, http.Header{"Date": {up}}, ""))}})
	if d := k.get("/").Header().Get("Date"); d != up {
		t.Errorf("forwarded Date = %q, want the Upstream's %q", d, up)
	}
}

// TestOverloadAnswers503 checks the in-flight ceiling: a full ceiling is
// 503 RZ-RT-005 at once, before any snapshot is pinned, with no access
// record and no work, counted in the connection's listener metrics, the
// Node responses and under route _unmatched.
func TestOverloadAnswers503(t *testing.T) {
	k := newKit(t, func(o *kitOptions) { o.units = 0 })
	lh := handlertest.NewListener()
	k.conn.MetricsV = lh.M
	var called atomic.Bool
	snap := k.publish(handlertest.Spec{Routes: []handlertest.Route{route("all", handlertest.ForwardFunc(func(context.Context, *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		called.Store(true)
		return nil, errors.New("unreachable")
	}))}})
	res := k.get("/")
	wantProblem(t, res, http.StatusServiceUnavailable, codeFull)
	if called.Load() || len(k.alog.Records()) != 0 || pins(snap) != 0 {
		t.Errorf("forwarded %t, records %d, pins %d, want no work at all", called.Load(), len(k.alog.Records()), pins(snap))
	}
	if ev := lh.Requests.Events(); len(ev) != 1 || ev[0].Status != 503 || ev[0].Origin != emit.OriginNode {
		t.Errorf("listener requests = %+v, want one 503 from the Node", ev)
	}
	if c := k.node.Responses.Codes(); !slices.Equal(c, []string{codeFull}) {
		t.Errorf("node responses = %v, want [RZ-RT-005]", c)
	}
	if s := k.plan.RouteHandles(unmatchedRoute).Requests.Statuses(); !slices.Equal(s, []int{503}) {
		t.Errorf("_unmatched requests = %v, want [503]", s)
	}
}

// TestNoPublishedSnapshot checks that a request finding no published
// snapshot gets 503 RZ-RT-016 without an access record.
func TestNoPublishedSnapshot(t *testing.T) {
	k := newKit(t)
	res := k.get("/")
	wantProblem(t, res, http.StatusServiceUnavailable, codeDrain)
	if len(k.alog.Records()) != 0 || k.units.InUse() != 0 {
		t.Errorf("records %d, units %d, want 0 and 0", len(k.alog.Records()), k.units.InUse())
	}
}

// TestDrainEndsRequestAtPin checks that a request pinning after the Drain
// deadline ends at once: 503 RZ-RT-016 without any work.
func TestDrainEndsRequestAtPin(t *testing.T) {
	k := newKit(t)
	var called atomic.Bool
	k.publish(handlertest.Spec{Routes: []handlertest.Route{route("all", handlertest.ForwardFunc(func(context.Context, *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		called.Store(true)
		return nil, errors.New("unreachable")
	}))}})
	if _, err := k.holder.EndAll(t.Context(), snapshot.EndDrain); err != nil {
		t.Fatalf("EndAll: %v", err)
	}
	res := k.get("/")
	wantProblem(t, res, http.StatusServiceUnavailable, codeDrain)
	if called.Load() {
		t.Error("an ended request reached the Forwarder")
	}
	if rec := k.record(); rec.Code != codeDrain || rec.Status != 503 {
		t.Errorf("record = %d %s, want 503 RZ-RT-016", rec.Status, rec.Code)
	}
}

// TestEndedWhileForwarding checks the ending protocol before commit: a
// request ended while its leg runs answers 503 with the end code, not its
// own cancellation error.
func TestEndedWhileForwarding(t *testing.T) {
	k := newKit(t)
	k.publish(handlertest.Spec{Routes: []handlertest.Route{route("all", handlertest.ForwardFunc(func(ctx context.Context, _ *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		if _, err := k.holder.EndAll(context.Background(), snapshot.EndDrain); err != nil {
			t.Errorf("EndAll: %v", err)
		}
		<-ctx.Done()
		return nil, errcode.Wrap("RZ-UP-003", ctx.Err())
	}))}})
	res := k.get("/")
	wantProblem(t, res, http.StatusServiceUnavailable, codeDrain)
	if rec := k.record(); rec.Code != codeDrain {
		t.Errorf("record code = %s, want RZ-RT-016", rec.Code)
	}
}

// TestRouteTimeoutCancelsContext checks that the Route timeout, counted
// from the request start on the handler's clock, cancels the request
// context with context.DeadlineExceeded.
func TestRouteTimeoutCancelsContext(t *testing.T) {
	k := newKit(t)
	entered := make(chan struct{})
	var cause atomic.Value
	k.publish(handlertest.Spec{Routes: []handlertest.Route{{Name: "slow", Timeout: 2 * time.Second, Forward: handlertest.ForwardFunc(func(ctx context.Context, _ *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		close(entered)
		<-ctx.Done()
		cause.Store(context.Cause(ctx))
		return nil, errcode.Wrap("RZ-UP-003", context.Cause(ctx))
	})}}})
	done := make(chan result, 1)
	go func() { done <- k.get("/") }()
	<-entered
	k.clk.Advance(2 * time.Second)
	res := <-done
	wantProblem(t, res, http.StatusGatewayTimeout, "RZ-UP-003")
	if c, _ := cause.Load().(error); !errors.Is(c, context.DeadlineExceeded) {
		t.Errorf("context cause = %v, want context.DeadlineExceeded", c)
	}
}

// TestDeadlines checks the connection deadlines: 10 s from admission, then
// the Route timeout from the request start once the Route is fixed.
func TestDeadlines(t *testing.T) {
	k := newKit(t)
	k.publish(handlertest.Spec{Routes: []handlertest.Route{{Name: "r", Timeout: 5 * time.Second, Forward: ok()}}})
	w := newRecordingWriter()
	k.h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	want := []time.Time{epoch().Add(AdmissionWindow), epoch().Add(5 * time.Second)}
	if !slices.EqualFunc(w.reads, want, time.Time.Equal) || !slices.EqualFunc(w.writes, want, time.Time.Equal) {
		t.Errorf("read deadlines %v, write deadlines %v, want both %v", w.reads, w.writes, want)
	}
}

// TestShortCircuitRunsResponsePhases checks a request Phase short-circuit:
// the Forwarder never runs, onResponse runs on the generated response,
// whose problem document carries the Filter's code and fields, counted as
// a Node response.
func TestShortCircuitRunsResponsePhases(t *testing.T) {
	k := newKit(t)
	deny := filterFunc(func(context.Context, phase.Phase, filter.Exchange) filter.Result {
		return filter.Deny(http.StatusUnauthorized, "RZ-AUTH-001", http.Header{"Www-Authenticate": {"Bearer"}})
	})
	var forwarded atomic.Bool
	fwd := handlertest.ForwardFunc(func(context.Context, *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		forwarded.Store(true)
		return nil, errors.New("unreachable")
	})
	k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", fwd,
		handlertest.Policy("cors", phase.ClassCORS, v1alpha1.PolicyTypeCORS, header("X-Resp", "1"), phase.OnResponse),
		handlertest.Policy("jwt", phase.ClassAuth, v1alpha1.PolicyTypeAuthJWT, deny, phase.OnRequestHeaders),
	)}})
	res := k.get("/")
	wantProblem(t, res, http.StatusUnauthorized, "RZ-AUTH-001")
	if res.Header().Get("Www-Authenticate") != "Bearer" || res.Header().Get("X-Resp") != "1" || res.Header().Get("Date") == "" {
		t.Errorf("fields = %v, want the challenge, onResponse's X-Resp and the Node's Date", res.Header())
	}
	if forwarded.Load() {
		t.Error("a short-circuited request reached the Forwarder")
	}
	if c := k.node.Responses.Codes(); !slices.Equal(c, []string{"RZ-AUTH-001"}) {
		t.Errorf("node responses = %v, want [RZ-AUTH-001]", c)
	}
	if rec := k.record(); rec.ShortCircuitPolicy != "jwt" || rec.Code != "RZ-AUTH-001" || rec.Status != 401 {
		t.Errorf("record = %+v, want the jwt short-circuit 401 RZ-AUTH-001", rec)
	}
}

// TestLegFailureProblem checks a leg that ended without a response: the
// problem document of its code at its status, the access record naming the
// leg's Upstream, Endpoint and attempts, and origin dependency.
func TestLegFailureProblem(t *testing.T) {
	k := newKit(t)
	fwd := handlertest.ForwardFunc(func(context.Context, *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		return nil, &upstream.LegError{Upstream: "orders", Endpoint: "10.0.0.1:80", Attempts: 3, Err: errcode.Wrap("RZ-UP-003", errors.New("timeout"))}
	})
	k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", fwd)}})
	res := k.get("/")
	wantProblem(t, res, http.StatusGatewayTimeout, "RZ-UP-003")
	if rec := k.record(); rec.Upstream != "orders" || rec.Endpoint != "10.0.0.1:80" || rec.Attempts != 3 || rec.Code != "RZ-UP-003" {
		t.Errorf("record = %+v, want orders 10.0.0.1:80 3 attempts RZ-UP-003", rec)
	}
	if ev := k.plan.ListenerHandles(handlertest.ListenerName).Requests.Events(); len(ev) != 1 || ev[0].Origin != emit.OriginDependency {
		t.Errorf("listener requests = %+v, want origin dependency", ev)
	}
}

// staleServer is a cache-class Substituter serving a stored response in
// place of an Upstream failure.
type staleServer struct{}

func (staleServer) Handle(context.Context, phase.Phase, filter.Exchange) filter.Result {
	return filter.Next()
}
func (staleServer) Substitute(context.Context, filter.Exchange) *filter.Response {
	return &filter.Response{Status: http.StatusOK, Header: http.Header{"X-Cache": {"stale"}}, Body: []byte("stale"), Cached: true}
}

// TestUpstreamFailureKind checks which Node-generated responses the
// Response Cache may replace: an RZ-UP failure is an Upstream failure it
// serves stale over, a Route timeout before any attempt is not.
func TestUpstreamFailureKind(t *testing.T) {
	for _, tt := range []struct {
		code   string
		status int
		body   string
	}{
		{"RZ-UP-005", http.StatusOK, "stale"},
		{"RZ-RT-007", http.StatusGatewayTimeout, ""},
	} {
		t.Run(tt.code, func(t *testing.T) {
			k := newKit(t)
			fwd := handlertest.ForwardFunc(func(context.Context, *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
				return nil, errcode.Wrap(tt.code, errors.New("leg"))
			})
			k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", fwd,
				handlertest.Policy("cache", phase.ClassCache, v1alpha1.PolicyTypeCache, staleServer{}, phase.OnRequestHeaders, phase.OnResponse))}})
			res := k.get("/")
			if res.Code != tt.status {
				t.Fatalf("status = %d, want %d", res.Code, tt.status)
			}
			if tt.body != "" && res.Body.String() != tt.body {
				t.Errorf("body = %q, want %q", res.Body.String(), tt.body)
			}
		})
	}
}

// TestRequestBodyTooLarge checks a declared Content-Length over
// limits.maxRequestBodyBytes: 413 RZ-RT-003 at once, the body unread.
func TestRequestBodyTooLarge(t *testing.T) {
	k := newKit(t)
	k.publish(handlertest.Spec{Limits: snapshot.Limits{MaxRequestBodyBytes: 100}, Routes: []handlertest.Route{route("r", ok())}})
	b := &countingReader{r: strings.NewReader(strings.Repeat("x", 200))}
	r := httptest.NewRequest(http.MethodPost, "/", b)
	r.ContentLength = 200
	res := k.do(r)
	wantProblem(t, res, http.StatusRequestEntityTooLarge, codeBodyTooLarge)
	if n := b.reads.Load(); n != 0 {
		t.Errorf("body reads = %d, want none before refusing the declared length", n)
	}
}

// countingReader counts its reads.
type countingReader struct {
	r     io.Reader
	reads atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	c.reads.Add(1)
	return c.r.Read(p)
}

// mustPrefixes parses CIDR prefixes.
func mustPrefixes(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// TestStreamedBodyCutWhileForwarding checks a streamed body that passes
// its limit while the leg sends it: 413 RZ-RT-003 before commit.
func TestStreamedBodyCutWhileForwarding(t *testing.T) {
	k := newKit(t)
	fwd := handlertest.ForwardFunc(func(_ context.Context, o *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		rd, err := o.Body.Open()
		if err != nil {
			return nil, err
		}
		_, err = io.Copy(io.Discard, rd)
		return nil, &upstream.LegError{Upstream: "orders", Err: errcode.Wrap(codeBodyTooLarge, err)}
	})
	k.publish(handlertest.Spec{Limits: snapshot.Limits{MaxRequestBodyBytes: 100}, Routes: []handlertest.Route{route("r", fwd)}})
	r := httptest.NewRequest(http.MethodPut, "/", strings.NewReader(strings.Repeat("x", 1000)))
	r.ContentLength = -1
	res := k.do(r)
	wantProblem(t, res, http.StatusRequestEntityTooLarge, codeBodyTooLarge)
}

// TestWebSocketUpgradeRejected checks a WebSocket upgrade on an http
// Route: 400 RZ-RT-023 after the match and before onRequestHeaders, with
// the response Phases run on it.
func TestWebSocketUpgradeRejected(t *testing.T) {
	k := newKit(t)
	var requestPhase atomic.Bool
	k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", ok(),
		handlertest.Policy("cors", phase.ClassCORS, v1alpha1.PolicyTypeCORS, header("X-Resp", "1"), phase.OnResponse),
		handlertest.Policy("auth", phase.ClassAuth, v1alpha1.PolicyTypeAuthJWT, filterFunc(func(context.Context, phase.Phase, filter.Exchange) filter.Result {
			requestPhase.Store(true)
			return filter.Next()
		}), phase.OnRequestHeaders),
	)}})
	r := httptest.NewRequest(http.MethodGet, "/chat", nil)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "WebSocket")
	res := k.do(r)
	wantProblem(t, res, http.StatusBadRequest, codeWebSocket)
	if res.Header().Get("X-Resp") != "1" || requestPhase.Load() {
		t.Errorf("onResponse ran %t, onRequestHeaders ran %t, want true and false", res.Header().Get("X-Resp") == "1", requestPhase.Load())
	}
	other := httptest.NewRequest(http.MethodGet, "/", nil)
	other.Header.Set("Upgrade", "h2c")
	if res := k.do(other); res.Code != http.StatusOK {
		t.Errorf("Upgrade: h2c status = %d, want 200 served as HTTP/1.1", res.Code)
	}
}

// TestUnservedProtocolIsFault checks that a Route whose Upstream protocol
// M1 does not serve answers 500 RZ-RT-025.
func TestUnservedProtocolIsFault(t *testing.T) {
	k := newKit(t)
	k.publish(handlertest.Spec{Routes: []handlertest.Route{{Name: "g", Protocol: v1alpha1.UpstreamProtocolGRPC, Forward: ok()}}})
	wantProblem(t, k.get("/"), http.StatusInternalServerError, codeFault)
}

// TestStripsConnectionOptions checks that the end-to-end fields a client's
// Connection field names never reach a Policy or the Upstream.
func TestStripsConnectionOptions(t *testing.T) {
	k := newKit(t)
	var hdr http.Header
	fwd := handlertest.ForwardFunc(func(_ context.Context, o *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		hdr = o.X.Header().Clone()
		return &snapshot.UpstreamResponse{Status: 200, Header: http.Header{}, Body: http.NoBody}, nil
	})
	k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", fwd)}})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Connection", "X-Secret")
	r.Header.Set("X-Secret", "1")
	r.Header.Set("X-Keep", "1")
	k.do(r)
	if hdr.Get("X-Secret") != "" || hdr.Get("Connection") != "" || hdr.Get("X-Keep") != "1" {
		t.Errorf("Forwarder header = %v, want X-Keep only", hdr)
	}
}

// TestTrustedProxySource checks the client address behind a trusted
// proxy: source.ip from X-Forwarded-For, written in the access record.
func TestTrustedProxySource(t *testing.T) {
	k := newKit(t)
	k.publish(handlertest.Spec{TrustedProxies: mustPrefixes(t, "192.0.2.0/24"), Routes: []handlertest.Route{route("r", ok())}})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	k.do(r)
	if rec := k.record(); rec.ClientAddress != "203.0.113.9" {
		t.Errorf("client address = %q, want 203.0.113.9", rec.ClientAddress)
	}
}

// TestRequestTrailersReachForwarder checks that the client's declared
// trailer section reaches the Forwarder.
func TestRequestTrailersReachForwarder(t *testing.T) {
	k := newKit(t)
	var tr http.Header
	fwd := handlertest.ForwardFunc(func(_ context.Context, o *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		tr = o.Trailer
		return &snapshot.UpstreamResponse{Status: 200, Header: http.Header{}, Body: http.NoBody}, nil
	})
	k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", fwd)}})
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("x"))
	r.Trailer = http.Header{"X-Sum": nil}
	k.do(r)
	if _, ok := tr["X-Sum"]; !ok {
		t.Errorf("Outbound.Trailer = %v, want the client's declared X-Sum", tr)
	}
}

// TestHTTP10ClientGetsNoHints checks that an HTTP/1.0 client, which must
// not receive a 1xx, gets no Early Hints relay and the 1.0 Via entry.
func TestHTTP10ClientGetsNoHints(t *testing.T) {
	k := newKit(t)
	var got snapshot.Outbound
	fwd := handlertest.ForwardFunc(func(_ context.Context, o *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		got = *o
		return &snapshot.UpstreamResponse{Status: 200, Header: http.Header{}, Body: http.NoBody}, nil
	})
	k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", fwd)}})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Proto, r.ProtoMajor, r.ProtoMinor = "HTTP/1.0", 1, 0
	k.do(r)
	if got.Hints != nil || got.Via != "1.0 ruralz" {
		t.Errorf("Outbound Hints %v Via %q, want none and 1.0 ruralz", got.Hints, got.Via)
	}
}

// TestFinalOnLog checks what onLog and Finish see: the committed status,
// code and Committed.
func TestFinalOnLog(t *testing.T) {
	k := newKit(t)
	var final filter.Final
	logger := &finisher{f: func(x filter.Exchange) { final = x.Final() }}
	k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", ok(),
		handlertest.Policy("quota", phase.ClassAdmission, v1alpha1.PolicyTypeQuota, logger, phase.OnRequestHeaders, phase.OnLog))}})
	k.get("/")
	if final.Status != http.StatusOK || !final.Committed || final.Code != "" || final.LegStatus != http.StatusOK {
		t.Errorf("Final = %+v, want 200 committed from a 200 leg", final)
	}
}

// finisher is a Filter whose Finish calls f.
type finisher struct{ f func(filter.Exchange) }

func (*finisher) Handle(context.Context, phase.Phase, filter.Exchange) filter.Result {
	return filter.Next()
}
func (f *finisher) Finish(_ context.Context, x filter.Exchange) { f.f(x) }

// TestClientGoneBeforeResponse checks a client that left while its leg
// ran: nothing is written, the request aborts, and the access record has
// status 0.
func TestClientGoneBeforeResponse(t *testing.T) {
	k := newKit(t)
	ctx, cancel := context.WithCancel(context.Background())
	fwd := handlertest.ForwardFunc(func(c context.Context, _ *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		cancel()
		<-c.Done()
		return nil, errcode.Wrap("RZ-UP-003", c.Err())
	})
	k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", fwd)}})
	res := k.do(httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))
	if !res.aborted || res.Body.Len() != 0 {
		t.Errorf("aborted %t, body %q, want an abort writing nothing", res.aborted, res.Body.String())
	}
	if rec := k.record(); rec.Status != 0 {
		t.Errorf("record status = %d, want 0", rec.Status)
	}
	if s := k.plan.RouteHandles("r").Requests.Statuses(); len(s) != 0 {
		t.Errorf("route statuses = %v, want none without a status", s)
	}
}
