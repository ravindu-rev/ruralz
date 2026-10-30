// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package forward

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/balance"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/discovery"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/health"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// cest is a zone east of UTC: Debug writes every time in UTC.
func cest() *time.Location { return time.FixedZone("CEST", 2*60*60) }

// sampleStatus returns runtimes as upstream.Manager.Status could report
// them, out of name order, one of them not live, and the body Debug must
// make of them.
func sampleStatus() ([]upstream.Status, adminapi.Upstreams) {
	at := func(h, m, s int) time.Time { return time.Date(2026, 9, 26, h, m, s, 0, cest()) }
	str := func(s string) *string { return &s }
	st := []upstream.Status{
		{
			Name: "orders", Live: true,
			Source:  discovery.Status{Type: "static"},
			Breaker: resilience.BreakerStats{State: resilience.StateHalfOpen, OpenedAt: at(13, 58, 0)},
			Balancers: []balance.Info{{
				Algorithm: balance.LeastRequest, Effective: balance.LeastRequest, Built: at(13, 0, 0),
			}},
		},
		{Name: "audit", Source: discovery.Status{Type: "static"}},
		{
			Name: "inventory", Live: true,
			Source: discovery.Status{
				Type: "dns", LastRefresh: at(13, 59, 30), Stale: true, Failures: 2,
				Err: errors.New("lookup inventory.internal: no such host"),
			},
			Breaker: resilience.BreakerStats{
				State: resilience.StateOpen, OpenedAt: at(13, 59, 58).Add(250 * time.Millisecond), OpenUntil: at(14, 0, 26),
				WindowLegs: 24, WindowFailures: 13,
			},
			InFlight: 3, Pending: 1, RetryOriginals: 3, RetryRetries: 1,
			Balancers: []balance.Info{{
				Algorithm: balance.RingHash, Effective: balance.Random, VirtualNodes: 0, Fallback: true,
				Bytes: 16, Endpoints: 2, Built: at(13, 59, 30), Stale: true,
			}},
			Endpoints: []upstream.EndpointStatus{
				{
					EndpointStatus: health.EndpointStatus{
						Identity: "inventory-0.internal:8080", Healthy: true, Ejected: true, EjectedUntil: at(14, 0, 30),
						Ejections: 1, Failures1s: 2,
					},
					Weight: 3, Addresses: []string{"10.0.0.7:8080", "[fd00::7]:8080"},
				},
				{
					EndpointStatus: health.EndpointStatus{
						Identity: "inventory-1.internal:8080", ConsecutiveErrors: 1, Suspect: true, Stalled: 1,
					},
					Weight: 1, InFlight: 3,
				},
			},
			Panic: true,
		},
	}
	want := adminapi.Upstreams{Upstreams: []adminapi.UpstreamInfo{
		{
			Name: "inventory",
			Source: adminapi.SourceInfo{
				Type: adminapi.SourceDNS, LastRefresh: str("2026-09-26T11:59:30Z"), Stale: true, Failures: 2,
				Error: str("lookup inventory.internal: no such host"),
			},
			Breaker: adminapi.BreakerInfo{
				State: adminapi.BreakerOpen, OpenedAt: str("2026-09-26T11:59:58.25Z"), OpenUntil: str("2026-09-26T12:00:26Z"),
				WindowLegs: 24, WindowFailures: 13,
			},
			InFlight: 3, Pending: 1,
			RetryBudget: adminapi.RetryBudgetInfo{Originals: 3, Retries: 1},
			Panic:       true,
			Balancers: []adminapi.BalancerInfo{{
				Algorithm: "ring-hash", Effective: "random", Fallback: true, Bytes: 16, Endpoints: 2,
				Built: str("2026-09-26T11:59:30Z"), Stale: true,
			}},
			Endpoints: []adminapi.EndpointInfo{
				{
					Identity: "inventory-0.internal:8080", Weight: 3, Healthy: true, Ejected: true,
					EjectedUntil: str("2026-09-26T12:00:30Z"), Ejections: 1, Failures1s: 2,
					Addresses: []string{"10.0.0.7:8080", "[fd00::7]:8080"},
				},
				{
					Identity: "inventory-1.internal:8080", Weight: 1, ConsecutiveErrors: 1, Suspect: true, Stalled: 1,
					InFlight: 3, Addresses: []string{},
				},
			},
		},
		{
			Name:    "orders",
			Source:  adminapi.SourceInfo{Type: adminapi.SourceStatic},
			Breaker: adminapi.BreakerInfo{State: adminapi.BreakerHalfOpen, OpenedAt: str("2026-09-26T11:58:00Z")},
			Balancers: []adminapi.BalancerInfo{{
				Algorithm: "least-request", Effective: "least-request", Built: str("2026-09-26T11:00:00Z"),
			}},
			Endpoints: []adminapi.EndpointInfo{},
		},
	}}
	return st, want
}

// TestDebug checks the /debug/upstreams body Debug makes of
// upstream.Manager.Status (docs/architecture/03-data-plane.md "Admin
// endpoints"): only the runtimes a live snapshot holds, in name order;
// every member of the source, breaker, bulkhead, retry budget, panic mode,
// balancers and Endpoints; times in RFC 3339 UTC and null when unset; the
// refresh error's text or null; lists [] when empty.
func TestDebug(t *testing.T) {
	st, want := sampleStatus()
	got := Debug(st)
	if !reflect.DeepEqual(got, want) {
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(want)
		t.Fatalf("Debug =\n%s\nwant\n%s", gb, wb)
	}
	// The body holds copies: changing the statuses afterwards changes it not.
	st[2].Endpoints[0].Addresses[0] = "10.9.9.9:8080"
	if a := got.Upstreams[0].Endpoints[0].Addresses[0]; a != "10.0.0.7:8080" {
		t.Errorf("addresses follow the status slice: %s", a)
	}
	if empty := Debug(nil); empty.Upstreams == nil || len(empty.Upstreams) != 0 {
		t.Errorf("Debug(nil) = %#v, want an empty, non-nil list", empty)
	}
}

// TestDebugBreakerStates checks the breaker state names of the body.
func TestDebugBreakerStates(t *testing.T) {
	for _, tc := range []struct {
		state resilience.State
		want  string
	}{
		{resilience.StateClosed, adminapi.BreakerClosed},
		{resilience.StateOpen, adminapi.BreakerOpen},
		{resilience.StateHalfOpen, adminapi.BreakerHalfOpen},
	} {
		if got := breakerState(tc.state); got != tc.want || got != tc.state.String() {
			t.Errorf("breakerState(%v) = %q, want %q", tc.state, got, tc.want)
		}
	}
}

// statuses is a fixed StatusSource.
type statuses []upstream.Status

func (s statuses) Status() []upstream.Status { return s }

// serveDebug runs the /debug/upstreams handler over src.
func serveDebug(t *testing.T, src StatusSource) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	DebugHandler(src).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/debug/upstreams", http.NoBody))
	return rec
}

// TestDebugHandler checks the /debug/upstreams handler: 200 with the Debug
// body as uncached application/json and its Content-Length, and
// {"upstreams":[]} when no live snapshot holds an Upstream.
func TestDebugHandler(t *testing.T) {
	st, want := sampleStatus()
	rec := serveDebug(t, statuses(st))
	wantBody, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	h := rec.Header()
	if rec.Code != http.StatusOK || h.Get("Content-Type") != "application/json" || h.Get("Cache-Control") != "no-store" ||
		h.Get("Content-Length") != strconv.Itoa(len(wantBody)) {
		t.Errorf("response = %d %v, want 200 with uncached application/json of %d bytes", rec.Code, h, len(wantBody))
	}
	if !bytes.Equal(rec.Body.Bytes(), wantBody) {
		t.Errorf("body =\n%s\nwant\n%s", rec.Body.Bytes(), wantBody)
	}
	for _, src := range []StatusSource{statuses(nil), statuses{{Name: "compiling", Source: discovery.Status{}}}} {
		if body := serveDebug(t, src).Body.String(); body != `{"upstreams":[]}` {
			t.Errorf("body without a live Upstream = %s, want {\"upstreams\":[]}", body)
		}
	}
}

// debugBody serves /debug/upstreams over the fixture's Manager and decodes
// the body; raw is the body as served.
func (f *fixture) debugBody() (body adminapi.Upstreams, raw string) {
	f.t.Helper()
	rec := serveDebug(f.t, f.m)
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		f.t.Fatalf("decode %s: %v", rec.Body.Bytes(), err)
	}
	return body, rec.Body.String()
}

// TestDebugLiveManager checks /debug/upstreams over a real Manager: no
// Upstream before the snapshot holding them activates; then each
// Upstream's live state, here a refused Endpoint ejected for ejectionTime,
// panic mode and an open breaker beside a healthy Upstream; and no
// Upstream once the snapshot closed.
func TestDebugLiveManager(t *testing.T) {
	srv := newServer(t, http.StatusOK, "b")
	fx := newFixture(t)
	a := breakerAfterOne(newUpstream("a", refusedAddr))
	a.Spec.HealthCheck = &v1alpha1.HealthCheck{Passive: &v1alpha1.PassiveHealthCheck{ConsecutiveErrors: ptr[int32](1)}}
	s := fx.compile(a, newUpstream("b", srv.addr))
	if _, raw := fx.debugBody(); raw != `{"upstreams":[]}` {
		t.Fatalf("body before activation = %s, want no Upstream", raw)
	}
	s.Activate()
	ua, _ := s.Upstream("a")
	if _, err := ua.Leg(t.Context(), outbound(nil), upstream.Target{}); codeOf(err) != "RZ-UP-001" {
		t.Fatalf("leg to the refused Endpoint = %v, want RZ-UP-001", err)
	}

	body, _ := fx.debugBody()
	if len(body.Upstreams) != 2 || body.Upstreams[0].Name != "a" || body.Upstreams[1].Name != "b" {
		t.Fatalf("Upstreams = %+v, want a and b", body.Upstreams)
	}
	at := func(d time.Duration) string { return epoch().Add(d).Format(time.RFC3339Nano) }
	ga, gb := body.Upstreams[0], body.Upstreams[1]
	if ga.Source.Type != adminapi.SourceStatic || ga.Source.Error != nil || !ga.Panic {
		t.Errorf("a source %+v, panic %v; want static without error, panic", ga.Source, ga.Panic)
	}
	if br := ga.Breaker; br.State != adminapi.BreakerOpen || br.OpenedAt == nil || *br.OpenedAt != at(0) || br.OpenUntil == nil ||
		br.WindowLegs != 1 || br.WindowFailures != 1 {
		t.Errorf("a breaker = %+v, want open at %s with an open-until time and 1 failed leg of 1", br, at(0))
	}
	if len(ga.Endpoints) != 1 {
		t.Fatalf("a Endpoints = %+v, want one", ga.Endpoints)
	}
	if e := ga.Endpoints[0]; e.Identity != refusedAddr || e.Weight != 1 || !e.Healthy || !e.Ejected || e.EjectedUntil == nil ||
		*e.EjectedUntil != at(30*time.Second) || e.Ejections != 1 || len(e.Addresses) != 1 || e.Addresses[0] != refusedAddr {
		t.Errorf("a Endpoint = %+v, want %s weight 1 ejected until %s (30 s ejectionTime), 1 ejection, its address", e, refusedAddr, at(30*time.Second))
	}
	if len(ga.Balancers) != 1 || ga.Balancers[0].Algorithm != "least-request" || ga.Balancers[0].Built == nil || *ga.Balancers[0].Built != at(0) {
		t.Errorf("a balancers = %+v, want one least-request built at %s", ga.Balancers, at(0))
	}
	if gb.Breaker.State != adminapi.BreakerClosed || gb.Breaker.OpenedAt != nil || gb.Panic || len(gb.Endpoints) != 1 ||
		gb.Endpoints[0].Ejected || gb.Endpoints[0].EjectedUntil != nil || gb.Endpoints[0].Addresses[0] != srv.addr {
		t.Errorf("b = %+v, want a closed breaker and one healthy Endpoint at %s", gb, srv.addr)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, raw := fx.debugBody(); raw != `{"upstreams":[]}` {
		t.Errorf("body after the snapshot closed = %s, want no Upstream", raw)
	}
}
